package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"oscar/internal/claude"
	"oscar/internal/config"
	"oscar/internal/policy"
	"oscar/internal/redact"
	"oscar/internal/store"
)

const serverName = "oscar"

func (e *Engine) renderPrompt(name string, data any) string {
	b, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		return ""
	}
	tpl, err := template.New(name).Parse(string(b))
	if err != nil {
		log.Printf("prompt %s: %v", name, err)
		return string(b)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, data); err != nil {
		log.Printf("prompt %s: %v", name, err)
	}
	return sb.String()
}

type sessionSpec struct {
	role    policy.Role
	taskID  string // "" for the Captain
	pctx    policy.Context
	tools   []claude.Tool
	prompt  string
	resume  string
	rc      config.Role
	addDirs []string
	// repoDirs are the main checkouts the session works on. Their .claude settings count too.
	repoDirs []string
}

// claudeSettings returns the user's Claude Code permission rules for a session, read from
// the same places the CLI reads (following setting_sources): the user's ~/.claude, the
// session's start dir, and the repos the session works on.
func (e *Engine) claudeSettings(repoDirs []string) *policy.Settings {
	home, _ := os.UserHomeDir()
	has := func(src string) bool {
		for _, s := range e.cfg.SettingSources {
			if s == src {
				return true
			}
		}
		return false
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if has("user") && home != "" {
		add(filepath.Join(home, ".claude", "settings.json"))
		add(filepath.Join(home, ".claude", "settings.local.json"))
	}
	for _, d := range append([]string{e.cfg.ClaudeCwd}, repoDirs...) {
		if d == "" {
			continue
		}
		if has("project") {
			add(filepath.Join(d, ".claude", "settings.json"))
		}
		if has("local") {
			add(filepath.Join(d, ".claude", "settings.local.json"))
		}
	}
	return policy.NewSettings(paths)
}

func (e *Engine) startSession(sp sessionSpec) (*claude.Session, error) {
	sp.pctx.ServerName = serverName
	sp.pctx.Allowed = e.allowed
	sp.pctx.Forwarded = portForwarded
	if sp.pctx.Settings == nil {
		sp.pctx.Settings = e.claudeSettings(sp.repoDirs)
	}
	opts := claude.Options{
		Bin:                e.cfg.ClaudeBin,
		Cwd:                e.cfg.ClaudeCwd,
		Model:              sp.rc.Model,
		Effort:             sp.rc.Effort,
		PermissionMode:     e.cfg.PermissionMode,
		SettingSources:     e.cfg.SettingSources,
		AppendSystemPrompt: sp.prompt,
		Resume:             sp.resume,
		AddDirs:            sp.addDirs,
		Env:                e.cfg.SessionEnv,
		ServerName:         serverName,
		Tools:              sp.tools,
		PreToolUse: func(ctx context.Context, in claude.HookInput) claude.HookDecision {
			d := policy.Check(sp.pctx, in.ToolName, in.ToolInput)
			switch d.Action {
			case policy.Allow:
				return claude.HookDecision{Decision: "allow", Reason: d.Reason}
			case policy.Gate:
				return claude.HookDecision{Decision: "ask", Reason: "Oscar gate: " + d.Reason}
			case policy.Deny:
				if sp.taskID != "" {
					e.logLine(sp.taskID, string(sp.role), "info", "denied "+in.ToolName+": "+d.Reason)
				}
				return claude.HookDecision{Decision: "deny", Reason: d.Reason}
			}
			return claude.HookDecision{}
		},
		CanUseTool: func(ctx context.Context, req claude.PermissionRequest) claude.PermissionResult {
			return e.approveTool(ctx, sp, req)
		},
		OnMessage: func(m claude.Message) { e.onMessage(sp, m) },
		OnStderr: func(line string) {
			if strings.TrimSpace(line) != "" {
				log.Printf("[%s %s] %s", sp.role, sp.taskID, redact.String(line))
			}
		},
	}
	return claude.Start(e.ctx, opts)
}

// approveTool answers can_use_tool: the CLI wants to run something that needs a yes.
func (e *Engine) approveTool(ctx context.Context, sp sessionSpec, req claude.PermissionRequest) claude.PermissionResult {
	if strings.HasPrefix(req.ToolName, "mcp__"+serverName+"__") && req.MCPServer != nil && req.MCPServer.Source == "sdk" {
		return claude.PermissionResult{Allow: true}
	}
	detail := toolDetail(req.ToolName, req.Input)
	// Compute the gate before taking the lock: it reads the always-allow rules, which lock too.
	pd := policy.Check(sp.pctx, req.ToolName, req.Input)
	e.mu.Lock()
	if sp.taskID != "" && req.ToolName == "Bash" {
		for _, c := range e.planCmds[sp.taskID] {
			if strings.TrimSpace(c) == strings.TrimSpace(detail) {
				e.mu.Unlock()
				e.logLine(sp.taskID, string(sp.role), "info", "running approved plan command: "+detail)
				return claude.PermissionResult{Allow: true}
			}
		}
	}
	reason := req.DecisionReason
	if reason == "" {
		reason = "needs approval"
	}
	p := store.Pending{ID: newID("tool"), Kind: store.PendingTool, Tool: req.ToolName, Input: req.Input,
		Question: fmt.Sprintf("Allow %s? (%s)", req.ToolName, strings.TrimPrefix(reason, "Oscar gate: ")), Detail: redact.String(detail),
		RuleKind: pd.Kind, RuleSubject: pd.Subject}
	ch := make(chan decision, 1)
	e.waiters[p.ID] = ch
	if sp.taskID == "" {
		p.Created = timeNow()
		e.capPend = append(e.capPend, p)
		e.publish(Event{Kind: "state"})
		go e.notify("Captain needs you", p.Question)
	} else if t, err := e.task(sp.taskID); err == nil {
		e.needsYou(t, p)
	}
	e.mu.Unlock()
	if sp.taskID != "" {
		e.logLine(sp.taskID, string(sp.role), "info", "waiting for approval: "+redact.String(detail))
	}

	var d decision
	select {
	case d = <-ch:
	case <-ctx.Done():
		d = decision{allow: false, note: "session ended"}
	}
	e.mu.Lock()
	delete(e.waiters, p.ID)
	if sp.taskID == "" {
		for i, q := range e.capPend {
			if q.ID == p.ID {
				e.capPend = append(e.capPend[:i], e.capPend[i+1:]...)
				break
			}
		}
		e.publish(Event{Kind: "state"})
	} else if t, err := e.task(sp.taskID); err == nil {
		e.removePending(t, p.ID)
		e.settle(t)
		e.save(t)
	}
	e.mu.Unlock()
	if d.allow {
		return claude.PermissionResult{Allow: true}
	}
	msg := "The user did not allow this."
	if d.note != "" {
		msg += " Their note: " + d.note
	}
	return claude.PermissionResult{Allow: false, Message: msg}
}

func toolDetail(tool string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	switch tool {
	case "Bash":
		s, _ := m["command"].(string)
		return s
	case "Edit", "Write", "MultiEdit", "Read":
		s, _ := m["file_path"].(string)
		return s
	case "NotebookEdit":
		s, _ := m["notebook_path"].(string)
		return s
	}
	return truncate(string(input), 2000)
}

// onMessage turns CLI output into log lines, chat lines and turn-end handling.
func (e *Engine) onMessage(sp sessionSpec, m claude.Message) {
	var msg struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		Cost    float64 `json:"total_cost_usd"`
		NumTurn int     `json:"num_turns"`
	}
	_ = json.Unmarshal(m.Raw, &msg)
	who := string(sp.role)
	switch m.Type {
	case "assistant":
		for _, c := range msg.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) == "" {
					continue
				}
				if sp.taskID == "" {
					if strings.TrimSpace(c.Text) != "NOOP" {
						e.chat("captain", c.Text)
					}
				} else {
					e.logLine(sp.taskID, who, "text", c.Text)
					if sp.role == policy.Worker {
						e.mu.Lock()
						e.lastText[sp.taskID] = c.Text
						e.mu.Unlock()
					}
				}
			case "tool_use":
				if sp.taskID != "" {
					e.logLine(sp.taskID, who, "tool", c.Name+": "+truncate(toolDetail(c.Name, c.Input), 600))
				}
			}
		}
	case "system":
		if m.Subtype == "init" {
			e.rememberSession(sp, m.SessionID)
		}
	case "result":
		if msg.NumTurn == 0 && !msg.IsError {
			// A resumed session first reports the end of the turn that was cut off before the
			// restart. Nothing ran in it; the real turn for our message follows.
			return
		}
		if sp.taskID != "" {
			e.logLine(sp.taskID, who, "result", fmt.Sprintf("turn ended (%d turns, $%.2f)%s", msg.NumTurn, msg.Cost, map[bool]string{true: " with error: " + truncate(msg.Result, 300), false: ""}[msg.IsError]))
		}
		go e.turnEnded(sp, msg.IsError, msg.Result)
	}
}

func (e *Engine) rememberSession(sp sessionSpec, id string) {
	if id == "" {
		return
	}
	if sp.taskID == "" {
		_ = e.st.SetValue("captain.json", map[string]string{"session_id": id})
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.task(sp.taskID)
	if err != nil {
		return
	}
	switch sp.role {
	case policy.Worker:
		if t.WorkerSession != id {
			t.WorkerSession = id
			e.save(t)
		}
	case policy.Reviewer:
		t.ReviewerSession = id
		e.save(t)
	}
}

func (e *Engine) turnEnded(sp sessionSpec, isError bool, result string) {
	e.mu.Lock()
	stopping := e.stopping
	e.mu.Unlock()
	if stopping {
		return // the task stays running/review on disk and is resumed after the restart
	}
	switch sp.role {
	case policy.Captain:
		e.mu.Lock()
		e.capBusy = false
		e.publish(Event{Kind: "state"})
		e.mu.Unlock()
		if isError {
			e.chat("oscar", "Captain turn failed: "+truncate(result, 300))
		}
	case policy.Worker:
		e.workerTurnEnded(sp.taskID, isError, result)
	case policy.Reviewer:
		e.reviewerTurnEnded(sp.taskID)
	}
}
