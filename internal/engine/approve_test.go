package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"oscar/internal/claude"
	"oscar/internal/config"
	"oscar/internal/policy"
)

// A gated tool call must register its approval without deadlocking: computing the gate
// kind reads the always-allow rules, which need the engine lock.
func TestApproveToolDoesNotDeadlock(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.TypesDir = t.TempDir()
	e, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	sp := sessionSpec{role: policy.Captain, pctx: policy.Context{Role: policy.Captain, Allowed: e.allowed}}
	in, _ := json.Marshal(map[string]string{"command": "ssh me@jump.example.com uptime"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan claude.PermissionResult, 1)
	go func() { done <- e.approveTool(ctx, sp, claude.PermissionRequest{ToolName: "Bash", Input: in}) }()
	select {
	case res := <-done:
		if res.Allow {
			t.Fatal("allowed without approval")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approveTool deadlocked")
	}
	// The engine must still answer.
	ok := make(chan struct{})
	go func() { e.Snapshot(0); close(ok) }()
	select {
	case <-ok:
	case <-time.After(time.Second):
		t.Fatal("engine lock is still held")
	}
}
