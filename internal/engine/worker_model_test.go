package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oscar/internal/config"
)

// The Captain picks the worker's model and effort in create_task, and the worker's
// claude process starts with them.
func TestCaptainChoosesWorkerModel(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir, cfg.WorkspaceDir, cfg.ClaudeCwd = t.TempDir(), t.TempDir(), t.TempDir(), dir
	cfg.ClaudeBin = fake
	cfg.MaxWorkers = 1
	cfg.Worker = config.Role{Model: "opus", Effort: "high"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.captainHook = func(string) {} // the fake worker stops at once; keep its escalation away from a real Captain
	create := func(args map[string]string) (string, error) {
		for _, tool := range e.captainTools() {
			if tool.Name == "create_task" {
				raw, _ := json.Marshal(args)
				return tool.Handler(ctx, raw)
			}
		}
		t.Fatal("no create_task tool")
		return "", nil
	}

	if _, err := create(map[string]string{"type": "debug", "title": "x", "brief": "y", "effort": "huge"}); err == nil {
		t.Error("a bad effort was accepted")
	}
	out, err := create(map[string]string{"type": "debug", "title": "Check logs", "brief": "look", "model": "sonnet", "effort": "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sonnet/low") {
		t.Errorf("reply does not name the worker model: %q", out)
	}
	var args string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(argsFile); err == nil && len(b) > 0 {
			args = string(b)
			break
		}
	}
	if !strings.Contains(args, "--model sonnet") || !strings.Contains(args, "--effort low") {
		t.Errorf("worker started with %q, want --model sonnet --effort low", args)
	}
}
