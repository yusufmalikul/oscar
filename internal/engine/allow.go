package engine

import (
	"errors"
	"time"

	"oscar/internal/redact"
	"oscar/internal/store"
)

const allowFile = "allow.json"

// AllowRule is one "Always allow" choice of the user: a gate kind (e.g. "remote-shell")
// and its subject (e.g. a host). Deny rules can never be allowed this way.
type AllowRule struct {
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
	Example string    `json:"example,omitempty"`
	At      time.Time `json:"at"`
}

func (e *Engine) allowed(kind, subject string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.allow {
		if r.Kind == kind && (r.Subject == subject || r.Subject == "*") {
			return true
		}
	}
	return false
}

// AllowRules returns a copy of the rules.
func (e *Engine) AllowRules() []AllowRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AllowRule{}, e.allow...)
}

// AddAllowRule saves a rule and lets every call already waiting on the same gate through.
func (e *Engine) AddAllowRule(kind, subject, example string) error {
	if kind == "" {
		return errors.New("this approval cannot be remembered; use Allow once")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.allow {
		if r.Kind == kind && r.Subject == subject {
			return nil
		}
	}
	e.allow = append(e.allow, AllowRule{Kind: kind, Subject: subject, Example: truncate(redact.String(example), 300), At: time.Now()})
	if err := e.st.SetValue(allowFile, e.allow); err != nil {
		return err
	}
	// Release other calls that wait on exactly this gate.
	release := func(ps []store.Pending) {
		for _, p := range ps {
			if p.Kind == store.PendingTool && p.RuleKind == kind && p.RuleSubject == subject {
				if ch := e.waiters[p.ID]; ch != nil {
					select {
					case ch <- decision{allow: true}:
					default:
					}
				}
			}
		}
	}
	release(e.capPend)
	for _, t := range e.tasks {
		release(t.Pending)
	}
	e.publish(Event{Kind: "state"})
	return nil
}

// RemoveAllowRule deletes a rule.
func (e *Engine) RemoveAllowRule(kind, subject string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.allow {
		if r.Kind == kind && r.Subject == subject {
			e.allow = append(e.allow[:i], e.allow[i+1:]...)
			e.publish(Event{Kind: "state"})
			return e.st.SetValue(allowFile, e.allow)
		}
	}
	return errors.New("no such rule")
}
