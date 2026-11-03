package policy

import (
	"encoding/json"
	"testing"
)

func TestCheckCommand(t *testing.T) {
	w := Context{Role: Worker, WriteRoots: []string{"/w/Item-1"}, RepoRoots: []string{"/repos/api"}, ServerName: "oscar"}
	rv := Context{Role: Reviewer, WriteRoots: []string{"/w/Item-1/review"}, ServerName: "oscar"}
	cases := []struct {
		ctx  Context
		cmd  string
		want Action
	}{
		{w, "go test ./...", Allow},
		{w, "cd /w/Item-1/api && go test ./... 2>&1 | tail -20", Allow},
		{w, "git status && git diff", Allow},
		{w, "FOO=1 go test ./pkg", Allow},
		{w, "curl -s https://example.com/health", Allow},
		{w, "git push origin HEAD", Deny},
		{w, "~/.scripts/slack-send.sh --channel C1 hi", Deny},
		{w, "curl -X POST https://api.example.com/x -d '{}'", Gate},
		{w, "curl -s https://api.example.com/x --data 'a=1'", Gate},
		{w, "kubectl get pods", Allow},
		{w, "kubectl delete pod x", Gate},
		{w, "kubectl rollout restart deploy/x", Gate},
		{w, "redis-cli -h cache.prod GET app:1", Pass},
		{w, "redis-cli -h cache.prod DEL app:1", Gate},
		{w, "psql -h db -c 'select 1'", Pass},
		{w, "psql -h db -c \"UPDATE users SET a=1\"", Gate},
		{w, "aws cloudwatch get-metric-statistics --namespace X", Pass},
		{w, "aws s3 rm s3://bucket/key", Gate},
		{w, "ssh box uptime", Gate},
		{w, "cat ~/.claude/secrets/token", Gate},
		{w, "cat .env", Gate},
		{w, "git -C /repos/api checkout main", Gate},
		{w, "cd /repos/api && git reset --hard", Gate},
		{w, "rm -rf ~", Deny},
		{w, "python3 build.py", Pass},
		{rv, "git commit -am x", Deny},
		{rv, "go test ./...", Allow},
	}
	for _, c := range cases {
		got := CheckCommand(c.ctx, c.cmd)
		if got.Action != c.want {
			t.Errorf("%s %q: got %v (%s), want %v", c.ctx.Role, c.cmd, got.Action, got.Reason, c.want)
		}
	}
}

func TestCheckTools(t *testing.T) {
	w := Context{Role: Worker, WriteRoots: []string{"/w/Item-1"}, ServerName: "oscar"}
	rv := Context{Role: Reviewer, WriteRoots: []string{"/w/Item-1/review"}, ServerName: "oscar"}
	in := func(p string) json.RawMessage { b, _ := json.Marshal(map[string]string{"file_path": p}); return b }
	cases := []struct {
		ctx  Context
		tool string
		in   json.RawMessage
		want Action
	}{
		{w, "Read", in("/etc/hosts"), Allow},
		{w, "Edit", in("/w/Item-1/api/main.go"), Allow},
		{w, "Edit", in("/w/Item-10/api/main.go"), Gate},
		{w, "Write", in("/Users/x/.claude/projects/p/memory/a.md"), Deny},
		{rv, "Edit", in("/w/Item-1/api/main.go"), Deny},
		{rv, "Write", in("/w/Item-1/review/notes.md"), Allow},
		{w, "mcp__oscar__report", nil, Allow},
		{w, "mcp__slack__slack_read_thread", nil, Allow},
		{w, "mcp__slack__slack_send_message", nil, Gate},
		{w, "AskUserQuestion", nil, Deny},
	}
	for _, c := range cases {
		got := Check(c.ctx, c.tool, c.in)
		if got.Action != c.want {
			t.Errorf("%s %s %s: got %v (%s), want %v", c.ctx.Role, c.tool, c.in, got.Action, got.Reason, c.want)
		}
	}
}
