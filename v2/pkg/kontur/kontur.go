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
// both point at on a node kontur set up.
const DefaultRuntimeEndpoint = "unix:///run/containerd/containerd.sock"

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
