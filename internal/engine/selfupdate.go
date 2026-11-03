package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// ExitRestart is the exit code that tells the supervisor to start the (new) binary again.
const ExitRestart = 75

const updateNote = "update.json"
const rollbackNote = "update-rollback.json"

// SelfUpdate merges the task's PR on Oscar's own repo (when taskID is set), then pulls,
// tests, builds and installs the new binary and restarts through the supervisor.
// It runs in the background and reports progress in the Captain chat.
func (e *Engine) SelfUpdate(taskID string) error {
	if e.cfg.Self.Repo == "" {
		return errors.New("self update is not configured (set self.repo)")
	}
	if os.Getenv("OSCAR_SUPERVISED") != "1" || e.Restart == nil {
		return errors.New("Oscar is not running under its supervisor; start it with `oscar` (no arguments)")
	}
	var prURL, prBranch string
	if taskID != "" {
		e.mu.Lock()
		t, err := e.task(taskID)
		if err == nil {
			for _, p := range t.PRs {
				if p.Repo == e.cfg.Self.Repo {
					prURL, prBranch = p.URL, p.Branch
				}
			}
		}
		e.mu.Unlock()
		if err != nil {
			return err
		}
		if prURL == "" {
			return fmt.Errorf("%s has no PR on %s", taskID, e.cfg.Self.Repo)
		}
	}
	e.mu.Lock()
	if e.updating != "" {
		e.mu.Unlock()
		return errors.New("an update is already running")
	}
	e.updating = "starting"
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()

	go func() {
		err := e.runSelfUpdate(prURL, prBranch)
		e.mu.Lock()
		e.updating = ""
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
		if err != nil {
			e.chat("oscar", "Update stopped: "+err.Error())
		}
	}()
	return nil
}

func (e *Engine) updateStep(s string) {
	e.mu.Lock()
	e.updating = s
	e.publish(Event{Kind: "state"})
	e.mu.Unlock()
	e.chat("oscar", "Update: "+s)
}

func (e *Engine) runSelfUpdate(prURL, prBranch string) error {
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Minute)
	defer cancel()
	repo := e.cfg.RepoPath(e.cfg.Self.Repo)
	branch := e.cfg.Self.Branch

	if prURL != "" {
		e.updateStep("merging " + prURL)
		if _, err := runIntegration(ctx, e.cfg.Integration(e.cfg.Self.Repo, "merge_pr"), map[string]any{
			"repo": e.cfg.Self.Repo, "repo_path": repo, "url": prURL, "branch": prBranch, "base": branch,
		}); err != nil {
			return fmt.Errorf("merge: %w", err)
		}
	}

	e.updateStep("pulling " + branch)
	if out, _ := output(ctx, repo, nil, "git", "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(out) != "" {
		return fmt.Errorf("%s has local changes; commit or stash them first:\n%s", repo, out)
	}
	before, _ := output(ctx, repo, nil, "git", "rev-parse", "--short", "HEAD")
	for _, args := range [][]string{{"fetch", "--quiet", "origin"}, {"checkout", "--quiet", branch}, {"merge", "--ff-only", "--quiet", "origin/" + branch}} {
		if err := runCmd(ctx, repo, nil, "git", args...); err != nil {
			return err
		}
	}
	after, _ := output(ctx, repo, nil, "git", "rev-parse", "--short", "HEAD")
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)

	e.updateStep("running tests")
	if out, err := output(ctx, repo, nil, "go", "test", "./..."); err != nil {
		return fmt.Errorf("tests failed, nothing installed:\n%s", truncate(out+"\n"+err.Error(), 3000))
	}

	e.updateStep("building")
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	next, prev := exe+".next", exe+".prev"
	if err := runCmd(ctx, repo, nil, "go", "build", "-o", next, "./cmd/oscar"); err != nil {
		return err
	}
	args := []string{"check"}
	if e.cfg.Path != "" {
		args = append(args, "-config", e.cfg.Path)
	}
	if out, err := output(ctx, "", nil, next, args...); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("the new build failed its self-check, nothing installed: %v %s", err, out)
	}

	e.updateStep(fmt.Sprintf("installing %s → %s and restarting", before, after))
	if err := copyFile(exe, prev); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		return err
	}
	_ = e.st.SetValue(updateNote, map[string]string{"from": before, "to": after, "at": time.Now().Format(time.RFC3339)})
	go e.Restart()
	return nil
}

// ReportUpdate tells the chat how the last update went. Called once at start.
func (e *Engine) ReportUpdate() {
	var u map[string]string
	if e.st.Value(rollbackNote, &u) == nil {
		e.chat("oscar", fmt.Sprintf("The new version crashed on start, so Oscar rolled back to the previous one (%s).", u["detail"]))
		_ = os.Remove(filepath.Join(e.st.Dir(), rollbackNote))
		_ = os.Remove(filepath.Join(e.st.Dir(), updateNote))
		return
	}
	if e.st.Value(updateNote, &u) == nil {
		e.chat("oscar", fmt.Sprintf("Updated %s → %s. Running tasks were resumed.", u["from"], u["to"]))
		_ = os.Remove(filepath.Join(e.st.Dir(), updateNote))
	}
}

// Version is the commit this binary was built from (Go stamps it at build time).
func (e *Engine) Version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty && rev != "" {
		rev += "+dirty"
	}
	return rev
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}
