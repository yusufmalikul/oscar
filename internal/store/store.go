// Package store keeps Oscar's state on disk so it survives restarts.
//
// Layout under the data dir:
//
//	counter              next item number
//	chat.jsonl           Captain chat, one message per line
//	captain.json         Captain session id
//	tasks/Item-N/task.json   task state (rewritten atomically)
//	tasks/Item-N/log.jsonl   worker / reviewer activity, append-only
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status values of a task.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusReview    = "review"
	StatusNeedsYou  = "needs_you"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Pending kinds: things that wait for the user.
const (
	PendingQuestion = "question" // worker asks; answer resumes the worker
	PendingTool     = "tool"     // a gated tool call waits for approve/deny
	PendingOutbox   = "outbox"   // message to post (Slack, PR comment)
	PendingType     = "type"     // new task type proposed by the Captain
	PendingPlan     = "plan"     // ops plan to approve before running
	PendingClosePR  = "close_pr" // decline an old PR and delete its branch after a rename
	PendingEscalate = "escalate" // reviewer failed too many rounds, or the worker stopped
)

type Pending struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Question string          `json:"question"`
	Options  []string        `json:"options,omitempty"`
	Detail   string          `json:"detail,omitempty"` // plan text, command, type definition
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	OutboxID string          `json:"outbox_id,omitempty"`
	// RuleKind and RuleSubject let the user "Always allow" a gated tool call.
	RuleKind    string    `json:"rule_kind,omitempty"`
	RuleSubject string    `json:"rule_subject,omitempty"`
	Created     time.Time `json:"created"`
}

type OutboxItem struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`            // slack | pr_comment | pr_update
	Title   string    `json:"title,omitempty"` // pr_update: new title (optional)
	URL     string    `json:"url,omitempty"`   // pr_update: the PR
	Target  string    `json:"target"`
	Channel string    `json:"channel,omitempty"`
	Thread  string    `json:"thread,omitempty"`
	Repo    string    `json:"repo,omitempty"`
	PR      string    `json:"pr,omitempty"`
	Path    string    `json:"path,omitempty"`
	Line    int       `json:"line,omitempty"`
	Text    string    `json:"text"`
	Edited  bool      `json:"edited,omitempty"`
	Status  string    `json:"status"` // draft | posted | skipped | failed
	Result  string    `json:"result,omitempty"`
	Updated time.Time `json:"updated"`
}

type Verdict struct {
	Round    int       `json:"round"`
	Pass     bool      `json:"pass"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings,omitempty"`
	At       time.Time `json:"at"`
}

type Finding struct {
	Severity string `json:"severity"`
	Where    string `json:"where"`
	Problem  string `json:"problem"`
	Scenario string `json:"scenario"`
	Kept     bool   `json:"kept"`
}

type PR struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	URL    string `json:"url"`
	Title  string `json:"title"`
}

type Worktree struct {
	Repo   string `json:"repo"`   // repo name (dir under repos_dir)
	Path   string `json:"path"`   // worktree path
	Branch string `json:"branch"` // oscar/item-n-slug
	Base   string `json:"base"`   // origin/<default>
}

type Task struct {
	ID       string   `json:"id"`
	N        int      `json:"n"`
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Brief    string   `json:"brief"`
	Source   string   `json:"source,omitempty"`
	Repos    []string `json:"repos"`
	Status   string   `json:"status"`
	Step     int      `json:"step"`
	StepNote string   `json:"step_note,omitempty"`
	Round    int      `json:"round"`
	// RoundBase is Round when the task was last reopened; the round limit counts from here.
	RoundBase int    `json:"round_base,omitempty"`
	Summary   string `json:"summary,omitempty"`

	Workspace       string     `json:"workspace"`
	Worktrees       []Worktree `json:"worktrees,omitempty"`
	WorkerSession   string     `json:"worker_session,omitempty"`
	ReviewerSession string     `json:"reviewer_session,omitempty"`

	Progress []string     `json:"progress,omitempty"`
	Pending  []Pending    `json:"pending,omitempty"`
	Outbox   []OutboxItem `json:"outbox,omitempty"`
	Verdicts []Verdict    `json:"verdicts,omitempty"`
	PRs      []PR         `json:"prs,omitempty"`
	Report   string       `json:"report,omitempty"` // final report (debug / ops / review)

	ReviewPassed bool      `json:"review_passed"`
	LastActivity time.Time `json:"last_activity,omitempty"` // last log line from a worker or reviewer
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

type LogLine struct {
	At   time.Time `json:"at"`
	Who  string    `json:"who"`  // worker | reviewer | oscar
	Kind string    `json:"kind"` // text | tool | result | error | info
	Text string    `json:"text"`
}

type ChatMessage struct {
	At   time.Time `json:"at"`
	From string    `json:"from"` // user | captain | oscar
	Text string    `json:"text"`
	// Seq is the message's line number in chat.jsonl, counting from 1. It is set
	// only when reading, so it is never written to the file.
	Seq int `json:"seq,omitempty"`
}

// Store is safe for concurrent use.
type Store struct {
	dir   string
	mu    sync.Mutex
	chatN int // lines in chat.jsonl; -1 until counted
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, chatN: -1}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) TaskDir(id string) string { return filepath.Join(s.dir, "tasks", id) }

// NextID reserves the next Item-N.
func (s *Store) NextID() (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, "counter")
	n := 0
	if b, err := os.ReadFile(p); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	if err := writeAtomic(p, []byte(strconv.Itoa(n))); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("Item-%d", n), n, nil
}

func (s *Store) SaveTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Updated = time.Now()
	dir := s.TaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "task.json"), b)
}

func (s *Store) LoadTasks() ([]*Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tasks"))
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(s.dir, "tasks", e.Name(), "task.json"))
		if err != nil {
			continue
		}
		var t Task
		if json.Unmarshal(b, &t) == nil {
			out = append(out, &t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

func (s *Store) AppendLog(id string, l LogLine) error {
	return appendJSON(filepath.Join(s.TaskDir(id), "log.jsonl"), l)
}

// ReadLog returns the last max lines of a task log.
func (s *Store) ReadLog(id string, max int) ([]LogLine, error) {
	return readJSONL[LogLine](filepath.Join(s.TaskDir(id), "log.jsonl"), max)
}

// AppendChat adds a message and returns its number (its Seq).
func (s *Store) AppendChat(m ChatMessage) (int, error) {
	path := filepath.Join(s.dir, "chat.jsonl")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chatN < 0 {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		s.chatN = bytes.Count(b, []byte{'\n'})
	}
	m.Seq = 0 // the number comes from the line, it is not stored
	if err := appendJSON(path, m); err != nil {
		return 0, err
	}
	s.chatN++
	return s.chatN, nil
}

// ReadChat returns the chat from message number from (counting from 1) to the
// end, oldest first. When from <= 0 it returns only the last `last` messages.
// Each message carries its number in Seq, so the UI can ask for older ones.
func (s *Store) ReadChat(from, last int) ([]ChatMessage, error) {
	f, err := os.Open(filepath.Join(s.dir, "chat.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ChatMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if from > 0 && n < from {
			continue
		}
		var m ChatMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		m.Seq = n
		out = append(out, m)
		if from <= 0 && last > 0 && len(out) > 2*last {
			out = append(out[:0], out[len(out)-last:]...) // keep memory small on a long chat
		}
	}
	if from <= 0 && last > 0 && len(out) > last {
		out = out[len(out)-last:]
	}
	return out, sc.Err()
}

// Value reads a small JSON value file (e.g. captain.json).
func (s *Store) Value(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) SetValue(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, name), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var appendMu sync.Mutex

func appendJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	appendMu.Lock()
	defer appendMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func readJSONL[T any](path string, max int) ([]T, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var v T
		if json.Unmarshal(sc.Bytes(), &v) == nil {
			out = append(out, v)
		}
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out, sc.Err()
}
