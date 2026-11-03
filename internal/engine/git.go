package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"oscar/internal/store"
)

func runCmd(ctx context.Context, dir string, stdin []byte, name string, args ...string) error {
	_, err := output(ctx, dir, stdin, name, args...)
	return err
}

func output(ctx context.Context, dir string, stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// runIntegration runs a configured integration command with payload as JSON on stdin and
// parses JSON from stdout when there is any.
func runIntegration(ctx context.Context, cmd []string, payload any) (map[string]any, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("integration not configured")
	}
	b, _ := json.Marshal(payload)
	out, err := output(ctx, "", b, cmd[0], cmd[1:]...)
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	if s := strings.TrimSpace(out); strings.HasPrefix(s, "{") {
		_ = json.Unmarshal([]byte(s), &res)
	} else if s != "" {
		res["output"] = s
	}
	return res, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func defaultBase(ctx context.Context, repo string) string {
	if out, err := output(ctx, repo, nil, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b
		}
	}
	for _, b := range []string{"origin/main", "origin/master"} {
		if runCmd(ctx, repo, nil, "git", "rev-parse", "--verify", "--quiet", b) == nil {
			return b
		}
	}
	return "HEAD"
}

// prepareWorktrees creates one worktree per repo for a code task.
func (e *Engine) prepareWorktrees(t *store.Task) ([]store.Worktree, error) {
	var out []store.Worktree
	for _, repo := range t.Repos {
		src := filepath.Join(e.cfg.ReposDir, repo)
		if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
			return out, fmt.Errorf("repo %s not found under %s", repo, e.cfg.ReposDir)
		}
		// Fetch is best effort: offline work should still start.
		_ = runCmd(e.ctx, src, nil, "git", "fetch", "--quiet", "origin")
		base := defaultBase(e.ctx, src)
		branch := fmt.Sprintf("oscar/%s-%s", strings.ToLower(t.ID), slug(t.Title))
		path := filepath.Join(t.Workspace, repo)
		if _, err := os.Stat(path); err == nil {
			out = append(out, store.Worktree{Repo: repo, Path: path, Branch: branch, Base: base})
			continue
		}
		if err := runCmd(e.ctx, src, nil, "git", "worktree", "add", "-b", branch, path, base); err != nil {
			// Branch may exist from an earlier attempt.
			if err2 := runCmd(e.ctx, src, nil, "git", "worktree", "add", path, branch); err2 != nil {
				return out, err
			}
		}
		out = append(out, store.Worktree{Repo: repo, Path: path, Branch: branch, Base: base})
	}
	return out, nil
}

// Diff returns the combined diff of all worktrees of a task against their base.
func (e *Engine) Diff(taskID string) (string, error) {
	e.mu.Lock()
	t, err := e.task(taskID)
	var wts []store.Worktree
	if err == nil {
		wts = append(wts, t.Worktrees...)
	}
	e.mu.Unlock()
	if err != nil {
		return "", err
	}
	return worktreeDiff(e.ctx, wts), nil
}

func worktreeDiff(ctx context.Context, wts []store.Worktree) string {
	var b strings.Builder
	for _, w := range wts {
		fmt.Fprintf(&b, "### %s (%s vs %s)\n", w.Repo, w.Branch, w.Base)
		stat, _ := output(ctx, w.Path, nil, "git", "diff", "--stat", w.Base)
		b.WriteString(stat)
		diff, _ := output(ctx, w.Path, nil, "git", "diff", w.Base)
		b.WriteString(diff)
		if un, _ := output(ctx, w.Path, nil, "git", "ls-files", "--others", "--exclude-standard"); strings.TrimSpace(un) != "" {
			b.WriteString("\nUntracked files (not committed):\n" + un)
		}
		b.WriteString("\n")
	}
	return b.String()
}
