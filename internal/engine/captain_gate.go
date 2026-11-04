package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"oscar/internal/claude"
	"oscar/internal/policy"
	"oscar/internal/redact"
)

// The Captain talks to the user in the chat, so its gated calls do not show a popup and do not
// block it. A gated call is refused with a request id; the Captain decides whether it needs the
// call at all, asks the user in the chat, and after the user's yes calls approve_own and runs
// the same call once more.

// capGrantTTL is how long the user's yes for one Captain call stays valid.
const capGrantTTL = 15 * time.Minute

type capAsk struct {
	Tool, Detail  string
	Kind, Subject string // for "always allow"
	Created       time.Time
}

func capKey(tool, detail string) string { return tool + "\x00" + strings.TrimSpace(detail) }

// captainScratch is the Captain's own dir for temp files and notes.
func (e *Engine) captainScratch() string { return filepath.Join(e.cfg.DataDir, "captain") }

// takeCapGrant reports whether the user approved this exact Captain call, and uses the approval up.
func (e *Engine) takeCapGrant(tool, detail string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := capKey(tool, detail)
	at, ok := e.capGrants[k]
	if !ok {
		return false
	}
	delete(e.capGrants, k)
	return time.Since(at) < capGrantTTL
}

// captainGate refuses a gated Captain call and tells the Captain how to get the user's yes.
func (e *Engine) captainGate(req claude.PermissionRequest, detail string, pd policy.Decision) claude.PermissionResult {
	if e.takeCapGrant(req.ToolName, detail) {
		e.chat("oscar", "Captain ran what you approved: "+truncate(redact.String(detail), 300))
		return claude.PermissionResult{Allow: true}
	}
	id := newID("ask")
	e.mu.Lock()
	e.capAsks[id] = capAsk{Tool: req.ToolName, Detail: detail, Kind: pd.Kind, Subject: pd.Subject, Created: timeNow()}
	e.mu.Unlock()
	reason := strings.TrimPrefix(req.DecisionReason, "Oscar gate: ")
	if reason == "" {
		reason = "needs approval"
	}
	return claude.PermissionResult{Allow: false, Message: fmt.Sprintf(
		"Not run: this needs the user's OK (%s). Request id: %s.\n"+
			"Decide first whether you need it. If you can reach the goal another way, do that (temp files go in %s or /tmp; "+
			"code changes in a repo go to a worker task). If you do need it, ask the user in the chat in plain words: what you "+
			"want to run, why, and what it may change. Then end your turn. If they say yes, call approve_own with this request id "+
			"and make the exact same call again.", reason, id, e.captainScratch())}
}

// approveOwn turns the user's yes in the chat into a one-time approval for a refused Captain call.
func (e *Engine) approveOwn(id string, always bool) (string, error) {
	e.mu.Lock()
	a, ok := e.capAsks[id]
	if !ok {
		e.mu.Unlock()
		return "", errors.New("unknown or used request id; make the call again to get a new one")
	}
	if !e.lastUserChat.After(a.Created) {
		e.mu.Unlock()
		return "", errors.New("the user has not replied since this was refused: ask them in the chat and wait for their yes")
	}
	delete(e.capAsks, id)
	e.capGrants[capKey(a.Tool, a.Detail)] = time.Now()
	e.mu.Unlock()
	msg := "Approved once. Make the exact same call again now."
	if always {
		if err := e.AddAllowRule(a.Kind, a.Subject, a.Detail); err != nil {
			msg += " Always-allow was not saved: " + err.Error()
		} else {
			msg += " Saved as always allowed (" + a.Kind + " " + a.Subject + ")."
		}
	}
	return msg, nil
}

func (e *Engine) approveOwnTool() claude.Tool {
	return claude.Tool{Name: "approve_own",
		Description: "After the user said yes in the chat to a call of yours that Oscar refused, pass its request id; then make the exact same call again. " +
			"always=true only when the user says to always allow it.",
		Schema: obj(map[string]any{"request_id": str, "always": map[string]any{"type": "boolean"}}, "request_id"),
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				RequestID string `json:"request_id"`
				Always    bool   `json:"always"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
			return e.approveOwn(strings.TrimSpace(a.RequestID), a.Always)
		}}
}
