package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSettings(t *testing.T, dir string, perms map[string][]string) string {
	t.Helper()
	p := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"permissions": perms})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Rules the user allowed in their own Claude Code sessions apply to Oscar sessions too.
func TestClaudeSettingsRules(t *testing.T) {
	dir := t.TempDir()
	p := writeSettings(t, dir, map[string][]string{
		"allow": {"Bash(ssh -fNL 5444:db:5432 me@jump.example.com)", "Bash(curl:*)", "Bash(tsh ssh *)",
			"Bash(./deploy.sh)", "mcp__chrome", "mcp__slack__slack_add_reaction", "WebFetch(domain:*.example.com)"},
		"ask":  {"Bash(git push:*)"},
		"deny": {"Bash(rm -rf:*)", "mcp__slack__slack_send_message"},
	})
	w := Context{Role: Worker, WriteRoots: []string{"/w/Item-1"}, RepoRoots: []string{"/repos/api"}, ServerName: "oscar",
		Settings: NewSettings([]string{p, filepath.Join(dir, "missing.json")})}
	cmds := []struct {
		cmd  string
		want Action
	}{
		{"ssh -fNL 5444:db:5432 me@jump.example.com", Allow},
		{"ssh -fNL 5444:db:5432 me@jump.example.com && sleep 2", Allow},
		{"ssh other.example.com", Gate},
		{"curl -X POST https://api.example.com/x -d '{}'", Allow},
		{"curl -X POST https://api.example.com/x -d '{}' | python3 x.py", Gate}, // every part must be allowed, else Oscar's gate applies
		{"tsh ssh node1 uptime", Allow},
		{"tsh ssh", Allow},
		{"./deploy.sh", Allow},
		{"./deploy.sh prod", Gate},
		{"git push origin HEAD", Gate},
		{"rm -rf build", Deny},
		{"~/.scripts/slack-send.sh hi", Deny},       // Oscar's own deny rules still win
		{"cd /repos/api && git reset --hard", Gate}, // the main checkout stays protected
	}
	for _, c := range cmds {
		if got := CheckCommand(w, c.cmd); got.Action != c.want {
			t.Errorf("%q: got %v (%s), want %v", c.cmd, got.Action, got.Reason, c.want)
		}
	}
	url := func(u string) json.RawMessage { b, _ := json.Marshal(map[string]string{"url": u}); return b }
	tools := []struct {
		tool string
		in   json.RawMessage
		want Action
	}{
		{"mcp__chrome__navigate", nil, Allow},
		{"mcp__slack__slack_add_reaction", nil, Allow},
		{"mcp__slack__slack_send_message", nil, Deny},
		{"mcp__slack__slack_create_canvas", nil, Gate},
		{"WebFetch", url("https://docs.example.com/a"), Allow},
	}
	for _, c := range tools {
		if got := Check(w, c.tool, c.in); got.Action != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.tool, got.Action, got.Reason, c.want)
		}
	}
	// An "ask" rule can be always-allowed in Oscar like any other gate.
	d := CheckCommand(w, "git push origin HEAD")
	if d.Kind != "settings-ask" || d.Subject != "Bash(git push:*)" {
		t.Errorf("ask gate: kind=%q subject=%q", d.Kind, d.Subject)
	}
}

// A rule the user adds while Oscar runs takes effect without a restart.
func TestClaudeSettingsReload(t *testing.T) {
	dir := t.TempDir()
	p := writeSettings(t, dir, map[string][]string{})
	w := Context{Role: Worker, Settings: NewSettings([]string{p})}
	if d := CheckCommand(w, "ssh box uptime"); d.Action != Gate {
		t.Fatalf("before: got %v", d.Action)
	}
	writeSettings(t, dir, map[string][]string{"allow": {"Bash(ssh box:*)", "Bash(ssh box uptime --long-enough-to-change-the-size)"}})
	if d := CheckCommand(w, "ssh box uptime"); d.Action != Allow {
		t.Errorf("after: got %v (%s), want allow", d.Action, d.Reason)
	}
}
