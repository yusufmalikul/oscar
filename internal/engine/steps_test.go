package engine

import (
	"testing"

	"oscar/internal/store"
)

func TestStepIndexMatchesLooseNames(t *testing.T) {
	e := &Engine{types: map[string]*TaskType{"verify": {Name: "verify", Steps: []string{"Understand", "Build rig", "Run scenarios", "Report", "Reviewer"}}}}
	tk := &store.Task{Type: "verify", Step: 0}
	cases := map[string]int{
		"Understand":                          0,
		"Build a local rig":                   1,
		"Run each scenario in a real browser": 2,
		"Write a report":                      3,
		"Reviewer":                            4,
		"reviewer":                            4,
	}
	for name, want := range cases {
		if got := e.stepIndex(tk, name); got != want {
			t.Errorf("%q: got %d, want %d", name, got, want)
		}
	}
	tk.Step = 2
	if got := e.stepIndex(tk, "something unrelated"); got != 2 {
		t.Errorf("unrelated name should keep the current step, got %d", got)
	}
}
