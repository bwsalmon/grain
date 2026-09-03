package main

// Settings without a restart: liveConfig re-reads grain_config once per
// reconcile tick and hands each change to whatever it configures, so a
// value changed in the UI (or by `grain settings`) changes what this
// process is doing rather than what its next process would do. These
// tests cover the three changes liveConfig applies itself -- the poll
// interval, the capability registry, and a sandbox backend's default VM
// shape -- the two settings it deliberately refuses to apply, and
// dispatchConfig's own per-dispatch half of the same promise.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// startupConfig is what a daemon has after loadConfig: flags parsed,
// grain_config seeded from them.
func startupConfig() config {
	return config{
		pollInterval: 30 * time.Second, maxWorkers: 1,
		agentFramework: model.AgentFrameworkAntigravity,
		geminiModel:    "gemini-2.5-pro", claudeModel: "claude-sonnet-5",
		githubHost: "github.com",
	}
}

// putConfig writes mc as the grain_config row an operator's own save
// through the Settings pane would have left behind.
func putConfig(t *testing.T, store *model.Store, mc model.Config) {
	t.Helper()
	if err := store.PutConfig(context.Background(), mc); err != nil {
		t.Fatalf("storing configuration: %v", err)
	}
}

func TestLiveConfigAppliesAChangedPollInterval(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, cfg.toModelConfig())
	live := newLiveConfig(store, nil, cfg, nil)
	deps := orchestrator.Deps{}

	// Nothing changed yet: the loop is told not to disturb its ticker.
	if live.refresh(ctx, &deps) {
		t.Fatal("refresh reported the poll interval changed when nothing had")
	}

	changed := cfg.toModelConfig()
	changed.PollInterval = 5 * time.Second
	putConfig(t, store, changed)

	if !live.refresh(ctx, &deps) {
		t.Fatal("refresh did not report the changed poll interval, so the ticker would keep the old one")
	}
	if got := live.current().pollInterval; got != 5*time.Second {
		t.Fatalf("poll interval in effect = %s, want 5s", got)
	}
}

// The other half of that promise: what liveConfig reports as in effect
// has to be what this *process* is running, not a copy of the row. A
// restart-only setting changed in the store is exactly the case where
// the two differ, and reporting the stored value would make the Settings
// pane say a change had been applied when nothing had applied it.
func TestLiveConfigNeverAdoptsARestartOnlySetting(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, cfg.toModelConfig())
	live := newLiveConfig(store, nil, cfg, nil)
	deps := orchestrator.Deps{}

	changed := cfg.toModelConfig()
	changed.GitHubHost = "github.internal"
	changed.GitHubInsecureHTTP = true
	changed.GeminiModel = "gemini-3-pro"
	putConfig(t, store, changed)
	live.refresh(ctx, &deps)

	now := live.current()
	if now.githubHost != "github.com" || now.githubInsecureHTTP {
		t.Fatalf("github host in effect = %q/%t, want the startup value github.com/false", now.githubHost, now.githubInsecureHTTP)
	}
	// And the setting alongside it, which *is* live, still moved -- the
	// refusal above is specific, not a blanket one.
	if now.geminiModel != "gemini-3-pro" {
		t.Fatalf("gemini model in effect = %q, want the stored gemini-3-pro", now.geminiModel)
	}
	if got := live.modelConfig().GitHubHost; got != "github.com" {
		t.Fatalf("modelConfig().GitHubHost = %q, want what the process is running with", got)
	}
}

// A GCP project set in Settings enables the gcp-key/gemini-key
// capabilities, which is only true if the registry a cycle resolves
// grants against is rebuilt -- otherwise a task granted one is refused
// by model.ResolveGrants until someone restarts the daemon.
func TestLiveConfigRebuildsCapabilityProvidersWhenGCPChanges(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, cfg.toModelConfig())
	live := newLiveConfig(store, nil, cfg, nil)
	deps := orchestrator.Deps{Config: orchestrator.Config{
		Capabilities: model.NewCapabilityRegistry(capabilityProviders(cfg, nil)...),
	}}

	if _, ok := deps.Config.Capabilities.Lookup("gcp-key"); ok {
		t.Fatal("gcp-key is registered with no GCP project configured")
	}

	changed := cfg.toModelConfig()
	changed.GCPProject = "acme-proj"
	changed.GCPServiceAccountEmail = "agent@acme-proj.iam.gserviceaccount.com"
	putConfig(t, store, changed)
	live.refresh(ctx, &deps)

	if _, ok := deps.Config.Capabilities.Lookup("gcp-key"); !ok {
		t.Fatal("gcp-key is still unregistered after a GCP project was configured -- the registry was not rebuilt")
	}
}

// shapedSandboxes is a sandbox backend that records the default shape it
// is told to adopt -- orchestrator.KonturSandboxes' own SetDefaultShape,
// which is what actually carries this on a real deployment, minus the
// VMs.
type shapedSandboxes struct {
	orchestrator.Sandboxes
	shape orchestrator.Shape
	set   int
}

func (s *shapedSandboxes) SetDefaultShape(shape orchestrator.Shape) {
	s.shape = shape
	s.set++
}

func TestLiveConfigPushesAChangedSandboxShapeToTheBackend(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, cfg.toModelConfig())
	sandboxes := &shapedSandboxes{}
	live := newLiveConfig(store, sandboxes, cfg, nil)
	deps := orchestrator.Deps{}

	live.refresh(ctx, &deps)
	if sandboxes.set != 0 {
		t.Fatalf("the backend was reshaped %d time(s) with nothing changed", sandboxes.set)
	}

	changed := cfg.toModelConfig()
	changed.SandboxCPUs = 4
	changed.SandboxMemoryMB = 8192
	changed.SandboxDiskGB = 40
	putConfig(t, store, changed)
	live.refresh(ctx, &deps)

	if want := (orchestrator.Shape{CPUs: 4, MemoryMB: 8192, DiskGB: 40}); sandboxes.shape != want {
		t.Fatalf("default shape = %+v, want %+v", sandboxes.shape, want)
	}

	// And a change to the disk alone reaches the backend too: the three
	// dimensions are compared independently (liveConfig's own reshape
	// check), not as one all-or-nothing pair of CPU and memory.
	changed.SandboxDiskGB = 80
	putConfig(t, store, changed)
	live.refresh(ctx, &deps)

	if want := (orchestrator.Shape{CPUs: 4, MemoryMB: 8192, DiskGB: 80}); sandboxes.shape != want {
		t.Fatalf("default shape after a disk-only change = %+v, want %+v", sandboxes.shape, want)
	}
}

// A backend with no shape to set -- orchestrator.HostSandboxes, a local
// directory -- is not an error, just a pair of settings with nothing to
// apply to. Nothing here may panic on one.
func TestLiveConfigToleratesABackendWithNoShape(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, cfg.toModelConfig())
	live := newLiveConfig(store, orchestrator.NewHostSandboxes(t.TempDir()), cfg, nil)
	deps := orchestrator.Deps{}

	changed := cfg.toModelConfig()
	changed.SandboxCPUs = 4
	putConfig(t, store, changed)
	live.refresh(ctx, &deps)

	if got := live.current().sandboxCPUs; got != 4 {
		t.Fatalf("sandboxCPUs in effect = %d, want the stored 4 even with nothing to apply it to", got)
	}
}

// A row that has no value for a field -- written by an older build, or
// by a migration that added the column -- must not be adopted as one: ""
// is not a model any framework can be asked for, and a zero duration is
// not an interval a ticker can be built from.
func TestLiveConfigKeepsStartupValuesForUnsetStoredFields(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()
	putConfig(t, store, model.Config{})
	live := newLiveConfig(store, nil, cfg, nil)
	deps := orchestrator.Deps{}

	live.refresh(ctx, &deps)

	now := live.current()
	if now.pollInterval != 30*time.Second || now.maxWorkers != 1 {
		t.Fatalf("poll interval/max workers = %s/%d, want the startup 30s/1", now.pollInterval, now.maxWorkers)
	}
	if now.geminiModel != "gemini-2.5-pro" || now.claudeModel != "claude-sonnet-5" {
		t.Fatalf("models = %q/%q, want the startup ones", now.geminiModel, now.claudeModel)
	}
	if now.agentFramework != model.AgentFrameworkAntigravity {
		t.Fatalf("agent framework = %q, want the startup one", now.agentFramework)
	}
}

// dispatchConfig is the per-dispatch half: the model a framework is
// asked for is read when a run is dispatched, so changing it in Settings
// reaches the next run rather than the next process.
func TestDispatchConfigFollowsTheStoredModels(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := startupConfig()

	// Nothing stored yet: the flags' own values, which are what seed the
	// row every other deployment already has.
	if got := dispatchConfig(ctx, store, cfg).geminiModel; got != "gemini-2.5-pro" {
		t.Fatalf("gemini model = %q with an empty store, want the flag's own", got)
	}

	changed := cfg.toModelConfig()
	changed.GeminiModel = "gemini-3-pro"
	changed.ClaudeModel = "claude-opus-5"
	changed.GitHubHost = "github.internal"
	putConfig(t, store, changed)

	live := dispatchConfig(ctx, store, cfg)
	if live.geminiModel != "gemini-3-pro" || live.claudeModel != "claude-opus-5" {
		t.Fatalf("models = %q/%q, want the stored ones", live.geminiModel, live.claudeModel)
	}
	// Restart-only settings stay restart-only here too: a dispatch reads
	// this to build a framework, not to re-point the git proxy.
	if live.githubHost != "github.com" {
		t.Fatalf("github host = %q, want the startup value", live.githubHost)
	}
}
