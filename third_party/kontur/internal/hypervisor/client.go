package hypervisor

import (
	"context"
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
	return c.do(ctx, http.MethodPut, path)
}

func (c *APIClient) get(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path)
}

func (c *APIClient) do(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, nil)
	if err != nil {
		return err
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
