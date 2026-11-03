package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"oscar/internal/config"
	"oscar/internal/store"
)

// A note typed when approving a plan must reach the worker, not only a note
// typed when rejecting it.
func TestPlanApproveKeepsNote(t *testing.T) {
	for _, allow := range []bool{true, false} {
		cfg := config.Default()
		cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
		e, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		cmds, _ := json.Marshal([]string{"deploy.sh flash"})
		tk := &store.Task{ID: "Item-10", N: 10, Type: "ops", Status: store.StatusNeedsYou,
			Pending: []store.Pending{{ID: "p1", Kind: store.PendingPlan, Question: "Approve this plan?", Input: cmds}}}
		e.tasks[tk.ID] = tk
		var sent string
		e.resumeHook = func(_, msg string) { sent = msg }

		if err := e.Decide(tk.ID, "p1", Decision{Allow: allow, Text: "skip the loki check"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sent, "skip the loki check") {
			t.Errorf("allow=%v: the worker did not get the note:\n%s", allow, sent)
		}
		log, _ := e.Log(tk.ID)
		if len(log) == 0 || !strings.Contains(log[len(log)-1].Text, "skip the loki check") {
			t.Errorf("allow=%v: the task log does not show the note: %+v", allow, log)
		}
	}
}
