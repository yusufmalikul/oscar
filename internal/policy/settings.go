package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Settings holds the permission rules (allow, ask, deny) from the user's Claude Code
// settings files, so Oscar sessions may do what the user already allows in their own
// sessions. Files are re-read when they change, so a rule added in a terminal session
// works in Oscar without a restart.
type Settings struct {
	paths []string

	mu    sync.Mutex
	stamp string
	rules permRules
}

type permRules struct{ allow, ask, deny []permRule }

// permRule is one rule such as "Bash(git push:*)", "mcp__slack" or "WebFetch(domain:x.com)".
type permRule struct {
	raw     string
	tool    string
	spec    string
	hasSpec bool
	re      *regexp.Regexp // Bash rules with * wildcards
}

// NewSettings reads rules from these settings.json files. Missing files are skipped.
func NewSettings(paths []string) *Settings {
	return &Settings{paths: paths}
}

func (s *Settings) current() permRules {
	if s == nil {
		return permRules{}
	}
	var sb strings.Builder
	for _, p := range s.paths {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&sb, "%s|%d|%d;", p, fi.ModTime().UnixNano(), fi.Size())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb.String() == s.stamp {
		return s.rules
	}
	var rs permRules
	for _, p := range s.paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f struct {
			Permissions struct {
				Allow []string `json:"allow"`
				Ask   []string `json:"ask"`
				Deny  []string `json:"deny"`
			} `json:"permissions"`
		}
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		rs.allow = append(rs.allow, parseRules(f.Permissions.Allow)...)
		rs.ask = append(rs.ask, parseRules(f.Permissions.Ask)...)
		rs.deny = append(rs.deny, parseRules(f.Permissions.Deny)...)
	}
	s.stamp, s.rules = sb.String(), rs
	return rs
}

func parseRules(list []string) []permRule {
	var out []permRule
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		r := permRule{raw: raw, tool: raw}
		if i := strings.Index(raw, "("); i > 0 && strings.HasSuffix(raw, ")") {
			r.tool, r.spec, r.hasSpec = raw[:i], raw[i+1:len(raw)-1], true
		}
		if r.tool == "Bash" && r.hasSpec && !strings.HasSuffix(r.spec, ":*") && strings.Contains(r.spec, "*") {
			r.re = bashGlob(r.spec)
		}
		if r.tool != "" {
			out = append(out, r)
		}
	}
	return out
}

// bashGlob turns "git add *" into a regexp. A trailing " *" also matches the bare command.
func bashGlob(spec string) *regexp.Regexp {
	optionalTail := strings.HasSuffix(spec, " *")
	if optionalTail {
		spec = strings.TrimSuffix(spec, " *")
	}
	parts := strings.Split(spec, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	expr := "^" + strings.Join(parts, ".*")
	if optionalTail {
		expr += "( .*)?"
	}
	re, err := regexp.Compile("(?s)" + expr + "$")
	if err != nil {
		return nil
	}
	return re
}

// matchBash reports whether one simple command (no &&, |, ;) fits a Bash rule.
func (r permRule) matchBash(cmd string) bool {
	if r.tool != "Bash" {
		return false
	}
	if !r.hasSpec || r.spec == "*" || r.spec == "" {
		return true
	}
	if p, ok := strings.CutSuffix(r.spec, ":*"); ok {
		return cmd == p || strings.HasPrefix(cmd, p+" ")
	}
	if r.re != nil {
		return r.re.MatchString(cmd)
	}
	return cmd == r.spec
}

// matchTool reports whether a non-Bash tool call fits the rule.
func (r permRule) matchTool(tool string, input json.RawMessage) bool {
	if strings.HasPrefix(r.tool, "mcp__") && !r.hasSpec {
		server := strings.TrimSuffix(r.tool, "__*")
		if r.tool == tool || (strings.Count(server, "__") == 1 && strings.HasPrefix(tool, server+"__")) {
			return true
		}
		return false
	}
	if r.tool != tool {
		return false
	}
	if !r.hasSpec || r.spec == "*" || r.spec == "" {
		return true
	}
	if tool == "WebFetch" {
		domain, ok := strings.CutPrefix(r.spec, "domain:")
		if !ok {
			return false
		}
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &in)
		u, err := url.Parse(in.URL)
		if err != nil || u.Hostname() == "" {
			return false
		}
		host := strings.ToLower(u.Hostname())
		domain = strings.ToLower(domain)
		if rest, ok := strings.CutPrefix(domain, "*."); ok {
			return host == rest || strings.HasSuffix(host, "."+rest)
		}
		return host == domain
	}
	// Path rules (Read(...), Edit(...)) and others are not interpreted here; the CLI still applies them.
	return false
}

func firstBash(rules []permRule, cmd string) *permRule {
	for i := range rules {
		if rules[i].matchBash(cmd) {
			return &rules[i]
		}
	}
	return nil
}

func firstTool(rules []permRule, tool string, input json.RawMessage) *permRule {
	for i := range rules {
		if rules[i].matchTool(tool, input) {
			return &rules[i]
		}
	}
	return nil
}

// settingsTool applies deny and ask rules to a non-Bash tool call. ok is false when no rule fits.
func settingsTool(c Context, tool string, input json.RawMessage) (Decision, bool) {
	rs := c.Settings.current()
	if r := firstTool(rs.deny, tool, input); r != nil {
		return Decision{Action: Deny, Reason: "Your Claude Code settings deny this (" + r.raw + ")."}, true
	}
	if r := firstTool(rs.ask, tool, input); r != nil {
		return gate(c, "settings-ask", r.raw, "your Claude Code settings ask before "+r.raw), true
	}
	return Decision{}, false
}

// settingsAllowsTool reports the allow rule that fits a non-Bash tool call, or "".
func settingsAllowsTool(c Context, tool string, input json.RawMessage) string {
	if r := firstTool(c.Settings.current().allow, tool, input); r != nil {
		return r.raw
	}
	return ""
}

// settingsCommand applies deny and ask rules to each part of a command line.
func settingsCommand(c Context, parts []string) (Decision, bool) {
	rs := c.Settings.current()
	for _, p := range parts {
		if r := firstBash(rs.deny, p); r != nil {
			return Decision{Action: Deny, Reason: "Your Claude Code settings deny this (" + r.raw + ")."}, true
		}
	}
	for _, p := range parts {
		if r := firstBash(rs.ask, p); r != nil {
			return gate(c, "settings-ask", r.raw, "your Claude Code settings ask before "+r.raw), true
		}
	}
	return Decision{}, false
}

// settingsAllowsCommand returns the allow rule used when every part of the command is either
// allowed by the user's settings or safe by Oscar's own rules, and at least one settings rule
// was needed. It returns "" otherwise.
func settingsAllowsCommand(c Context, parts []string) string {
	rs := c.Settings.current()
	if len(rs.allow) == 0 {
		return ""
	}
	plain := c
	plain.Settings = nil
	used := ""
	for _, p := range parts {
		if r := firstBash(rs.allow, p); r != nil {
			used = r.raw
			continue
		}
		if CheckCommand(plain, p).Action == Allow {
			continue
		}
		return ""
	}
	return used
}
