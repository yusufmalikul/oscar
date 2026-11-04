package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"oscar/internal/store"
)

// deletable says whether a task with this status may be deleted: only finished tasks.
func deletable(status string) bool {
	return status == store.StatusDone || status == store.StatusCancelled || status == store.StatusFailed
}

// Delete removes a finished task from the board and cleans up what Oscar owns for it: its
// git worktrees, its workspace (work/<id>) and its task folder (tasks/<id>, with the log).
// Branches, remote branches and PRs are kept.
//
// When any worktree has uncommitted changes or commits that are on no remote, nothing is
// deleted: the task record is the only pointer to that work, so it stays until the user
// pushes or throws the work away.
func (e *Engine) Delete(taskID string) error {
	e.mu.Lock()
	t, err := e.task(taskID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if !deletable(t.Status) {
		e.mu.Unlock()
		return fmt.Errorf("%s is %s; only done, cancelled or failed tasks can be deleted", taskID, t.Status)
	}
	if e.workers[taskID] != nil || e.reviewers[taskID] != nil {
		e.mu.Unlock()
		return fmt.Errorf("%s still has a worker or reviewer running; try again in a moment", taskID)
	}
	if want := filepath.Join(e.cfg.WorkspaceDir, taskID); t.Workspace != "" && filepath.Clean(t.Workspace) != want {
		e.mu.Unlock()
		return fmt.Errorf("%s: workspace %s is not under Oscar's work dir; not deleting", taskID, t.Workspace)
	}
	// Take the task off the board while the git work runs, so nothing reopens or messages it.
	delete(e.tasks, taskID)
	wts := append([]store.Worktree{}, t.Worktrees...)
	e.mu.Unlock()

	restore := func(err error) error {
		e.mu.Lock()
		e.tasks[taskID] = t
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
		return err
	}

	for _, w := range wts {
		if err := e.worktreeSafeToRemove(t, w); err != nil {
			return restore(fmt.Errorf("%s not deleted: %w", taskID, err))
		}
	}
	for _, w := range wts {
		if err := e.removeWorktree(w); err != nil {
			return restore(fmt.Errorf("%s not deleted: %w", taskID, err))
		}
	}
	if t.Workspace != "" {
		if err := os.RemoveAll(t.Workspace); err != nil {
			return restore(fmt.Errorf("%s not deleted: %w", taskID, err))
		}
	}
	// The task is gone from the board now; a leftover folder only wastes disk.
	_ = os.RemoveAll(e.st.TaskDir(taskID))

	e.mu.Lock()
	delete(e.waiting, taskID)
	delete(e.planCmds, taskID)
	delete(e.finished, taskID)
	delete(e.lastText, taskID)
	delete(e.queued, taskID)
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	return nil
}

// worktreeSafeToRemove refuses a worktree with work that exists nowhere else.
func (e *Engine) worktreeSafeToRemove(t *store.Task, w store.Worktree) error {
	if rel, err := filepath.Rel(t.Workspace, w.Path); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("worktree %s is not inside the task workspace", w.Path)
	}
	if _, err := os.Stat(w.Path); os.IsNotExist(err) {
		return nil // already gone; prune cleans git's record
	}
	st, err := output(e.ctx, w.Path, nil, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("cannot check worktree %s: %w", w.Path, err)
	}
	if strings.TrimSpace(st) != "" {
		return fmt.Errorf("worktree %s has uncommitted changes; commit and push them, or discard them, first", w.Path)
	}
	n, err := output(e.ctx, w.Path, nil, "git", "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return fmt.Errorf("cannot check worktree %s: %w", w.Path, err)
	}
	if c, _ := strconv.Atoi(strings.TrimSpace(n)); c > 0 {
		return fmt.Errorf("worktree %s has %d commit(s) that are not pushed; push %s first", w.Path, c, w.Branch)
	}
	return nil
}

// removeWorktree removes one worktree through the repo it belongs to, then prunes.
func (e *Engine) removeWorktree(w store.Worktree) error {
	src := e.cfg.RepoPath(w.Repo)
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return fmt.Errorf("repo %s not found at %s", w.Repo, src)
	}
	if _, err := os.Stat(w.Path); err == nil {
		if err := runCmd(e.ctx, src, nil, "git", "worktree", "remove", w.Path); err != nil {
			return err
		}
	}
	return runCmd(e.ctx, src, nil, "git", "worktree", "prune")
}
