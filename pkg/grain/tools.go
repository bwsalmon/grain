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
//     phrase on a row, and here it is a file write in this container that
//     cannot fail and costs no vsock hop.
//
// That last one is the agent's route and the cheapest, but not the only
// one: anything running inside the sandbox sets the same field through
// GuestActivityFile. Setup needs it, because it runs before there is an
// agent to call a tool, and so does a long guest command the agent is
// blocked on.
//
// There is no seventh tool, and nothing outside the sandbox is a tool at
// all. An escape hatch is a CLI in the guest with a credential
// placed beside it (Spec.Placements), which the agent runs with
// run_command: "grainctl open-pull-request --title ..." rather than a
// tool call the shim would have to declare, relay and keep in sync. That
// is how the git proxy already works, and reusing its shape rather than
// inventing one is the point -- a deployment adds a capability by
// shipping a binary and a placement, with no change to this package, no
// merged tools/list, and no connection whose failure costs the agent
// those tools for the rest of the run.
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
