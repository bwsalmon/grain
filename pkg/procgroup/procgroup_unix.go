//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
)

// prepare sets Setpgid so cmd's process becomes the leader of a new
// process group that anything it forks (a shell's own pipeline, `claude`
// forking its own mcpserver child) inherits, then overrides Cancel to
// SIGKILL the negative pid -- POSIX's own "signal the whole group" idiom
// -- instead of exec.CommandContext's default of killing cmd.Process
// alone.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
