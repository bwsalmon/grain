// Package kontur resolves how to reach a sandbox VM that bwsalmon/kontur
// is running under a static kubelet (see that repo's top-level README and
// deploy/static-kubelet/README.md) -- the two pieces of information
// mcp.SSHRunner needs that neither `kontur vm list` nor `kontur vm create`
// prints on its own:
//
//   - The external port netshim forwards to the VM's guest, which kontur
//     itself persists per VM. Port reads it straight out of kontur's own
//     state file rather than importing bwsalmon/kontur as a Go module --
//     the same "read the shape, don't import the writer" choice
//     v2/pkg/secrets makes for a Kubernetes Secret volume mount, and one
//     this package needs for the same reason: pulling in kontur's module
//     graph (containerd, cloud-hypervisor's own client, ...) to read one
//     integer out of a JSON file it already writes to a well-known path
//     would be a strange trade.
//   - The pod IP that port actually answers on. kontur never records this
//     at all -- there is no apiserver for a static pod to report its
//     assigned address back to, which is the entire premise of running a
//     kubelet standalone -- so PodIP asks containerd directly, via
//     crictl, the exact tool deploy/static-kubelet/README.md already
//     points an operator at by hand ("crictl ... is the standalone
//     equivalent for inspecting pods/containers").
//
// Create and Delete, unlike Port and PodIP, do not read kontur's own state
// -- they just run the `kontur` binary itself ("kontur vm create"/"kontur
// vm delete"), the same command an operator would type by hand. That is a
// different, much shallower kind of dependency than importing
// bwsalmon/kontur as a Go module would be (see above): this package still
// never needs to agree with kontur's own code on any Go type, only on the
// two subcommand names and the state file Port and PodIP already read.
package kontur

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultStateDir matches bwsalmon/kontur's own default: internal/cli's
// defaultStateDir, which every "kontur vm" subcommand's "-state-dir" flag
// defaults to.
const DefaultStateDir = "/var/lib/kontur/vms"

// DefaultRuntimeEndpoint matches the containerd CRI socket
// deploy/static-kubelet/containerd-config.toml and kubelet-config.yaml
// both point at on a node kontur set up. Only meaningful for
// BackendStaticPod (below); the docker backend has no CRI to ask.
const DefaultRuntimeEndpoint = "unix:///run/containerd/containerd.sock"

// BackendDocker is the value `kontur vm create -backend` takes to run a VM
// directly against a local docker daemon instead of writing a static pod
// manifest for a standalone kubelet to pick up -- bwsalmon/kontur's own
// internal/staticpod.BackendDocker, duplicated here for the same "read the
// shape, don't import the writer" reason PodName is (see the package doc
// comment). It needs neither `konturctl setup` nor containerd/CNI/kubelet
// on the host, which is why bwsalmon/agents#353 asked for grain's own
// sandboxes to run under it rather than the default static-pod backend.
const BackendDocker = "docker"

// PodName returns the static pod name kontur gives a VM's manifest --
// bwsalmon/kontur's internal/staticpod.PodName, duplicated here rather
// than imported for the reason the package doc gives: this is a name
// kontur derives on its own, not one this package is free to choose
// independently of it.
func PodName(vmName string) string {
	return "kontur-vm-" + vmName
}

// vmState is the subset of bwsalmon/kontur's internal/staticpod.VMSpec
// this package needs -- just the one field, out of the one
// "<state-dir>/<name>.json" file kontur's own staticpod.Save/Load already
// read and write, that PodIP's crictl lookup can't recover on its own.
type vmState struct {
	Port int `json:"port"`
}

// Port reads the external port kontur assigned VM name at "kontur vm
// create"/"update" time, out of stateDir (see DefaultStateDir). This is
// the port SSHRunner should connect to -- guestPort (also in the state
// file) is only meaningful inside the VM's own network namespace.
func Port(stateDir, name string) (int, error) {
	path := filepath.Join(stateDir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("kontur: reading %s: %w", path, err)
	}
	var s vmState
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, fmt.Errorf("kontur: parsing %s: %w", path, err)
	}
	if s.Port < 1 || s.Port > 65535 {
		return 0, fmt.Errorf("kontur: %s has no valid port", path)
	}
	return s.Port, nil
}

// crictl runs `crictl -runtime-endpoint runtimeEndpoint <args...>` and
// returns its stdout, wrapping a non-zero exit (crictl's own error, e.g.
// "not found", ends up on stderr) into the returned error so PodIP's
// caller sees why the lookup failed rather than just that it did.
func crictl(ctx context.Context, runtimeEndpoint string, args ...string) ([]byte, error) {
	full := append([]string{"--runtime-endpoint", runtimeEndpoint}, args...)
	cmd := exec.CommandContext(ctx, "crictl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kontur: crictl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// PodIP resolves the CNI-assigned IP address of the static pod backing VM
// vmName, via two crictl calls -- `crictl pods` to turn PodName(vmName)
// into a sandbox ID (a pod's name isn't itself something `inspectp` takes)
// then `crictl inspectp` for that ID's network status -- the same two
// steps deploy/static-kubelet/README.md's own "crictl ps -a" / "crictl
// logs <container-id>" walkthrough takes to go from a pod's name to its
// detail. This is the address netshim's DNAT rule (inside the pod's own
// network namespace) forwards Port to the guest on; it changes on every
// pod recreate, so nothing caches it across calls.
func PodIP(ctx context.Context, runtimeEndpoint, vmName string) (string, error) {
	podName := PodName(vmName)
	out, err := crictl(ctx, runtimeEndpoint, "pods", "--name", podName, "--state", "Ready", "-o", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("kontur: parsing crictl pods output for %q: %w", podName, err)
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("kontur: no ready pod named %q -- is the VM up?", podName)
	}

	out, err = crictl(ctx, runtimeEndpoint, "inspectp", "-o", "json", list.Items[0].ID)
	if err != nil {
		return "", err
	}
	var status struct {
		Status struct {
			Network struct {
				IP string `json:"ip"`
			} `json:"network"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("kontur: parsing crictl inspectp output for %q: %w", podName, err)
	}
	if status.Status.Network.IP == "" {
		return "", fmt.Errorf("kontur: pod %q has no network IP yet", podName)
	}
	return status.Status.Network.IP, nil
}

// dockerNetnsContainerName returns the name of the otherwise-idle
// container bwsalmon/kontur's docker backend (internal/dockervm) starts
// per VM purely to hold open the network namespace netshim and the VM
// container share -- see that package's own doc comment for why a plain
// docker container needs one at all. Duplicated here for the same reason
// PodName is: this package reads kontur's own naming convention rather
// than importing it.
func dockerNetnsContainerName(vmName string) string {
	return PodName(vmName) + "-netns"
}

// dockerIPFormat is a docker inspect Go template that prefers
// NetworkSettings.IPAddress (populated on docker's default bridge
// network) and falls back to the first network under
// NetworkSettings.Networks (where IPAddress is always empty instead) --
// covering a container attached to a named network, e.g. via `docker run
// --network`.
const dockerIPFormat = `{{if .NetworkSettings.IPAddress}}{{.NetworkSettings.IPAddress}}{{else}}{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}{{end}}`

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

// DockerPodIP resolves the address netshim's DNAT rules listen on for a VM
// created with `kontur vm create -backend docker` (BackendDocker): the
// docker-assigned address of that VM's network-namespace-holder container
// (dockerNetnsContainerName) -- what internal/netshim/setup.go's
// ensurePortForward calls "the pod IP" when there is no real Kubernetes
// pod to ask. The docker backend has no CRI for crictl to ask the way
// PodIP (above) does, which is why this is a separate function rather
// than a branch inside that one; callers building a docker-backed
// KonturSandboxes call this instead of PodIP.
func DockerPodIP(ctx context.Context, vmName string) (string, error) {
	name := dockerNetnsContainerName(vmName)
	ip, err := dockerInspect(ctx, dockerIPFormat, name)
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", fmt.Errorf("kontur: container %q has no network IP yet -- is the VM up?", name)
	}
	return ip, nil
}

// vm runs `kontur vm <subcommand> name -state-dir stateDir <extraArgs...>`,
// the shape every `kontur vm` subcommand shares (DefaultStateDir's own doc
// comment). It returns kontur's combined stdout+stderr on failure, folded
// into the error, the same "let the caller see why" reasoning crictl above
// already uses.
func vm(ctx context.Context, subcommand, stateDir, name string, extraArgs ...string) error {
	args := append([]string{"vm", subcommand, name, "-state-dir", stateDir}, extraArgs...)
	cmd := exec.CommandContext(ctx, "kontur", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kontur: vm %s %s: %w: %s", subcommand, name, err, strings.TrimSpace(output.String()))
	}
	return nil
}

// Create runs "kontur vm create", bringing name up. extraArgs carries
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

// Delete runs "kontur vm delete", tearing name down.
func Delete(ctx context.Context, stateDir, name string) error {
	return vm(ctx, "delete", stateDir, name)
}
