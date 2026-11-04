// Package policy decides, in code, what a Claude session may do without asking.
//
// Every tool call passes through PreToolUse first. Check returns one of:
//
//	Allow — run it, no question.
//	Gate  — it must wait for the user. A gate cannot be turned off by a task type or by Claude.
//	Deny  — never allowed in this role; the message tells Claude what to do instead.
//	Pass  — no opinion; the CLI's own permission mode decides, and if it would prompt,
//	        the prompt comes to the user through Oscar.
//
// The user's own Claude Code permission rules (Settings) count too: what they allow in their
// terminal sessions is allowed here, what they ask about is gated, what they deny is denied.
// Oscar's own deny rules and the main-checkout gate still come first.
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
	// Allowed reports whether the user chose "Always allow" for this kind of gate and subject.
	Allowed func(kind, subject string) bool
	// Forwarded reports whether a local TCP port is a tunnel to somewhere else (ssh -L,
	// kubectl port-forward). Such ports are treated as remote.
	Forwarded func(port string) bool
	// Settings are the permission rules from the user's Claude Code settings files. May be nil.
	Settings *Settings
}

type Decision struct {
	Action Action
	Reason string
	// Kind and Subject describe a gate so the user can "Always allow" it,
	// e.g. Kind "remote-shell", Subject "jump.example.com". Empty when not a gate.
	Kind    string
	Subject string
}

// gate builds a Gate decision, or Allow when the user always allows this kind and subject.
func gate(c Context, kind, subject, reason string) Decision {
	if c.Allowed != nil && c.Allowed(kind, subject) {
		return Decision{Allow, "always allowed by you: " + reason, kind, subject}
	}
	return Decision{Gate, reason, kind, subject}
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
		return Decision{Action: Allow, Reason: "oscar tool"}
	}
	if msg, ok := blockedTools[tool]; ok {
		return Decision{Action: Deny, Reason: msg}
	}
	if tool != "Bash" {
		if d, ok := settingsTool(c, tool, input); ok {
			return d
		}
	}
	if readOnlyTools[tool] {
		return Decision{Action: Allow, Reason: "read-only tool"}
	}
	if editTools[tool] {
		d := checkEdit(c, input)
		if d.Action == Gate {
			if rule := settingsAllowsTool(c, tool, input); rule != "" {
				return Decision{Action: Allow, Reason: "allowed in your Claude Code settings: " + rule}
			}
		}
		return d
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
			return Decision{Action: Allow, Reason: "read-only MCP tool"}
		}
		if rule := settingsAllowsTool(c, tool, input); rule != "" {
			return Decision{Action: Allow, Reason: "allowed in your Claude Code settings: " + rule}
		}
		return gate(c, "mcp-write", tool, "MCP tool that may change something outside: "+tool)
	}
	if rule := settingsAllowsTool(c, tool, input); rule != "" {
		return Decision{Action: Allow, Reason: "allowed in your Claude Code settings: " + rule}
	}
	return Decision{Action: Pass, Reason: ""}
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
		return Decision{Action: Gate, Reason: "edit without a path"}
	}
	if under(p, c.WriteRoots) {
		return Decision{Action: Allow, Reason: "inside the task workspace"}
	}
	if c.Role == Reviewer {
		return Decision{Action: Deny, Reason: "The reviewer does not change the work. Write notes in your scratch dir and report findings with the verdict tool."}
	}
	if strings.Contains(p, "/.claude/projects/") && strings.Contains(p, "/memory/") && c.Role == Worker {
		return Decision{Action: Deny, Reason: "Workers do not write memory. Put what you learned in the `learnings` field of the finish tool; the Captain decides what to keep."}
	}
	// "Always allow" for edits is stored per directory and covers everything below it.
	dir := filepath.Dir(filepath.Clean(p))
	if c.Allowed != nil {
		for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
			if c.Allowed("edit-outside", d) {
				return Decision{Allow, "always allowed by you: edits in " + d, "edit-outside", d}
			}
		}
	}
	return Decision{Gate, "edit outside the task workspace: " + p, "edit-outside", dir}
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
	kind   string
}

func r(expr string, a Action, reason string) rule {
	return rule{re: regexp.MustCompile(expr), action: a, reason: reason, kind: "deny"}
}

func g(kind, expr, reason string) rule {
	return rule{re: regexp.MustCompile(expr), action: Gate, reason: reason, kind: kind}
}

// hardRules apply to every role. First match wins, so order matters: deny before gate.
var hardRules = []rule{
	r(`(?i)slack-send|chat\.postMessage|hooks\.slack\.com|chat\.update`, Deny, "Do not post to Slack yourself. Put the message in the outbox with the oscar outbox tool; the user approves it and Oscar posts it."),
	r(`\brm\s+(-\w*[rf]\w*\s+)+\s*(/|~|\$HOME)(\s|$)`, Deny, "Refusing to delete a whole home or root directory."),
	g("force-push", `\bgit\s+push\b[^|;&]*(\s(-f|--force|--force-with-lease|--force-if-includes|--mirror|--delete|-d|--prune)\b|\s\+\S|\s:\S)`, "rewrites or deletes a remote branch"),
	g("push-main", pushMainExpr, "pushes to the main branch"),
	g("github-write", `\bgh\s+(pr|issue|release|repo)\s+(create|merge|close|edit|comment|review|delete|ready)\b`, "changes something on GitHub"),
	g("github-write", `\bgh\s+api\b.*(-X|--method)\s*(POST|PUT|PATCH|DELETE)`, "writes through the GitHub API"),
	g("k8s-write", `(?i)\b(kubectl|oc)\s+(apply|create|delete|edit|patch|replace|scale|rollout|exec|cp|drain|cordon|uncordon|set|label|annotate|taint)\b`, "changes a Kubernetes cluster"),
	g("infra-write", `\bhelm\s+(install|upgrade|uninstall|rollback|delete)\b|\bterraform\s+(apply|destroy|import)\b`, "changes infrastructure"),
	g("publish", `\bdocker\s+push\b|\bnpm\s+publish\b`, "publishes an artifact"),
	g("remote-shell", `\b(ssh|tsh\s+ssh|scp|sftp)\s`, "runs on a remote machine"),
	g("remote-copy", `\brsync\b[^|;&]*\S+:`, "copies to or from a remote machine"),
	g("deploy", `(?i)\b(deploy|rollback)\b`, "looks like a deploy or rollback"),
	g("ci", `(?i)\b(jenkins)\b[^|;&]*(build|buildWithParameters|stop|doDelete)`, "starts or stops a CI job"),
	g("aws-write", `(?i)\baws\s+\S+\s+(?:\S+\s+)*?(put|delete|create|update|modify|terminate|stop|start|reboot|run|invoke|send|publish|rm|cp|mv|sync|restore|attach|detach|associate|authorize|revoke|tag|untag)[a-z-]*\b`, "changes something in AWS"),
	g("redis-write", `(?i)\b(redis-cli|valkey-cli)\b[^|;&]*\b(del|unlink|set|setex|mset|hset|hdel|lpush|rpush|lpop|rpop|sadd|srem|zadd|zrem|expire|persist|rename|flushdb|flushall|config\s+set|eval)\b`, "writes to Redis/Valkey"),
	g("db-write", `(?i)\b(psql|mysql|mariadb|sqlite3|mongosh|mongo|clickhouse-client)\b[^|]*\b(insert|update|delete|drop|alter|truncate|create|grant|revoke|vacuum|reindex|cluster|copy\s+\S+\s+from)\b`, "writes to a database"),
	g("http-write", `(?i)\b(curl|wget|http|httpie|xh)\b[^|;&]*(\s-X\s*(POST|PUT|PATCH|DELETE)|\s--request\s+(POST|PUT|PATCH|DELETE)|\s(-d|--data|--data-raw|--data-binary|--data-urlencode|-F|--form|-T|--upload-file|--json)\b|\s(POST|PUT|PATCH|DELETE)\s)`, "sends data to a server"),
	g("secret-read", `/\.claude/secrets/|\bsecurity\s+find-(generic|internet)-password\b|(^|\s)(cat|less|more|head|tail|grep)\s+[^|;&]*\.env(\.\w+)?(\s|$)`, "reads a secret"),
}

// A push whose refspec names main or master, e.g. "git push origin main" or "HEAD:main".
const pushMainExpr = `\bgit\s+push\b[^|;&]*\s(?:\S*:)?(?:refs/heads/)?(main|master)(\s|$)`

var rePushMain = regexp.MustCompile(pushMainExpr)

var reviewerRules = []rule{
	r(`\bgit\s+(commit|push|reset|checkout|switch|rebase|merge|stash|cherry-pick|revert|clean|am|apply)\b`, Deny, "The reviewer does not change the branch. Report what is wrong with the verdict tool."),
}

// safe commands: auto-allow when every part of the command line is one of these.
var safePrefix = regexp.MustCompile(`^(ls|pwd|cat|head|tail|wc|grep|rg|ag|find|fd|tree|file|stat|du|df|which|type|echo|printf|true|false|date|sort|uniq|cut|tr|diff|cmp|jq|yq|basename|dirname|realpath|readlink|env|printenv|test|\[|sed\s+-n|awk|column|nl|tee\s+/dev/null|xargs\s+(grep|cat|wc|ls)|cd|sleep|` +
	`git\s+(status|log|diff|show|branch|rev-parse|ls-files|ls-tree|blame|fetch|remote\s+-v|config\s+--get|add|commit|push|restore|switch\s+-c|checkout\s+-b|stash|worktree\s+list|merge-base|describe|shortlog|grep|cat-file|for-each-ref|symbolic-ref)|` +
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
		return Decision{Action: Pass, Reason: ""}
	}
	if c.Role == Reviewer {
		for _, rl := range reviewerRules {
			if rl.re.MatchString(cmd) {
				return Decision{Action: rl.action, Reason: rl.reason}
			}
		}
	}
	for _, rl := range hardRules {
		if rl.action == Deny && rl.re.MatchString(cmd) {
			return Decision{Action: Deny, Reason: rl.reason}
		}
	}
	parts := commandParts(cmd)
	if c.Settings != nil {
		if d, ok := settingsCommand(c, parts); ok {
			return d
		}
	}
	if d, ok := checkMainCheckout(c, cmd); ok {
		return d
	}
	if c.Settings != nil {
		if rule := settingsAllowsCommand(c, parts); rule != "" {
			return Decision{Action: Allow, Reason: "allowed in your Claude Code settings: " + rule}
		}
	}
	for _, rl := range hardRules {
		if rl.action == Deny || !rl.re.MatchString(cmd) {
			continue
		}
		subject := subjectOf(rl.kind, cmd)
		if rl.kind == "http-write" && subject == "" && !anyForwarded(c, localHTTPPorts(cmd)) {
			continue // every URL in the command is on this machine
		}
		if rl.kind == "http-write" && subject == "" {
			subject = "tunnelled localhost"
		}
		if (rl.kind == "db-write" || rl.kind == "redis-write") && isLocalHost(subject) {
			if !anyForwarded(c, []string{dbPort(rl.kind, cmd)}) {
				continue // a database on this machine
			}
			subject = "tunnelled " + subject + ":" + dbPort(rl.kind, cmd)
		}
		return gate(c, rl.kind, subject, rl.reason)
	}
	for _, part := range parts {
		if !safePrefix.MatchString(part) {
			return Decision{Action: Pass, Reason: ""}
		}
	}
	return Decision{Action: Allow, Reason: "safe command"}
}

// commandParts splits a command line at &&, ||, ;, | and newlines, and drops a leading
// "(" and env assignments like FOO=1, so each part starts with the program name.
func commandParts(cmd string) []string {
	var out []string
	for _, part := range splitter.Split(cmd, -1) {
		part = strings.TrimSpace(strings.TrimLeft(part, "( "))
		for strings.Contains(strings.SplitN(part, " ", 2)[0], "=") && strings.Contains(part, " ") {
			part = strings.TrimSpace(strings.SplitN(part, " ", 2)[1])
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

var mutatingGit = regexp.MustCompile(`\bgit\s+(-C\s+(\S+)\s+)?(commit|checkout|switch|reset|merge|rebase|stash|clean|branch\s+-[dDmM]|worktree\s+remove|pull|push|cherry-pick|revert|am|apply|restore)\b`)

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
			return Decision{Gate, "changes the main checkout " + root + " instead of the task worktree", "main-checkout", root}, true
		}
		if strings.Contains(cmd, "cd "+root+" ") || strings.Contains(cmd, "cd "+root+"&") || strings.HasSuffix(cmd, "cd "+root) {
			return Decision{Gate, "changes the main checkout " + root + " instead of the task worktree", "main-checkout", root}, true
		}
	}
	return Decision{}, false
}

// ---------- subjects for "Always allow" ----------

var (
	reSchemeHost = regexp.MustCompile(`(?i)\bhttps?://(\[[^\]]+\]|[^/:\s'"]+)`)
	reBareHost   = regexp.MustCompile(`(?i)^(\[::1\]|[a-z0-9-]+(?:\.[a-z0-9-]+)*)(:\d+)?(/\S*)?$`)
	reUserHost   = regexp.MustCompile(`\b[\w.-]+@([A-Za-z0-9.-]+)`)
	reDashH      = regexp.MustCompile(`\s-h\s*([^\s'"]+)`)
	reDSNHost    = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|rediss?)://(?:[^@/\s]*@)?(\[[^\]]+\]|[^:/\s'"?]+)`)
	reEnvName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reAwsService = regexp.MustCompile(`(?i)\baws\s+([a-z0-9-]+)`)
)

func isLocalHost(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "0.0.0.0" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

// subjectOf returns what a gate is about (a host, a service), used to remember "Always allow".
// For http-write it returns "" when every URL is on this machine.
func subjectOf(kind, cmd string) string {
	switch kind {
	case "http-write":
		var hosts []string
		for _, m := range reSchemeHost.FindAllStringSubmatch(cmd, -1) {
			hosts = append(hosts, m[1])
		}
		for _, tok := range strings.Fields(cmd) {
			tok = strings.Trim(tok, `'";`)
			// VAR=host:port/path assignments hold URLs too (scheme URLs are found above).
			if k, v, ok := strings.Cut(tok, "="); ok && reEnvName.MatchString(k) {
				tok = strings.Trim(v, `'";`)
			}
			if tok == "" || strings.HasPrefix(tok, "-") || strings.Contains(tok, "://") || strings.Contains(tok, "=") {
				continue
			}
			if m := reBareHost.FindStringSubmatch(tok); m != nil && (strings.Contains(m[1], ".") || isLocalHost(m[1])) && (m[2] != "" || m[3] != "" || isLocalHost(m[1])) {
				hosts = append(hosts, m[1])
			}
		}
		for _, h := range hosts {
			if !isLocalHost(h) {
				return strings.ToLower(h)
			}
		}
		if len(hosts) == 0 {
			return "unknown"
		}
		return ""
	case "remote-shell", "remote-copy":
		if m := reUserHost.FindStringSubmatch(cmd); m != nil {
			return m[1]
		}
		f := strings.Fields(cmd)
		for i := 1; i < len(f); i++ {
			if strings.HasPrefix(f[i], "-") || f[i] == "ssh" {
				continue
			}
			return f[i]
		}
	case "db-write", "redis-write":
		if m := reDSNHost.FindStringSubmatch(cmd); m != nil {
			return strings.Trim(m[1], "[]")
		}
		if m := reDashH.FindStringSubmatch(cmd); m != nil {
			return m[1]
		}
		return "localhost"
	case "push-main":
		if m := rePushMain.FindStringSubmatch(cmd); m != nil {
			return m[1]
		}
	case "aws-write":
		if m := reAwsService.FindStringSubmatch(cmd); m != nil {
			return m[1]
		}
	}
	return ""
}

var (
	reLocalPort = regexp.MustCompile(`(?i)(?:^|[\s'"/@])(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]):(\d+)`)
	reDashP     = regexp.MustCompile(`\s-p\s*(\d+)`)
	reDSNPort   = regexp.MustCompile(`(?i)://(?:[^@/\s]*@)?(?:localhost|127\.0\.0\.1|\[::1\]):(\d+)`)
)

func anyForwarded(c Context, ports []string) bool {
	if c.Forwarded == nil {
		return false
	}
	for _, p := range ports {
		if p != "" && c.Forwarded(p) {
			return true
		}
	}
	return false
}

func localHTTPPorts(cmd string) []string {
	var out []string
	for _, m := range reLocalPort.FindAllStringSubmatch(cmd, -1) {
		out = append(out, m[1])
	}
	return out
}

func dbPort(kind, cmd string) string {
	if m := reDSNPort.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	if m := reDashP.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	if kind == "redis-write" {
		return "6379"
	}
	return "5432"
}
