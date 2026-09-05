// Package granule is one run: the contract a grain is configured by and
// reports through, and the binary that honours it.
//
// It is named for what that binary is -- a small grain, one per run. See
// docs/grain.md, "Three binaries, one per trust zone": graind serves,
// grain calls, granule is one run.
//
// # Two halves, deliberately together
//
// The contract is Spec (what a granule is given), Status and Record (what
// it emits), and the environment and file layout that carry them
// (env.go, files.go). The implementation is everything that boots a VMM,
// provisions a guest and writes those records.
//
// They live in one package because they have to agree, and a change to
// either is then a change in the same place as the other -- the same
// instinct that makes this repo build a guest and its konturctl from one
// commit. The cost is that a controller importing this for a Status also
// links the code that forks kontur; that is stdlib-only and has no init,
// so the cost is conceptual rather than real.
//
// What is *not* here is how a controller manages many of these: Grains,
// Grain, Reconcile and Policy are pkg/grain's, because a granule never
// touches them. That split is not a judgement call -- it is what the
// imports already said.
//
// # Nothing here is called into
//
// A granule reads its environment and the tree at Root before it starts,
// writes records to stdout and a Result to FileTerminationLog on the way
// out, and has no inbound surface of any kind. That is the property the
// controller's whole read path rests on, so it is worth stating in the
// package that would be the one to break it.
package granule

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Guest is the sandbox as granule reaches it: one vsock hop away, over
// kontur's own guest session.
//
// An interface rather than a concrete type because every test in this
// package would otherwise need a VM. The real implementation forks the
// kontur binary sitting beside granule in the same image, which is the
// cheap version of the same call the daemon makes today -- `fork kontur
// exec -> vsock -> guest` rather than `fork docker CLI -> dockerd RPC ->
// kontur exec -> vsock -> guest`.
type Guest interface {
	// Ready reports whether the guest is up and will run a command. It
	// is one attempt, not a poll: the caller owns the waiting, because
	// the caller is the one narrating the wait.
	Ready(ctx context.Context) error

	// Exec runs one command in the guest and returns its exit code. A
	// non-zero code is not an error -- the command ran and said no --
	// and err is reserved for not having been able to ask at all, which
	// is the distinction the whole setup path turns on.
	Exec(ctx context.Context, cmd []string, stdout, stderr io.Writer) (int, error)

	// Unpack streams a tar into the guest, rooted at /. It is how
	// everything grain puts in a sandbox gets there: placements, the
	// setup script, and the grain client itself.
	Unpack(ctx context.Context, tar io.Reader) error

	// ReadFile reads one guest file. Absent is not an error, and returns
	// no bytes: the one caller reads GuestActivityFile, which usually
	// does not exist.
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// VMM is the hypervisor granule starts as its own child.
//
// kontur's run mode does not require being PID 1 -- it installs a
// signal.Notify handler for SIGTERM and reaps nothing -- so it runs
// happily as a child, provided somebody forwards it the signal. That
// somebody is granule, which is PID 1 (see Run).
type VMM interface {
	// Start boots the VM and returns once it is launched, not once the
	// guest is up. console receives the guest's serial output, which
	// kontur writes to its own stdout.
	Start(ctx context.Context, console io.Writer) error
	// Shutdown asks for a graceful power-off and waits for it.
	Shutdown(ctx context.Context) error
	// Wait blocks until the VMM exits on its own.
	Wait() error
}

// KonturBinary is where the kontur binary sits in a grain's container
// image. Fixed rather than looked up on PATH: granule forks it as a
// child on every guest call, and a PATH lookup that resolved to
// something else would be a sandbox escape wearing a familiar name.
const KonturBinary = "/usr/local/bin/kontur"

// konturGuest reaches the guest by forking the kontur binary in this
// container, which is exactly what `docker exec <container> kontur exec`
// does today with two fewer hops in front of it.
type konturGuest struct{ bin string }

// NewGuest returns the Guest backed by the kontur binary at bin, or
// KonturBinary when bin is empty.
func NewGuest(bin string) Guest {
	if bin == "" {
		bin = KonturBinary
	}
	return konturGuest{bin: bin}
}

func (g konturGuest) Ready(ctx context.Context) error {
	// -timeout 0 is a single attempt: the caller is already looping and
	// narrating, and a probe that waited as well would only race it.
	cmd := exec.CommandContext(ctx, g.bin, "ready", "-timeout", "0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("guest not ready: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g konturGuest) Exec(ctx context.Context, cmd []string, stdout, stderr io.Writer) (int, error) {
	args := append([]string{"exec", "--"}, cmd...)
	c := exec.CommandContext(ctx, g.bin, args...)
	c.Stdout, c.Stderr = stdout, stderr
	err := c.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		// kontur exec propagates the guest command's own code, so this
		// is the command saying no rather than the session failing.
		return ee.ExitCode(), nil
	}
	return 0, fmt.Errorf("running %q in the guest: %w", strings.Join(cmd, " "), err)
}

func (g konturGuest) Unpack(ctx context.Context, tar io.Reader) error {
	c := exec.CommandContext(ctx, g.bin, "cp", "-tar", "-", "/")
	c.Stdin = tar
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("unpacking into the guest: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

func (g konturGuest) ReadFile(ctx context.Context, path string) ([]byte, error) {
	c := exec.CommandContext(ctx, g.bin, "cp", path, "-")
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		// Absent is the common case for the one file this reads, and a
		// missing file is not a failure to report -- it means nothing in
		// the guest has said anything yet.
		return nil, nil
	}
	return out.Bytes(), nil
}

// konturVMM boots the VM by forking `kontur run`, whose configuration is
// the CHV_* environment this container already has. granule reads none
// of it: the shape is kontur's vocabulary and passes straight through
// (pkg/grain/env.go).
type konturVMM struct {
	bin  string
	cmd  *exec.Cmd
	done chan error
}

// NewVMM returns the VMM backed by the kontur binary at bin, or
// KonturBinary when bin is empty.
func NewVMM(bin string) VMM {
	if bin == "" {
		bin = KonturBinary
	}
	return &konturVMM{bin: bin, done: make(chan error, 1)}
}

func (v *konturVMM) Start(ctx context.Context, console io.Writer) error {
	// Deliberately not CommandContext: cancelling this context must not
	// SIGKILL a running VM out from under a graceful shutdown. Stopping
	// is Shutdown's job and it has its own path.
	v.cmd = exec.Command(v.bin, "run")
	v.cmd.Stdout, v.cmd.Stderr = console, console
	if err := v.cmd.Start(); err != nil {
		return fmt.Errorf("starting the VMM: %w", err)
	}
	go func() { v.done <- v.cmd.Wait() }()
	return nil
}

func (v *konturVMM) Shutdown(ctx context.Context) error {
	if v.cmd == nil || v.cmd.Process == nil {
		return nil
	}
	// SIGTERM is what kontur's run mode listens for, and what it turns
	// into a guest power-off bounded by its own CHV_SHUTDOWN_TIMEOUT.
	// Forwarding it is granule's job precisely because kontur is no
	// longer PID 1 and will not be sent one by the runtime.
	_ = v.cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-v.done:
		return err
	case <-ctx.Done():
		_ = v.cmd.Process.Kill()
		return ctx.Err()
	}
}

func (v *konturVMM) Wait() error { return <-v.done }
