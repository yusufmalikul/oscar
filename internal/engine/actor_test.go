package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"oscar/internal/store"
)

func captainCall(t *testing.T, e *Engine, name string, args map[string]string) {
	t.Helper()
	for _, tool := range e.captainTools() {
		if tool.Name == name {
			raw, _ := json.Marshal(args)
			if _, err := tool.Handler(context.Background(), raw); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no %s tool", name)
}

func lastLog(t *testing.T, e *Engine, id string) store.LogLine {
	t.Helper()
	ls, err := e.st.ReadLog(id, 100)
	if err != nil || len(ls) == 0 {
		t.Fatalf("no log for %s: %v", id, err)
	}
	return ls[len(ls)-1]
}

// The task log says who really sent a message: the user in the task panel, the user through
// the Captain, or the Captain on its own.
func TestLogSaysWhoSentIt(t *testing.T) {
	e, _ := relayEngine(t)
	var toWorker string
	e.resumeHook = func(_, m string) { toWorker = m }
	tk := &store.Task{ID: "Item-12", N: 12, Type: "debug", Status: store.StatusRunning}
	e.tasks[tk.ID] = tk

	if err := e.MessageWorker(tk.ID, "from the panel", ActorUser); err != nil {
		t.Fatal(err)
	}
	if l := lastLog(t, e, tk.ID); l.Who != "you" {
		t.Errorf("panel message logged as %q", l.Who)
	}

	if err := e.Chat("tell Item-12 to rebase"); err != nil {
		t.Fatal(err)
	}
	captainCall(t, e, "message_worker", map[string]string{"id": "Item-12", "text": "please rebase"})
	if l := lastLog(t, e, tk.ID); l.Who != "you (via Captain)" {
		t.Errorf("message the user asked for logged as %q", l.Who)
	}
	if !strings.Contains(toWorker, "user") || !strings.Contains(toWorker, "Captain") {
		t.Errorf("worker was told %q", toWorker)
	}

	e.captainTurnEnded() // the Captain answered the user; the next turn is an Oscar event
	captainCall(t, e, "message_worker", map[string]string{"id": "Item-12", "text": "FYI"})
	if l := lastLog(t, e, tk.ID); l.Who != "captain" {
		t.Errorf("the Captain's own message logged as %q", l.Who)
	}
	if !strings.HasPrefix(toWorker, "Message from the Captain") {
		t.Errorf("worker was told %q", toWorker)
	}
}
