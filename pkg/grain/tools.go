package grain

// BuiltinTools are the tools a grain serves itself, and the whole of what
// it understands. Each is about the sandbox and nothing else:
//
//   - run_command, read_file, write_file, edit_file reach the guest over
//     vsock;
//   - recreate_sandbox throws the guest away and builds a fresh one,
//     which is a local kontur call now that the VMM is the shim's own
//     child;
//   - status writes Status.Activity, which the controller reads off the
//     record stream. It is the one escape hatch that becomes *fully*
//     local: today's update_status is an HTTP hop to the daemon to put a
//     phrase on a row.
//
// Every other tool an agent can call comes from an ordinary MCP server
// the controller runs, reached over the upstream connection (SocketUpstream).
// The shim merges those into its own tools/list and relays their calls; it
// holds no vocabulary for them, and never sees their declarations except
// as whatever that server advertised.
//
// This is a list rather than a registry because it is the only tool
// knowledge in this package: whoever wrote a tool is who knows what it
// does, and for these six that is grain.
var BuiltinTools = []string{
	"run_command",
	"read_file",
	"write_file",
	"edit_file",
	"recreate_sandbox",
	"status",
}
