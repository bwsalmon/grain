// Package kontur drives the `konturctl` binary: creating, updating and
// deleting the sandbox VMs bwsalmon/kontur runs, plus the one `docker
// inspect` a caller needs to tell a VM whose container died from one
// still on its way up.
//
// It never reads kontur's own state files and never resolves an address
// for a VM, because nothing here needs one: a VM's guest is reached by
// exec'ing into its own container (mcp.DockerExecRunner), where the
// guest's address is already configured and directly reachable. This
// package used to carry a Port (kontur's state file) and a PodIP/
// DockerPodIP (crictl or `docker inspect`) for the SSH-to-a-forwarded-
// port transport that preceded it; all three existed only to describe a
// route in from outside the VM's network namespace, and went away with
// it.
//
// What is left is a deliberately shallow dependency on kontur: the
// `konturctl` subcommand names, the container names it derives from a VM
// name (PodName), and one `docker inspect` field. Never the `kontur`
// binary itself, which is a different program with a different job --
// the container-facing entrypoint that boots a single VM, sets up
// netshim networking, or execs into the guest (bwsalmon/kontur's own
// cmd/kontur/main.go doc comment: "distinct from cmd/konturctl, which is
// the operator-facing CLI"). This package still never needs to agree
// with kontur's own code on any Go type.
package kontur

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultStateDir matches bwsalmon/kontur's own default: internal/cli's
// defaultStateDir, which every "konturctl vm" subcommand's "-state-dir"
// flag defaults to.
const DefaultStateDir = "/var/lib/kontur/vms"

// DefaultCPUs and DefaultMemoryMB match bwsalmon/kontur's own default VM
// shape -- internal/staticpod.Defaults()'s own CPUs/MemoryMB, duplicated
// here for the same "read the shape, don't import the writer" reason
// BackendDocker and DefaultStateDir are. This is the shape `konturctl vm
// create` gives a VM whose caller passes neither -cpus nor -memory-mb,
// which is exactly what grain's own zero-means-unset SandboxCPUs/
// SandboxMemoryMB (model.Config, model.Task) leaves in place.
const (
	DefaultCPUs     = 2
	DefaultMemoryMB = 2048
)

// BackendDocker is the value `konturctl vm create -backend` takes to run a VM
// directly against a local docker daemon instead of writing a static pod
// manifest for a standalone kubelet to pick up -- bwsalmon/kontur's own
// internal/staticpod.BackendDocker, duplicated here for the same "read the
// shape, don't import the writer" reason PodName is (see the package doc
// comment). It needs neither `konturctl setup` nor containerd/CNI/kubelet
// on the host, which is why bwsalmon/agents#353 asked for grain's own
// sandboxes to run under it rather than the default static-pod backend.
const BackendDocker = "docker"

// NetModeFlat and NetModeNAT are the values `konturctl vm create -net`
// takes -- bwsalmon/kontur's own netshim.ModeFlat/ModeNAT, duplicated
// here for the same "read the shape, don't import the writer" reason
// BackendDocker and PodName are.
//
// NAT mode puts every guest on a private subnet inside its network
// namespace and shares that namespace's single IP between them with DNAT
// and masquerade rules, so each VM needs an address and an external port
// assigned to it. Flat mode splices one guest straight onto the segment
// the container runtime already put the namespace on, where it takes over
// the address and MAC assigned to it -- so nothing has to assign either,
// and the guest reaches the network as an ordinary container does.
//
// Flat mode also gives the guest a second NIC: it now answers to the
// namespace's own address, so anything inside that namespace dialing it
// would reach the local stack instead, and "kontur exec" (which is how
// this repo reaches a guest at all -- mcp.DockerExecRunner) goes over a
// private control link instead. The guest has to configure its end of
// that link, which kontur's own guest overlay does in its
// kontur-control-net service; a guest image not built from that overlay
// has no route in at all under this mode. scripts/kontur/build-guest.sh
// builds on it for exactly this reason.
const (
	NetModeFlat = "flat"
	NetModeNAT  = "nat"
)

// PodName returns the static pod name kontur gives a VM's manifest --
// bwsalmon/kontur's internal/staticpod.PodName, duplicated here rather
// than imported for the reason the package doc gives: this is a name
// kontur derives on its own, not one this package is free to choose
// independently of it.
func PodName(vmName string) string {
	return "kontur-vm-" + vmName
}

// Exists reports whether kontur has state for VM name under stateDir --
// i.e. whether `konturctl vm create` has ever succeeded for it. kontur
// writes one "<state-dir>/<name>.json" per VM (its own staticpod.Save)
// and removes it on delete, so the file's presence is the same signal a
// `konturctl vm list` would report, without this package having to agree
// with kontur on anything inside it.
func Exists(stateDir, name string) bool {
	_, err := os.Stat(filepath.Join(stateDir, name+".json"))
	return err == nil
}

// List is every VM name kontur has state for under stateDir, in
// whatever order the directory yields. It reads the same "<name>.json"
// files Exists probes for one at a time -- kontur's own staticpod.Save
// writes one per VM and removes it on delete -- so this is the same
// signal a `konturctl vm list` would report, without this package having
// to agree with kontur on anything inside them.
//
// A missing state directory is not an error: a deployment that has never
// created a VM has no directory yet, and "no VMs" is the right answer
// rather than a failure a caller has to special-case.
func List(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kontur: reading VM state directory %s: %w", stateDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, nil
}

// dockerInspect runs `docker inspect -f format name` and returns its
// trimmed stdout, folding stderr into the returned error the same way
// crictl above does.
func dockerInspect(ctx context.Context, format, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", format, name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kontur: docker inspect %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// DockerContainerStatus returns the docker State.Status (e.g. "running",
// "exited", "created", "dead") of vmName's own VM container under
// BackendDocker -- kontur.PodName(vmName), not the "-netns" holder
// DockerPodIP resolves. "konturctl vm create" under this backend starts
// that container with a plain "docker run -d" (bwsalmon/kontur's own
// internal/dockervm), which -- like any "docker run -d" -- reports
// success the moment the container starts, not once whatever it execs
// has proven itself alive: a guest that fails before finishing boot
// (a bad disk/kernel path, cloud-hypervisor itself crashing, ...) still
// makes kontur.Create return nil, and the container goes on to exit
// within seconds -- confirmed by hand against a real docker daemon with a
// deliberately broken disk path. KonturSandboxes.waitForGuestExec polls this
// so that case fails fast with the container's own status instead of
// silently waiting out the full ReadyTimeout dialing a port nothing will
// ever answer.
func DockerContainerStatus(ctx context.Context, vmName string) (string, error) {
	return dockerInspect(ctx, `{{.State.Status}}`, PodName(vmName))
}

// vm runs `konturctl vm <subcommand> name -state-dir stateDir
// <extraArgs...>`, the shape every `konturctl vm` subcommand shares
// (DefaultStateDir's own doc comment). It returns konturctl's combined
// stdout+stderr on failure, folded into the error, the same "let the
// caller see why" reasoning crictl above already uses.
func vm(ctx context.Context, subcommand, stateDir, name string, extraArgs ...string) error {
	args := append([]string{"vm", subcommand, name, "-state-dir", stateDir}, extraArgs...)
	cmd := exec.CommandContext(ctx, "konturctl", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kontur: vm %s %s: %w: %s", subcommand, name, err, strings.TrimSpace(output.String()))
	}
	return nil
}

// Create runs "konturctl vm create", bringing name up. extraArgs carries
// whatever else create needs beyond a name and -state-dir -- guest image,
// guest SSH port, resource sizing, and so on -- since this package has no
// way to know those without importing bwsalmon/kontur itself (see the
// package doc comment), and they are a deployment's own choice, not
// something Create can default sensibly on its own. Port(stateDir, name)
// is not valid to call until the create this starts has actually
// finished; callers that need the assigned port should call Create and
// then Port, not assume one implies the other is already true.
func Create(ctx context.Context, stateDir, name string, extraArgs ...string) error {
	return vm(ctx, "create", stateDir, name, extraArgs...)
}

// Delete runs "konturctl vm delete", tearing name down.
func Delete(ctx context.Context, stateDir, name string) error {
	return vm(ctx, "delete", stateDir, name)
}

// There is no Update. "konturctl vm update" had exactly one caller,
// orchestrator.KonturSandboxes.Reshape, which resized a slot's
// already-created VM to one task's own -cpus/-memory-mb (bwsalmon/
// agents#534) -- a partial update that existed only because the VM
// outlived the task that wanted it a different size. A sandbox is built
// for one run now, so its size is decided once, at create time
// (KonturConfig.createArgs), and there is nothing left to update.
