package hypervisor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newFakeAPIServer starts an HTTP server listening on a unix socket at a
// fresh path under t.TempDir, standing in for cloud-hypervisor's API.
func newFakeAPIServer(t *testing.T, handler http.Handler) (socketPath string) {
	t.Helper()
	socketPath = filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listening on %s: %v", socketPath, err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return socketPath
}

func TestAPIClient_PowerButton(t *testing.T) {
	var gotMethod, gotPath string
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.PowerButton(context.Background()); err != nil {
		t.Fatalf("PowerButton() error = %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/vm.power-button" {
		t.Errorf("got %s %s, want PUT /api/v1/vm.power-button", gotMethod, gotPath)
	}
}

func TestAPIClient_ShutdownVMM(t *testing.T) {
	var gotPath string
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.ShutdownVMM(context.Background()); err != nil {
		t.Fatalf("ShutdownVMM() error = %v", err)
	}
	if gotPath != "/api/v1/vmm.shutdown" {
		t.Errorf("got path %s, want /api/v1/vmm.shutdown", gotPath)
	}
}

func TestAPIClient_ErrorStatusReturnsError(t *testing.T) {
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not booted", http.StatusMethodNotAllowed)
	}))

	c := NewAPIClient(socket)
	if err := c.PowerButton(context.Background()); err == nil {
		t.Fatal("expected error for 405 response, got nil")
	}
}

func TestAPIClient_WaitReady(t *testing.T) {
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	c := NewAPIClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
}

func TestAPIClient_WaitReadyTimesOutWithoutServer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "no-such.sock")
	c := NewAPIClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx); err == nil {
		t.Fatal("expected WaitReady to time out when nothing is listening, got nil")
	}
}
