package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A tool call that misses a required field must not reach the handler; Claude gets an
// error that tells it to call again with the field.
func TestMCPRejectsMissingRequiredFields(t *testing.T) {
	called := false
	s := &Session{opts: Options{ServerName: "oscar", Tools: []Tool{{
		Name:   "verdict",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"pass": map[string]any{"type": "boolean"}, "summary": map[string]any{"type": "string"}}, "required": []string{"pass", "summary"}},
		Handler: func(ctx context.Context, a json.RawMessage) (string, error) {
			called = true
			return "ok", nil
		},
	}}}, ctx: context.Background()}

	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"verdict","arguments":{}}}`)
	b, _ := json.Marshal(s.handleMCP("oscar", raw))
	if called {
		t.Fatal("handler ran without required fields")
	}
	if !strings.Contains(string(b), `"isError":true`) || !strings.Contains(string(b), "pass") {
		t.Fatalf("want an error naming the missing field, got %s", b)
	}

	raw = json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"verdict","arguments":{"pass":true,"summary":"fine"}}}`)
	s.handleMCP("oscar", raw)
	if !called {
		t.Fatal("handler did not run with all fields present")
	}
}
