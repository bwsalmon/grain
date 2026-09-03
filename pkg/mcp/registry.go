package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Result is a tool's outcome -- the same two fields v1's ToolResult carries,
// text plus an error flag, both reported back to the model as a single text
// content block (isError never changes the JSON-RPC envelope itself: a
// failed tool call is still a successful RPC that answered "it failed").
type Result struct {
	Text    string
	IsError bool
}

// Tool is one entry in tools/list plus the function that answers tools/call
// for it. InputSchema is a plain JSON Schema object, exactly the shape v1's
// TOOLS list uses, so it can be handed to any client -- Gemini's function
// declarations are a straightforward translation of it, and so would
// Claude's or anything else's be.
//
// Handler receives the ctx the call arrived under -- bwsalmon/agents#346's
// own wiring gap: every Handler used to build its own context.Background()
// internally, so cancelling the agent.Framework.Run call driving it had no
// way to reach whatever exec.CommandContext a Handler started. A Handler
// that shells out (run_command, sshRunCommandTool) must pass ctx straight
// through so that killing it is actually possible from outside.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args map[string]any) Result
}

// Registry holds the tools one server run advertises and answers the three
// JSON-RPC methods a client needs: initialize, tools/list, tools/call.
// Handle is decoupled from stdio transport so it's directly testable with
// plain bytes in, bytes out -- no subprocess, no real pipes required,
// mirroring v1's McpServer.handle.
type Registry struct {
	tools map[string]Tool
	order []string
	// deadline, when AnnounceDeadline has been called, is when grain
	// cancels the run this registry serves -- read on every tools/call so
	// that a run approaching it is told so on every answer it gets. nil
	// (no deadline announced) leaves every result exactly as its handler
	// wrote it. See run_deadline.go.
	deadline *runDeadline
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds tools to the registry, in the order given, appended after
// whatever is already registered. Registering a name a second time replaces
// its handler in place without changing tools/list's ordering.
func (r *Registry) Register(tools ...Tool) {
	for _, t := range tools {
		if _, exists := r.tools[t.Name]; !exists {
			r.order = append(r.order, t.Name)
		}
		r.tools[t.Name] = t
	}
}

// Handle answers one already-unmarshaled-into-bytes JSON-RPC request line
// and returns the response line to write back, or nil for a notification
// (no id) that has no response at all -- notifications/initialized is the
// only one a client sends here.
func (r *Registry) Handle(ctx context.Context, line []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return mustMarshalLine(rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
		})
	}

	switch req.Method {
	case "initialize":
		return mustMarshalLine(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			},
		})
	case "notifications/initialized":
		return nil
	case "tools/list":
		list := make([]map[string]any, 0, len(r.order))
		for _, name := range r.order {
			t := r.tools[name]
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		return mustMarshalLine(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": list}})
	case "tools/call":
		return r.handleCall(ctx, req.ID, req.Params)
	default:
		if len(req.ID) == 0 {
			return nil
		}
		return mustMarshalLine(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %s", req.Method)},
		})
	}
}

func (r *Registry) handleCall(ctx context.Context, id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return mustMarshalLine(rpcResponse{
				JSONRPC: "2.0", ID: id,
				Error: &rpcError{Code: -32602, Message: "invalid params: " + err.Error()},
			})
		}
	}

	t, ok := r.tools[p.Name]
	if !ok {
		return mustMarshalLine(rpcResponse{
			JSONRPC: "2.0", ID: id,
			Error: &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %s", p.Name)},
		})
	}

	args := p.Arguments
	if args == nil {
		args = map[string]any{}
	}
	result := t.Handler(ctx, args)
	// The deadline notice goes on every answer, a failed one included:
	// the run is just as close to being cancelled when its command
	// exited 1, and that is exactly the turn where it might otherwise
	// start a long repair it has no time to push.
	text := withDeadlineNotice(result.Text, r.deadlineNotice())
	return mustMarshalLine(rpcResponse{
		JSONRPC: "2.0", ID: id,
		Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": result.IsError,
		},
	})
}
