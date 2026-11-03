package engine

import (
	"context"
	"testing"

	"oscar/internal/config"
	"oscar/internal/store"
)

// When a second message is queued, the end of the first turn must not close the
// session or escalate.
func TestTurnEndWithQueuedMessage(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
	e, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk := &store.Task{ID: "Item-7", N: 7, Type: "feature", Status: store.StatusRunning}
	e.tasks[tk.ID] = tk
	e.queued[tk.ID] = 2
	e.workerTurnEnded(tk.ID, false, "")
	if len(tk.Pending) != 0 || tk.Status != store.StatusRunning {
		t.Fatalf("escalated with a message still queued: %+v", tk.Pending)
	}
	e.workerTurnEnded(tk.ID, false, "")
	if len(tk.Pending) != 1 {
		t.Fatalf("no escalation after the last turn: %+v", tk.Pending)
	}
}
