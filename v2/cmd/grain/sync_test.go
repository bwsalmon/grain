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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
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
	maxConcurrent := 2
	geminiModel := "gemini-test"
	claudeModel := "claude-test"
	githubHost := "github.example"

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	db, err := sqlite.Open(sqlite.DefaultConfig(dataDir))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	srv := httptest.NewServer(ui.NewServer(ui.Config{Actor: ui.DefaultActor("operator"), Capabilities: ui.DefaultCapabilities()}, store))
	defer srv.Close()

	path := writeSyncConfig(t, dir, syncConfig{Settings: settingsRequest(pollInterval, maxConcurrent, geminiModel, claudeModel, githubHost)})

	if err := cmdSync(context.Background(), []string{"-config", path, "-server", srv.URL}); err != nil {
		t.Fatalf("cmdSync: %v", err)
	}

	// A second sync against the same, now-already-applied settings must
	// succeed too -- the "safe to re-run, nothing to do if nothing
	// changed" bar this file's own doc comment sets.
	if err := cmdSync(context.Background(), []string{"-config", path, "-server", srv.URL}); err != nil {
		t.Fatalf("cmdSync (second run): %v", err)
	}

	settings, err := ui.NewHTTPClient(srv.URL).GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.PollInterval != pollInterval || settings.GeminiModel != geminiModel ||
		settings.ClaudeModel != claudeModel || settings.GitHubHost != githubHost {
		t.Fatalf("settings = %+v, want poll interval %q, gemini model %q, claude model %q, github host %q",
			settings, pollInterval, geminiModel, claudeModel, githubHost)
	}
	if settings.MaxConcurrent != 2 {
		t.Fatalf("settings.MaxConcurrent = %v, want 2", settings.MaxConcurrent)
	}
}

// settingsRequest builds a syncConfig's own settings section with every
// field the first-ever UpdateSettings call requires
// (ui.Client.UpdateSettings's own doc comment) filled in.
func settingsRequest(pollInterval string, maxConcurrent int, geminiModel, claudeModel, githubHost string) *ui.UpdateSettingsRequest {
	return &ui.UpdateSettingsRequest{
		PollInterval:  &pollInterval,
		MaxConcurrent: &maxConcurrent,
		GeminiModel:   &geminiModel,
		ClaudeModel:   &claudeModel,
		GitHubHost:    &githubHost,
	}
}
