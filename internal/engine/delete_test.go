package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oscar/internal/config"
	"oscar/internal/store"
)

// After a delete, the Captain tools and the task actions say the task is not found, the
// UI gets a state event, and a late log line does not bring the task folder back.
func TestDeletedTaskIsNotFound(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
	cfg.WorkspaceDir = filepath.Join(cfg.DataDir, "work")
	e, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk := &store.Task{ID: "Item-3", N: 3, Type: "debug", Status: store.StatusFailed, Workspace: filepath.Join(cfg.WorkspaceDir, "Item-3")}
	if err := os.MkdirAll(tk.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	e.tasks[tk.ID] = tk
	e.mu.Lock()
	e.save(tk)
	e.mu.Unlock()
	events, stop := e.Subscribe()
	defer stop()

	if err := e.Delete("Item-3"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Kind != "state" {
			t.Fatalf("event %q, want state", ev.Kind)
		}
	default:
		t.Fatal("no state event after delete")
	}
	if _, err := os.Stat(tk.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace left: %v", err)
	}

	tools := map[string]func(context.Context, json.RawMessage) (string, error){}
	for _, tool := range e.captainTools() {
		tools[tool.Name] = tool.Handler
	}
	for _, c := range []struct{ name, args string }{
		{"get_task", `{"id":"item-3"}`},
		{"reopen_task", `{"id":"Item-3","instruction":"again"}`},
		{"message_worker", `{"id":"Item-3","text":"hi"}`},
		{"cancel_task", `{"id":"Item-3"}`},
	} {
		h := tools[c.name]
		if h == nil {
			t.Fatalf("no tool %s", c.name)
		}
		if _, err := h(context.Background(), json.RawMessage(c.args)); err == nil || !strings.Contains(err.Error(), "task not found: Item-3") {
			t.Errorf("%s: err %v, want task not found", c.name, err)
		}
	}
	if out, _ := tools["list_tasks"](context.Background(), nil); strings.Contains(out, "Item-3") {
		t.Errorf("list_tasks still lists it: %s", out)
	}
	if err := e.Delete("Item-3"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := e.Diff("Item-3"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("diff: %v", err)
	}
	e.logLine("Item-3", "worker", "text", "late line")
	if _, err := os.Stat(e.st.TaskDir("Item-3")); !os.IsNotExist(err) {
		t.Fatalf("task folder came back: %v", err)
	}
}
