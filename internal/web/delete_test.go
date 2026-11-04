package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"oscar/internal/config"
	"oscar/internal/engine"
	"oscar/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

type deleteRig struct {
	cfg config.Config
	st  *store.Store
	src string // the user's main checkout of repo "app"
	url string
}

// newDeleteRig makes a repo "app" with a remote, and one task per entry in tasks. Each task
// with status done/cancelled gets a worktree on its own branch, pushed to the remote.
func newDeleteRig(t *testing.T, tasks map[string]string) *deleteRig {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = filepath.Join(root, "data"), filepath.Join(root, "types")
	cfg.ReposDir, cfg.WorkspaceDir = filepath.Join(root, "repos"), filepath.Join(root, "data", "work")
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "-q", "--bare", origin)
	src := filepath.Join(cfg.ReposDir, "app")
	git(t, root, "clone", "-q", origin, src)
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, src, "push", "-q", "origin", "HEAD:main")

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for id, status := range tasks {
		n++
		ws := filepath.Join(cfg.WorkspaceDir, id)
		branch := "feature/" + strings.ToLower(id)
		wt := filepath.Join(ws, "app")
		git(t, src, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
		if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, wt, "add", ".")
		git(t, wt, "commit", "-q", "-m", "work")
		git(t, wt, "push", "-q", "origin", "HEAD:"+branch)
		if err := os.MkdirAll(filepath.Join(st.TaskDir(id), "worker"), 0o700); err != nil {
			t.Fatal(err)
		}
		tk := &store.Task{ID: id, N: n, Type: "feature", Title: "t", Repos: []string{"app"}, Status: status, Workspace: ws,
			Worktrees: []store.Worktree{{Repo: "app", Path: wt, Branch: branch, Base: "origin/main"}}}
		if err := st.SaveTask(tk); err != nil {
			t.Fatal(err)
		}
	}
	e, err := engine.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(e, fstest.MapFS{}).Handler())
	t.Cleanup(srv.Close)
	return &deleteRig{cfg: cfg, st: st, src: src, url: srv.URL}
}

func (r *deleteRig) delete(t *testing.T, id string) (int, string) {
	t.Helper()
	res, err := http.Post(r.url+"/api/tasks/"+id+"/delete", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ Error string }
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Error
}

func (r *deleteRig) onBoard(t *testing.T, id string) bool {
	t.Helper()
	res, err := http.Get(r.url + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var s struct{ Tasks []store.Task }
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	for _, tk := range s.Tasks {
		if tk.ID == id {
			return true
		}
	}
	return false
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestDeleteDoneTask(t *testing.T) {
	r := newDeleteRig(t, map[string]string{"Item-1": store.StatusDone, "Item-2": store.StatusCancelled})
	for _, id := range []string{"Item-1", "Item-2"} {
		if code, msg := r.delete(t, id); code != http.StatusOK {
			t.Fatalf("delete %s: %d %s", id, code, msg)
		}
		if r.onBoard(t, id) {
			t.Errorf("%s still on the board", id)
		}
		for _, p := range []string{filepath.Join(r.cfg.WorkspaceDir, id), r.st.TaskDir(id)} {
			if exists(p) {
				t.Errorf("%s left behind", p)
			}
		}
		branch := "feature/" + strings.ToLower(id)
		if strings.Contains(git(t, r.src, "worktree", "list"), id) {
			t.Errorf("git still lists the worktree of %s", id)
		}
		// Branches stay: local and on the remote.
		git(t, r.src, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		git(t, r.src, "ls-remote", "--exit-code", "origin", "refs/heads/"+branch)
	}
	if !exists(filepath.Join(r.src, "a.txt")) {
		t.Fatal("the main checkout was touched")
	}
	// A deleted id is simply unknown now.
	if code, _ := r.delete(t, "Item-1"); code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", code)
	}
}

func TestDeleteRefusesUnfinishedTask(t *testing.T) {
	for _, status := range []string{store.StatusRunning, store.StatusReview, store.StatusNeedsYou, store.StatusQueued} {
		r := newDeleteRig(t, map[string]string{"Item-1": status})
		code, msg := r.delete(t, "Item-1")
		if code != http.StatusBadRequest || !strings.Contains(msg, "only done, cancelled or failed") {
			t.Fatalf("%s: got %d %q", status, code, msg)
		}
		if !r.onBoard(t, "Item-1") || !exists(filepath.Join(r.cfg.WorkspaceDir, "Item-1", "app", "b.txt")) {
			t.Fatalf("%s: task or worktree was removed", status)
		}
	}
}

func TestDeleteKeepsUnsavedWork(t *testing.T) {
	cases := map[string]func(t *testing.T, wt string){
		"unpushed commit": func(t *testing.T, wt string) {
			_ = os.WriteFile(filepath.Join(wt, "c.txt"), []byte("c\n"), 0o600)
			git(t, wt, "add", ".")
			git(t, wt, "commit", "-q", "-m", "local only")
		},
		"uncommitted change": func(t *testing.T, wt string) {
			_ = os.WriteFile(filepath.Join(wt, "b.txt"), []byte("changed\n"), 0o600)
		},
		"untracked file": func(t *testing.T, wt string) {
			_ = os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new\n"), 0o600)
		},
	}
	for name, dirty := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDeleteRig(t, map[string]string{"Item-1": store.StatusDone})
			wt := filepath.Join(r.cfg.WorkspaceDir, "Item-1", "app")
			dirty(t, wt)
			code, msg := r.delete(t, "Item-1")
			if code != http.StatusBadRequest || !strings.Contains(msg, "not deleted") {
				t.Fatalf("got %d %q", code, msg)
			}
			if !r.onBoard(t, "Item-1") || !exists(filepath.Join(r.st.TaskDir("Item-1"), "task.json")) || !exists(filepath.Join(wt, "b.txt")) {
				t.Fatal("task, task folder or worktree was removed")
			}
		})
	}
}

func TestDeleteUnknownTask(t *testing.T) {
	r := newDeleteRig(t, nil)
	if code, msg := r.delete(t, "Item-404"); code != http.StatusNotFound || !strings.Contains(msg, "task not found") {
		t.Fatalf("got %d %q", code, msg)
	}
}
