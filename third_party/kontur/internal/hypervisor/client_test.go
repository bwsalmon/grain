package hypervisor

import (
	"context"
	"io"
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

func TestAPIClient_PauseAndResume(t *testing.T) {
	var gotMethods, gotPaths []string
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method)
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if err := c.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	wantPaths := []string{"/api/v1/vm.pause", "/api/v1/vm.resume"}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("got paths %v, want %v", gotPaths, wantPaths)
	}
	for i, want := range wantPaths {
		if gotPaths[i] != want || gotMethods[i] != http.MethodPut {
			t.Errorf("request %d = %s %s, want PUT %s", i, gotMethods[i], gotPaths[i], want)
		}
	}
}

func TestAPIClient_Snapshot(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.Snapshot(context.Background(), "/var/lib/kontur/snapshot"); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/vm.snapshot" {
		t.Errorf("got %s %s, want PUT /api/v1/vm.snapshot", gotMethod, gotPath)
	}
	if want := `{"destination_url":"file:///var/lib/kontur/snapshot"}`; gotBody != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestAPIClient_Resize(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody []byte
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.Resize(context.Background(), 2*1024*1024*1024); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/vm.resize" {
		t.Errorf("got %s %s, want PUT /api/v1/vm.resize", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	wantBody := `{"desired_ram":2147483648}`
	if string(gotBody) != wantBody {
		t.Errorf("body = %s, want %s", gotBody, wantBody)
	}
}

func TestAPIClient_ResizeCPUs(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody []byte
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewAPIClient(socket)
	if err := c.ResizeCPUs(context.Background(), 4); err != nil {
		t.Fatalf("ResizeCPUs() error = %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/vm.resize" {
		t.Errorf("got %s %s, want PUT /api/v1/vm.resize", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	wantBody := `{"desired_vcpus":4}`
	if string(gotBody) != wantBody {
		t.Errorf("body = %s, want %s", gotBody, wantBody)
	}
}

func TestAPIClient_ResizeCPUsSurfacesPendingRemovalError(t *testing.T) {
	socket := newFakeAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "a cpu removal is still pending", http.StatusTooManyRequests)
	}))

	c := NewAPIClient(socket)
	if err := c.ResizeCPUs(context.Background(), 4); err == nil {
		t.Fatal("expected error for 429 response, got nil")
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
