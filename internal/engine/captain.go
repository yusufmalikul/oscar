package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"oscar/internal/claude"
	"oscar/internal/policy"
	"oscar/internal/store"

	"gopkg.in/yaml.v3"
)

// The Captain is the one long-lived session the user chats with.

func (e *Engine) startCaptain() error {
	var saved struct {
		SessionID string `json:"session_id"`
	}
	_ = e.st.Value("captain.json", &saved)
	e.mu.Lock()
	types := make([]*TaskType, 0, len(e.types))
	for _, t := range e.types {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	prompt := e.renderPrompt("captain.md", map[string]any{"Types": types, "Repos": e.repoNames, "Rules": e.cfg.Rules(), "Self": e.cfg.Self.Repo,
		"Scratch": e.captainScratch()})
	e.mu.Unlock()

	scratch := e.captainScratch()
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}
	sp := sessionSpec{role: policy.Captain, tools: e.captainTools(), prompt: prompt, resume: saved.SessionID, rc: e.cfg.Captain,
		addDirs: []string{scratch}, pctx: policy.Context{Role: policy.Captain, WriteRoots: []string{scratch}}}
	s, err := e.startSession(sp)
	if err != nil && saved.SessionID != "" {
		// A broken saved session should not block the Captain forever.
		sp.resume = ""
		s, err = e.startSession(sp)
	}
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.captain = s
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	go func() {
		<-s.Done()
		e.mu.Lock()
		if e.captain == s {
			e.captain = nil
			e.capDoing = ""
		}
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
	}()
	return nil
}

// Chat sends the user's message to the Captain.
func (e *Engine) Chat(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	e.chat("user", text)
	e.mu.Lock()
	e.lastUserChat = time.Now()
	e.mu.Unlock()
	return e.sendCaptain(text, "Captain is replying…")
}

// sendCaptain sends a message to the Captain, starting it when it is not running.
// doing is shown in the UI while the Captain works on it.
func (e *Engine) sendCaptain(text, doing string) error {
	e.mu.Lock()
	e.capDoing = doing
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	if e.captainHook != nil {
		e.captainHook(text)
		return nil
	}
	e.mu.Lock()
	s := e.captain
	e.mu.Unlock()
	if s == nil {
		if err := e.startCaptain(); err != nil {
			e.mu.Lock()
			e.capDoing = ""
			e.publish(Event{Kind: "state"})
			e.mu.Unlock()
			e.chat("oscar", "The Captain could not start: "+err.Error())
			return err
		}
		e.mu.Lock()
		s = e.captain
		e.mu.Unlock()
	}
	return s.Send(text)
}

// captainActor says on whose behalf the Captain acts now: the user's, when the user wrote in
// the chat after the Captain's last finished turn; otherwise its own.
func (e *Engine) captainActor() Actor {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastUserChat.After(e.capTurnEnded) {
		return ActorViaCaptain
	}
	return ActorCaptain
}

// captainTurnEnded records that the Captain finished a turn.
func (e *Engine) captainTurnEnded() {
	e.mu.Lock()
	e.capDoing = ""
	e.capTurnEnded = time.Now()
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
}

// relayToCaptain tells the Captain that a task needs the user, so the Captain can say it in
// the chat and pass the user's reply back with answer_worker. The item also stays in the UI.
func (e *Engine) relayToCaptain(taskID, title string, p store.Pending) {
	var b strings.Builder
	fmt.Fprintf(&b, "[Oscar event, not from the user] %s (%s) needs the user.\n", taskID, title)
	fmt.Fprintf(&b, "pending_id: %s\nkind: %s\n", p.ID, p.Kind)
	if p.Tool != "" {
		fmt.Fprintf(&b, "tool: %s\n", p.Tool)
	}
	fmt.Fprintf(&b, "question: %s\n", p.Question)
	if len(p.Options) > 0 {
		fmt.Fprintf(&b, "options: %s\n", strings.Join(p.Options, " | "))
	}
	if p.Detail != "" {
		fmt.Fprintf(&b, "detail:\n%s\n", truncate(p.Detail, 1500))
	}
	b.WriteString("\nTell the user in the chat, in plain words and a few short lines, what the worker needs and the choices " +
		"(for a command: what it does and what it may change). Then pass their reply on with answer_worker. " +
		"If the user already answered this clearly earlier in this chat and it is a question (not an approval), you may answer it " +
		"yourself with answer_worker and tell the user what you answered.")
	if err := e.sendCaptain(b.String(), "Captain is passing on what "+taskID+" needs…"); err != nil {
		log.Printf("relay %s to the Captain: %v", taskID, err)
	}
}

// answerWorker resolves a task's pending item for the Captain. Approvals that act outside
// (a gated command, an ops plan, a post, closing a PR) need a reply from the user in the chat
// written after the item appeared; the Captain cannot give them on its own.
func (e *Engine) answerWorker(taskID, pendingID string, d Decision) error {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	var p *store.Pending
	for i := range t.Pending {
		if t.Pending[i].ID == pendingID {
			p = &t.Pending[i]
		}
	}
	if p == nil {
		e.mu.Unlock()
		return errors.New("that item is no longer waiting (the user may have answered it in the UI); check get_task")
	}
	fromUser := e.lastUserChat.After(p.Created)
	kind := p.Kind
	e.mu.Unlock()
	switch kind {
	case store.PendingQuestion, store.PendingEscalate:
	default:
		if !fromUser {
			return errors.New("this needs the user's own yes or no: ask them in the chat and pass on their reply")
		}
	}
	d.By = ActorCaptain
	if fromUser {
		d.By = ActorViaCaptain
	}
	return e.Decide(taskID, pendingID, d)
}

// tellCaptainDone lets the Captain suggest a follow-up. It stays silent when there is none.
func (e *Engine) tellCaptainDone(taskID string) {
	e.mu.Lock()
	t, err := e.task(taskID)
	s := e.captain
	if err != nil || s == nil {
		e.mu.Unlock()
		return
	}
	note := fmt.Sprintf("[Oscar event, not from the user] %s (%s) is done: %s", t.ID, t.Title, truncate(t.Summary, 800))
	e.capDoing = "Captain is checking what " + t.ID + " finished…"
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	_ = s.Send(note + "\n\nCheck the user's goal behind this task (what they asked for in this chat). If the goal is not reached yet, " +
		"take the next step now: reopen_task when the fix belongs to a task that already exists (for example re-run a verify task after its " +
		"bugs were fixed), or create_task for new work. Tell the user in one or two lines what you started. Do not start work the user did not ask for. " +
		"If nothing is needed, reply with exactly: NOOP")
}

// workerModel describes the model and effort a task's worker runs with, e.g. "sonnet/low".
func (e *Engine) workerModel(t *store.Task) string {
	m, ef := e.cfg.Worker.Model, e.cfg.Worker.Effort
	if t.Model != "" {
		m = t.Model
	}
	if t.Effort != "" {
		ef = t.Effort
	}
	if m == "" {
		m = "default"
	}
	if ef == "" {
		return m
	}
	return m + "/" + ef
}

func (e *Engine) captainTools() []claude.Tool {
	return []claude.Tool{
		{Name: "create_task", Description: "Create a task. type must be one of the known types; repos are directory names (needed for code types). Put external ids in the title. model and effort choose the worker's model; leave them empty for the default.",
			Schema: obj(map[string]any{"type": str, "title": str, "brief": str, "source": str, "repos": strList,
				"model":  map[string]any{"type": "string", "description": "Worker model: an alias (opus, sonnet, haiku) or a full id (e.g. claude-opus-5-5). Empty = default."},
				"effort": map[string]any{"type": "string", "enum": Efforts, "description": "Worker reasoning effort. Empty = default."}},
				"type", "title", "brief"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Type, Title, Brief, Source, Model, Effort string
					Repos                                     []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				t, err := e.CreateTask(a.Type, a.Title, a.Brief, a.Source, a.Repos, a.Model, a.Effort)
				if err != nil {
					return "", err
				}
				return "Created " + t.ID + " (worker: " + e.workerModel(t) + "). It starts when a worker slot is free.", nil
			}},
		{Name: "propose_type", Description: "Propose a new task type as YAML (fields: name, description, code, reviewer, output [draft_pr|report|outbox], steps, guide, review_focus). The user approves it once in the UI.",
			Schema: obj(map[string]any{"yaml": str, "why": str}, "yaml", "why"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ YAML, Why string }
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				var ty TaskType
				if err := yaml.Unmarshal([]byte(a.YAML), &ty); err != nil {
					return "", fmt.Errorf("bad yaml: %w", err)
				}
				if err := ty.validate(); err != nil {
					return "", err
				}
				e.mu.Lock()
				defer e.mu.Unlock()
				if e.types[ty.Name] != nil {
					return "", fmt.Errorf("type %s already exists", ty.Name)
				}
				p := store.Pending{ID: newID("type"), Kind: store.PendingType, Question: "Approve new task type “" + ty.Name + "”? " + a.Why, Detail: a.YAML}
				p.Created = timeNow()
				e.capPend = append(e.capPend, p)
				e.publish(Event{Kind: "state"})
				go e.notify("New task type", ty.Name)
				return "Proposal sent to the user. Tell them in one line; create tasks of this type after they approve.", nil
			}},
		{Name: "list_tasks", Description: "List tasks with status.", Schema: obj(map[string]any{}),
			Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
				e.mu.Lock()
				defer e.mu.Unlock()
				var ids []string
				for id := range e.tasks {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return e.tasks[ids[i]].N < e.tasks[ids[j]].N })
				var b strings.Builder
				for _, id := range ids {
					t := e.tasks[id]
					fmt.Fprintf(&b, "%s [%s] %s · %s · step %q · worker %s\n", t.ID, t.Type, t.Title, t.Status, t.StepNote, e.workerModel(t))
				}
				if b.Len() == 0 {
					return "No tasks.", nil
				}
				return b.String(), nil
			}},
		{Name: "get_task", Description: "Full detail of one task (brief, progress, pending decisions, verdicts, PRs, report).",
			Schema: obj(map[string]any{"id": str}, "id"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID string }
				_ = json.Unmarshal(raw, &a)
				e.mu.Lock()
				defer e.mu.Unlock()
				t, err := e.task(normalizeID(a.ID))
				if err != nil {
					return "", err
				}
				b, _ := json.MarshalIndent(t, "", "  ")
				return string(b), nil
			}},
		{Name: "message_worker", Description: "Pass an instruction from the user to the worker of a task.",
			Schema: obj(map[string]any{"id": str, "text": str}, "id", "text"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID, Text string }
				_ = json.Unmarshal(raw, &a)
				if err := e.MessageWorker(normalizeID(a.ID), a.Text, e.captainActor()); err != nil {
					return "", err
				}
				return "Sent.", nil
			}},
		{Name: "reopen_task", Description: "Reopen a done task with a new instruction (e.g. fix a bug found later in the same PR). The worker continues in the same branch and the open PR updates.",
			Schema: obj(map[string]any{"id": str, "instruction": str}, "id", "instruction"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID, Instruction string }
				_ = json.Unmarshal(raw, &a)
				if err := e.Reopen(normalizeID(a.ID), a.Instruction, e.captainActor()); err != nil {
					return "", err
				}
				return "Reopened.", nil
			}},
		{Name: "answer_worker", Description: "Pass the user's reply to something a worker needs (a question, a command to approve, an ops plan, a message to post, an escalation). " +
			"pending_id comes from the Oscar event or get_task. For a question: option and/or text. For approvals: allow true or false, text as a note " +
			"(for a post, text replaces the message). always=true only when the user says to always allow it. Approvals need the user's reply in this chat.",
			Schema: obj(map[string]any{"id": str, "pending_id": str, "allow": map[string]any{"type": "boolean"}, "option": str, "text": str,
				"always": map[string]any{"type": "boolean"}}, "id", "pending_id"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					ID           string `json:"id"`
					PendingID    string `json:"pending_id"`
					Allow        bool   `json:"allow"`
					Option, Text string
					Always       bool `json:"always"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if err := e.answerWorker(normalizeID(a.ID), a.PendingID, Decision{Allow: a.Allow, Option: a.Option, Text: a.Text, Always: a.Always}); err != nil {
					return "", err
				}
				return "Passed on to the worker.", nil
			}},
		e.approveOwnTool(),
		{Name: "cancel_task", Description: "Cancel a task (only when the user asks).",
			Schema: obj(map[string]any{"id": str}, "id"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID string }
				_ = json.Unmarshal(raw, &a)
				if err := e.Cancel(normalizeID(a.ID), e.captainActor()); err != nil {
					return "", err
				}
				return "Cancelled.", nil
			}},
	}
}

func normalizeID(id string) string {
	id = strings.TrimSpace(id)
	if n := strings.TrimPrefix(strings.ToLower(id), "item-"); n != strings.ToLower(id) {
		return "Item-" + n
	}
	return id
}

// approveType saves a proposed type into the local types dir and loads it.
func (e *Engine) approveType(p store.Pending) error {
	var ty TaskType
	if err := yaml.Unmarshal([]byte(p.Detail), &ty); err != nil {
		return err
	}
	if err := ty.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(e.cfg.TypesDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(e.cfg.TypesDir, ty.Name+".yaml"), []byte(p.Detail), 0o600); err != nil {
		return err
	}
	e.mu.Lock()
	e.types[ty.Name] = &ty
	e.mu.Unlock()
	return nil
}

var errNoPending = errors.New("no such pending item")
