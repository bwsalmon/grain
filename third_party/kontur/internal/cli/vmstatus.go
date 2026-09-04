package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/bwsalmon/kontur/internal/dockervm"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// defaultStatusTimeout bounds one "vm status": the probe behind it is a
// single attempt at reaching the guest, so anything longer than this is
// docker itself being slow rather than a guest taking its time.
const defaultStatusTimeout = 30 * time.Second

// runVMStatus answers "is this VM's guest up?" without waiting for the
// answer to be yes -- the question "vm wait" only answers by blocking
// until it can.
//
// It reports through its exit status as well as its output, so it is
// usable in a script's own condition ("konturctl vm status web ||
// ..."): zero when the guest answered, one when it did not. A VM that
// cannot be found, or a docker that cannot be reached, is an error
// instead -- the question could not be asked at all, which is a
// different thing from the answer being no.
func runVMStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm status")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm status "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	timeout := fs.Duration("timeout", defaultStatusTimeout, "how long to give the check before reporting the guest as unreachable; this asks once rather than waiting for a boot -- \"konturctl vm wait\" is the one that waits")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	spec, err := staticpod.Load(*stateDir, name)
	if err != nil {
		return fmt.Errorf("VM %q not found in %s (\"konturctl vm list -state-dir %s\" shows what is): %w", name, *stateDir, *stateDir, err)
	}
	backend := spec.BackendOrDefault()
	if backend != staticpod.BackendDocker {
		return errNoStatusForBackend(name, backend)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	st, err := dockervm.Inspect(ctx, &dockervm.Docker{}, name)
	if err != nil {
		return err
	}

	writeStatus(stdout, name, backend, st)
	if !st.Ready {
		// The status has already been printed, and it says everything
		// there is to say: exitStatusError is what keeps Run from adding
		// a "konturctl: ..." line claiming this command failed, when
		// what happened is that it answered.
		return &exitStatusError{code: 1}
	}
	return nil
}

// writeStatus prints one VM's status as fields rather than as a table
// row: there is one VM here, and the fields are worth reading whole --
// the container line is what a caller needs to reach for "docker logs",
// and the guest line is why it isn't ready yet.
func writeStatus(w io.Writer, name, backend string, st dockervm.Status) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "NAME\t%s\n", name)
	fmt.Fprintf(tw, "BACKEND\t%s\n", backend)
	containerState := "not running"
	if st.Running {
		containerState = "running"
	}
	fmt.Fprintf(tw, "CONTAINER\t%s (%s)\n", st.Container, containerState)

	switch {
	case st.Ready:
		fmt.Fprintf(tw, "GUEST\tready\n")
	case !st.Running:
		// Nothing was probed, so there is no guest-side reason to give:
		// say where to look instead of implying the guest was asked.
		fmt.Fprintf(tw, "GUEST\tunreachable (the VM's container is not running -- \"docker logs %s\")\n", st.Container)
	default:
		fmt.Fprintf(tw, "GUEST\tnot ready\n")
		fmt.Fprintf(tw, "WHY\t%s\n", st.Detail)
	}
	tw.Flush()
}

// errNoStatusForBackend explains that konturctl can only report on
// docker-backed VMs, and hands over the command that asks the same
// question by hand for the one it can't.
//
// Readiness is decided by running a command in the guest, so this
// inherits exec's limitation exactly the way waiting does (see
// errNoWaitForBackend): there is no way into a static-pod VM's container
// that doesn't go through crictl, which konturctl neither installs nor
// drives.
func errNoStatusForBackend(name, backend string) error {
	const endpoint = "unix:///run/containerd/containerd.sock"
	return fmt.Errorf(`reporting a VM's status is only implemented for -backend %s, and VM %q runs under -backend %s.
Readiness is decided by running a command in the guest, so this has exec's limitation: ask it by hand through crictl instead (see deploy/static-kubelet/README.md):

  id=$(crictl --runtime-endpoint %s ps -q --name %s --state Running)
  crictl --runtime-endpoint %s exec "$id" kontur ready -timeout 0

Under a real kubelet the pod's own readiness reports this: the rendered manifest gives the VM container a "kontur ready" readinessProbe, so "kubectl wait --for=condition=Ready" waits for the guest rather than for the container.`,
		staticpod.BackendDocker, name, backend, endpoint, name, endpoint)
}
