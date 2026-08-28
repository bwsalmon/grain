package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// SSHRunner runs argv on a remote host over SSH -- the transport
// NewSSHSandboxTools' tools use in place of sandbox_tools.go's direct
// filesystem calls, so a kontur-managed VM (see package kontur and
// bwsalmon/agents#256) can stand in for NewSandboxTools' local-directory
// stand-in the same way grain/automation/ssh.py's SshRunner already lets a
// v1 dispatch reach a real libvirt-managed sandbox.
//
// The flags Run passes are ported from SshRunner's own, for the same two
// reasons its docstring gives, both found live against a real sandbox VM,
// not reasoned about:
//
//   - UserKnownHostsFile=/dev/null, StrictHostKeyChecking=accept-new: a
//     sandbox at a fixed address gets a *new* host key every recreate: the
//     default known_hosts file pins the first key it sees, so the very
//     next recreate turns every dispatch into "REMOTE HOST IDENTIFICATION
//     HAS CHANGED". A sandbox is authenticated by its fixed address on a
//     firewalled network and this runner's own key, not by remembering an
//     ephemeral VM's host identity.
//   - IdentityAgent=none: an ambient SSH_AUTH_SOCK can leave ssh probing a
//     stale or unresponsive agent socket before it ever gets to
//     authentication, hanging the entire connection indefinitely with no
//     ConnectTimeout covering that phase. This runner brings its own key;
//     it has no use for an agent, forwarded or otherwise.
type SSHRunner struct {
	User string
	Host string
	// Port is the external port that reaches the remote host's sshd --
	// for a kontur VM, netshim's forwarded port (kontur.Port), not
	// GuestPort itself. Zero means ssh's own default (22).
	Port int
	// KeyPath is a private key file ssh -i authenticates with.
	KeyPath string
}

// Run executes argv on the remote host, piping stdin to it verbatim if
// non-empty. exitCode is -1 for a connection failure (ssh's own exit code
// for one, e.g. a host that never answers) rather than a real remote exit
// status -- the same distinction sandbox_tools.go's local run_command
// makes for a command that never started at all.
//
// The remote command is one shell-quoted string, not a trailing argv:
// SSH's protocol has no concept of an argv array for exec requests, only a
// single command string that the client builds by joining its trailing
// arguments -- OpenSSH does this with a plain, *unquoted* space, so
// shellJoin (this file) has to produce a string that survives the remote
// shell's own re-parsing intact, the same problem shlex.join(argv) solves
// in SshRunner.
func (r *SSHRunner) Run(ctx context.Context, argv []string, stdin string) (stdout, stderr string, exitCode int) {
	args := []string{
		"-i", r.KeyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentityAgent=none",
		"-o", "ConnectTimeout=10",
	}
	if r.Port != 0 {
		args = append(args, "-p", fmt.Sprintf("%d", r.Port))
	}
	args = append(args, fmt.Sprintf("%s@%s", r.User, r.Host), "--", shellJoin(argv))

	cmd := exec.CommandContext(ctx, "ssh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return outBuf.String(), errBuf.String(), exitErr.ExitCode()
		}
		return outBuf.String(), err.Error(), -1
	}
	return outBuf.String(), errBuf.String(), 0
}

// shellJoin renders argv as a single POSIX-shell-safe string, quoting each
// element so OpenSSH's own unquoted re-join of its trailing arguments (see
// Run's doc comment) parses back into exactly argv again on the remote
// end.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote quotes s for a POSIX shell, single-quoting anything outside a
// small safe set of characters -- a literal single quote inside s is
// handled by closing the quote, escaping the quote character itself, then
// reopening it.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r == '=' || r == ',':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
