package dockervm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ExecOptions describes one command run inside a VM's guest.
type ExecOptions struct {
	// Command is the guest command and its arguments. Empty asks for an
	// interactive login shell, which is what "kontur exec" with no
	// arguments already means.
	Command []string

	// TTY allocates a terminal for the session ("docker exec -t"). Only
	// valid when Stdin is a terminal -- docker refuses it otherwise with
	// "the input device is not a TTY" -- so callers check that first.
	TTY bool

	// Env are extra "KEY=value" pairs set on the docker exec, i.e. on
	// "kontur exec" itself rather than on the guest command: the guest
	// side takes its settings (KONTUR_EXEC_USER,
	// KONTUR_EXEC_CONNECT_TIMEOUT) from the environment of the process
	// that dials it.
	Env []string

	// Workdir and GuestEnv are the other half of that distinction: they
	// belong to the command inside the guest, so they are passed as
	// "kontur exec"'s own -w and -e flags rather than as environment of
	// the process in the container. Empty leaves both to the guest's
	// defaults -- the account's home directory and the login environment
	// the agent builds (see internal/agent).
	Workdir  string
	GuestEnv []string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a command inside the guest of VM name and returns the
// command's own exit status.
//
// The path is "docker exec <container> kontur exec -- <command>": the
// container is a scratch image with no shell in it, so "kontur exec" is
// what bridges the gap between the container and the guest running inside
// it (see the top-level README's "Execing into a VM"). Everything about
// this is what a caller would otherwise type by hand; the point of having
// it here is that the container name is derived from the VM name rather
// than being an implementation detail callers have to know.
//
// The returned error is about being unable to *run* the command at all --
// no such VM, a docker daemon that refused. A guest command that ran and
// exited non-zero is not an error: its status comes back as the int, so
// callers can pass it on the way a shell would. The two are told apart by
// checking the container first, so that docker's own 125/126/127 aren't
// silently reported as the guest's.
func Exec(ctx context.Context, d *Docker, name string, opts ExecOptions) (int, error) {
	container := vmContainerName(name)
	running, err := d.running(ctx, container)
	if err != nil {
		return 0, err
	}
	if !running {
		return 0, fmt.Errorf("VM %q is not running: no container %s (check \"konturctl vm list\", or \"docker logs %s\" if it exited)", name, container, container)
	}

	// -i always, TTY or not: a session proxies stdin to the guest command
	// byte for byte, which is what makes "... -- sh -c 'cat > /tmp/f' <
	// file" work (see the README's Flow 4). Without it docker closes
	// stdin immediately and every such pipeline writes an empty file.
	args := []string{"exec", "-i"}
	if opts.TTY {
		args = append(args, "-t")
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	args = append(args, container, "kontur", "exec")
	if opts.Workdir != "" {
		args = append(args, "-w", opts.Workdir)
	}
	for _, e := range opts.GuestEnv {
		args = append(args, "-e", e)
	}
	if len(opts.Command) > 0 {
		args = append(args, "--")
		args = append(args, opts.Command...)
	}

	cmd := exec.CommandContext(ctx, d.binary(), args...)
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("docker exec on %s: %w", container, err)
	}
	return 0, nil
}

// readyProbeArgs is the command a readiness probe runs inside a VM's
// container: "kontur ready" makes one attempt to reach the guest and
// exits 0 only if it answered (see cmd/kontur's "ready" mode).
//
// "-timeout 0" -- one attempt, no retrying of its own -- is what makes
// it usable inside a loop that is already retrying. A probe that keeps
// trying for the 30 seconds "kontur exec" gives a dial by default
// (guestexec's defaultConnectTimeout) swallows the caller's deadline
// whole: a "-ready-timeout 10s" then reported timing out after 10s three
// times as long after the fact, and the container-exited check below --
// the failure this loop is here to fail fast on -- did not get to run
// until the probe it is meant to cut short had finished anyway. With the
// retrying out here instead, a probe against a guest that is not up costs
// one dial -- guestexec gives that 3 seconds -- and the deadline below is
// the thing that decides when to stop.
var readyProbeArgs = []string{"kontur", "ready", "-timeout", "0"}

// errProbeUnsupported is what a probe against a VM whose image predates
// "kontur ready" comes back as, so that callers stop rather than retry:
// no amount of waiting adds a mode to an image that doesn't have one.
var errProbeUnsupported = errors.New(`the VM's kontur image has no "kontur ready" mode, so its guest cannot be probed; rebuild or repull the image and re-create the VM ("konturctl vm update <name>")`)

// Status is what is knowable about a VM from outside it: whether its
// container is running, and whether the guest inside that container
// answers.
//
// The two are separate on purpose. A container that is up with a guest
// that isn't is the ordinary state of a VM that was created a second
// ago, and it is also what a guest that failed to boot looks like
// forever -- which is the distinction "is it ready?" alone cannot draw.
type Status struct {
	// Container is the docker container this VM runs in, named here so a
	// caller can point at "docker logs" without deriving it again.
	Container string

	// Running reports whether that container exists and is running.
	Running bool

	// Ready reports whether the guest answered the probe. Always false
	// when Running is false, since there is nothing to probe through.
	Ready bool

	// Detail is why the guest did not answer, when it didn't: the probe's
	// own message, which says whether the socket was missing (the VMM has
	// not started) or the port refused (the guest has not started the
	// agent). Empty when Ready.
	Detail string
}

// Inspect answers "is this VM's guest up?" once, without waiting, which
// is the question "vm wait" only answers by blocking until it can say
// yes.
//
// An error means the question could not be asked -- docker was
// unreachable, or the image cannot be probed at all. A VM that is simply
// not ready is not an error: that is a Status with Ready false and
// Detail saying why.
func Inspect(ctx context.Context, d *Docker, name string) (Status, error) {
	st := Status{Container: vmContainerName(name)}
	running, err := d.running(ctx, st.Container)
	if err != nil {
		return st, err
	}
	st.Running = running
	if !running {
		return st, nil
	}

	if err := probeReady(ctx, d, name); err != nil {
		if errors.Is(err, errProbeUnsupported) {
			return st, err
		}
		st.Detail = err.Error()
		return st, nil
	}
	st.Ready = true
	return st, nil
}

// WaitReady blocks until the guest of VM name accepts a command, giving
// up at timeout -- or sooner, if the VM's container exits, which no
// amount of further waiting would fix.
//
// This is the piece "konturctl vm create" doesn't do: it returns once the
// containers are started, which is before the guest has booted, so
// anything that wants to talk to the guest has to wait for it first. A
// caller doing it by hand polls "kontur exec -- true" in a shell loop;
// this is the same poll, against the same definition of ready (see
// guestexec.Ready), written once.
func WaitReady(ctx context.Context, d *Docker, name string, timeout time.Duration) error {
	container := vmContainerName(name)
	deadline := time.Now().Add(timeout)
	for {
		st, err := Inspect(ctx, d, name)
		if err != nil {
			return err
		}
		if st.Ready {
			return nil
		}
		if !st.Running {
			return fmt.Errorf("the VM container %s exited before its guest became reachable (see \"docker logs %s\")", container, container)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the guest of %q to become reachable (see \"docker logs %s\"): %s", timeout, name, container, st.Detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// probeReady runs one "kontur ready" in the VM's container. The error it
// returns on failure is the probe's own output rather than a wrapped
// "docker exec ... exit status 1", because that output is the only thing
// that says which half of the boot is still outstanding, and it ends up
// in front of whoever asked.
func probeReady(ctx context.Context, d *Docker, name string) error {
	args := append([]string{"exec", vmContainerName(name)}, readyProbeArgs...)
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		// An older image answers a mode it doesn't have with main's own
		// "unknown mode" line, which would otherwise be retried until the
		// timeout and then reported as a guest that never booted.
		if strings.Contains(detail, `unknown mode "ready"`) {
			return fmt.Errorf("%w (%s)", errProbeUnsupported, detail)
		}
		return errors.New(detail)
	}
	return nil
}

// running reports whether a container exists and is running. A container
// that isn't there at all is reported as "not running" rather than as an
// error: to a caller reaching for a VM the two are the same thing, and
// the message it then prints names the container either way.
func (d *Docker) running(ctx context.Context, container string) (bool, error) {
	var out bytes.Buffer
	if err := d.run(ctx, &out, "inspect", "-f", "{{.State.Running}}", container); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out.String()) == "true", nil
}
