package grain

import (
	"fmt"
	"regexp"
)

// BuiltinTools are the tools a grain serves itself, and the whole of what
// it understands. Each is about the sandbox and nothing else:
//
//   - run_command, read_file, write_file, edit_file reach the guest over
//     vsock;
//   - recreate_sandbox throws the guest away and builds a fresh one,
//     which is a local kontur call now that the VMM is the shim's own
//     child;
//   - status writes Status.Activity, which the controller reads on its
//     next poll. It is the one escape hatch that becomes *fully* local:
//     today's update_status is an HTTP hop to the daemon to put a phrase
//     on a row, and as a built-in it is a file write that cannot fail and
//     costs the agent nothing.
//
// Everything else a deployment wants an agent to be able to do is
// declared as an ordinary MCP tool (Spec.Tools) and forwarded. There is
// no list here of open_pull_request, ask_question and the rest, and that
// is the point: those are grain-the-product's vocabulary, and a grain
// runs an agent in a sandbox without knowing why.
var BuiltinTools = []string{
	"run_command",
	"read_file",
	"write_file",
	"edit_file",
	"recreate_sandbox",
	"status",
}

// toolName is what a declared tool may be called. Deliberately narrow:
// the declaration lands in a file named after it (DirTools), so a name
// with a slash or a dot-dot in it is a path, and MCP's own tool names are
// identifier-shaped in practice anyway.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ToolDecl is one tool the controller declares and the grain forwards --
// an MCP tool declaration and nothing more, which is why it has no
// handler and no hint about what the tool does.
//
// The shim advertises these to the agent alongside its built-ins in
// tools/list, holds any call to one open, and surfaces it as Status.Call
// for the controller to serve. It reads the description and schema only
// to pass them on; it never validates a call against the schema, because
// the controller is better placed to say what a bad argument means and
// "passes them through without understanding them" is the property worth
// keeping.
//
// It is deliberately mcp.Tool minus the handler, so a controller that
// already has a tool registry can declare from it directly.
type ToolDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// checkTools rejects a tool set that cannot be served unambiguously: a
// name that is not a name, two declarations of one name, or a
// declaration shadowing a built-in.
//
// Refused rather than resolved by precedence, for the reason a version
// mismatch is refused: a grain that quietly picked one of two tools named
// run_command would give the agent a tool other than the one somebody
// meant it to have, and nothing downstream could tell.
func checkTools(decls []ToolDecl) error {
	builtin := make(map[string]bool, len(BuiltinTools))
	for _, name := range BuiltinTools {
		builtin[name] = true
	}
	seen := make(map[string]bool, len(decls))
	for _, d := range decls {
		switch {
		case !toolName.MatchString(d.Name):
			return fmt.Errorf("grain: %q is not a usable tool name", d.Name)
		case builtin[d.Name]:
			return fmt.Errorf("grain: a declared tool is named %q, which is one of the grain's own", d.Name)
		case seen[d.Name]:
			return fmt.Errorf("grain: two tools are declared as %q", d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}
