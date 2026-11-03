// Package config loads ~/.oscar/config.yaml. Everything specific to one person or one
// company (paths, repos, integrations, team rules) lives there, not in this repo.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Role struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

type Config struct {
	// Path is the file this config was loaded from (not read from YAML).
	Path string `yaml:"-"`

	Listen    string `yaml:"listen"`
	DataDir   string `yaml:"data_dir"`
	ClaudeBin string `yaml:"claude_bin"`
	// Sessions start in this directory, so they pick up its CLAUDE.md, memory and skills.
	ClaudeCwd string `yaml:"claude_cwd"`
	// Git repos live directly under this directory.
	ReposDir string `yaml:"repos_dir"`
	// Worktrees for tasks are created here.
	WorkspaceDir string `yaml:"workspace_dir"`
	// Permission mode for sessions: "auto" lets the CLI's own classifier approve safe actions.
	PermissionMode  string   `yaml:"permission_mode"`
	SettingSources  []string `yaml:"setting_sources"`
	MaxWorkers      int      `yaml:"max_workers"`
	MaxReviewRounds int      `yaml:"max_review_rounds"`

	Captain  Role `yaml:"captain"`
	Worker   Role `yaml:"worker"`
	Reviewer Role `yaml:"reviewer"`

	// Free text added to every prompt: team rules, reply language, conventions.
	RulesFile string `yaml:"rules_file"`
	// Extra task type definitions (and approved new types).
	TypesDir string `yaml:"types_dir"`

	// Integrations are local executables. Each gets a JSON object on stdin and may print
	// JSON on stdout (e.g. {"url": "..."}). Arguments may use ~.
	Integrations struct {
		DraftPR   []string `yaml:"draft_pr"`
		PRComment []string `yaml:"pr_comment"`
		SlackPost []string `yaml:"slack_post"`
		Notify    []string `yaml:"notify"`
		MergePR   []string `yaml:"merge_pr"` // only used for Oscar's own repo, on the user's click
		ClosePR   []string `yaml:"close_pr"` // decline/close a PR (after a branch rename), on the user's click
	} `yaml:"integrations"`

	// Repos outside repos_dir, by name (for example Oscar's own source).
	ExtraRepos map[string]string `yaml:"extra_repos"`
	// Per-repo integrations that replace the global ones (e.g. GitHub for one repo, Bitbucket for the rest).
	RepoIntegrations map[string]RepoIntegrations `yaml:"repo_integrations"`
	// Repos that are public. Their diffs and PR text are checked for ForbiddenWords before a PR is opened.
	PublicRepos    []string `yaml:"public_repos"`
	ForbiddenWords []string `yaml:"forbidden_words"`

	// Self is Oscar's own repo name (in extra_repos or repos_dir). It enables "Merge & update".
	Self struct {
		Repo   string `yaml:"repo"`
		Branch string `yaml:"branch"` // default main
	} `yaml:"self"`

	// Extra env for every Claude session (e.g. to silence the user's own notification hooks).
	SessionEnv []string `yaml:"session_env"`
}

type RepoIntegrations struct {
	DraftPR   []string `yaml:"draft_pr"`
	PRComment []string `yaml:"pr_comment"`
	MergePR   []string `yaml:"merge_pr"`
	ClosePR   []string `yaml:"close_pr"`
}

// Default returns the defaults used when the file does not set a field.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen:          "127.0.0.1:7777",
		DataDir:         filepath.Join(home, ".oscar"),
		ClaudeBin:       "claude",
		ClaudeCwd:       home,
		ReposDir:        filepath.Join(home, "dev"),
		PermissionMode:  "auto",
		SettingSources:  []string{"user", "project", "local"},
		MaxWorkers:      4,
		MaxReviewRounds: 3,
		Captain:         Role{Model: "opus"},
		Worker:          Role{Model: "opus"},
		Reviewer:        Role{Model: "opus"},
		SessionEnv:      []string{"OSCAR_SESSION=1"},
	}
}

// Load reads path (missing file = defaults) and expands ~ in paths.
func Load(path string) (Config, error) {
	c := Default()
	c.Path = path
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, err
		}
	}
	for _, p := range []*string{&c.DataDir, &c.ClaudeCwd, &c.ReposDir, &c.WorkspaceDir, &c.RulesFile, &c.TypesDir, &c.ClaudeBin} {
		*p = Expand(*p)
	}
	if c.WorkspaceDir == "" {
		c.WorkspaceDir = filepath.Join(c.DataDir, "work")
	}
	if c.TypesDir == "" {
		c.TypesDir = filepath.Join(c.DataDir, "types")
	}
	expandAll := func(l []string) {
		for i := range l {
			l[i] = Expand(l[i])
		}
	}
	for _, l := range [][]string{c.Integrations.DraftPR, c.Integrations.PRComment, c.Integrations.SlackPost, c.Integrations.Notify, c.Integrations.MergePR, c.Integrations.ClosePR} {
		expandAll(l)
	}
	for _, ri := range c.RepoIntegrations {
		expandAll(ri.DraftPR)
		expandAll(ri.PRComment)
		expandAll(ri.MergePR)
		expandAll(ri.ClosePR)
	}
	for k, v := range c.ExtraRepos {
		c.ExtraRepos[k] = Expand(v)
	}
	if c.Self.Branch == "" {
		c.Self.Branch = "main"
	}
	return c, nil
}

// Expand replaces a leading ~ with the home dir.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Rules returns the content of the rules file, or "".
func (c Config) Rules() string {
	if c.RulesFile == "" {
		return ""
	}
	b, err := os.ReadFile(c.RulesFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RepoPath returns where a repo lives on disk.
func (c Config) RepoPath(name string) string {
	if p, ok := c.ExtraRepos[name]; ok {
		return p
	}
	return filepath.Join(c.ReposDir, name)
}

// Integration returns the command for kind ("draft_pr", "pr_comment", "merge_pr") for a repo.
func (c Config) Integration(repo, kind string) []string {
	if ri, ok := c.RepoIntegrations[repo]; ok {
		switch kind {
		case "draft_pr":
			if len(ri.DraftPR) > 0 {
				return ri.DraftPR
			}
		case "pr_comment":
			if len(ri.PRComment) > 0 {
				return ri.PRComment
			}
		case "merge_pr":
			if len(ri.MergePR) > 0 {
				return ri.MergePR
			}
		case "close_pr":
			if len(ri.ClosePR) > 0 {
				return ri.ClosePR
			}
		}
	}
	switch kind {
	case "draft_pr":
		return c.Integrations.DraftPR
	case "pr_comment":
		return c.Integrations.PRComment
	case "merge_pr":
		return c.Integrations.MergePR
	case "close_pr":
		return c.Integrations.ClosePR
	}
	return nil
}

// IsPublic reports whether repo is listed as public.
func (c Config) IsPublic(repo string) bool {
	for _, r := range c.PublicRepos {
		if r == repo {
			return true
		}
	}
	return false
}
