package engine

import (
	"context"
	"testing"

	"oscar/internal/config"
	"oscar/internal/store"
)

// After a reopen, the next review round must accept a verdict even though an
// earlier round with the same count already has one.
func TestVerdictAfterReopen(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
	e, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk := &store.Task{ID: "Item-9", N: 9, Type: "feature", Status: store.StatusDone, Round: 1, ReviewPassed: true,
		Verdicts: []store.Verdict{{Round: 1, Pass: true, Summary: "first"}}, Workspace: t.TempDir()}
	e.tasks[tk.ID] = tk
	e.resumeHook = func(string, string) {} // do not start a real worker

	if err := e.Reopen(tk.ID, "fix the menu", ActorUser); err != nil {
		t.Fatal(err)
	}
	// What submit_for_review does:
	e.mu.Lock()
	if left := e.roundsLeft(tk); left != cfg.MaxReviewRounds {
		t.Errorf("after reopen: %d rounds left, want %d", left, cfg.MaxReviewRounds)
	}
	tk.Round++
	tk.Status = store.StatusReview
	round := tk.Round
	e.mu.Unlock()

	if _, err := e.applyVerdict(tk.ID, round, store.Verdict{Pass: true, Summary: "second"}); err != nil {
		t.Fatalf("verdict after reopen rejected: %v", err)
	}
	if !tk.ReviewPassed || len(tk.Verdicts) != 2 {
		t.Fatalf("verdict not recorded: passed=%v verdicts=%d", tk.ReviewPassed, len(tk.Verdicts))
	}
}
