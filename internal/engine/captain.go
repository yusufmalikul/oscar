package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	prompt := e.renderPrompt("captain.md", map[string]any{"Types": types, "Repos": e.repoNames, "Rules": e.cfg.Rules(), "Self": e.cfg.Self.Repo})
	e.mu.Unlock()

	sp := sessionSpec{role: policy.Captain, tools: e.captainTools(), prompt: prompt, resume: saved.SessionID, rc: e.cfg.Captain,
		pctx: policy.Context{Role: policy.Captain}}
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
			e.capBusy = false
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
	s := e.captain
	e.mu.Unlock()
	if s == nil {
		if err := e.startCaptain(); err != nil {
			e.chat("oscar", "The Captain could not start: "+err.Error())
			return err
		}
		e.mu.Lock()
		s = e.captain
		e.mu.Unlock()
	}
	e.mu.Lock()
	e.capBusy = true
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	return s.Send(text)
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
	e.capBusy = true
	e.mu.Unlock()
	_ = s.Send(note + "\n\nCheck the user's goal behind this task (what they asked for in this chat). If the goal is not reached yet, " +
		"take the next step now: reopen_task when the fix belongs to a task that already exists (for example re-run a verify task after its " +
		"bugs were fixed), or create_task for new work. Tell the user in one or two lines what you started. Do not start work the user did not ask for. " +
		"If nothing is needed, reply with exactly: NOOP")
}

func (e *Engine) captainTools() []claude.Tool {
	return []claude.Tool{
		{Name: "create_task", Description: "Create a task. type must be one of the known types; repos are directory names (needed for code types). Put external ids in the title.",
			Schema: obj(map[string]any{"type": str, "title": str, "brief": str, "source": str, "repos": strList}, "type", "title", "brief"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Type, Title, Brief, Source string
					Repos                      []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				t, err := e.CreateTask(a.Type, a.Title, a.Brief, a.Source, a.Repos)
				if err != nil {
					return "", err
				}
				return "Created " + t.ID + ". It starts when a worker slot is free.", nil
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
					fmt.Fprintf(&b, "%s [%s] %s · %s · step %q\n", t.ID, t.Type, t.Title, t.Status, t.StepNote)
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
				if err := e.MessageWorker(normalizeID(a.ID), a.Text); err != nil {
					return "", err
				}
				return "Sent.", nil
			}},
		{Name: "reopen_task", Description: "Reopen a done task with a new instruction (e.g. fix a bug found later in the same PR). The worker continues in the same branch and the open PR updates.",
			Schema: obj(map[string]any{"id": str, "instruction": str}, "id", "instruction"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID, Instruction string }
				_ = json.Unmarshal(raw, &a)
				if err := e.Reopen(normalizeID(a.ID), a.Instruction); err != nil {
					return "", err
				}
				return "Reopened.", nil
			}},
		{Name: "cancel_task", Description: "Cancel a task (only when the user asks).",
			Schema: obj(map[string]any{"id": str}, "id"),
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ ID string }
				_ = json.Unmarshal(raw, &a)
				if err := e.Cancel(normalizeID(a.ID)); err != nil {
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
