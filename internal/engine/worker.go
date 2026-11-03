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

func timeNow() time.Time { return time.Now() }

// CreateTask adds a task to the queue.
func (e *Engine) CreateTask(typ, title, brief, source string, repos []string) (*store.Task, error) {
	e.mu.Lock()
	ty := e.types[typ]
	e.mu.Unlock()
	if ty == nil {
		return nil, fmt.Errorf("unknown task type %q (use propose_type for a new one)", typ)
	}
	if ty.Code && len(repos) == 0 {
		return nil, fmt.Errorf("type %s needs at least one repo", typ)
	}
	for _, r := range repos {
		if ty.Code && !contains(e.repoNames, r) {
			return nil, fmt.Errorf("unknown repo %q; known repos: %s", r, strings.Join(e.repoNames, ", "))
		}
	}
	id, n, err := e.st.NextID()
	if err != nil {
		return nil, err
	}
	t := &store.Task{ID: id, N: n, Type: typ, Title: redact.String(title), Brief: redact.String(brief),
		Source: source, Repos: repos, Status: store.StatusQueued, Step: -1, Created: time.Now(),
		Workspace: filepath.Join(e.cfg.WorkspaceDir, id)}
	e.mu.Lock()
	e.tasks[id] = t
	e.save(t)
	e.mu.Unlock()
	go e.schedule()
	return t, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// schedule starts queued tasks while there are free worker slots.
func (e *Engine) schedule() {
	for {
		e.mu.Lock()
		if len(e.workers)+len(e.reviewers) >= e.cfg.MaxWorkers {
			e.mu.Unlock()
			return
		}
		var next *store.Task
		for _, t := range e.tasks {
			if t.Status == store.StatusQueued && (next == nil || t.N < next.N) {
				next = t
			}
		}
		if next == nil {
			e.mu.Unlock()
			return
		}
		next.Status = store.StatusRunning
		next.Step = 0
		e.save(next)
		e.mu.Unlock()
		if err := e.startWorker(next.ID, ""); err != nil {
			e.fail(next.ID, err)
		}
	}
}

func (e *Engine) fail(taskID string, err error) {
	e.logLine(taskID, "oscar", "error", err.Error())
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, er := e.task(taskID); er == nil {
		e.needsYou(t, store.Pending{Kind: store.PendingEscalate, Question: "Oscar hit an error: " + truncate(err.Error(), 400) + ". Reply to retry, or cancel the task."})
	}
}

func (e *Engine) scratch(t *store.Task, role string) string {
	d := filepath.Join(e.st.TaskDir(t.ID), role)
	_ = os.MkdirAll(d, 0o700)
	return d
}

// startWorker starts (or resumes) the worker process of a task and sends msg.
// An empty msg on a new session sends the brief.
func (e *Engine) startWorker(taskID, msg string) error {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if s := e.workers[taskID]; s != nil {
		if msg == "" {
			e.mu.Unlock()
			return nil
		}
		e.queued[taskID]++
		e.mu.Unlock()
		return s.Send(msg)
	}
	ty := e.types[t.Type]
	needTrees := ty.Code && len(t.Worktrees) == 0
	e.mu.Unlock()

	if err := os.MkdirAll(t.Workspace, 0o700); err != nil {
		return err
	}
	if needTrees {
		e.logLine(taskID, "oscar", "info", "creating worktrees")
		wts, err := e.prepareWorktrees(t)
		e.mu.Lock()
		t.Worktrees = wts
		e.save(t)
		e.mu.Unlock()
		if err != nil {
			return err
		}
	}

	e.mu.Lock()
	scratch := e.scratch(t, "worker")
	var repoRoots []string
	for _, r := range t.Repos {
		repoRoots = append(repoRoots, e.cfg.RepoPath(r))
	}
	prompt := e.renderPrompt("worker.md", map[string]any{
		"Task": t, "Type": ty, "Scratch": scratch, "ReposDir": e.cfg.ReposDir, "Rules": e.cfg.Rules(),
	})
	resume := t.WorkerSession
	first := msg
	if first == "" {
		first = "Task " + t.ID + ": " + t.Title + "\n\n" + t.Brief
		if t.Source != "" {
			first += "\n\nSource: " + t.Source
		}
	}
	e.mu.Unlock()

	sp := sessionSpec{role: policy.Worker, taskID: taskID, tools: e.workerTools(taskID), prompt: prompt, resume: resume,
		rc: e.cfg.Worker, addDirs: []string{t.Workspace, scratch},
		pctx: policy.Context{Role: policy.Worker, WriteRoots: []string{t.Workspace, scratch}, RepoRoots: repoRoots}}
	e.logLine(taskID, "oscar", "info", map[bool]string{true: "resuming worker", false: "starting worker"}[resume != ""])
	s, err := e.startSession(sp)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.workers[taskID] = s
	e.waiting[taskID] = false
	e.queued[taskID] = 1 // the first message below
	if t.Status == store.StatusQueued || t.Status == store.StatusNeedsYou && len(t.Pending) == 0 {
		t.Status = store.StatusRunning
	}
	e.save(t)
	e.mu.Unlock()
	go func() {
		<-s.Done()
		e.mu.Lock()
		if e.workers[taskID] == s {
			delete(e.workers, taskID)
		}
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
		go e.schedule()
	}()
	return s.Send(first)
}

// resumeWorker sends msg to the worker, starting it from its saved session when needed.
func (e *Engine) resumeWorker(taskID, msg string) {
	if err := e.startWorker(taskID, msg); err != nil {
		e.fail(taskID, err)
	}
}

func (e *Engine) workerTurnEnded(taskID string, isError bool, result string) {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return
	}
	s := e.workers[taskID]
	if e.queued[taskID] > 0 {
		e.queued[taskID]--
	}
	if e.queued[taskID] > 0 {
		// Another message is queued in this session; its turn follows. Keep the session open.
		e.mu.Unlock()
		return
	}
	waiting := e.waiting[taskID] || e.finished[taskID]
	hasBlockedTool := false
	for _, p := range t.Pending {
		if p.Kind == store.PendingTool {
			hasBlockedTool = true
		}
	}
	if !waiting && !hasBlockedTool && t.Status != store.StatusCancelled && t.Status != store.StatusDone {
		q := "The worker stopped without reporting. Its last message: " + truncate(e.lastText[taskID], 600)
		if isError {
			q = "The worker turn failed: " + truncate(result, 400)
		}
		e.needsYou(t, store.Pending{Kind: store.PendingEscalate, Question: q})
	}
	if !hasBlockedTool {
		delete(e.workers, taskID) // the process is closed below; the next message resumes it
	}
	e.mu.Unlock()
	if s != nil && !hasBlockedTool {
		s.Close()
	}
	go e.schedule()
}

// ---------- worker tools ----------

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

var str = map[string]any{"type": "string"}
var strList = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

func (e *Engine) workerTools(taskID string) []claude.Tool {
	h := func(f func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error)) func(context.Context, json.RawMessage) (string, error) {
		return func(_ context.Context, raw json.RawMessage) (string, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			t, err := e.task(taskID)
			if err != nil {
				return "", err
			}
			if t.Status == store.StatusCancelled {
				return "", errors.New("the task was cancelled; stop now")
			}
			return f(t, e.types[t.Type], raw)
		}
	}
	return []claude.Tool{
		{Name: "report", Description: "Report the step you are starting, with one short line of progress.",
			Schema: obj(map[string]any{"step": str, "note": str}, "step", "note"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct{ Step, Note string }
				_ = json.Unmarshal(raw, &a)
				e.setStep(t, a.Step)
				if a.Note != "" {
					t.Progress = append(t.Progress, redact.String(a.Note))
				}
				e.save(t)
				return "ok", nil
			})},
		{Name: "ask", Description: "Ask the user a question and wait. End your turn after calling this; the answer arrives as the next message.",
			Schema: obj(map[string]any{"question": str, "options": strList}, "question"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct {
					Question string
					Options  []string
				}
				_ = json.Unmarshal(raw, &a)
				e.waiting[t.ID] = true
				e.needsYou(t, store.Pending{Kind: store.PendingQuestion, Question: redact.String(a.Question), Options: a.Options})
				return "Question sent to the user. End your turn now and wait for the answer.", nil
			})},
		{Name: "request_approval", Description: "Ops tasks: send the exact plan and the exact commands that change state. End your turn and wait. After approval, only these commands run without asking.",
			Schema: obj(map[string]any{"plan": str, "commands": strList}, "plan", "commands"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct {
					Plan     string
					Commands []string
				}
				_ = json.Unmarshal(raw, &a)
				in, _ := json.Marshal(a.Commands)
				e.setStep(t, "You approve")
				e.waiting[t.ID] = true
				e.needsYou(t, store.Pending{Kind: store.PendingPlan, Question: "Approve this plan?", Detail: redact.String(a.Plan), Input: in})
				return "Plan sent. End your turn now and wait for approval.", nil
			})},
		{Name: "outbox", Description: "Draft a message for the user to approve: kind=slack (channel id, thread ts) or kind=pr_comment (repo, pr number, optional path+line for inline). Oscar posts it after approval; you never post.",
			Schema: obj(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"slack", "pr_comment"}},
				"text": str, "channel": str, "thread": str, "target": str, "repo": str, "pr": str, "path": str, "line": map[string]any{"type": "integer"}}, "kind", "text"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var o store.OutboxItem
				if err := json.Unmarshal(raw, &o); err != nil {
					return "", err
				}
				if o.Kind == "slack" && o.Channel == "" {
					return "", errors.New("slack needs channel (the channel id from the link)")
				}
				if o.Kind == "pr_comment" && (o.Repo == "" || o.PR == "") {
					return "", errors.New("pr_comment needs repo and pr")
				}
				o.ID, o.Status, o.Updated = newID("o"), "draft", time.Now()
				o.Text = redact.String(o.Text)
				if o.Target == "" {
					o.Target = map[string]string{"slack": "Slack " + o.Channel, "pr_comment": o.Repo + " PR #" + o.PR}[o.Kind]
				}
				t.Outbox = append(t.Outbox, o)
				e.save(t)
				return "Draft saved (" + o.ID + "). The user sees it after you finish or after review.", nil
			})},
		{Name: "submit_for_review", Description: "Hand the work to a fresh reviewer. Include a short summary and the evidence (test output before/after, queries, links). End your turn after calling this.",
			Schema: obj(map[string]any{"summary": str, "evidence": str}, "summary", "evidence"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct{ Summary, Evidence string }
				_ = json.Unmarshal(raw, &a)
				if !ty.Reviewer {
					return "", errors.New("this task type has no reviewer; call finish")
				}
				if e.roundsLeft(t) <= 0 {
					return "", errors.New("no review rounds left; the user has been asked")
				}
				t.Summary = redact.String(a.Summary)
				t.Report = redact.String(a.Evidence)
				t.Round++
				e.setStep(t, "Reviewer")
				t.Status = store.StatusReview
				e.waiting[t.ID] = true
				e.save(t)
				go e.startReviewer(t.ID, a.Summary+"\n\nEvidence from the worker:\n"+a.Evidence)
				return "Submitted. End your turn now; you will get the verdict as a message.", nil
			})},
		{Name: "open_draft_pr", Description: "After the review passed: Oscar pushes your branch and opens a draft PR. Returns the URL.",
			Schema:  obj(map[string]any{"repo": str, "title": str, "description": str}, "repo", "title", "description"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) { return e.openDraftPR(ctx, taskID, raw) }},
		{Name: "rename_branch", Description: "Rename your branch (e.g. to the team's naming style). Oscar renames it, pushes the new branch and, when a PR is open, opens a new draft PR from it (title/description required then) and asks the user to decline the old PR and delete the old branch.",
			Schema: obj(map[string]any{"repo": str, "new_name": str, "title": str, "description": str}, "repo", "new_name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return e.renameBranch(ctx, taskID, raw)
			}},
		{Name: "update_pr", Description: "Propose a new description (and optionally title) for a PR this task opened. The user sees it in the outbox, can edit it, and Oscar updates the PR when they press Post.",
			Schema: obj(map[string]any{"repo": str, "description": str, "title": str}, "repo", "description"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct{ Repo, Description, Title string }
				_ = json.Unmarshal(raw, &a)
				var pr *store.PR
				for i := range t.PRs {
					if t.PRs[i].Repo == a.Repo {
						pr = &t.PRs[i]
					}
				}
				if pr == nil {
					return "", fmt.Errorf("this task has no PR on %s", a.Repo)
				}
				if w := forbiddenWord(e.cfg.Rules(), a.Title+"\n"+a.Description); w != "" {
					return "", fmt.Errorf("the text mentions %q, which the local rules forbid in PR text", w)
				}
				o := store.OutboxItem{ID: newID("o"), Kind: "pr_update", Repo: a.Repo, URL: pr.URL, Title: redact.String(a.Title),
					Text: redact.String(a.Description), Target: "description of " + pr.URL, Status: "draft", Updated: time.Now()}
				t.Outbox = append(t.Outbox, o)
				e.save(t)
				return "Draft saved; the user approves it after you finish. Call finish now.", nil
			})},
		{Name: "finish", Description: "Finish the task with a short summary for the user and any lessons worth remembering.",
			Schema: obj(map[string]any{"summary": str, "learnings": str}, "summary"),
			Handler: h(func(t *store.Task, ty *TaskType, raw json.RawMessage) (string, error) {
				var a struct{ Summary, Learnings string }
				_ = json.Unmarshal(raw, &a)
				if ty.Reviewer && !t.ReviewPassed {
					return "", errors.New("submit_for_review first; the task is not reviewed yet")
				}
				if ty.Output == "draft_pr" && len(t.PRs) < len(t.Worktrees) {
					return "", errors.New("open the draft PR for every repo with changes first (or ask the user)")
				}
				t.Summary = redact.String(a.Summary)
				if a.Learnings != "" {
					t.Progress = append(t.Progress, "Learnings: "+redact.String(a.Learnings))
				}
				e.finished[t.ID] = true
				e.waiting[t.ID] = true
				e.setStep(t, ty.Steps[len(ty.Steps)-1])
				e.releaseOutbox(t)
				e.settle(t)
				e.save(t)
				return "Finished.", nil
			})},
	}
}

// releaseOutbox turns draft outbox items into decisions for the user. Must hold e.mu.
func (e *Engine) releaseOutbox(t *store.Task) {
	for _, o := range t.Outbox {
		if o.Status != "draft" {
			continue
		}
		exists := false
		for _, p := range t.Pending {
			if p.OutboxID == o.ID {
				exists = true
			}
		}
		if !exists {
			q := "Post this to " + o.Target + "?"
			t.Pending = append(t.Pending, store.Pending{ID: newID("p"), Kind: store.PendingOutbox, OutboxID: o.ID, Question: q, Created: time.Now()})
		}
	}
	if len(t.Pending) > 0 {
		t.Status = store.StatusNeedsYou
		go e.notify(t.ID+" needs you", "Messages are ready to post")
	}
}

func (e *Engine) openDraftPR(ctx context.Context, taskID string, raw json.RawMessage) (string, error) {
	var a struct{ Repo, Title, Description string }
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return "", err
	}
	ty := e.types[t.Type]
	if ty.Output != "draft_pr" {
		e.mu.Unlock()
		return "", errors.New("this task type does not open PRs")
	}
	if !t.ReviewPassed {
		e.mu.Unlock()
		return "", errors.New("the review has not passed yet")
	}
	var wt *store.Worktree
	for i := range t.Worktrees {
		if t.Worktrees[i].Repo == a.Repo {
			wt = &t.Worktrees[i]
		}
	}
	existing := ""
	var superseded *store.PR
	for _, p := range t.PRs {
		if p.Repo != a.Repo {
			continue
		}
		if wt := worktreeOf(t, a.Repo); wt != nil && p.Branch != "" && p.Branch != wt.Branch {
			// The branch was renamed after this PR was opened: the PR still points at the old
			// branch, so a new PR is needed and the old one should be closed.
			pp := p
			superseded = &pp
			continue
		}
		existing = p.URL
	}
	e.mu.Unlock()
	if wt == nil {
		return "", fmt.Errorf("no worktree for repo %s", a.Repo)
	}
	if w := forbiddenWord(e.cfg.Rules(), a.Title+"\n"+a.Description); w != "" {
		return "", fmt.Errorf("the PR text mentions %q, which the local rules forbid in PR text; remove it and call again", w)
	}
	if out, _ := output(ctx, wt.Path, nil, "git", "status", "--porcelain"); strings.TrimSpace(out) != "" {
		return "", errors.New("the worktree has uncommitted changes; commit them first")
	}
	if e.cfg.IsPublic(a.Repo) {
		diff, _ := output(ctx, wt.Path, nil, "git", "diff", wt.Base+"...HEAD")
		log, _ := output(ctx, wt.Path, nil, "git", "log", "--format=%an %ae%n%B", wt.Base+"..HEAD")
		if hit := findForbidden(e.cfg.ForbiddenWords, map[string]string{"PR title/description": a.Title + "\n" + a.Description, "diff": diff, "commit messages/authors": log}); hit != "" {
			return "", fmt.Errorf("%s is a public repo and %s. Remove it (rewrite the commits if needed) and call again", a.Repo, hit)
		}
	}
	e.logLine(taskID, "oscar", "info", "pushing "+wt.Branch+" (the repo's pre-push hook may run tests first)")
	e.mu.Lock()
	e.setStep(t, "Draft PR")
	t.StepNote = "Draft PR: pushing " + wt.Branch + " (pre-push hook may run tests)"
	e.save(t)
	e.mu.Unlock()
	started := time.Now()
	if err := runCmd(ctx, wt.Path, nil, "git", "push", "-u", "origin", wt.Branch); err != nil {
		return "", err
	}
	e.logLine(taskID, "oscar", "info", fmt.Sprintf("pushed in %s", time.Since(started).Round(time.Second)))
	if existing != "" {
		// A reopened task: pushing the same branch updates the PR that is already open.
		e.logLine(taskID, "oscar", "info", "pushed new commits to the existing PR "+existing)
		return "Pushed. The existing draft PR now has your new commits: " + existing + ". Update its description only if the user asked.", nil
	}
	res, err := runIntegration(ctx, e.cfg.Integration(a.Repo, "draft_pr"), map[string]any{
		"repo": a.Repo, "repo_path": e.cfg.RepoPath(a.Repo), "worktree": wt.Path,
		"branch": wt.Branch, "base": strings.TrimPrefix(wt.Base, "origin/"), "title": a.Title, "description": a.Description,
	})
	if err != nil {
		return "", fmt.Errorf("draft PR integration: %w (the branch is pushed)", err)
	}
	url, _ := res["url"].(string)
	e.mu.Lock()
	if superseded != nil {
		t.PRs = removePR(t.PRs, a.Repo)
	}
	t.PRs = append(t.PRs, store.PR{Repo: a.Repo, Branch: wt.Branch, URL: url, Title: a.Title})
	e.setStep(t, "Draft PR")
	if superseded != nil {
		in, _ := json.Marshal(map[string]string{"repo": a.Repo, "url": superseded.URL, "branch": superseded.Branch, "new": wt.Branch})
		t.Progress = append(t.Progress, "Moved PR "+superseded.URL+" to "+url)
		e.needsYou(t, store.Pending{Kind: store.PendingClosePR, Input: in,
			Question: "Decline the old PR " + superseded.URL + " and delete branch " + superseded.Branch + "?",
			Detail:   "The work moved to branch " + wt.Branch + ", new PR: " + url})
	}
	e.save(t)
	e.mu.Unlock()
	e.logLine(taskID, "oscar", "info", "draft PR opened: "+url)
	return "Draft PR opened: " + url, nil
}

// forbiddenWord reads lines like "forbidden_in_pr: word1, word2" from the rules text.
func forbiddenWord(rules, text string) string {
	low := strings.ToLower(text)
	for _, line := range strings.Split(rules, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(strings.ToLower(k)) != "forbidden_in_pr" {
			continue
		}
		for _, w := range strings.Split(v, ",") {
			w = strings.TrimSpace(strings.ToLower(w))
			if w != "" && strings.Contains(low, w) {
				return w
			}
		}
	}
	return ""
}

// findForbidden returns a description of the first forbidden word found in any part, or "".
func findForbidden(words []string, parts map[string]string) string {
	for name, text := range parts {
		low := strings.ToLower(text)
		for _, w := range words {
			w = strings.TrimSpace(strings.ToLower(w))
			if w != "" && strings.Contains(low, w) {
				return fmt.Sprintf("the %s contains %q, which must not appear in a public repo", name, w)
			}
		}
	}
	return ""
}

func (e *Engine) renameBranch(ctx context.Context, taskID string, raw json.RawMessage) (string, error) {
	var a struct{ Repo, NewName, Title, Description string }
	_ = json.Unmarshal(raw, &a)
	var ar struct {
		NewName string `json:"new_name"`
	}
	_ = json.Unmarshal(raw, &ar)
	a.NewName = strings.TrimSpace(ar.NewName)
	if a.NewName == "" || runCmd(ctx, "", nil, "git", "check-ref-format", "--branch", a.NewName) != nil {
		return "", fmt.Errorf("%q is not a valid branch name", a.NewName)
	}
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return "", err
	}
	idx := -1
	for i := range t.Worktrees {
		if t.Worktrees[i].Repo == a.Repo {
			idx = i
		}
	}
	if idx < 0 {
		e.mu.Unlock()
		return "", fmt.Errorf("no worktree for repo %s", a.Repo)
	}
	wt := t.Worktrees[idx]
	var oldPR *store.PR
	for i := range t.PRs {
		if t.PRs[i].Repo == a.Repo {
			p := t.PRs[i]
			oldPR = &p
		}
	}
	e.mu.Unlock()
	if wt.Branch == a.NewName {
		return "The branch already has that name.", nil
	}
	if oldPR != nil && (strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Description) == "") {
		return "", errors.New("a PR is open for this branch: pass title and description for the new PR")
	}
	if w := forbiddenWord(e.cfg.Rules(), a.NewName+"\n"+a.Title+"\n"+a.Description); w != "" {
		return "", fmt.Errorf("the new name or PR text mentions %q, which the local rules forbid", w)
	}
	if err := runCmd(ctx, wt.Path, nil, "git", "branch", "-m", wt.Branch, a.NewName); err != nil {
		return "", err
	}
	oldBranch := wt.Branch
	e.mu.Lock()
	t.Worktrees[idx].Branch = a.NewName
	e.save(t)
	e.mu.Unlock()
	e.logLine(taskID, "oscar", "info", "renamed branch "+oldBranch+" → "+a.NewName)
	if oldPR == nil {
		return "Renamed " + oldBranch + " → " + a.NewName + ". It is pushed when you open the draft PR.", nil
	}
	// A PR is open on the old branch. Bitbucket and GitHub cannot change a PR's source branch,
	// so push the new branch, open a new draft PR, and ask the user to close the old one.
	if !t.ReviewPassed {
		return "Renamed locally. The open PR still points at " + oldBranch + "; it moves to the new branch when the review passes and you call open_draft_pr.", nil
	}
	e.mu.Lock()
	t.PRs = removePR(t.PRs, a.Repo)
	e.mu.Unlock()
	res, err := e.openDraftPR(ctx, taskID, mustRaw(map[string]string{"repo": a.Repo, "title": a.Title, "description": a.Description}))
	if err != nil {
		e.mu.Lock()
		t.PRs = append(t.PRs, *oldPR) // keep tracking the old PR if the new one failed
		e.save(t)
		e.mu.Unlock()
		return "", fmt.Errorf("renamed, but the new PR failed: %w", err)
	}
	e.mu.Lock()
	t.Progress = append(t.Progress, "Moved PR "+oldPR.URL+" to branch "+a.NewName)
	in, _ := json.Marshal(map[string]string{"repo": a.Repo, "url": oldPR.URL, "branch": oldBranch, "new": a.NewName})
	e.needsYou(t, store.Pending{Kind: store.PendingClosePR, Input: in,
		Question: "Decline the old PR " + oldPR.URL + " and delete branch " + oldBranch + "?",
		Detail:   "The work moved to branch " + a.NewName + ". " + res})
	e.mu.Unlock()
	return res + " The user is asked to decline the old PR " + oldPR.URL + " and delete the old branch.", nil
}

func removePR(prs []store.PR, repo string) []store.PR {
	out := prs[:0]
	for _, p := range prs {
		if p.Repo != repo {
			out = append(out, p)
		}
	}
	return out
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func worktreeOf(t *store.Task, repo string) *store.Worktree {
	for i := range t.Worktrees {
		if t.Worktrees[i].Repo == repo {
			return &t.Worktrees[i]
		}
	}
	return nil
}
