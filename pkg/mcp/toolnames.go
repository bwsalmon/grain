package mcp

import "strings"

// ToolNamespace is the mcpServers key both agent frameworks write into
// their CLI's MCP configuration (agent/claude's --mcp-config,
// agent/antigravity's settings file) -- matching v1's own "grain-sandbox"
// naming (dispatch.py's _mcp_config_json) purely for continuity; neither
// CLI cares what this string is.
//
// It is not protocol.go's serverName, which is what this server calls
// itself in the initialize handshake. This is what the client filed the
// server under, and the two have never had to agree.
const ToolNamespace = "grain-sandbox"

// QualifiedToolName renders a tool this package registers the way a CLI
// that loaded it from an MCP config reports and admits it:
// "mcp__<namespace>__<tool>". Both frameworks' allowedTools build their
// --allowedTools list with it.
func QualifiedToolName(tool string) string {
	return "mcp__" + ToolNamespace + "__" + tool
}

// BareToolName undoes QualifiedToolName: given whatever name a CLI
// reported a call under, it returns the name this package actually
// registered the tool as. A name without the prefix -- a CLI's own native
// tool, or a framework whose runtime calls tools in-process -- comes back
// unchanged.
//
// Every Framework must put a call's name through this before recording it
// as an agent.ToolCall, and agent.ToolCall.Name's own doc comment says so.
// The prefix is a detail of how a CLI namespaces the MCP servers it
// loaded, not part of the tool's identity, and a consumer downstream of
// agent.Result matches on the identity: orchestrator.ProcessResult decides
// whether a run asked a question, left a closing comment, or proposed a
// task by comparing against "ask_question", "comment_on_issue" and
// "propose_task" exactly. It compared against names that arrived
// "mcp__grain-sandbox__"-prefixed and so never matched any of them, which
// silently cost every real run its question and its closing comment: the
// run was recorded no_action, and the human who was supposed to be asked
// was never asked. Every test of that path scripted the bare name, which
// is why nothing caught it.
func BareToolName(reported string) string {
	return strings.TrimPrefix(reported, "mcp__"+ToolNamespace+"__")
}
