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

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
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
	maxWorkers := 2
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
	srv := httptest.NewServer(ui.NewServer(ui.Config{Actor: ui.DefaultActor("operator"), Capabilities: ui.OfferedCapabilities()}, store))
	defer srv.Close()

	path := writeSyncConfig(t, dir, syncConfig{Settings: settingsRequest(pollInterval, maxWorkers, geminiModel, claudeModel, githubHost)})

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
	if settings.MaxWorkers != 2 {
		t.Fatalf("settings.MaxWorkers = %v, want 2", settings.MaxWorkers)
	}
}

// settingsRequest builds a syncConfig's own settings section with every
// field the first-ever UpdateSettings call requires
// (ui.Client.UpdateSettings's own doc comment) filled in.
func settingsRequest(pollInterval string, maxWorkers int, geminiModel, claudeModel, githubHost string) *ui.UpdateSettingsRequest {
	return &ui.UpdateSettingsRequest{
		PollInterval:  &pollInterval,
		MaxWorkers:    &maxWorkers,
		GeminiModel:   &geminiModel,
		ClaudeModel:   &claudeModel,
		GitHubHost:    &githubHost,
	}
}

// printSettingsDiff is the whole of what a sync run reports, and its one
// failure mode is silence: a setting the config file really changed, left
// out of the table, prints as "nothing changed" against a deployment that
// just changed. That has happened once already (the sandbox VM shape),
// so this walks every field a "settings" section can set and insists the
// diff names it.
func TestPrintSettingsDiffNamesEverySettingASyncCanChange(t *testing.T) {
	before := ui.Settings{
		EnvironmentName: "staging", PollInterval: "30s", MaxWorkers: 1, MaxMergers: 1,
		GeminiModel: "gemini-old", ClaudeModel: "claude-old", MaxAgentTurns: 40,
		GitHubHost: "github.com", GCPProject: "grain-old", GCPServiceAccountEmail: "old@example.com",
		SandboxCPUs: 2, SandboxMemoryMB: 2048, SandboxDiskGB: 20,
		TargetRepos: []string{"acme/widgets"}, DefaultCapabilities: []string{"self-debug"},
		AgentFramework: "antigravity",
	}
	after := ui.Settings{
		EnvironmentName: "prod", PollInterval: "45s", MaxWorkers: 3, MaxMergers: 2,
		GeminiModel: "gemini-new", ClaudeModel: "claude-new", MaxAgentTurns: 80,
		GitHubHost: "github.example", GitHubInsecureHTTP: true,
		GCPProject: "grain-new", GCPServiceAccountEmail: "new@example.com",
		SandboxCPUs: 4, SandboxMemoryMB: 8192, SandboxDiskGB: 30,
		TargetRepos: []string{"acme/widgets", "acme/gadgets"}, DefaultCapabilities: []string{"self-debug", "gcp-key"},
		AgentFramework: "claude", NewestFirst: true, ShowClosedByDefault: true,
		ApprovedByDefault: true, AutoMergeByDefault: true,
		PromptExtension: "Run `make lint` before you push.",
	}

	out := captureStdout(t, func() { printSettingsDiff(before, after) })

	for _, name := range []string{
		"environment name", "poll interval", "max workers", "max mergers",
		"gemini model", "claude model", "max agent turns",
		"github host", "github insecure http", "gcp project", "gcp agent service account",
		"sandbox cpus", "sandbox memory mb", "sandbox disk gb", "prompt extension",
		"target repos", "default capabilities", "agent framework",
		"newest first", "show closed by default", "approved by default", "auto merge by default",
	} {
		if !strings.Contains(out, "  "+name+": ") {
			t.Errorf("printSettingsDiff did not report %q as changed; it printed:\n%s", name, out)
		}
	}
	// The two lists read as lists, not as Go slices: this is a workflow
	// log, and `["acme/widgets" "acme/gadgets"]` is not how anything else
	// here names a set.
	if !strings.Contains(out, `"acme/widgets" -> "acme/widgets, acme/gadgets"`) {
		t.Errorf("target repos are not reported as a joined list; printed:\n%s", out)
	}
}

// The other half of the two list fields above: they are compared as the
// joined strings they are printed as, so two equal lists must not read as
// a change. A sync run that reported one on every tick would be as
// useless as one that reported none.
func TestPrintSettingsDiffSaysNothingChangedWhenNothingDid(t *testing.T) {
	settings := ui.Settings{
		EnvironmentName: "staging", PollInterval: "30s", MaxWorkers: 1,
		TargetRepos: []string{"acme/widgets"},
	}
	out := captureStdout(t, func() { printSettingsDiff(settings, settings) })
	if !strings.Contains(out, "already up to date, nothing changed") {
		t.Errorf("printSettingsDiff printed %q, want the no-op line", out)
	}
	if strings.Contains(out, "settings changed:") {
		t.Errorf("printSettingsDiff reported a change where there was none: %q", out)
	}
}
