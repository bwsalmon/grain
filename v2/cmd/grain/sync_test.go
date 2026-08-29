package main

// sync_test.go exercises cmdSync's "settings" path end to end against a
// real embedded SQLite store -- the same "nothing here is a fake standing
// in for the store" discipline demo_test.go already holds cmd/grain's
// own tests to. The "gcp" path is pkg/gcpsetup's own, covered network-
// free by gcpsetup_test.go; syncGCP itself is exercised for its
// validation only, the same boundary setup_test.go draws for
// cmdSetupGCP.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func writeSyncConfig(t *testing.T, dir string, cfg syncConfig) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sync.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCmdSyncRequiresConfig(t *testing.T) {
	if err := cmdSync(context.Background(), nil); err == nil {
		t.Fatal("expected an error with no -config")
	}
}

func TestCmdSyncRejectsAConfigWithNeitherSection(t *testing.T) {
	dir := t.TempDir()
	path := writeSyncConfig(t, dir, syncConfig{})
	err := cmdSync(context.Background(), []string{"-config", path})
	if err == nil || !strings.Contains(err.Error(), "nothing to sync") {
		t.Fatalf("cmdSync() = %v, want a \"nothing to sync\" error", err)
	}
}

func TestCmdSyncGCPSectionRequiresProject(t *testing.T) {
	dir := t.TempDir()
	path := writeSyncConfig(t, dir, syncConfig{GCP: &syncGCPConfig{}})
	err := cmdSync(context.Background(), []string{"-config", path})
	if err == nil || !strings.Contains(err.Error(), "gcp.project") {
		t.Fatalf("cmdSync() = %v, want a gcp.project error", err)
	}
}

func TestCmdSyncAppliesSettingsAgainstAnEmbeddedStore(t *testing.T) {
	pollInterval := "45s"
	slots := []string{"a", "b"}
	geminiModel := "gemini-test"
	githubHost := "github.example"

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	path := writeSyncConfig(t, dir, syncConfig{Settings: settingsRequest(pollInterval, slots, geminiModel, githubHost)})

	if err := cmdSync(context.Background(), []string{"-config", path, "-data-dir", dataDir}); err != nil {
		t.Fatalf("cmdSync: %v", err)
	}

	// A second sync against the same, now-already-applied settings must
	// succeed too -- the "safe to re-run, nothing to do if nothing
	// changed" bar this file's own doc comment sets.
	if err := cmdSync(context.Background(), []string{"-config", path, "-data-dir", dataDir}); err != nil {
		t.Fatalf("cmdSync (second run): %v", err)
	}

	c, closeStore, err := buildClient(dataDir, "", "")
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	defer closeStore()
	settings, err := c.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.PollInterval != pollInterval || settings.GeminiModel != geminiModel || settings.GitHubHost != githubHost {
		t.Fatalf("settings = %+v, want poll interval %q, gemini model %q, github host %q",
			settings, pollInterval, geminiModel, githubHost)
	}
	if len(settings.Slots) != 2 || settings.Slots[0] != "a" || settings.Slots[1] != "b" {
		t.Fatalf("settings.Slots = %v, want [a b]", settings.Slots)
	}
}

// settingsRequest builds a syncConfig's own settings section with every
// field the first-ever UpdateSettings call requires
// (ui.Client.UpdateSettings's own doc comment) filled in.
func settingsRequest(pollInterval string, slots []string, geminiModel, githubHost string) *ui.UpdateSettingsRequest {
	return &ui.UpdateSettingsRequest{
		PollInterval: &pollInterval,
		Slots:        &slots,
		GeminiModel:  &geminiModel,
		GitHubHost:   &githubHost,
	}
}
