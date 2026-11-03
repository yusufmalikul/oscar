// Package engine runs Oscar: the Captain session, one worker per task, a fresh reviewer per
// review round, and every decision that waits for the user. All rules about what may happen
// next live here, in code; Claude only reports through tools.
package engine

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"oscar/internal/claude"
	"oscar/internal/config"
	"oscar/internal/redact"
	"oscar/internal/store"

	"gopkg.in/yaml.v3"
)

//go:embed types/*.yaml
var builtinTypes embed.FS

//go:embed prompts/*.md
var promptFS embed.FS

// TaskType is one task type definition (types/*.yaml).
type TaskType struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Code        bool     `yaml:"code" json:"code"`
	Reviewer    bool     `yaml:"reviewer" json:"reviewer"`
	Output      string   `yaml:"output" json:"output"` // draft_pr | report | outbox
	Steps       []string `yaml:"steps" json:"steps"`
	Guide       string   `yaml:"guide" json:"guide"`
	ReviewFocus string   `yaml:"review_focus" json:"review_focus"`
}

func (t *TaskType) validate() error {
	if t.Name == "" || len(t.Steps) == 0 {
		return errors.New("a type needs a name and steps")
	}
	switch t.Output {
	case "draft_pr", "report", "outbox":
	default:
		return fmt.Errorf("output must be draft_pr, report or outbox, not %q", t.Output)
	}
	if t.Output == "draft_pr" && !t.Code {
		return errors.New("output draft_pr needs code: true")
	}
	return nil
}

// Event is pushed to UI subscribers.
type Event struct {
	Kind   string `json:"kind"` // state | log | chat
	TaskID string `json:"task_id,omitempty"`
	Data   any    `json:"data,omitempty"`
}

type decision struct {
	allow bool
	note  string
}

type Engine struct {
	cfg config.Config
	st  *store.Store
	ctx context.Context

	mu        sync.Mutex
	tasks     map[string]*store.Task
	types     map[string]*TaskType
	workers   map[string]*claude.Session
	reviewers map[string]*claude.Session
	waiting   map[string]bool          // task id → worker ended its turn on purpose
	waiters   map[string]chan decision // pending id → blocked tool call
	planCmds  map[string][]string      // task id → commands approved in the ops plan
	finished  map[string]bool          // task id → worker called finish
	lastText  map[string]string        // task id → last worker text (for escalations)
	capPend   []store.Pending          // gated actions of the Captain
	captain   *claude.Session
	capBusy   bool
	subs      map[chan Event]struct{}
	repoNames []string
	updating  string // current self-update step, "" when idle
	stopping  bool   // Shutdown started: sessions that end now are not escalated
	allow     []AllowRule
	version   string

	// Restart is set by the server: it shuts down and exits with ExitRestart.
	Restart func()
}

func New(ctx context.Context, cfg config.Config) (*Engine, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, st: st, ctx: ctx,
		tasks: map[string]*store.Task{}, types: map[string]*TaskType{},
		workers: map[string]*claude.Session{}, reviewers: map[string]*claude.Session{},
		waiting: map[string]bool{}, waiters: map[string]chan decision{}, planCmds: map[string][]string{},
		finished: map[string]bool{}, lastText: map[string]string{}, subs: map[chan Event]struct{}{},
	}
	_ = st.Value(allowFile, &e.allow)
	if err := e.loadTypes(); err != nil {
		return nil, err
	}
	e.repoNames = listRepos(cfg.ReposDir)
	for name, p := range cfg.ExtraRepos {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil && !contains(e.repoNames, name) {
			e.repoNames = append(e.repoNames, name)
		}
	}
	sort.Strings(e.repoNames)
	ts, err := st.LoadTasks()
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		e.tasks[t.ID] = t
	}
	return e, nil
}

func (e *Engine) loadTypes() error {
	load := func(fsys fs.FS, dir string) error {
		files, err := fs.Glob(fsys, filepath.Join(dir, "*.yaml"))
		if err != nil {
			return err
		}
		for _, f := range files {
			b, err := fs.ReadFile(fsys, f)
			if err != nil {
				return err
			}
			var t TaskType
			if err := yaml.Unmarshal(b, &t); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			if err := t.validate(); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			e.types[t.Name] = &t
		}
		return nil
	}
	if err := load(builtinTypes, "types"); err != nil {
		return err
	}
	if _, err := os.Stat(e.cfg.TypesDir); err == nil {
		return load(os.DirFS(e.cfg.TypesDir), ".")
	}
	return nil
}

func listRepos(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, en := range entries {
		if !en.IsDir() || strings.HasPrefix(en.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, en.Name(), ".git")); err == nil {
			out = append(out, en.Name())
		}
	}
	return out
}

// Start resumes work after a restart and starts the Captain.
func (e *Engine) Start() {
	e.mu.Lock()
	var resume []*store.Task
	for _, t := range e.tasks {
		// Blocked tool calls died with the old processes.
		var keep []store.Pending
		for _, p := range t.Pending {
			if p.Kind != store.PendingTool {
				keep = append(keep, p)
			}
		}
		if len(keep) != len(t.Pending) {
			t.Pending = keep
			if t.Status == store.StatusNeedsYou && len(keep) == 0 {
				t.Status = store.StatusRunning
			}
		}
		if t.Status == store.StatusRunning || t.Status == store.StatusReview {
			resume = append(resume, t)
		}
		e.save(t)
	}
	e.mu.Unlock()
	for _, t := range resume {
		if t.Status == store.StatusReview {
			go e.startReviewer(t.ID, "")
		} else {
			go e.resumeWorker(t.ID, "Oscar was restarted. Continue the task from where you were. If a command you started was not run, run it again if it is still needed.")
		}
	}
	e.version = e.Version()
	e.ReportUpdate()
	go e.schedule()
	go func() {
		if err := e.startCaptain(); err != nil {
			e.chat("oscar", "The Captain could not start: "+err.Error())
		}
	}()
}

// ---------- subscriptions ----------

func (e *Engine) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 64)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

// publish must be called with e.mu held.
func (e *Engine) publish(ev Event) {
	for ch := range e.subs {
		select {
		case ch <- ev:
		default: // slow client; it will resync on the next state event
		}
	}
}

// save persists t and tells the UI. Must be called with e.mu held.
func (e *Engine) save(t *store.Task) {
	if err := e.st.SaveTask(t); err != nil {
		log.Printf("save %s: %v", t.ID, err)
	}
	e.publish(Event{Kind: "state"})
}

func (e *Engine) logLine(taskID, who, kind, text string) {
	l := store.LogLine{At: time.Now(), Who: who, Kind: kind, Text: redact.String(text)}
	_ = e.st.AppendLog(taskID, l)
	e.mu.Lock()
	e.publish(Event{Kind: "log", TaskID: taskID, Data: l})
	e.mu.Unlock()
}

func (e *Engine) chat(from, text string) {
	m := store.ChatMessage{At: time.Now(), From: from, Text: redact.String(text)}
	_ = e.st.AppendChat(m)
	e.mu.Lock()
	e.publish(Event{Kind: "chat", Data: m})
	e.mu.Unlock()
}

// ---------- snapshot for the UI ----------

type State struct {
	Tasks      []*store.Task        `json:"tasks"`
	Types      map[string]*TaskType `json:"types"`
	Chat       []store.ChatMessage  `json:"chat"`
	CapPending []store.Pending      `json:"captain_pending"`
	Health     map[string]any       `json:"health"`
}

func (e *Engine) Snapshot() State {
	chat, _ := e.st.ReadChat(300)
	e.mu.Lock()
	defer e.mu.Unlock()
	ts := make([]*store.Task, 0, len(e.tasks))
	for _, t := range e.tasks {
		c := *t
		ts = append(ts, &c)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].N < ts[j].N })
	if chat == nil {
		chat = []store.ChatMessage{}
	}
	return State{Tasks: ts, Types: e.types, Chat: chat, CapPending: append([]store.Pending{}, e.capPend...),
		Health: map[string]any{
			"workers": len(e.workers), "reviewers": len(e.reviewers), "max_workers": e.cfg.MaxWorkers,
			"captain": e.captain != nil, "captain_busy": e.capBusy,
			"allow_rules": len(e.allow), "self_repo": e.cfg.Self.Repo, "updating": e.updating, "version": e.version,
		}}
}

func (e *Engine) Log(taskID string) ([]store.LogLine, error) { return e.st.ReadLog(taskID, 2000) }

// ---------- helpers ----------

func (e *Engine) task(id string) (*store.Task, error) {
	t, ok := e.tasks[id]
	if !ok {
		return nil, fmt.Errorf("no task %s", id)
	}
	return t, nil
}

func (e *Engine) stepIndex(t *store.Task, name string) int {
	ty := e.types[t.Type]
	if ty == nil {
		return t.Step
	}
	for i, s := range ty.Steps {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(name)) {
			return i
		}
	}
	return t.Step
}

func (e *Engine) setStep(t *store.Task, name string) {
	t.Step = e.stepIndex(t, name)
	t.StepNote = name
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// needsYou adds a pending decision and notifies. Must be called with e.mu held.
func (e *Engine) needsYou(t *store.Task, p store.Pending) {
	p.Created = time.Now()
	if p.ID == "" {
		p.ID = newID("p")
	}
	t.Pending = append(t.Pending, p)
	t.Status = store.StatusNeedsYou
	e.save(t)
	go e.notify(t.ID+" needs you", p.Question)
}

func (e *Engine) removePending(t *store.Task, id string) (store.Pending, bool) {
	for i, p := range t.Pending {
		if p.ID == id {
			t.Pending = append(t.Pending[:i], t.Pending[i+1:]...)
			return p, true
		}
	}
	return store.Pending{}, false
}

// settle sets the status after a pending item was resolved. Must be called with e.mu held.
func (e *Engine) settle(t *store.Task) {
	if len(t.Pending) > 0 {
		t.Status = store.StatusNeedsYou
		return
	}
	if e.finished[t.ID] && !e.outboxOpen(t) {
		t.Status = store.StatusDone
		go e.tellCaptainDone(t.ID)
		return
	}
	if e.reviewers[t.ID] != nil {
		t.Status = store.StatusReview
		return
	}
	t.Status = store.StatusRunning
}

func (e *Engine) outboxOpen(t *store.Task) bool {
	for _, o := range t.Outbox {
		if o.Status == "draft" {
			return true
		}
	}
	return false
}

func (e *Engine) notify(title, body string) {
	cmd := e.cfg.Integrations.Notify
	if len(cmd) == 0 {
		if runtime.GOOS != "darwin" {
			return
		}
		script := fmt.Sprintf("display notification %q with title %q", truncate(body, 180), "Oscar · "+title)
		_ = runCmd(e.ctx, "", nil, "osascript", "-e", script)
		return
	}
	_, _ = runIntegration(e.ctx, cmd, map[string]any{"title": "Oscar · " + title, "body": body})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Shutdown closes every Claude process. State is on disk; the next Start resumes.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.stopping = true
	var all []*claude.Session
	for _, s := range e.workers {
		all = append(all, s)
	}
	for _, s := range e.reviewers {
		all = append(all, s)
	}
	if e.captain != nil {
		all = append(all, e.captain)
	}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(s *claude.Session) { defer wg.Done(); s.Close() }(s)
	}
	wg.Wait()
}
