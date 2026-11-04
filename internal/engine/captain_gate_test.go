package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"oscar/internal/claude"
	"oscar/internal/policy"
)

func approveOwn(t *testing.T, e *Engine, id string) error {
	t.Helper()
	for _, tool := range e.captainTools() {
		if tool.Name == "approve_own" {
			raw, _ := json.Marshal(map[string]any{"request_id": id})
			_, err := tool.Handler(context.Background(), raw)
			return err
		}
	}
	t.Fatal("no approve_own tool")
	return nil
}

// A gated call of the Captain shows no popup and does not block the Captain. The Captain is
// told to decide or ask the user in the chat; after the user's yes it may run the same call once.
func TestCaptainGateGoesThroughChat(t *testing.T) {
	e, _ := relayEngine(t)
	sp := sessionSpec{role: policy.Captain, pctx: policy.Context{Role: policy.Captain, Allowed: e.allowed}}
	in, _ := json.Marshal(map[string]string{"command": "ssh me@jump.example.com uptime"})
	req := claude.PermissionRequest{ToolName: "Bash", Input: in, DecisionReason: "Oscar gate: runs on a remote machine"}
	call := func() claude.PermissionResult {
		done := make(chan claude.PermissionResult, 1)
		go func() { done <- e.approveTool(context.Background(), sp, req) }()
		select {
		case r := <-done:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("the Captain is blocked waiting for a popup")
		}
		return claude.PermissionResult{}
	}

	r := call()
	if r.Allow {
		t.Fatal("allowed without the user")
	}
	if n := len(e.Snapshot(0).CapPending); n != 0 {
		t.Errorf("%d popup(s) for the Captain, want none", n)
	}
	id := regexp.MustCompile(`ask-\w+`).FindString(r.Message)
	if id == "" {
		t.Fatalf("no request id in %q", r.Message)
	}
	if err := approveOwn(t, e, id); err == nil {
		t.Fatal("approve_own worked before the user replied")
	}
	if err := e.Chat("ya, jalankan"); err != nil {
		t.Fatal(err)
	}
	if err := approveOwn(t, e, id); err != nil {
		t.Fatal(err)
	}
	if r := call(); !r.Allow {
		t.Fatalf("the approved call was denied: %s", r.Message)
	}
	if r := call(); r.Allow {
		t.Error("one approval allowed the call twice")
	}
}
