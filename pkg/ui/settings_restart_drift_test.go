package ui

// restartOnlySettings (settings.go) is the same kind of hand-maintained
// list about UpdateSettingsRequest that `grain sync`'s diff table was
// (cmd/grain/sync.go's settingsDiffRows), and it fails the same silent
// way: a setting a running daemon cannot actually adopt, but that nothing
// names here, is reported to the Settings pane as applied the moment it
// is saved, and the deployment then runs on a value no one is told is not
// in effect.
//
// The list itself cannot be derived from a type -- whether a setting can
// be applied live is a fact about cmd/grain/daemon.go's liveConfig, not
// about this struct, and it is deliberately an allowlist of two rather
// than a reflect-based comparison (restartOnlySetting.Differs' own doc
// comment on why). What can be made structural is that no setting escapes
// the question: every field of UpdateSettingsRequest is either named here
// as restart-only or named below as applied live, by whatever picks it
// up, so a new setting is in neither list until somebody has decided
// which of the two it is.
//
// This lives in package ui rather than ui_test because
// restartOnlySettings is unexported, and it is that list -- not the
// RestartRequired copy of it a response carries -- that is the thing
// worth comparing against.

import (
	"reflect"
	"strings"
	"testing"
)

// settingsAppliedLive is every UpdateSettingsRequest field a *running*
// daemon adopts on its own, mapped to the piece that applies it --
// cmd/grain/daemon.go's liveConfig doc comment, restated as something a
// test can hold against the type instead of prose that goes quietly
// stale. Together with restartOnlySettings it has to account for every
// settable field, which is the whole point of writing it down.
var settingsAppliedLive = map[string]string{
	"pollInterval":           "the reconcile loop's own ticker (liveConfig.refresh)",
	"maxWorkers":             "orchestrator.RunCycle's own per-cycle re-read",
	"maxMergers":             "orchestrator.RunCycle's own per-cycle re-read",
	"maxAgentTurns":          "orchestrator.RunCycle's own per-cycle re-read",
	"agentFramework":         "dispatchConfig's own per-dispatch re-read",
	"geminiModel":            "dispatchConfig's own per-dispatch re-read",
	"geminiEffort":           "dispatchConfig's own per-dispatch re-read",
	"claudeModel":            "dispatchConfig's own per-dispatch re-read",
	"codexModel":             "dispatchConfig's own per-dispatch re-read",
	"gcpProject":             "the capability registry the next cycle resolves grants against (liveConfig.refresh)",
	"gcpServiceAccountEmail": "the capability registry the next cycle resolves grants against (liveConfig.refresh)",
	"sandboxCpus":            "the default shape the next sandbox is built at (liveConfig.refresh, via defaultShaper)",
	"sandboxMemoryMb":        "the default shape the next sandbox is built at (liveConfig.refresh, via defaultShaper)",
	"sandboxDiskGb":          "the default shape the next sandbox is built at (liveConfig.refresh, via defaultShaper)",
	"promptExtension":        "the deployment-wide text RunCycle refreshes out of grain_config every cycle (orchestrator.resolvePromptExtension)",
	"agentGitName":           "orchestrator.RunCycle's own per-cycle re-read (the identity the next run's sandbox is given, Sandbox.ConfigureGitCredentials)",
	"agentGitEmail":          "orchestrator.RunCycle's own per-cycle re-read (the identity the next run's sandbox is given, Sandbox.ConfigureGitCredentials)",
	"targetRepos":            "pkg/ui, which reads grain_config per request (and Client.setTargetRepos, in step with the write)",
	"defaultCapabilities":    "pkg/ui, which reads grain_config per request (CreateTask's own defaults)",
	"newestFirst":            "pkg/ui, which reads grain_config per request",
	"showClosedByDefault":    "pkg/ui, which reads grain_config per request",
	"approvedByDefault":      "pkg/ui, which reads grain_config per request",
	"autoMergeByDefault":     "pkg/ui, which reads grain_config per request",
	"environmentName":        "pkg/ui, which reads grain_config per request (GET /api/config's own copy)",
	"timeZone":               "orchestrator.RunCycle's own per-cycle re-read (when a wall-clock schedule fires), and pkg/ui per request (GET /api/config's own copy, which is what the frontend formats every timestamp against)",
}

func TestEverySettingIsEitherAppliedLiveOrRestartOnly(t *testing.T) {
	restartOnly := map[string]bool{}
	for _, s := range restartOnlySettings {
		restartOnly[s.Key] = true
	}

	settable := map[string]bool{}
	for _, field := range jsonNames(reflect.TypeOf(UpdateSettingsRequest{})) {
		settable[field] = true
		if restartOnly[field] && settingsAppliedLive[field] != "" {
			t.Errorf("%q is both restart-only and listed as applied live: it is one or the other, "+
				"and the pane annotates it on the strength of the first", field)
			continue
		}
		if restartOnly[field] || settingsAppliedLive[field] != "" {
			continue
		}
		t.Errorf("UpdateSettingsRequest has a %q field that is neither in restartOnlySettings nor in "+
			"settingsAppliedLive: if a running daemon cannot adopt it, the Settings pane reports it as "+
			"applied the moment it is saved and the deployment runs on a value nobody is told is not in "+
			"effect -- add it to restartOnlySettings, or to settingsAppliedLive naming what applies it",
			field)
	}

	// Both lists the other way round, so neither can drift into naming
	// settings that no longer exist: a key no config file and no pane can
	// set annotates nothing, and reads as an accounting it isn't.
	for field := range settingsAppliedLive {
		if !settable[field] {
			t.Errorf("settingsAppliedLive names %q, which is not an UpdateSettingsRequest field -- "+
				"nothing can change it, so nothing applies it live", field)
		}
	}
	for field := range restartOnly {
		if !settable[field] {
			t.Errorf("restartOnlySettings names %q, which is not an UpdateSettingsRequest field -- "+
				"nothing can change it, so no restart is ever owed for it", field)
		}
	}
}

// The keys are also how the frontend finds the input to annotate
// (SettingsOverlay.jsx's restartHint/restartChip look each one up against
// the field names its own form uses, which are Settings' JSON names). A
// key that names no Settings field annotates nothing at all, silently:
// the pane renders exactly as if the setting applied live.
func TestRestartOnlySettingsAreNamedAsTheSettingsFieldsTheyAnnotate(t *testing.T) {
	reported := map[string]bool{}
	for _, field := range jsonNames(reflect.TypeOf(Settings{})) {
		reported[field] = true
	}
	for _, key := range restartRequiredKeys() {
		if !reported[key] {
			t.Errorf("restartOnlySettings names %q, which is not a Settings field: the pane looks its "+
				"restart annotation up by that name and finds no input to put it on", key)
		}
	}
}

// jsonNames is a struct's fields by the JSON name they are marshaled
// with -- the name a config file, an API caller and the frontend all
// spell the setting with, and so the only name these lists can agree on.
func jsonNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}
