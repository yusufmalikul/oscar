package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"oscar/internal/config"
	"oscar/internal/store"
)

func relayEngine(t *testing.T) (*Engine, func() []string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
	e, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	e.captainHook = func(msg string) { mu.Lock(); sent = append(sent, msg); mu.Unlock() }
	e.resumeHook = func(string, string) {}
	return e, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), sent...) }
}

func answerWorker(t *testing.T, e *Engine, args map[string]any) error {
	t.Helper()
	for _, tool := range e.captainTools() {
		if tool.Name == "answer_worker" {
			raw, _ := json.Marshal(args)
			_, err := tool.Handler(context.Background(), raw)
			return err
		}
	}
	t.Fatal("no answer_worker tool")
	return nil
}

// When a worker needs the user, the Captain hears about it and can tell the user in the chat.
func TestWorkerNeedReachesCaptain(t *testing.T) {
	e, sent := relayEngine(t)
	tk := &store.Task{ID: "Item-3", N: 3, Type: "debug", Title: "Check logs", Status: store.StatusRunning}
	e.tasks[tk.ID] = tk
	e.mu.Lock()
	e.needsYou(tk, store.Pending{ID: "p7", Kind: store.PendingQuestion, Question: "Which env?", Options: []string{"staging", "prod"}})
	e.mu.Unlock()
	var got string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && got == ""; time.Sleep(10 * time.Millisecond) {
		if s := sent(); len(s) > 0 {
			got = s[0]
		}
	}
	for _, want := range []string{"Item-3", "p7", "Which env?", "staging", "answer_worker"} {
		if !strings.Contains(got, want) {
			t.Errorf("Captain message lacks %q:\n%s", want, got)
		}
	}
}

// The Captain passes the user's yes on to a gated tool call, but cannot approve it on its own.
func TestCaptainApprovesOnlyAfterUserReply(t *testing.T) {
	e, _ := relayEngine(t)
	ch := make(chan decision, 1)
	tk := &store.Task{ID: "Item-4", N: 4, Type: "debug", Status: store.StatusNeedsYou,
		Pending: []store.Pending{{ID: "p1", Kind: store.PendingTool, Tool: "Bash", Detail: "ssh box uptime", Created: time.Now()}}}
	e.tasks[tk.ID] = tk
	e.waiters["p1"] = ch

	if err := answerWorker(t, e, map[string]any{"id": "Item-4", "pending_id": "p1", "allow": true}); err == nil {
		t.Fatal("the Captain approved a gated call without a reply from the user")
	}
	if err := e.Chat("ya, boleh"); err != nil {
		t.Fatal(err)
	}
	if err := answerWorker(t, e, map[string]any{"id": "Item-4", "pending_id": "p1", "allow": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-ch:
		if !d.allow {
			t.Error("the call was denied")
		}
	case <-time.After(time.Second):
		t.Fatal("the tool call was not released")
	}
}

// A plain question may be answered by the Captain from what the user already said.
func TestCaptainAnswersQuestion(t *testing.T) {
	e, _ := relayEngine(t)
	var msg string
	e.resumeHook = func(_, m string) { msg = m }
	tk := &store.Task{ID: "Item-5", N: 5, Type: "debug", Status: store.StatusNeedsYou,
		Pending: []store.Pending{{ID: "q1", Kind: store.PendingQuestion, Question: "Which env?", Created: time.Now()}}}
	e.tasks[tk.ID] = tk
	if err := answerWorker(t, e, map[string]any{"id": "Item-5", "pending_id": "q1", "text": "staging"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "staging") || !strings.Contains(msg, "Captain") {
		t.Errorf("worker got %q, want the answer and who gave it", msg)
	}
	if len(tk.Pending) != 0 {
		t.Error("the question is still pending")
	}
}

// The status line says what the Captain is doing, not only that it is busy.
func TestCaptainStatusSaysWhatItDoes(t *testing.T) {
	e, _ := relayEngine(t)
	doing := func() string { s, _ := e.Snapshot(0).Health["captain_doing"].(string); return s }
	if err := e.Chat("halo"); err != nil {
		t.Fatal(err)
	}
	if got := doing(); !strings.Contains(got, "replying") {
		t.Errorf("after a user message: %q", got)
	}
	e.relayToCaptain("Item-3", "Check logs", store.Pending{ID: "p1", Kind: store.PendingQuestion, Question: "Which env?"})
	if got := doing(); !strings.Contains(got, "Item-3") {
		t.Errorf("after a worker request: %q", got)
	}
}
