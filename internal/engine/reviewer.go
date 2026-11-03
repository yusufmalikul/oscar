package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"oscar/internal/claude"
	"oscar/internal/policy"
	"oscar/internal/redact"
	"oscar/internal/store"
)

const maxDiffInPrompt = 80_000

// startReviewer starts a fresh reviewer for the current round. workerNote is the worker's
// summary and evidence; empty after a restart (it is read from the task then).
func (e *Engine) startReviewer(taskID, workerNote string) {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return
	}
	if old := e.reviewers[taskID]; old != nil {
		e.mu.Unlock()
		old.Close()
		e.mu.Lock()
	}
	ty := e.types[t.Type]
	if workerNote == "" {
		workerNote = t.Summary + "\n\nEvidence from the worker:\n" + t.Report
	}
	scratch := e.scratch(t, fmt.Sprintf("review-%d", t.Round))
	wts := append([]store.Worktree(nil), t.Worktrees...)
	prompt := e.renderPrompt("reviewer.md", map[string]any{"Task": t, "Type": ty, "Scratch": scratch, "Rules": e.cfg.Rules()})
	brief := t.Brief
	round := t.Round
	e.mu.Unlock()

	var msg strings.Builder
	fmt.Fprintf(&msg, "Review round %d of task %s.\n\n## Brief given to the worker\n%s\n\n## What the worker says\n%s\n", round, taskID, brief, workerNote)
	if len(wts) > 0 {
		diff := worktreeDiff(e.ctx, wts)
		diffFile := filepath.Join(scratch, "diff.patch")
		_ = os.WriteFile(diffFile, []byte(diff), 0o600)
		if len(diff) > maxDiffInPrompt {
			diff = diff[:maxDiffInPrompt] + "\n[diff truncated; full diff in " + diffFile + "]"
		}
		fmt.Fprintf(&msg, "\n## Diff\n```diff\n%s\n```\n", diff)
	}
	msg.WriteString("\nCheck it, run the tests yourself, then call the verdict tool exactly once.")

	var roots []string
	for _, w := range wts {
		roots = append(roots, w.Path)
	}
	sp := sessionSpec{role: policy.Reviewer, taskID: taskID, tools: e.reviewerTools(taskID, round), prompt: prompt,
		rc: e.cfg.Reviewer, addDirs: append([]string{scratch}, roots...),
		pctx: policy.Context{Role: policy.Reviewer, WriteRoots: []string{scratch}}}
	e.logLine(taskID, "oscar", "info", fmt.Sprintf("starting reviewer (round %d)", round))
	s, err := e.startSession(sp)
	if err != nil {
		e.fail(taskID, err)
		return
	}
	e.mu.Lock()
	e.reviewers[taskID] = s
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	go func() {
		<-s.Done()
		e.mu.Lock()
		if e.reviewers[taskID] == s {
			delete(e.reviewers, taskID)
		}
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
		go e.schedule()
	}()
	if err := s.Send(msg.String()); err != nil {
		e.fail(taskID, err)
	}
}

func (e *Engine) reviewerTools(taskID string, round int) []claude.Tool {
	finding := obj(map[string]any{
		"severity": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		"where": str, "problem": str, "scenario": str,
	}, "severity", "where", "problem", "scenario")
	return []claude.Tool{{
		Name:        "verdict",
		Description: "Give the verdict, once. pass=true only if you would ship it as is.",
		Schema: obj(map[string]any{"pass": map[string]any{"type": "boolean"}, "summary": str,
			"findings": map[string]any{"type": "array", "items": finding}}, "pass", "summary"),
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			var v store.Verdict
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", err
			}
			return e.applyVerdict(taskID, round, v)
		},
	}}
}

func (e *Engine) applyVerdict(taskID string, round int, v store.Verdict) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.task(taskID)
	if err != nil {
		return "", err
	}
	if t.Round != round {
		return "", errors.New("this review round is over")
	}
	for _, old := range t.Verdicts {
		if old.Round == round {
			return "", errors.New("verdict already given for this round")
		}
	}
	v.Round, v.At = round, time.Now()
	v.Summary = redact.String(v.Summary)
	for i := range v.Findings {
		v.Findings[i].Kept = true
	}
	t.Verdicts = append(t.Verdicts, v)
	ty := e.types[t.Type]

	if v.Pass {
		t.ReviewPassed = true
		t.Status = store.StatusRunning
		e.save(t)
		switch ty.Output {
		case "draft_pr":
			go e.resumeWorker(taskID, "Review passed: "+v.Summary+"\n\nOpen the draft PR for each repo with open_draft_pr (short, minimal description), then call finish.")
		default:
			go e.resumeWorker(taskID, "Review passed: "+v.Summary+"\n\nCall finish now with a short summary for the user.")
		}
		return "Verdict recorded: pass. You are done.", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Review round %d failed: %s\n", round, v.Summary)
	for _, f := range v.Findings {
		fmt.Fprintf(&b, "- [%s] %s — %s. Scenario: %s\n", f.Severity, f.Where, f.Problem, f.Scenario)
	}
	if round >= e.cfg.MaxReviewRounds {
		e.needsYou(t, store.Pending{Kind: store.PendingEscalate,
			Question: fmt.Sprintf("Review failed %d times. Last findings:\n%s\nReply with guidance to continue, or cancel.", round, b.String())})
		return "Verdict recorded: fail. The user decides what happens next. You are done.", nil
	}
	t.Status = store.StatusRunning
	e.save(t)
	b.WriteString("\nFix these (or explain with evidence why a finding is wrong), then submit_for_review again.")
	go e.resumeWorker(taskID, b.String())
	return "Verdict recorded: fail. You are done.", nil
}

func (e *Engine) reviewerTurnEnded(taskID string) {
	e.mu.Lock()
	t, err := e.task(taskID)
	s := e.reviewers[taskID]
	delete(e.reviewers, taskID)
	if err == nil {
		got := false
		for _, v := range t.Verdicts {
			if v.Round == t.Round {
				got = true
			}
		}
		if !got && t.Status == store.StatusReview {
			e.needsYou(t, store.Pending{Kind: store.PendingEscalate, Question: "The reviewer stopped without a verdict. Reply \"retry review\" to start a new reviewer, or give guidance."})
		}
	}
	e.mu.Unlock()
	if s != nil {
		s.Close()
	}
	go e.schedule()
}
