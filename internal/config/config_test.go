package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRepoIntegrations(t *testing.T) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(p, []byte(`
repos_dir: /repos
extra_repos: {oscar: ~/src/oscar}
integrations: {draft_pr: ["~/bin/bb.sh"]}
repo_integrations: {oscar: {draft_pr: ["~/bin/gh.sh"], merge_pr: ["~/bin/gh-merge.sh"]}}
public_repos: [oscar]
self: {repo: oscar}
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RepoPath("oscar"); got != filepath.Join(home, "src/oscar") {
		t.Errorf("RepoPath oscar = %s", got)
	}
	if got := c.RepoPath("api"); got != "/repos/api" {
		t.Errorf("RepoPath api = %s", got)
	}
	if got := c.Integration("oscar", "draft_pr"); len(got) != 1 || got[0] != filepath.Join(home, "bin/gh.sh") {
		t.Errorf("oscar draft_pr = %v", got)
	}
	if got := c.Integration("api", "draft_pr"); len(got) != 1 || got[0] != filepath.Join(home, "bin/bb.sh") {
		t.Errorf("api draft_pr = %v", got)
	}
	if !c.IsPublic("oscar") || c.IsPublic("api") || c.Self.Branch != "main" {
		t.Errorf("public/self wrong: %+v", c.Self)
	}
}
