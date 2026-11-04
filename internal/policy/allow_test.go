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
		{`psql postgres://u:p@db.example.com:5432/app -c "UPDATE t SET a=1"`, "db-write", "db.example.com"},
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
	if d := CheckCommand(w, `~/.scripts/slack-send.sh --channel C1 hi`); d.Action != Deny {
		t.Errorf("slack send: got %v, want deny", d.Action)
	}
}

func TestLocalDatabaseWritesAreAllowed(t *testing.T) {
	w := Context{Role: Worker}
	for _, cmd := range []string{
		`psql -h 127.0.0.1 -U postgres -d app_test -c "UPDATE apps SET a=1"`,
		`psql postgres://postgres:x@localhost:5432/app -c "insert into t values (1)"`,
		`psql -d app_test -c "delete from t"`,
		`redis-cli -h 127.0.0.1 DEL app:1`,
		`redis-cli DEL app:1`,
	} {
		if d := CheckCommand(w, cmd); d.Action == Gate {
			t.Errorf("%q: got gate (%s), want no gate", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{
		`psql -h db.example.com -c "UPDATE t SET a=1"`,
		`redis-cli -h cache.example.com DEL app:1`,
	} {
		if d := CheckCommand(w, cmd); d.Action != Gate {
			t.Errorf("%q: got %v, want gate", cmd, d.Action)
		}
	}
}

// A localhost port that is an SSH tunnel or port-forward reaches a remote server and stays gated.
func TestTunnelledLocalhostStaysGated(t *testing.T) {
	w := Context{Role: Worker, Forwarded: func(port string) bool { return port == "5443" || port == "8081" }}
	for _, cmd := range []string{
		`psql -h 127.0.0.1 -p 5443 -U app -d prod -c "UPDATE t SET a=1"`,
		`psql postgres://u:p@127.0.0.1:5443/prod -c "delete from t"`,
		`curl -X POST localhost:8081/api -d '{}'`,
	} {
		if d := CheckCommand(w, cmd); d.Action != Gate {
			t.Errorf("%q: got %v, want gate", cmd, d.Action)
		}
	}
	if d := CheckCommand(w, `psql -h 127.0.0.1 -p 5432 -d app_test -c "UPDATE t SET a=1"`); d.Action == Gate {
		t.Errorf("plain local db: got gate")
	}
}

func TestHostInShellVariable(t *testing.T) {
	w := Context{Role: Worker}
	cmd := "U=localhost:8383/api/v2/customer_rooms\nfor b in a b; do curl -s -o /tmp/r.json $U -d \"$b\"; done"
	if d := CheckCommand(w, cmd); d.Action == Gate {
		t.Errorf("local URL in a variable: got gate (%s %q)", d.Reason, d.Subject)
	}
	cmd = "U=https://api.example.com/x; curl -X POST $U -d 1"
	if d := CheckCommand(w, cmd); d.Action != Gate || d.Subject != "api.example.com" {
		t.Errorf("remote URL in a variable: got %v %q", d.Action, d.Subject)
	}
}
