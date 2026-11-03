package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"oscar/internal/redact"
	"oscar/internal/store"
)

// Decision is the user's answer to a pending item.
type Decision struct {
	Allow  bool   `json:"allow"`
	Text   string `json:"text"`   // free answer, note, or edited message
	Option string `json:"option"` // chosen option of a question
}

// Decide resolves a pending item of a task. taskID "" means a Captain item.
func (e *Engine) Decide(taskID, pendingID string, d Decision) error {
	if taskID == "" {
		return e.decideCaptain(pendingID, d)
	}
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	var p store.Pending
	found := false
	for _, q := range t.Pending {
		if q.ID == pendingID {
			p, found = q, true
		}
	}
	if !found {
		e.mu.Unlock()
		return errNoPending
	}

	switch p.Kind {
	case store.PendingTool:
		ch := e.waiters[p.ID]
		e.mu.Unlock()
		if ch == nil {
			return errors.New("that tool call is no longer waiting")
		}
		ch <- decision{allow: d.Allow, note: d.Text}
		e.logLine(taskID, "you", "info", fmt.Sprintf("%s: %s", map[bool]string{true: "allowed", false: "denied"}[d.Allow], p.Detail))
		return nil

	case store.PendingQuestion:
		ans := strings.TrimSpace(d.Option)
		if strings.TrimSpace(d.Text) != "" {
			if ans != "" {
				ans += " — "
			}
			ans += strings.TrimSpace(d.Text)
		}
		if ans == "" {
			e.mu.Unlock()
			return errors.New("empty answer")
		}
		e.removePending(t, p.ID)
		e.waiting[t.ID] = false
		e.settle(t)
		e.save(t)
		e.mu.Unlock()
		e.logLine(taskID, "you", "text", "Q: "+p.Question+"\nA: "+ans)
		go e.resumeWorker(taskID, "The user answered your question.\nQuestion: "+p.Question+"\nAnswer: "+ans)
		return nil

	case store.PendingPlan:
		var cmds []string
		_ = json.Unmarshal(p.Input, &cmds)
		e.removePending(t, p.ID)
		e.waiting[t.ID] = false
		var msg string
		if d.Allow {
			e.planCmds[t.ID] = cmds
			e.setStep(t, "Run")
			msg = "The user approved the plan. Run exactly these commands, in order, then verify:\n- " + strings.Join(cmds, "\n- ")
		} else {
			msg = "The user did not approve the plan."
			if d.Text != "" {
				msg += " Their note: " + d.Text
			}
			msg += "\nChange the plan and call request_approval again, or ask."
		}
		e.settle(t)
		e.save(t)
		e.mu.Unlock()
		e.logLine(taskID, "you", "info", map[bool]string{true: "plan approved", false: "plan rejected: " + d.Text}[d.Allow])
		go e.resumeWorker(taskID, msg)
		return nil

	case store.PendingOutbox:
		var item *store.OutboxItem
		for i := range t.Outbox {
			if t.Outbox[i].ID == p.OutboxID {
				item = &t.Outbox[i]
			}
		}
		if item == nil {
			e.mu.Unlock()
			return errors.New("outbox item missing")
		}
		if !d.Allow {
			item.Status, item.Updated = "skipped", time.Now()
			e.removePending(t, p.ID)
			e.settle(t)
			e.save(t)
			e.mu.Unlock()
			e.logLine(taskID, "you", "info", "skipped message to "+item.Target)
			return nil
		}
		if txt := strings.TrimSpace(d.Text); txt != "" && txt != item.Text {
			item.Text, item.Edited = txt, true
		}
		o := *item
		e.mu.Unlock()
		res, perr := e.post(o)
		e.mu.Lock()
		defer e.mu.Unlock()
		if perr != nil {
			item.Status, item.Result = "failed", perr.Error()
			e.save(t)
			return fmt.Errorf("posting failed (still in the outbox, you can retry): %w", perr)
		}
		item.Status, item.Result, item.Updated = "posted", res, time.Now()
		e.removePending(t, p.ID)
		e.settle(t)
		e.save(t)
		go e.logLine(taskID, "oscar", "info", "posted to "+o.Target+" "+res)
		return nil

	case store.PendingEscalate:
		reply := strings.TrimSpace(d.Text)
		e.removePending(t, p.ID)
		retryReview := strings.EqualFold(reply, "retry review")
		if retryReview {
			t.Status = store.StatusReview
		} else {
			e.waiting[t.ID] = false
			t.Status = store.StatusRunning
			if t.Round >= e.cfg.MaxReviewRounds && !t.ReviewPassed {
				t.Round = 0 // the user chose to continue: give the worker fresh rounds
			}
		}
		e.save(t)
		e.mu.Unlock()
		if reply == "" {
			reply = "Continue."
		}
		e.logLine(taskID, "you", "text", reply)
		if retryReview {
			go e.startReviewer(taskID, "")
		} else {
			go e.resumeWorker(taskID, "Message from the user: "+reply)
		}
		return nil
	}
	e.mu.Unlock()
	return fmt.Errorf("unknown pending kind %s", p.Kind)
}

func (e *Engine) post(o store.OutboxItem) (string, error) {
	var res map[string]any
	var err error
	switch o.Kind {
	case "slack":
		res, err = runIntegration(e.ctx, e.cfg.Integrations.SlackPost, map[string]any{"channel": o.Channel, "thread": o.Thread, "text": o.Text})
	case "pr_comment":
		res, err = runIntegration(e.ctx, e.cfg.Integrations.PRComment, map[string]any{
			"repo": o.Repo, "repo_path": filepath.Join(e.cfg.ReposDir, o.Repo), "pr": o.PR, "path": o.Path, "line": o.Line, "text": o.Text})
	default:
		return "", fmt.Errorf("unknown outbox kind %s", o.Kind)
	}
	if err != nil {
		return "", err
	}
	if u, ok := res["url"].(string); ok {
		return u, nil
	}
	return "", nil
}

func (e *Engine) decideCaptain(pendingID string, d Decision) error {
	e.mu.Lock()
	var p store.Pending
	idx := -1
	for i, q := range e.capPend {
		if q.ID == pendingID {
			p, idx = q, i
		}
	}
	if idx < 0 {
		e.mu.Unlock()
		return errNoPending
	}
	switch p.Kind {
	case store.PendingTool:
		ch := e.waiters[p.ID]
		e.mu.Unlock()
		if ch == nil {
			return errors.New("that tool call is no longer waiting")
		}
		ch <- decision{allow: d.Allow, note: d.Text}
		return nil
	case store.PendingType:
		e.capPend = append(e.capPend[:idx], e.capPend[idx+1:]...)
		e.publish(Event{Kind: "state"})
		s := e.captain
		e.mu.Unlock()
		var note string
		if d.Allow {
			if err := e.approveType(p); err != nil {
				return err
			}
			note = "[Oscar event] The user approved the new task type. You can create the task now."
		} else {
			note = "[Oscar event] The user rejected the new task type. " + d.Text
		}
		if s != nil {
			e.mu.Lock()
			e.capBusy = true
			e.mu.Unlock()
			_ = s.Send(note)
		}
		return nil
	}
	e.mu.Unlock()
	return fmt.Errorf("unknown pending kind %s", p.Kind)
}

// EditOutbox changes a draft before it is approved.
func (e *Engine) EditOutbox(taskID, outboxID, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.task(taskID)
	if err != nil {
		return err
	}
	for i := range t.Outbox {
		if t.Outbox[i].ID == outboxID {
			if t.Outbox[i].Status != "draft" && t.Outbox[i].Status != "failed" {
				return errors.New("already posted or skipped")
			}
			t.Outbox[i].Text = redact.String(strings.TrimSpace(text))
			t.Outbox[i].Edited = true
			t.Outbox[i].Updated = time.Now()
			e.save(t)
			return nil
		}
	}
	return errors.New("no such outbox item")
}

// MessageWorker sends a free message from the user to the worker of a task.
func (e *Engine) MessageWorker(taskID, text string) error {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if t.Status == store.StatusDone || t.Status == store.StatusCancelled || t.Status == store.StatusQueued {
		e.mu.Unlock()
		return fmt.Errorf("%s is %s", taskID, t.Status)
	}
	e.waiting[taskID] = false
	e.mu.Unlock()
	e.logLine(taskID, "you", "text", text)
	go e.resumeWorker(taskID, "Message from the user: "+text)
	return nil
}

// Cancel stops a task. Worktrees are kept so nothing is lost.
func (e *Engine) Cancel(taskID string) error {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	t.Status = store.StatusCancelled
	for _, p := range t.Pending {
		if ch := e.waiters[p.ID]; ch != nil {
			ch <- decision{allow: false, note: "task cancelled"}
		}
	}
	t.Pending = nil
	w, r := e.workers[taskID], e.reviewers[taskID]
	delete(e.workers, taskID)
	delete(e.reviewers, taskID)
	e.save(t)
	e.mu.Unlock()
	if w != nil {
		go w.Close()
	}
	if r != nil {
		go r.Close()
	}
	e.logLine(taskID, "you", "info", "cancelled")
	go e.schedule()
	return nil
}
