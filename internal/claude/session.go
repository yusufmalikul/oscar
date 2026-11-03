// Package claude runs the `claude` CLI as a child process and talks to it over
// the stream-json protocol (the same protocol the official Agent SDKs use).
//
// Wire format, one JSON object per line on stdin/stdout:
//
//	host → cli  {"type":"control_request","request_id":..,"request":{"subtype":"initialize",..}}
//	host → cli  {"type":"user","message":{"role":"user","content":".."},"parent_tool_use_id":null}
//	cli  → host {"type":"control_request","request_id":..,"request":{"subtype":"hook_callback"|"can_use_tool"|"mcp_message",..}}
//	host → cli  {"type":"control_response","response":{"subtype":"success","request_id":..,"response":{..}}}
//	cli  → host assistant / user / system / result messages
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tool is an in-process MCP tool that Claude can call. Its handler runs inside Oscar.
type Tool struct {
	Name        string
	Description string
	// Schema is a JSON Schema object for the tool input.
	Schema  map[string]any
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

// HookInput is what the CLI sends for a PreToolUse hook.
type HookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
	Cwd       string          `json:"cwd"`
}

// HookDecision is the PreToolUse answer. Empty Decision means "no opinion".
type HookDecision struct {
	Decision string // "", "allow", "ask", "deny"
	Reason   string
}

// PermissionRequest is a can_use_tool request: the CLI asks the host whether a tool call may run.
type PermissionRequest struct {
	ToolName       string          `json:"tool_name"`
	Input          json.RawMessage `json:"input"`
	ToolUseID      string          `json:"tool_use_id"`
	DecisionReason string          `json:"decision_reason"`
	BlockedPath    string          `json:"blocked_path"`
	MCPServer      *struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"mcp_server"`
}

// PermissionResult answers a PermissionRequest.
type PermissionResult struct {
	Allow        bool
	UpdatedInput json.RawMessage // optional; defaults to the original input
	Message      string          // reason shown to Claude on deny
}

// Options configures one Claude session.
type Options struct {
	Bin                string // path to the claude binary
	Cwd                string
	Model              string
	Effort             string
	PermissionMode     string
	SettingSources     []string
	AppendSystemPrompt string
	Resume             string // session id to resume
	AddDirs            []string
	DisallowedTools    []string
	Env                []string // extra KEY=VALUE pairs

	ServerName string // in-process MCP server name, e.g. "oscar"
	Tools      []Tool

	PreToolUse func(ctx context.Context, in HookInput) HookDecision
	CanUseTool func(ctx context.Context, req PermissionRequest) PermissionResult
	OnMessage  func(m Message)
	OnStderr   func(line string)
}

// Message is one non-control message from the CLI.
type Message struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Raw       json.RawMessage `json:"-"`
}

// Session is a running claude process.
type Session struct {
	opts   Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	reqSeq  atomic.Int64
	pmu     sync.Mutex
	pending map[string]chan json.RawMessage

	sessionID atomic.Value // string
	done      chan struct{}
	waitErr   error
}

// Start spawns claude and runs the initialize handshake.
func Start(parent context.Context, o Options) (*Session, error) {
	if o.Bin == "" {
		o.Bin = "claude"
	}
	args := []string{
		"--output-format", "stream-json", "--verbose",
		"--input-format", "stream-json",
		"--permission-prompt-tool=stdio",
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	args = append(args, "--setting-sources="+strings.Join(o.SettingSources, ","))
	if o.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}
	for _, d := range o.AddDirs {
		args = append(args, "--add-dir", d)
	}
	if len(o.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(o.DisallowedTools, ","))
	}

	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, o.Bin, args...)
	cmd.Dir = o.Cwd
	cmd.Env = append(cleanEnv(os.Environ()), o.Env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	s := &Session{opts: o, cmd: cmd, stdin: stdin, ctx: ctx, cancel: cancel,
		pending: map[string]chan json.RawMessage{}, done: make(chan struct{})}
	s.sessionID.Store(o.Resume)

	go s.readStderr(stderr)
	go s.readLoop(stdout)
	go func() {
		s.waitErr = cmd.Wait()
		cancel()
		close(s.done)
	}()

	if _, err := s.control(ctx, s.initializeRequest(), 60*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return s, nil
}

// cleanEnv drops variables that make a nested claude think it runs inside another session.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_SSE") || k == "CLAUDE_CODE_ENTRYPOINT" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *Session) initializeRequest() map[string]any {
	req := map[string]any{"subtype": "initialize"}
	if s.opts.PreToolUse != nil {
		req["hooks"] = map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "*", "hookCallbackIds": []string{"pre_tool_use"}}},
		}
	}
	if s.opts.ServerName != "" && len(s.opts.Tools) > 0 {
		req["sdkMcpServers"] = []string{s.opts.ServerName}
		req["sdkMcpServerManifests"] = map[string]any{
			s.opts.ServerName: map[string]any{
				"initializeResult": s.mcpInitResult(),
				"toolsListResult":  s.mcpToolsList(),
			},
		}
	}
	return req
}

func (s *Session) mcpInitResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.opts.ServerName, "version": "1"},
	}
}

func (s *Session) mcpToolsList() map[string]any {
	tools := make([]any, 0, len(s.opts.Tools))
	for _, t := range s.opts.Tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	return map[string]any{"tools": tools}
}

// SessionID is the Claude session id, known after the first system/init message.
func (s *Session) SessionID() string { v, _ := s.sessionID.Load().(string); return v }

// Done is closed when the process exits.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is the process exit error, valid after Done.
func (s *Session) Err() error { return s.waitErr }

// Send queues a user message.
func (s *Session) Send(text string) error {
	return s.write(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil,
	})
}

// Interrupt stops the current turn.
func (s *Session) Interrupt() error {
	_, err := s.control(s.ctx, map[string]any{"subtype": "interrupt"}, 30*time.Second)
	return err
}

// Close ends the session: closes stdin, then kills the process if it does not exit.
func (s *Session) Close() {
	s.wmu.Lock()
	_ = s.stdin.Close()
	s.wmu.Unlock()
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		s.cancel()
		<-s.done
	}
}

func (s *Session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return errors.New("claude session has exited")
	default:
	}
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *Session) control(ctx context.Context, req map[string]any, timeout time.Duration) (json.RawMessage, error) {
	id := fmt.Sprintf("oscar-%d", s.reqSeq.Add(1))
	ch := make(chan json.RawMessage, 1)
	s.pmu.Lock()
	s.pending[id] = ch
	s.pmu.Unlock()
	defer func() { s.pmu.Lock(); delete(s.pending, id); s.pmu.Unlock() }()

	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		var env struct {
			Subtype  string          `json:"subtype"`
			Error    string          `json:"error"`
			Response json.RawMessage `json:"response"`
		}
		_ = json.Unmarshal(r, &env)
		if env.Subtype == "error" {
			return nil, errors.New(env.Error)
		}
		return env.Response, nil
	case <-s.done:
		return nil, fmt.Errorf("claude exited: %v", s.waitErr)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, errors.New("timed out waiting for claude")
	}
}

func (s *Session) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if s.opts.OnStderr != nil {
			s.opts.OnStderr(sc.Text())
		}
	}
}

func (s *Session) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var head struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  json.RawMessage `json:"response"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.Type {
		case "control_request":
			go s.handleControl(head.RequestID, head.Request)
		case "control_response":
			var r struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(head.Response, &r)
			s.pmu.Lock()
			ch := s.pending[r.RequestID]
			s.pmu.Unlock()
			if ch != nil {
				ch <- head.Response
			}
		case "control_cancel_request":
			// The CLI gave up on a request (e.g. the turn was interrupted). Nothing to do:
			// our answer, if it comes later, is ignored by the CLI.
		default:
			if head.SessionID != "" {
				s.sessionID.Store(head.SessionID)
			}
			if s.opts.OnMessage != nil {
				s.opts.OnMessage(Message{Type: head.Type, Subtype: head.Subtype, SessionID: head.SessionID, Raw: line})
			}
		}
	}
}

func (s *Session) respond(id string, resp any) {
	_ = s.write(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": id, "response": resp},
	})
}

func (s *Session) respondError(id, msg string) {
	_ = s.write(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "error", "request_id": id, "error": msg},
	})
}

func (s *Session) handleControl(id string, raw json.RawMessage) {
	var req struct {
		Subtype    string          `json:"subtype"`
		CallbackID string          `json:"callback_id"`
		Input      json.RawMessage `json:"input"`
		ServerName string          `json:"server_name"`
		Message    json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		s.respondError(id, "bad request")
		return
	}
	switch req.Subtype {
	case "hook_callback":
		var in HookInput
		_ = json.Unmarshal(req.Input, &in)
		d := HookDecision{}
		if s.opts.PreToolUse != nil {
			d = s.opts.PreToolUse(s.ctx, in)
		}
		if d.Decision == "" {
			s.respond(id, map[string]any{})
			return
		}
		s.respond(id, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       d.Decision,
			"permissionDecisionReason": d.Reason,
		}})
	case "can_use_tool":
		var pr PermissionRequest
		_ = json.Unmarshal(raw, &pr)
		res := PermissionResult{Allow: false, Message: "no approval handler"}
		if s.opts.CanUseTool != nil {
			res = s.opts.CanUseTool(s.ctx, pr)
		}
		if res.Allow {
			in := res.UpdatedInput
			if len(in) == 0 {
				in = pr.Input
			}
			s.respond(id, map[string]any{"behavior": "allow", "updatedInput": in, "toolUseID": pr.ToolUseID})
		} else {
			s.respond(id, map[string]any{"behavior": "deny", "message": res.Message, "toolUseID": pr.ToolUseID})
		}
	case "mcp_message":
		s.respond(id, map[string]any{"mcp_response": s.handleMCP(req.ServerName, req.Message)})
	default:
		s.respondError(id, "unsupported control request: "+req.Subtype)
	}
}

func (s *Session) handleMCP(server string, raw json.RawMessage) any {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &m)
	reply := func(result any) any { return map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result} }
	if server != s.opts.ServerName {
		return map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown server"}}
	}
	switch m.Method {
	case "initialize":
		return reply(s.mcpInitResult())
	case "tools/list":
		return reply(s.mcpToolsList())
	case "tools/call":
		for _, t := range s.opts.Tools {
			if t.Name != m.Params.Name {
				continue
			}
			if missing := missingRequired(t.Schema, m.Params.Arguments); len(missing) > 0 {
				msg := "Error: missing required field(s): " + strings.Join(missing, ", ") +
					". Nothing was recorded. Load the tool schema if needed and call again with all required fields."
				return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}, "isError": true})
			}
			text, err := t.Handler(s.ctx, m.Params.Arguments)
			if err != nil {
				return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "Error: " + err.Error()}}, "isError": true})
			}
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
		}
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "Error: unknown tool " + m.Params.Name}}, "isError": true})
	default:
		return reply(map[string]any{})
	}
}

// missingRequired lists required top-level fields that are absent or null in args.
func missingRequired(schema map[string]any, args json.RawMessage) []string {
	var req []string
	switch r := schema["required"].(type) {
	case []string:
		req = r
	case []any:
		for _, x := range r {
			if s, ok := x.(string); ok {
				req = append(req, s)
			}
		}
	}
	if len(req) == 0 {
		return nil
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(args, &got)
	var missing []string
	for _, k := range req {
		if v, ok := got[k]; !ok || string(v) == "null" {
			missing = append(missing, k)
		}
	}
	return missing
}
