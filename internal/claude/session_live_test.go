package claude

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveToolCall runs the real claude CLI. It is skipped unless OSCAR_LIVE=1.
func TestLiveToolCall(t *testing.T) {
	if os.Getenv("OSCAR_LIVE") != "1" {
		t.Skip("set OSCAR_LIVE=1 to run against the real claude CLI")
	}
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	done := make(chan struct{}, 1)
	s, err := Start(ctx, Options{
		Cwd: t.TempDir(), Model: "haiku", PermissionMode: "auto", SettingSources: []string{},
		ServerName: "oscar",
		Tools: []Tool{
			{Name: "ping", Description: "Ping Oscar", Schema: map[string]any{"type": "object", "properties": map[string]any{}},
				Handler: func(ctx context.Context, a json.RawMessage) (string, error) {
					mu.Lock(); got = append(got, "ping"); mu.Unlock(); return "pong", nil
				}},
			{Name: "note", Description: "Leave a note", Schema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
				Handler: func(ctx context.Context, a json.RawMessage) (string, error) {
					mu.Lock(); got = append(got, "note:"+string(a)); mu.Unlock(); return "ok", nil
				}},
		},
		PreToolUse: func(ctx context.Context, in HookInput) HookDecision {
			mu.Lock(); got = append(got, "hook:"+in.ToolName); mu.Unlock()
			if in.ToolName == "Bash" && strings.Contains(string(in.ToolInput), "rm ") {
				return HookDecision{Decision: "deny", Reason: "no rm"}
			}
			return HookDecision{}
		},
		CanUseTool: func(ctx context.Context, r PermissionRequest) PermissionResult { return PermissionResult{Allow: true} },
		OnMessage: func(m Message) {
			if m.Type == "result" {
				select { case done <- struct{}{}: default: }
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("Call the oscar ping tool, then the oscar note tool with text \"hi\". Nothing else."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "ping") || !strings.Contains(joined, `note:{"text":"hi"}`) {
		t.Fatalf("tools not called as expected: %s", joined)
	}
	if s.SessionID() == "" {
		t.Fatal("no session id")
	}
	t.Log(joined)
}
