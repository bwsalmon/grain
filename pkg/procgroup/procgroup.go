// Package procgroup makes exec.CommandContext's cancellation reach a
// command's whole process tree, not just the one process it directly
// forked.
//
// exec.CommandContext's default Cancel only ever signals cmd.Process
// itself (os/exec's own doc comment: "Cancel ... calls cmd.Process.Kill()").
// That is not enough for bwsalmon/agents#346: killing `claude` on cancel
// leaves its own child mcpserver process (and anything run_command's own
// `bash -c` spawned under it) running as an orphan, since neither inherits
// the cancelled ctx -- the same gap for a local run_command's own `bash -c`
// forking a background pipeline. Prepare puts cmd in its own process
// group and overrides Cancel to signal the whole group instead, so
// cancelling the ctx a command was built with kills everything that
// command's own tree ever forked, the same "kill -TERM -$pgid" shape a
// process supervisor uses for exactly this reason.
package procgroup

import "os/exec"

// Prepare puts cmd in its own process group and arms its Cancel so that
// ctx being cancelled (cmd must have been built with exec.CommandContext)
// kills that whole group, not just cmd.Process. Call it before cmd.Start.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
}
