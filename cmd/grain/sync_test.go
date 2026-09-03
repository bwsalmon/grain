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
	"reflect"
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
		PollInterval: &pollInterval,
		MaxWorkers:   &maxWorkers,
		GeminiModel:  &geminiModel,
		ClaudeModel:  &claudeModel,
		GitHubHost:   &githubHost,
	}
}

// printSettingsDiff is the whole of what a sync run reports, and its one
// failure mode is silence: a setting the config file really changed, with
// no row in settingsDiffRows, prints as "nothing changed" against a
// deployment that just changed. That has happened twice -- the sandbox VM
// shape, then seven settings at once -- and both times the table was a
// hand-written list nobody was reminded to add to.
//
// This is the reminder, and it is derived rather than written down:
// ui.UpdateSettingsRequest's own JSON tags are the definition of what a
// config file can set (syncSettings hands UpdateSettings the whole
// request), so every one of them must be a row here or a deliberate,
// reasoned entry in settingsDiffExceptions.
func TestSettingsDiffRowsCoverEveryUpdatableSetting(t *testing.T) {
	rows := map[string]string{}
	for _, row := range settingsDiffRows {
		if other, dup := rows[row.field]; dup {
			t.Errorf("settingsDiffRows has two rows for %q (%q and %q); one setting is one line of output",
				row.field, other, row.name)
		}
		rows[row.field] = row.name
	}

	settable := map[string]bool{}
	for _, field := range updateSettingsFields() {
		settable[field] = true
		if _, ok := rows[field]; ok {
			continue
		}
		if why, excused := settingsDiffExceptions[field]; excused {
			if strings.TrimSpace(why) == "" {
				t.Errorf("settingsDiffExceptions excuses %q with no reason", field)
			}
			continue
		}
		t.Errorf("ui.UpdateSettingsRequest has a %q field with no settingsDiffRows row: "+
			"a config file setting it changes the deployment, and `grain sync` prints "+
			"\"already up to date, nothing changed\" -- add a row, or a settingsDiffExceptions "+
			"entry saying why it cannot be diffed", field)
	}

	// The other direction, so the table cannot drift into naming settings
	// that no longer exist: a row (or an exception) for a field
	// UpdateSettingsRequest does not have reports on nothing, and reads as
	// coverage it isn't.
	for field := range rows {
		if !settable[field] {
			t.Errorf("settingsDiffRows has a row for %q, which is not a ui.UpdateSettingsRequest field -- "+
				"no config file can set it, so the row diffs something no sync ever changes", field)
		}
	}
	for field := range settingsDiffExceptions {
		if !settable[field] {
			t.Errorf("settingsDiffExceptions excuses %q, which is not a ui.UpdateSettingsRequest field -- "+
				"a stale exception excuses nothing and hides the next real one", field)
		}
	}
}

// updateSettingsFields is every setting a "settings" section can carry,
// by the JSON name it is spelled with -- ui.UpdateSettingsRequest's own
// tags, which is where cmdSync unmarshals that section into.
func updateSettingsFields() []string {
	t := reflect.TypeOf(ui.UpdateSettingsRequest{})
	fields := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, name)
	}
	return fields
}

// Having a row is half of it; the row has to actually report the change.
// Every setting is moved off its zero value at once (settingsWithEveryFieldChanged)
// rather than by a hand-written pair of ui.Settings, so this cannot go
// stale the way the list it replaced did: a row reading a field nothing
// changed, or reading nothing at all, fails here.
func TestPrintSettingsDiffReportsEveryRowItHas(t *testing.T) {
	before := ui.Settings{}
	after := settingsWithEveryFieldChanged()

	out := captureStdout(t, func() { printSettingsDiff(before, after) })

	for _, row := range settingsDiffRows {
		if !strings.Contains(out, "  "+row.name+": ") {
			t.Errorf("printSettingsDiff did not report %q (%s) as changed; it printed:\n%s",
				row.name, row.field, out)
		}
	}
}

// settingsWithEveryFieldChanged is a settings row with every field moved
// off its zero value, so that a diff against the zero ui.Settings has to
// report every row there is.
func settingsWithEveryFieldChanged() ui.Settings {
	var s ui.Settings
	v := reflect.ValueOf(&s).Elem()
	for i := range v.NumField() {
		switch f := v.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("changed")
		case reflect.Int:
			f.SetInt(7)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			// Only the []string ones matter here -- they are what the two
			// list rows join -- and a slice of anything else (the
			// capability statuses) is left alone rather than filled with a
			// zero element that would say nothing.
			if f.Type().Elem().Kind() == reflect.String {
				f.Set(reflect.ValueOf([]string{"one", "two"}))
			}
		}
	}
	return s
}

// The lists read as lists, not as Go slices: this is a workflow log, and
// `["acme/widgets" "acme/gadgets"]` is not how anything else here names a
// set.
func TestPrintSettingsDiffJoinsListSettings(t *testing.T) {
	before := ui.Settings{TargetRepos: []string{"acme/widgets"}}
	after := ui.Settings{TargetRepos: []string{"acme/widgets", "acme/gadgets"}}

	out := captureStdout(t, func() { printSettingsDiff(before, after) })

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
