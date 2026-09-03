package ui_test

// bwsalmon/agents#536: the sandbox health pane's own API surface -- the
// same enabled-flag conventions logs_test.go's own tests already
// exercise for the debug section's other pane.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
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
		{Backend: "kontur", Name: "grain-0", Ready: true, LoadAverage: "0.1 0.2 0.3", MemoryUsedMB: 100, MemoryTotalMB: 200},
		{Backend: "kontur", Name: "grain-1", Error: "unreachable"},
	}}
	client.Config.HostStats = func() (ui.HostPressure, error) {
		return ui.HostPressure{
			LoadAverage1: 1.5, MemoryUsedMB: 512, MemoryTotalMB: 1024,
			// One figure per filesystem the daemon's state sits on, not
			// one "disk" figure -- the store's small volume and the much
			// larger one sandboxes were given (grain/task-148).
			Disks: []ui.DiskUsage{
				{Holds: []string{"store"}, Path: "/var/lib/grain", UsedMB: 4096, TotalMB: 20480},
				{Holds: []string{"sandboxes", "docker"}, Path: "/var/lib/grain-sandbox", UsedMB: 61440, TotalMB: 102400},
			},
		}, nil
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
	if len(got.Host.Disks) != 2 {
		t.Fatalf("host.Disks = %v, want one entry per filesystem", got.Host.Disks)
	}
	// The sandbox volume, which is the one a runaway build fills and the
	// one this pane showed no figure for at all before grain/task-148.
	sandboxVolume := got.Host.Disks[1]
	if sandboxVolume.TotalMB != 102400 || sandboxVolume.UsedMB != 61440 {
		t.Errorf("host.Disks[1] usage = %d/%d MB, want 61440/102400", sandboxVolume.UsedMB, sandboxVolume.TotalMB)
	}
	// Two names on one row: the daemon found the sandbox root and
	// docker's data root to be the same filesystem and said so once.
	if strings.Join(sandboxVolume.Holds, ",") != "sandboxes,docker" {
		t.Errorf("host.Disks[1].Holds = %v, want everything that shares that filesystem", sandboxVolume.Holds)
	}
}

func TestGetSandboxHealthReportsHostErrorWithoutHidingSandboxes(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Sandboxes = fakeSandboxHealth{snapshots: []ui.SandboxSnapshot{{Backend: "host", Name: "/tmp/sandbox-0", Ready: true}}}
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
