package policy

import "testing"

func TestLocalhostWritesAreAllowed(t *testing.T) {
	w := Context{Role: Worker}
	for _, cmd := range []string{
		`curl -s -X POST localhost:9910/api/v2/customer_rooms -d '{}'`,
		`curl -X POST http://127.0.0.1:7798/register -d '{"a":1}'`,
		`curl --data 'x=1' http://[::1]:8080/`,
	} {
		if d := CheckCommand(w, cmd); d.Action != Allow {
			t.Errorf("%q: got %v (%s), want allow", cmd, d.Action, d.Reason)
		}
	}
	if d := CheckCommand(w, `curl -X POST https://api.example.com/x -d '{}'`); d.Action != Gate {
		t.Errorf("remote POST: got %v, want gate", d.Action)
	}
	// One remote URL in the same command is enough to gate it.
	if d := CheckCommand(w, `curl -X POST localhost:1/a -d 1 && curl -X POST https://api.example.com -d 1`); d.Action != Gate {
		t.Errorf("mixed local+remote: got %v, want gate", d.Action)
	}
}

func TestGateHasKindAndSubject(t *testing.T) {
	w := Context{Role: Worker}
	cases := []struct{ cmd, kind, subject string }{
		{`curl -X POST https://api.example.com/v1/x -d '{}'`, "http-write", "api.example.com"},
		{`ssh -fNL 5448:db.internal:5432 me@jump.example.com && sleep 2`, "remote-shell", "jump.example.com"},
		{`redis-cli -h cache.example.com DEL app:1`, "redis-write", "cache.example.com"},
		{`psql postgres://u:p@127.0.0.1:5448/app -c "UPDATE t SET a=1"`, "db-write", "127.0.0.1"},
	}
	for _, c := range cases {
		d := CheckCommand(w, c.cmd)
		if d.Action != Gate || d.Kind != c.kind || d.Subject != c.subject {
			t.Errorf("%q: got %v kind=%q subject=%q, want gate %q %q", c.cmd, d.Action, d.Kind, d.Subject, c.kind, c.subject)
		}
	}
}

func TestAlwaysAllowRules(t *testing.T) {
	allowed := map[string]bool{"remote-shell|jump.example.com": true}
	w := Context{Role: Worker, Allowed: func(kind, subject string) bool { return allowed[kind+"|"+subject] }}
	if d := CheckCommand(w, `ssh me@jump.example.com uptime`); d.Action != Allow {
		t.Errorf("allowed ssh: got %v (%s)", d.Action, d.Reason)
	}
	if d := CheckCommand(w, `ssh me@other.example.com uptime`); d.Action != Gate {
		t.Errorf("other host: got %v, want gate", d.Action)
	}
	// Deny rules can never be allowed.
	allowed["deny|"] = true
	if d := CheckCommand(w, `git push origin HEAD`); d.Action != Deny {
		t.Errorf("git push: got %v, want deny", d.Action)
	}
}
