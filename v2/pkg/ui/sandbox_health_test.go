package ui_test

// bwsalmon/agents#536: the sandbox health pane's own API surface -- the
// same enabled-flag conventions logs_test.go's own tests already
// exercise for the debug section's other pane.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

type fakeSandboxHealth struct {
	snapshots []ui.SandboxSnapshot
}

func (f fakeSandboxHealth) Health(ctx context.Context) []ui.SandboxSnapshot { return f.snapshots }

func TestGetSandboxHealthReportsUnavailableWithNeitherConfigured(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/sandboxes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestGetSandboxHealthReportsEverySandboxAndTheHost(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Sandboxes = fakeSandboxHealth{snapshots: []ui.SandboxSnapshot{
		{Slot: "0", Backend: "kontur", Name: "grain-0", Ready: true, LoadAverage: "0.1 0.2 0.3", MemoryUsedMB: 100, MemoryTotalMB: 200},
		{Slot: "1", Backend: "kontur", Name: "grain-1", Error: "unreachable"},
	}}
	client.Config.HostStats = func() (ui.HostPressure, error) {
		return ui.HostPressure{LoadAverage1: 1.5, MemoryUsedMB: 512, MemoryTotalMB: 1024}, nil
	}

	rec := do(t, srv, http.MethodGet, "/api/sandboxes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[struct {
		Enabled   bool                 `json:"enabled"`
		Sandboxes []ui.SandboxSnapshot `json:"sandboxes"`
		Host      *ui.HostPressure     `json:"host"`
	}](t, rec)

	if !got.Enabled {
		t.Fatal("enabled = false, want true")
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("sandboxes = %v, want 2 entries", got.Sandboxes)
	}
	if got.Sandboxes[1].Error != "unreachable" {
		t.Errorf("sandboxes[1].Error = %q, want %q", got.Sandboxes[1].Error, "unreachable")
	}
	if got.Host == nil || got.Host.MemoryTotalMB != 1024 {
		t.Fatalf("host = %v, want MemoryTotalMB 1024", got.Host)
	}
}

func TestGetSandboxHealthReportsHostErrorWithoutHidingSandboxes(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Sandboxes = fakeSandboxHealth{snapshots: []ui.SandboxSnapshot{{Slot: "0", Backend: "host", Name: "/tmp/sandbox-0", Ready: true}}}
	client.Config.HostStats = func() (ui.HostPressure, error) { return ui.HostPressure{}, errors.New("not on linux") }

	rec := do(t, srv, http.MethodGet, "/api/sandboxes", "")
	got := decode[struct {
		Enabled   bool                 `json:"enabled"`
		Sandboxes []ui.SandboxSnapshot `json:"sandboxes"`
		Host      *ui.HostPressure     `json:"host"`
		HostError string               `json:"hostError"`
	}](t, rec)

	if !got.Enabled {
		t.Fatal("enabled = false, want true")
	}
	if len(got.Sandboxes) != 1 {
		t.Fatalf("sandboxes = %v, want 1 entry", got.Sandboxes)
	}
	if got.Host != nil {
		t.Errorf("host = %v, want nil", got.Host)
	}
	if got.HostError != "not on linux" {
		t.Errorf("hostError = %q, want %q", got.HostError, "not on linux")
	}
}
