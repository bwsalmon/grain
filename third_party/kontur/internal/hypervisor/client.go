package hypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// APIClient talks to the cloud-hypervisor HTTP API exposed over the local
// unix socket given to --api-socket.
type APIClient struct {
	httpClient *http.Client
}

// NewAPIClient returns a client bound to the unix socket at socketPath. The
// socket does not need to exist yet: dialing is deferred to the first
// request.
func NewAPIClient(socketPath string) *APIClient {
	dialer := net.Dialer{}
	return &APIClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// PowerButton simulates an ACPI power button press in the guest, asking it
// to shut down cleanly. cloud-hypervisor exits on its own once the guest
// powers off (unless started with --no-shutdown, which this runtime never
// sets).
func (c *APIClient) PowerButton(ctx context.Context) error {
	return c.put(ctx, "/api/v1/vm.power-button")
}

// ShutdownVMM forcefully stops the VMM process without waiting on the
// guest. Used as a fallback when the guest does not respond to the power
// button in time.
func (c *APIClient) ShutdownVMM(ctx context.Context) error {
	return c.put(ctx, "/api/v1/vmm.shutdown")
}

// Pause suspends every vCPU. cloud-hypervisor requires this before
// Snapshot, and it's also the "suspend" half of Runner.Suspend.
func (c *APIClient) Pause(ctx context.Context) error {
	return c.put(ctx, "/api/v1/vm.pause")
}

// Resume unpauses a VM previously paused by Pause, or restored (with
// resume left off) from a snapshot via BuildArgs's "--restore".
func (c *APIClient) Resume(ctx context.Context) error {
	return c.put(ctx, "/api/v1/vm.resume")
}

// Snapshot writes a full snapshot of the VM's state -- enough to restore
// an identical VM later via "--restore", see BuildArgs -- to destDir.
// The VM must already be paused (see Pause), and cloud-hypervisor
// requires destDir to already exist.
func (c *APIClient) Snapshot(ctx context.Context, destDir string) error {
	return c.putJSON(ctx, "/api/v1/vm.snapshot", struct {
		DestinationURL string `json:"destination_url"`
	}{DestinationURL: "file://" + destDir})
}

// Resize asks cloud-hypervisor to live-resize the guest's RAM to
// desiredRAMBytes, via the memory hotplug device configured at boot (see
// internal/config's CHV_MEMORY_HOTPLUG/CHV_MEMORY_MAX_MB and
// hypervisor.BuildArgs). desiredRAMBytes must fall between the VM's
// starting size (CHV_MEMORY_MB) and its ceiling (CHV_MEMORY_MAX_MB);
// cloud-hypervisor rejects the request otherwise, or if hotplug wasn't
// enabled at boot at all. The resize itself is asynchronous from the
// guest's point of view -- a virtio-mem-aware kernel picks up the change
// on its own, with no guarantee of exactly when. See the README's
// "Memory hotplug" section for how this interacts with Snapshot/suspend.
func (c *APIClient) Resize(ctx context.Context, desiredRAMBytes uint64) error {
	return c.putJSON(ctx, "/api/v1/vm.resize", struct {
		DesiredRAM uint64 `json:"desired_ram"`
	}{DesiredRAM: desiredRAMBytes})
}

// ResizeCPUs asks cloud-hypervisor to live-resize the guest's vCPU count
// to desiredVCPUs, via the ACPI CPU hotplug headroom configured at boot
// (see internal/config's CHV_CPUS_MAX and hypervisor.BuildArgs/cpusArg).
// desiredVCPUs must fall between 1 and the VM's ceiling (CHV_CPUS_MAX,
// which defaults to CHV_CPUS itself, i.e. no headroom); cloud-hypervisor
// rejects the request otherwise.
//
// Both directions are asynchronous from the guest's point of view, same
// as memory hotplug -- but unlike virtio-mem, newly added vCPUs are not
// auto-onlined: a guest kernel must online them itself (e.g. `echo 1 >
// /sys/devices/system/cpu/cpuN/online`) before it will actually use
// them. Removal is guest-driven too (no online/offline command needed),
// but only takes effect once the guest acknowledges the ACPI eject; a
// second resize call made before that finishes is rejected by
// cloud-hypervisor with 429 ("a cpu removal is still pending"), which
// surfaces here as a plain error -- wait for one shrink to complete
// rather than retrying blindly. See the README's "CPU hotplug" section
// for how this interacts with Snapshot/suspend.
func (c *APIClient) ResizeCPUs(ctx context.Context, desiredVCPUs uint32) error {
	return c.putJSON(ctx, "/api/v1/vm.resize", struct {
		DesiredVCPUs uint32 `json:"desired_vcpus"`
	}{DesiredVCPUs: desiredVCPUs})
}

// WaitReady polls the API socket until it accepts connections or ctx is
// done. cloud-hypervisor creates the socket very early in startup, but the
// runtime may race it right after Start returns.
func (c *APIClient) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := c.get(ctx, "/api/v1/vmm.ping"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *APIClient) put(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodPut, path, nil)
}

func (c *APIClient) putJSON(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request body for %s %s: %w", http.MethodPut, path, err)
	}
	return c.do(ctx, http.MethodPut, path, bytes.NewReader(b))
}

func (c *APIClient) get(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *APIClient) do(ctx context.Context, method, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cloud-hypervisor api %s %s: %s: %s", method, path, resp.Status, string(body))
	}
	return nil
}
