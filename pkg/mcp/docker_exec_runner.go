package mcp

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// DockerExecRunner runs argv inside a kontur-managed VM's guest through
// `docker exec <container> kontur exec` -- the remoteRunner contract
// NewSSHSandboxTools and ConfigureGitCredentialsOverSSH take, reaching the
// guest from *inside* its own VM container rather than over a TCP
// connection to netshim's externally forwarded port.
//
// bwsalmon/kontur already ships the guest-side half of this: its
// internal/guestexec ("kontur exec", one of the modes cmd/kontur
// dispatches) SSHes to the guest's own tap-attached address, which the
// docker backend records as KONTUR_EXEC_ADDR when it starts the VM
// container (internal/dockervm's Create). That address is reachable
// without any address translation at all, since the container shares the
// network namespace netshim set the tap up in -- guestexec's own Config.Addr
// doc comment: "reachable directly from this container's own network
// namespace ... without going through NETSHIM_GUEST_PORT's external DNAT
// at all."
//
// The point of routing through it is that nothing outside the VM's own
// container has to be able to reach the guest, so none of the machinery
// that makes such a connection possible has to exist or be discovered:
// netshim's inbound DNAT rules, the external port kontur assigns per VM
// (read out of kontur's state file), and the container address that port
// answers on (a `docker inspect` away) are all only there to serve a
// caller connecting from outside -- which is why pkg/kontur carries none
// of the three any more.
//
// Auth is still the guest's own sshd -- "kontur exec" SSHes the last hop
// -- with the same account and the same keypair a deployment already baked
// into its guest image (scripts/setup.sh's ensure_kontur_ssh_key, whose
// public half scripts/kontur/guest-setup.sh installs as the "debian"
// account's only authorized_keys entry). Only how that hop is reached
// differs, which is why User here is the value KonturConfig.SSHUser
// carries.
type DockerExecRunner struct {
	// Container is the docker container to exec into: a VM's own
	// container, not the "-netns" holder that merely owns the network
	// namespace -- kontur.PodName(vmName), the name internal/dockervm
	// gives it. Only the VM container has the `kontur` binary's
	// KONTUR_EXEC_ADDR set (the holder runs the same image in "sleep"
	// mode with no such variable), so this is the only one of the two
	// where "kontur exec" can work at all.
	Container string

	// User is the guest account to log in as, passed through as
	// KONTUR_EXEC_USER. Empty leaves guestexec's own default ("root",
	// the only account kontur's *reference* guest image creates) in
	// place -- which is not what a grain guest image wants, since
	// scripts/kontur/guest-setup.sh creates and authorizes "debian"
	// instead.
	User string

	// KeyPath is the private key "kontur exec" authenticates with,
	// passed through as KONTUR_EXEC_KEY. It is a path *inside the VM
	// container*, not on the host -- so a deployment wanting to use its
	// own key (rather than the dedicated one kontur's own Dockerfile
	// bakes into the image and authorizes on its own guest image, which
	// a foreign CHV_DISK_IMAGE does not authorize) has to place it
	// somewhere the container can already read. The images directory
	// internal/dockervm mounts read-only at /images is such a place, and
	// needs no change to kontur to use.
	//
	// Empty leaves guestexec's default (/etc/kontur/exec_id_ed25519,
	// kontur's own baked-in key) in place, which only works against
	// kontur's own guest image.
	KeyPath string

	// ConnectTimeout bounds how long "kontur exec" retries its initial
	// connection to the guest before giving up, passed through as
	// KONTUR_EXEC_CONNECT_TIMEOUT. Zero leaves guestexec's own default
	// (30s) in place.
	ConnectTimeout time.Duration
}

// Run executes argv inside the guest, piping stdin to it verbatim if
// non-empty, and is what satisfies the remoteRunner contract.
//
// Unlike an `ssh host <command>` call, argv is passed through as a real
// argv rather than shell-quoted into one string first: "kontur exec --" takes its trailing
// arguments as a slice and does that join itself (guestexec.Run, which
// shellJoins into the single command string SSH's exec request actually
// carries). Quoting here as well would double-quote every argument.
//
// exitCode follows remoteRunner's convention: the guest command's own
// status when it ran, and -1 when it never did. `docker exec` propagates the
// exit status of the process it started, and "kontur exec" exits with the
// guest command's own (cmd/kontur's runGuestSession ends in os.Exit(code)),
// so a real remote status arrives here intact. A failure *before* the
// guest command ran -- no such container, a docker daemon that is not
// answering, an unreachable or unauthenticated guest -- surfaces as
// exit 1 either way, which is why execFailedBeforeGuest below has to tell
// the two apart by what was written to stderr rather than by status alone.
func (r *DockerExecRunner) Run(ctx context.Context, argv []string, stdin string) (stdout, stderr string, exitCode int) {
	args := r.execArgs(argv, stdin != "")

	cmd := exec.CommandContext(ctx, "docker", args...)
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
			code := exitErr.ExitCode()
			if code == 1 && execFailedBeforeGuest(errBuf.String()) {
				return outBuf.String(), errBuf.String(), -1
			}
			return outBuf.String(), errBuf.String(), code
		}
		return outBuf.String(), err.Error(), -1
	}
	return outBuf.String(), errBuf.String(), 0
}

// execFailedBeforeGuest reports whether stderr looks like it came from
// something that failed before the guest command ever ran, rather than
// from the guest command itself -- the distinction ssh's own reserved exit
// status would give for free but `docker exec` cannot provide, since it
// reports the exit status of whatever it started and both failures below
// exit 1.
//
// Both are recognized by a prefix on the *first* line of stderr:
//
//   - "kontur: exec: ..." -- cmd/kontur's own log prefix for its "exec"
//     mode, which it sets before anything can fail and which reaches this
//     stderr only via log.Fatalf (guestexec never logs on a successful
//     session, so nothing else in the stream can carry it).
//   - "Error ..." -- the docker CLI's own failures, e.g. "Error response
//     from daemon: ..." for a daemon that is not answering and "Error: No
//     such container: ..." for a VM whose container is not there.
//
// A guest command's own stderr is the only other thing on this stream,
// and would have to open with one of those two prefixes on its first line
// to be misread. The cost of that misreading is a -1 where a 1 belonged:
// the same "the command did not run" report an unreachable sandbox
// already gets, which is the safer of the two directions to err in.
func execFailedBeforeGuest(stderr string) bool {
	line := stderr
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "kontur: exec: ") || strings.HasPrefix(line, "Error")
}

// execArgs renders the `docker exec` arguments Run executes, kept
// separate from Run itself so a test can assert on the invocation without
// a docker daemon anywhere. interactive adds -i, which is needed only
// when there is stdin to pipe through: without it docker gives the
// exec'd process no stdin at all, which is what a command reading from a
// closed stdin should see anyway.
func (r *DockerExecRunner) execArgs(argv []string, interactive bool) []string {
	args := []string{"exec"}
	if interactive {
		args = append(args, "-i")
	}
	if r.User != "" {
		args = append(args, "-e", "KONTUR_EXEC_USER="+r.User)
	}
	if r.KeyPath != "" {
		args = append(args, "-e", "KONTUR_EXEC_KEY="+r.KeyPath)
	}
	if r.ConnectTimeout > 0 {
		args = append(args, "-e", "KONTUR_EXEC_CONNECT_TIMEOUT="+r.ConnectTimeout.String())
	}
	args = append(args, r.Container, "kontur", "exec", "--")
	return append(args, argv...)
}
