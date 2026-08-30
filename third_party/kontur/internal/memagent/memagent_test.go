package memagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeResizer records every Resize call it receives instead of talking
// to a real cloud-hypervisor API socket.
type fakeResizer struct {
	mu    sync.Mutex
	calls []uint64
	err   error
}

func (f *fakeResizer) Resize(_ context.Context, desiredRAMBytes uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, desiredRAMBytes)
	return nil
}

func (f *fakeResizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeResizer) lastCall() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return 0
	}
	return f.calls[len(f.calls)-1]
}

// startServer starts a Server on an ephemeral loopback port and returns
// its address plus a cancel func that stops it.
func startServer(t *testing.T, cfg Config, api Resizer) (addr string, cancel context.CancelFunc) {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Addr)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr = ln.Addr().String()
	ln.Close()
	cfg.Addr = addr

	s := New(cfg, api)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	// Wait for the listener to actually be accepting before returning.
	waitForListener(t, addr)

	t.Cleanup(func() {
		cancel()
		<-done
	})
	return addr, cancel
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never started accepting connections", addr)
}

func signal(t *testing.T, addr, line string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		t.Fatalf("writing signal: %v", err)
	}
}

func waitForCallCount(t *testing.T, api *fakeResizer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if api.callCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Resize called %d time(s), want at least %d", api.callCount(), want)
}

func TestServer_GrowsOnPressureSignal(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 42.00")
	waitForCallCount(t, api, 1)

	wantBytes := uint64(512) * 1024 * 1024
	if got := api.lastCall(); got != wantBytes {
		t.Errorf("Resize called with %d bytes, want %d (256+256 MiB)", got, wantBytes)
	}
}

func TestServer_CapsGrowthAtMaxMB(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 1900, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 99.99")
	waitForCallCount(t, api, 1)

	wantBytes := uint64(2048) * 1024 * 1024
	if got := api.lastCall(); got != wantBytes {
		t.Errorf("Resize called with %d bytes, want %d (capped at 2048 MiB)", got, wantBytes)
	}
}

func TestServer_IgnoresSignalAlreadyAtCeiling(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 2048, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 1.00")
	// Give the (non-)call a moment to (not) happen, then confirm it never
	// did -- there's no success signal to wait on for a call that's
	// expected not to occur.
	time.Sleep(200 * time.Millisecond)
	if got := api.callCount(); got != 0 {
		t.Errorf("Resize called %d time(s), want 0 when already at ceiling", got)
	}
}

func TestServer_RespectsCooldown(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 10.00")
	waitForCallCount(t, api, 1)

	signal(t, addr, "PRESSURE 10.00")
	time.Sleep(200 * time.Millisecond)
	if got := api.callCount(); got != 1 {
		t.Errorf("Resize called %d time(s), want exactly 1 within the cooldown window", got)
	}
}

func TestServer_GrowsAgainAfterCooldownElapses(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: 50 * time.Millisecond}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 10.00")
	waitForCallCount(t, api, 1)

	time.Sleep(100 * time.Millisecond)
	signal(t, addr, "PRESSURE 10.00")
	waitForCallCount(t, api, 2)

	wantBytes := uint64(768) * 1024 * 1024
	if got := api.lastCall(); got != wantBytes {
		t.Errorf("Resize called with %d bytes, want %d (256+256+256 MiB)", got, wantBytes)
	}
}

func TestServer_ResizeErrorDoesNotAdvanceState(t *testing.T) {
	api := &fakeResizer{err: errors.New("cloud-hypervisor rejected the resize")}
	cfg := Config{StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: 10 * time.Millisecond}
	addr, _ := startServer(t, cfg, api)

	signal(t, addr, "PRESSURE 10.00")
	time.Sleep(200 * time.Millisecond)

	api.mu.Lock()
	api.err = nil
	api.mu.Unlock()

	signal(t, addr, "PRESSURE 10.00")
	waitForCallCount(t, api, 1)

	wantBytes := uint64(512) * 1024 * 1024
	if got := api.lastCall(); got != wantBytes {
		t.Errorf("Resize called with %d bytes, want %d: a failed resize should not have advanced the current-size bookkeeping", got, wantBytes)
	}
}

func TestServer_IgnoresBlankConnections(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}
	addr, _ := startServer(t, cfg, api)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	conn.Close()

	time.Sleep(200 * time.Millisecond)
	if got := api.callCount(); got != 0 {
		t.Errorf("Resize called %d time(s), want 0 for a connection that sent nothing", got)
	}
}

func TestServer_StopsOnContextCancellation(t *testing.T) {
	api := &fakeResizer{}
	cfg := Config{Addr: "127.0.0.1:0", StartMB: 256, MaxMB: 2048, StepMB: 256, Cooldown: time.Hour}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Addr)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.Addr = addr

	s := New(cfg, api)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	waitForListener(t, addr)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() error = %v, want nil after context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}
