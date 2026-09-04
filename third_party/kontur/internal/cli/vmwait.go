package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bwsalmon/kontur/internal/dockervm"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// defaultReadyTimeout is how long "vm create -wait", "vm wait" and "vm
// run" give a guest to answer before giving up. Generous on purpose: a
// cold boot of a stock guest is seconds, but a guest that runs a setup
// script or grows its filesystem onto a larger -disk-size-mb can take
// considerably longer, and the wait ends early anyway if the VM's
// container exits.
const defaultReadyTimeout = 3 * time.Minute

// runVMWait blocks until a VM's guest answers a command, so a script can
// wait for a VM created earlier without inventing its own poll loop.
//
// "vm create" returns once the containers are started, which is before
// the guest has booted; "vm create -wait" folds this in for the VM it
// makes, and this is the same wait for a VM that already exists -- one
// started without -wait, one being restarted, or one a different script
// created.
func runVMWait(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm wait")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm wait "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	timeout := fs.Duration("timeout", defaultReadyTimeout, "how long to wait for the guest to answer before giving up")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	spec, err := staticpod.Load(*stateDir, name)
	if err != nil {
		return fmt.Errorf("VM %q not found in %s (\"konturctl vm list -state-dir %s\" shows what is): %w", name, *stateDir, *stateDir, err)
	}
	return waitForGuest(ctx, name, spec.BackendOrDefault(), *timeout, stdout)
}

// waitForGuest is the readiness wait itself, shared by "vm wait" and "vm
// create -wait": it says what it is waiting for, waits, and says when the
// guest answered.
//
// Progress goes to the writer the caller passes rather than always to
// stderr, because who owns stdout differs between them -- "vm create"'s
// own output is progress, "vm run"'s stdout belongs to the guest command.
func waitForGuest(ctx context.Context, name, backend string, timeout time.Duration, progress io.Writer) error {
	if backend != staticpod.BackendDocker {
		return errNoWaitForBackend(name, backend)
	}
	fmt.Fprintf(progress, "waiting for the guest of %q (up to %s)\n", name, timeout)
	if err := dockervm.WaitReady(ctx, &dockervm.Docker{}, name, timeout); err != nil {
		return err
	}
	fmt.Fprintf(progress, "VM %q is ready\n", name)
	return nil
}

// errNoWaitForBackend explains that konturctl can only wait for
// docker-backed VMs, and hands over the command that does the same thing
// by hand for the one it can't.
//
// Readiness here means "the guest answers a command", which is exec's own
// path -- so waiting inherits exactly the limitation exec has (see
// errNoExecForBackend): there is no way into a static-pod VM's container
// that doesn't go through crictl, which konturctl neither installs nor
// drives.
func errNoWaitForBackend(name, backend string) error {
	const endpoint = "unix:///run/containerd/containerd.sock"
	return fmt.Errorf(`waiting for a guest is only implemented for -backend %s, and VM %q runs under -backend %s.
Readiness is decided by running a command in the guest, so this has exec's limitation: poll it by hand through crictl instead (see deploy/static-kubelet/README.md):

  id=$(crictl --runtime-endpoint %s ps -q --name %s --state Running)
  until crictl --runtime-endpoint %s exec "$id" kontur ready -timeout 0; do sleep 2; done

Under a real kubelet there is nothing to poll: the rendered manifest gives the VM container a "kontur ready" readinessProbe, so the pod's own Ready condition is this wait.`,
		staticpod.BackendDocker, name, backend, endpoint, name, endpoint)
}
