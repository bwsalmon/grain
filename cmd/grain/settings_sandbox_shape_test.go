package main

// The deployment-wide sandbox VM shape (ui.Settings.SandboxCPUs/
// SandboxMemoryMB, bwsalmon/agents#534) used to be reachable only from
// the Settings pane or a raw PUT /api/settings: `grain settings` had no
// flag for it, printed nothing about it, and `grain sync` applied a
// change to it while reporting "nothing changed". These cover the three
// halves of that gap -- set it, read it back, and see a sync say it
// moved -- against a real embedded store, the same discipline
// sync_test.go holds cmd/grain's own tests to.

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
)

// captureStdout runs fn with os.Stdout redirected and returns what it
// printed -- printer writes straight to stdout (it is a CLI's output,
// not a value anything returns), so reading it back is the only way to
// assert on what an operator actually sees.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()

	fn()

	w.Close()
	os.Stdout = old
	return <-done
}

// settingsTestServer starts a ui.Server over a real embedded SQLite
// store with settings already saved once, since the first-ever
// UpdateSettings demands the fields ui.Client.UpdateSettings's own doc
// comment lists and the sandbox shape is not among them.
func settingsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	srv := httptest.NewServer(ui.NewServer(ui.Config{Actor: ui.DefaultActor("operator"), Capabilities: ui.OfferedCapabilities()}, store))
	t.Cleanup(srv.Close)

	if _, err := ui.NewHTTPClient(srv.URL).UpdateSettings(context.Background(),
		*settingsRequest("30s", 2, "gemini-test", "claude-test", "github.example")); err != nil {
		t.Fatalf("seeding settings: %v", err)
	}
	return srv
}

func TestCmdSettingsSetsAndClearsTheSandboxShape(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, []string{"-sandbox-cpus", "4", "-sandbox-memory-mb", "8192", "-sandbox-disk-gb", "40"}); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.SandboxCPUs != 4 || settings.SandboxMemoryMB != 8192 || settings.SandboxDiskGB != 40 {
		t.Fatalf("sandbox shape = %d cpus/%d MiB/%d GiB, want 4/8192/40",
			settings.SandboxCPUs, settings.SandboxMemoryMB, settings.SandboxDiskGB)
	}

	// 0 is a real value here -- "leave bwsalmon/kontur's own default in
	// place" -- so a flag set to it must be applied rather than read as
	// an omitted flag, which is what the fs.Visit convention buys.
	captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, []string{"-sandbox-cpus", "0"}); err != nil {
			t.Errorf("cmdSettings clearing sandbox-cpus: %v", err)
		}
	})
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.SandboxCPUs != 0 {
		t.Errorf("SandboxCPUs after -sandbox-cpus=0 = %d, want 0", settings.SandboxCPUs)
	}
	if settings.SandboxMemoryMB != 8192 {
		t.Errorf("SandboxMemoryMB = %d, want the untouched 8192", settings.SandboxMemoryMB)
	}
	if settings.SandboxDiskGB != 40 {
		t.Errorf("SandboxDiskGB = %d, want the untouched 40", settings.SandboxDiskGB)
	}

	// Disk clears back to its own default the same way, which for disk
	// means "as large as the guest image behind the VM" rather than a
	// number kontur names.
	captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, []string{"-sandbox-disk-gb", "0"}); err != nil {
			t.Errorf("cmdSettings clearing sandbox-disk-gb: %v", err)
		}
	})
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.SandboxDiskGB != 0 {
		t.Errorf("SandboxDiskGB after -sandbox-disk-gb=0 = %d, want 0", settings.SandboxDiskGB)
	}
	if settings.SandboxMemoryMB != 8192 {
		t.Errorf("SandboxMemoryMB = %d, want the untouched 8192", settings.SandboxMemoryMB)
	}
}

func TestCmdSettingsPrintsTheSandboxShape(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	// Unset: what prints is the shape actually in effect (kontur's own
	// default, ui.Settings.SandboxCPUsDefault), not the stored 0.
	out := captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	// Disk is the exception: ui.Settings names no default for it (there
	// is no SandboxDiskGBDefault -- a VM's disk is however large its
	// guest image is), so an unset disk can only say that it is unset.
	for _, want := range []string{"sandbox cpus:", "sandbox memory mb:", "kontur default", "sandbox disk gb: unset"} {
		if !strings.Contains(out, want) {
			t.Errorf("`grain settings` printed %q, which does not contain %q", out, want)
		}
	}

	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		SandboxCPUs: intPtr(4), SandboxMemoryMB: intPtr(8192), SandboxDiskGB: intPtr(40),
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	out = captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	for _, want := range []string{"sandbox cpus:   4", "sandbox memory mb: 8192", "sandbox disk gb: 40"} {
		if !strings.Contains(out, want) {
			t.Errorf("`grain settings` printed %q, which does not contain %q", out, want)
		}
	}
	if strings.Contains(out, "kontur default") {
		t.Errorf("`grain settings` printed %q, which still calls a set shape a default", out)
	}
}

func TestPrintSettingsDiffReportsTheSandboxShape(t *testing.T) {
	before := ui.Settings{SandboxCPUs: 2, SandboxMemoryMB: 2048}
	after := ui.Settings{SandboxCPUs: 8, SandboxMemoryMB: 2048}

	out := captureStdout(t, func() { printSettingsDiff(before, after) })
	if !strings.Contains(out, "sandbox cpus") || !strings.Contains(out, "\"8\"") {
		t.Errorf("printSettingsDiff printed %q, want the sandbox cpus change", out)
	}
	if strings.Contains(out, "nothing changed") {
		t.Errorf("printSettingsDiff printed %q for a real change", out)
	}
	if strings.Contains(out, "sandbox memory mb") {
		t.Errorf("printSettingsDiff printed %q, which names a field that did not change", out)
	}

	out = captureStdout(t, func() { printSettingsDiff(before, before) })
	if !strings.Contains(out, "nothing changed") {
		t.Errorf("printSettingsDiff printed %q for an unchanged settings row, want \"nothing changed\"", out)
	}

	// Disk on its own, since it is the dimension a config file could
	// move with neither of the other two: before this row existed, such
	// a sync applied the change and then reported "nothing changed".
	disked := ui.Settings{SandboxCPUs: 2, SandboxMemoryMB: 2048, SandboxDiskGB: 40}
	out = captureStdout(t, func() { printSettingsDiff(before, disked) })
	if !strings.Contains(out, "sandbox disk gb") || !strings.Contains(out, "\"40\"") {
		t.Errorf("printSettingsDiff printed %q, want the sandbox disk gb change", out)
	}
	if strings.Contains(out, "nothing changed") {
		t.Errorf("printSettingsDiff printed %q for a real disk change", out)
	}
}

func TestSandboxShapeValue(t *testing.T) {
	if got := sandboxShapeValue(4, 2); got != "4" {
		t.Errorf("sandboxShapeValue(4, 2) = %q, want %q", got, "4")
	}
	if got := sandboxShapeValue(0, 2); !strings.Contains(got, "2") || !strings.Contains(got, "default") {
		t.Errorf("sandboxShapeValue(0, 2) = %q, want kontur's own default named", got)
	}
	if got := sandboxShapeValue(0, 0); got != "unset" {
		t.Errorf("sandboxShapeValue(0, 0) = %q, want %q", got, "unset")
	}
}

func intPtr(v int) *int { return &v }
