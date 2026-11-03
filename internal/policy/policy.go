// Package policy decides, in code, what a Claude session may do without asking.
//
// Every tool call passes through PreToolUse first. Check returns one of:
//
//	Allow — run it, no question.
//	Gate  — it must wait for the user. A gate cannot be turned off by a task type or by Claude.
//	Deny  — never allowed in this role; the message tells Claude what to do instead.
//	Pass  — no opinion; the CLI's own permission mode decides, and if it would prompt,
//	        the prompt comes to the user through Oscar.
package policy

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

type Action int

const (
	Pass Action = iota
	Allow
	Gate
	Deny
)

func (a Action) String() string {
	return [...]string{"pass", "allow", "gate", "deny"}[a]
}

type Role string

const (
	Captain  Role = "captain"
	Worker   Role = "worker"
	Reviewer Role = "reviewer"
)

// Context describes who is calling.
type Context struct {
	Role       Role
	WriteRoots []string // absolute dirs this session may edit freely
	RepoRoots  []string // main checkouts; mutating git there is gated
	ServerName string   // our in-process MCP server, e.g. "oscar"
}

type Decision struct {
	Action Action
	Reason string
}

var readOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "WebSearch": true, "WebFetch": true,
	"ToolSearch": true, "TodoWrite": true, "TaskCreate": true, "TaskUpdate": true, "TaskList": true, "TaskGet": true,
	"Skill": true, "Agent": true, "Task": true, "BashOutput": true, "KillShell": true, "TaskStop": true,
	"ListMcpResourcesTool": true, "ReadMcpResourceTool": true, "Monitor": true, "LSP": true,
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// Tools that would talk to the user through a UI Oscar does not have, or leave the session.
var blockedTools = map[string]string{
	"AskUserQuestion": "There is no interactive user here. Use the oscar ask tool instead.",
	"EnterPlanMode":   "Plan mode is not used under Oscar. Report your plan with the oscar tools.",
	"ExitPlanMode":    "Plan mode is not used under Oscar.",
	"EnterWorktree":   "Oscar manages worktrees. Work in the paths you were given.",
	"ExitWorktree":    "Oscar manages worktrees.",
}

var mcpReadVerb = regexp.MustCompile(`(?i)(^|_)(read|get|list|search|query|fetch|describe|view|find)(_|$)`)

// Check classifies one tool call.
func Check(c Context, tool string, input json.RawMessage) Decision {
	if c.ServerName != "" && strings.HasPrefix(tool, "mcp__"+c.ServerName+"__") {
		return Decision{Allow, "oscar tool"}
	}
	if msg, ok := blockedTools[tool]; ok {
		return Decision{Deny, msg}
	}
	if readOnlyTools[tool] {
		return Decision{Allow, "read-only tool"}
	}
	if editTools[tool] {
		return checkEdit(c, input)
	}
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		return CheckCommand(c, in.Command)
	}
	if strings.HasPrefix(tool, "mcp__") {
		name := tool[strings.LastIndex(tool, "__")+2:]
		if mcpReadVerb.MatchString(name) {
			return Decision{Allow, "read-only MCP tool"}
		}
		return Decision{Gate, "MCP tool that may change something outside: " + tool}
	}
	return Decision{Pass, ""}
}

func checkEdit(c Context, input json.RawMessage) Decision {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &in)
	p := in.FilePath
	if p == "" {
		p = in.NotebookPath
	}
	if p == "" {
		return Decision{Gate, "edit without a path"}
	}
	if under(p, c.WriteRoots) {
		return Decision{Allow, "inside the task workspace"}
	}
	if c.Role == Reviewer {
		return Decision{Deny, "The reviewer does not change the work. Write notes in your scratch dir and report findings with the verdict tool."}
	}
	if strings.Contains(p, "/.claude/projects/") && strings.Contains(p, "/memory/") && c.Role == Worker {
		return Decision{Deny, "Workers do not write memory. Put what you learned in the `learnings` field of the finish tool; the Captain decides what to keep."}
	}
	return Decision{Gate, "edit outside the task workspace: " + p}
}

func under(p string, roots []string) bool {
	p = filepath.Clean(p)
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// --- Bash ---

type rule struct {
	re     *regexp.Regexp
	action Action
	reason string
}

func r(expr string, a Action, reason string) rule {
	return rule{regexp.MustCompile(expr), a, reason}
}

// hardRules apply to every role. First match wins, so order matters: deny before gate.
var hardRules = []rule{
	r(`\bgit\s+push\b`, Deny, "Workers never push. Oscar pushes the branch itself when it opens the draft PR (oscar open_draft_pr)."),
	r(`(?i)slack-send|chat\.postMessage|hooks\.slack\.com|chat\.update`, Deny, "Do not post to Slack yourself. Put the message in the outbox with the oscar outbox tool; the user approves it and Oscar posts it."),
	r(`\bgh\s+(pr|issue|release|repo)\s+(create|merge|close|edit|comment|review|delete|ready)\b`, Gate, "changes something on GitHub"),
	r(`\bgh\s+api\b.*(-X|--method)\s*(POST|PUT|PATCH|DELETE)`, Gate, "writes through the GitHub API"),
	r(`(?i)\b(kubectl|oc)\s+(apply|create|delete|edit|patch|replace|scale|rollout|exec|cp|drain|cordon|uncordon|set|label|annotate|taint)\b`, Gate, "changes a Kubernetes cluster"),
	r(`\bhelm\s+(install|upgrade|uninstall|rollback|delete)\b|\bterraform\s+(apply|destroy|import)\b`, Gate, "changes infrastructure"),
	r(`\bdocker\s+push\b|\bnpm\s+publish\b`, Gate, "publishes an artifact"),
	r(`\b(ssh|tsh\s+ssh|scp|sftp)\s`, Gate, "runs on a remote machine"),
	r(`\brsync\b[^|;&]*\S+:`, Gate, "copies to or from a remote machine"),
	r(`(?i)\b(deploy|rollback)\b`, Gate, "looks like a deploy or rollback"),
	r(`(?i)\b(jenkins)\b[^|;&]*(build|buildWithParameters|stop|doDelete)`, Gate, "starts or stops a CI job"),
	r(`(?i)\baws\s+\S+\s+(?:\S+\s+)*?(put|delete|create|update|modify|terminate|stop|start|reboot|run|invoke|send|publish|rm|cp|mv|sync|restore|attach|detach|associate|authorize|revoke|tag|untag)[a-z-]*\b`, Gate, "changes something in AWS"),
	r(`(?i)\b(redis-cli|valkey-cli)\b[^|;&]*\b(del|unlink|set|setex|mset|hset|hdel|lpush|rpush|lpop|rpop|sadd|srem|zadd|zrem|expire|persist|rename|flushdb|flushall|config\s+set|eval)\b`, Gate, "writes to Redis/Valkey"),
	r(`(?i)\b(psql|mysql|mariadb|sqlite3|mongosh|mongo|clickhouse-client)\b[^|]*\b(insert|update|delete|drop|alter|truncate|create|grant|revoke|vacuum|reindex|cluster|copy\s+\S+\s+from)\b`, Gate, "writes to a database"),
	r(`(?i)\b(curl|wget|http|httpie|xh)\b[^|;&]*(\s-X\s*(POST|PUT|PATCH|DELETE)|\s--request\s+(POST|PUT|PATCH|DELETE)|\s(-d|--data|--data-raw|--data-binary|--data-urlencode|-F|--form|-T|--upload-file|--json)\b|\s(POST|PUT|PATCH|DELETE)\s)`, Gate, "sends data to a server"),
	r(`/\.claude/secrets/|\bsecurity\s+find-(generic|internet)-password\b|(^|\s)(cat|less|more|head|tail|grep)\s+[^|;&]*\.env(\.\w+)?(\s|$)`, Gate, "reads a secret"),
	r(`\brm\s+(-\w*[rf]\w*\s+)+\s*(/|~|\$HOME)(\s|$)`, Deny, "Refusing to delete a whole home or root directory."),
}

var reviewerRules = []rule{
	r(`\bgit\s+(commit|push|reset|checkout|switch|rebase|merge|stash|cherry-pick|revert|clean|am|apply)\b`, Deny, "The reviewer does not change the branch. Report what is wrong with the verdict tool."),
}

// safe commands: auto-allow when every part of the command line is one of these.
var safePrefix = regexp.MustCompile(`^(ls|pwd|cat|head|tail|wc|grep|rg|ag|find|fd|tree|file|stat|du|df|which|type|echo|printf|true|false|date|sort|uniq|cut|tr|diff|cmp|jq|yq|basename|dirname|realpath|readlink|env|printenv|test|\[|sed\s+-n|awk|column|nl|tee\s+/dev/null|xargs\s+(grep|cat|wc|ls)|cd|sleep|` +
	`git\s+(status|log|diff|show|branch|rev-parse|ls-files|ls-tree|blame|fetch|remote\s+-v|config\s+--get|add|commit|restore|switch\s+-c|checkout\s+-b|stash|worktree\s+list|merge-base|describe|shortlog|grep|cat-file|for-each-ref|symbolic-ref)|` +
	`go\s+(test|build|vet|run|mod|list|env|version|fmt|doc|generate)|gofmt|golangci-lint|staticcheck|` +
	`mix\s+(test|compile|format\s+--check-formatted|deps\.get|credo|dialyzer|hex\.outdated)|elixir\s+--version|` +
	`npm\s+(test|run|ci|ls|outdated)|npx\s+(tsc|eslint|prettier|vitest|jest)|yarn\s+(test|lint|build)|pnpm\s+(test|lint|build)|` +
	`make\s+(test|lint|build|check)|cargo\s+(test|build|check|clippy|fmt)|python3?\s+-m\s+(pytest|unittest)|pytest|` +
	`docker\s+(ps|images|logs|inspect|compose\s+(ps|logs))|kubectl\s+(get|describe|logs|top|explain|config\s+(get-contexts|current-context))|` +
	`curl\s|wget\s|dig\s|nslookup\s|host\s|ping\s+-c)`)

var splitter = regexp.MustCompile(`\s*(\|\||&&|;|\||\n)\s*`)

// CheckCommand classifies a Bash command line.
func CheckCommand(c Context, cmd string) Decision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Decision{Pass, ""}
	}
	if c.Role == Reviewer {
		for _, rl := range reviewerRules {
			if rl.re.MatchString(cmd) {
				return Decision{rl.action, rl.reason}
			}
		}
	}
	for _, rl := range hardRules {
		if rl.re.MatchString(cmd) {
			return Decision{rl.action, rl.reason}
		}
	}
	if d, ok := checkMainCheckout(c, cmd); ok {
		return d
	}
	for _, part := range splitter.Split(cmd, -1) {
		part = strings.TrimSpace(strings.TrimLeft(part, "( "))
		if part == "" {
			continue
		}
		// env assignments like FOO=1 go test
		for strings.Contains(strings.SplitN(part, " ", 2)[0], "=") && strings.Contains(part, " ") {
			part = strings.TrimSpace(strings.SplitN(part, " ", 2)[1])
		}
		if !safePrefix.MatchString(part) {
			return Decision{Pass, ""}
		}
	}
	return Decision{Allow, "safe command"}
}

var mutatingGit = regexp.MustCompile(`\bgit\s+(-C\s+(\S+)\s+)?(commit|checkout|switch|reset|merge|rebase|stash|clean|branch\s+-[dDmM]|worktree\s+remove|pull|cherry-pick|revert|am|apply|restore)\b`)

// checkMainCheckout gates mutating git commands aimed at a main checkout instead of a worktree.
func checkMainCheckout(c Context, cmd string) (Decision, bool) {
	if len(c.RepoRoots) == 0 {
		return Decision{}, false
	}
	m := mutatingGit.FindStringSubmatch(cmd)
	if m == nil {
		return Decision{}, false
	}
	for _, root := range c.RepoRoots {
		if m[2] != "" && filepath.Clean(m[2]) == filepath.Clean(root) {
			return Decision{Gate, "changes the main checkout " + root + " instead of the task worktree"}, true
		}
		if strings.Contains(cmd, "cd "+root+" ") || strings.Contains(cmd, "cd "+root+"&") || strings.HasSuffix(cmd, "cd "+root) {
			return Decision{Gate, "changes the main checkout " + root + " instead of the task worktree"}, true
		}
	}
	return Decision{}, false
}
