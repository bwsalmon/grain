package ui_test

// bwsalmon/agents#427: a targetRepos entry with no covering credential in
// the ladder the git proxy resolves pushes against is a deployment
// misconfiguration that otherwise only surfaces later, confusingly, as a
// 500 "no credential configured" from the proxy itself (core_test.go's
// own TestHandleNoConfiguredCredentialIs500NotForwarded). These tests
// cover the other half: Settings flagging the same gap directly, at the
// moment targetRepos is widened, when this UI is colocated with the
// proxy's own credential ladder (Config.Credentials, nil otherwise).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// loadCredentialSet writes a credentials.json covering exactly the
// owner/repo patterns given and returns the CredentialSet LoadCredentialSet
// reads back -- the same shape cmd/grain/daemon.go's own startUIServer
// loads at startup, minus the token files, which Select doesn't need to
// answer whether a pattern covers a repo.
func loadCredentialSet(t *testing.T, patterns map[string]string) *gitproxy.CredentialSet {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := gitproxy.LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func testClientWithCredentials(t *testing.T, credentials *gitproxy.CredentialSet) (*ui.Client, *model.Store, context.Context) {
	t.Helper()
	c, store, ctx := testClient(t)
	c.Config.Credentials = credentials
	return c, store, ctx
}

func TestSettingsFlagsTargetReposWithNoCoveringCredential(t *testing.T) {
	credentials := loadCredentialSet(t, map[string]string{"acme/covered": "bot"})
	c, _, ctx := testClientWithCredentials(t, credentials)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	repos := []string{"acme/covered", "acme/gap"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{TargetRepos: &repos})
	if err != nil {
		t.Fatalf("setting target repos: %v", err)
	}
	want := []string{"acme/gap"}
	if !reflect.DeepEqual(got.TargetReposMissingCredentials, want) {
		t.Fatalf("targetReposMissingCredentials = %v, want %v", got.TargetReposMissingCredentials, want)
	}

	// GetSettings recomputes the same thing, not a stale copy from the
	// update -- the ladder is what's authoritative, not a cached list.
	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.TargetReposMissingCredentials, want) {
		t.Fatalf("GetSettings targetReposMissingCredentials = %v, want %v", read.TargetReposMissingCredentials, want)
	}
}

func TestSettingsHasNoGapWhenTheLadderCoversEveryTargetRepo(t *testing.T) {
	credentials := loadCredentialSet(t, map[string]string{"*": "bot"})
	c, _, ctx := testClientWithCredentials(t, credentials)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	repos := []string{"acme/widgets", "acme/gadgets"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{TargetRepos: &repos})
	if err != nil {
		t.Fatalf("setting target repos: %v", err)
	}
	if len(got.TargetReposMissingCredentials) != 0 {
		t.Fatalf("targetReposMissingCredentials = %v, want none", got.TargetReposMissingCredentials)
	}
}

// TestUpdateSettingsRoundTripsShowClosedByDefault is bwsalmon/agents#537's
// global default: unset it reads back false (hide closed tasks by
// default), and setting it sticks through a GetSettings read the same
// way NewestFirst already does (client_test.go's own
// TestNewestFirstSettingMovesNewTasksToTheFrontOfTheQueue).
func TestUpdateSettingsRoundTripsShowClosedByDefault(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.ShowClosedByDefault {
		t.Fatalf("ShowClosedByDefault = true with nothing set, want false")
	}

	show := true
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{ShowClosedByDefault: &show}); err != nil {
		t.Fatal(err)
	}
	read, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !read.ShowClosedByDefault {
		t.Fatalf("ShowClosedByDefault = false after UpdateSettings, want true")
	}
}

// TestUpdateSettingsRoundTripsTaskDefaults is bwsalmon/agents#612's own
// pair of global defaults: unset, both read back true
// (model.DefaultConfig -- NewTaskOverlay.jsx's own "Queue immediately"
// and "Auto-merge once checks pass" checkboxes start checked), and
// turning either off sticks through a GetSettings read the same way
// TestUpdateSettingsRoundTripsShowClosedByDefault already covers for
// ShowClosedByDefault.
//
// The first save is what makes the "unset" half worth pinning: it goes
// through the same PutConfig every other save does, binding both columns,
// so a first save that never mentions these two has to write the default
// rather than the Go zero value beside it.
func TestUpdateSettingsRoundTripsTaskDefaults(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !read.ApprovedByDefault {
		t.Fatalf("ApprovedByDefault = false with nothing set, want true")
	}
	if !read.AutoMergeByDefault {
		t.Fatalf("AutoMergeByDefault = false with nothing set, want true")
	}

	approved, autoMerge := false, false
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		ApprovedByDefault:  &approved,
		AutoMergeByDefault: &autoMerge,
	}); err != nil {
		t.Fatal(err)
	}
	read, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.ApprovedByDefault {
		t.Fatalf("ApprovedByDefault = true after UpdateSettings, want false")
	}
	if read.AutoMergeByDefault {
		t.Fatalf("AutoMergeByDefault = true after UpdateSettings, want false")
	}
}

// grain/task-14: the default capability set round-trips like every
// other setting, and the Capabilities tab's own per-capability status
// says which ones it names -- a capability every task is filed holding
// is the one whose readiness an operator most needs to see.
func TestUpdateSettingsRoundTripsDefaultCapabilities(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.DefaultCapabilities) != 0 {
		t.Fatalf("DefaultCapabilities = %v with nothing set, want none", read.DefaultCapabilities)
	}

	// Repeated deliberately: a picker can produce a duplicate, and this
	// is a set.
	defaults := []string{"gcp-key", "gcp-key"}
	saved, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{DefaultCapabilities: &defaults})
	if err != nil {
		t.Fatalf("setting default capabilities: %v", err)
	}
	if !reflect.DeepEqual(saved.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("DefaultCapabilities = %v, want [gcp-key] with the repeat dropped", saved.DefaultCapabilities)
	}
	read, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("DefaultCapabilities = %v after a re-read, want [gcp-key]", read.DefaultCapabilities)
	}
	for _, status := range read.Capabilities {
		if want := status.ID == "gcp-key"; status.Default != want {
			t.Errorf("capability %s: Default = %t, want %t", status.ID, status.Default, want)
		}
	}

	// Empty is a value, not "leave it alone": it is how the set is turned
	// back off.
	none := []string{}
	cleared, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{DefaultCapabilities: &none})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.DefaultCapabilities) != 0 {
		t.Fatalf("DefaultCapabilities = %v after clearing, want none", cleared.DefaultCapabilities)
	}
}

// Defaulting a capability no task could be granted by hand would be a
// setting that failed silently at every filing, so it is refused while
// whoever chose it is still looking at it -- unlike a stored id grain
// has since retired, which CreateTask skips (TestCreateTaskSkipsRetired
// DefaultCapability).
func TestUpdateSettingsRejectsUnknownDefaultCapability(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	defaults := []string{"not-a-real-capability"}
	_, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{DefaultCapabilities: &defaults})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a validation error", err)
	}
	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.DefaultCapabilities) != 0 {
		t.Fatalf("DefaultCapabilities = %v after a refused save, want none stored", read.DefaultCapabilities)
	}
}

// TestGetSettingsReportsTaskDefaultsBeforeAnythingIsSaved is the same
// pair read on a deployment with no grain_config row at all -- the state
// an operator opening Settings on a fresh install sees. Configured is
// false there, but the two checkboxes still have to describe what filing
// a task would actually do, which is model.DefaultConfig's "on" rather
// than the zero value of a Settings nobody has written yet.
func TestGetSettingsReportsTaskDefaultsBeforeAnythingIsSaved(t *testing.T) {
	c, _, ctx := testClient(t)

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.Configured {
		t.Fatalf("Configured = true with nothing saved, want false")
	}
	if !read.ApprovedByDefault {
		t.Fatalf("ApprovedByDefault = false with nothing saved, want true")
	}
	if !read.AutoMergeByDefault {
		t.Fatalf("AutoMergeByDefault = false with nothing saved, want true")
	}
}

// TestUpdateSettingsRoundTripsAgentFramework is bwsalmon/agents#609's own
// setting: unset it reads back "antigravity"
// (model.AgentFrameworkAntigravity, the framework a deployment that has
// never chosen one runs), and setting it to "claude" sticks through a
// GetSettings read the same way ShowClosedByDefault's own round trip does
// above.
func TestUpdateSettingsRoundTripsAgentFramework(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.AgentFramework != model.AgentFrameworkAntigravity {
		t.Fatalf("AgentFramework = %q with nothing set, want %q", read.AgentFramework, model.AgentFrameworkAntigravity)
	}

	claude := model.AgentFrameworkClaude
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{AgentFramework: &claude}); err != nil {
		t.Fatal(err)
	}
	read, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("AgentFramework = %q after UpdateSettings, want %q", read.AgentFramework, model.AgentFrameworkClaude)
	}
}

// TestUpdateSettingsRejectsUnknownAgentFramework is UpdateSettings' own
// allow-list check: anything other than model.AgentFrameworkAntigravity/
// AgentFrameworkClaude (or the legacy "gemini" spelling, normalized to
// the former) is a validation error, not a value silently
// stored, the same way an unparseable pollInterval or an empty
// geminiModel already are.
func TestUpdateSettingsRejectsUnknownAgentFramework(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	bogus := "chatgpt"
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{AgentFramework: &bogus}); err == nil {
		t.Fatal("UpdateSettings with an unknown agentFramework: want an error, got nil")
	}
}

func TestSettingsSkipsTheCredentialCheckWithNoCredentialsConfigured(t *testing.T) {
	// The default testClient -- Config.Credentials left nil, as a UI not
	// colocated with the proxy's secrets directory always leaves it.
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	repos := []string{"acme/widgets"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{TargetRepos: &repos})
	if err != nil {
		t.Fatalf("setting target repos: %v", err)
	}
	if len(got.TargetReposMissingCredentials) != 0 {
		t.Fatalf("targetReposMissingCredentials = %v, want none reported with no ladder to check", got.TargetReposMissingCredentials)
	}
}

// The restart-only settings and the pending-restart report they exist
// for: every other setting on this pane reaches the running daemon
// within a poll interval (cmd/grain/daemon.go's liveConfig), so the two
// that do not have to say so -- both up front, as an annotation on the
// field, and afterwards, once one has been changed and is sitting saved
// but not applied.

// contains reports whether keys names key -- Settings.RestartRequired
// and PendingRestart are small, unordered lists, so this reads better at
// the call sites below than a sorted reflect.DeepEqual would.
func contains(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// runningConfig points a test client's Config.RunningConfig at a fixed
// model.Config -- standing in for the daemon liveConfig would otherwise
// be answering for.
func runningConfig(c *ui.Client, running model.Config) {
	c.Config.RunningConfig = func() model.Config { return running }
}

func TestSettingsReportsWhichSettingsNeedARestart(t *testing.T) {
	c, _, ctx := testClient(t)

	// Before anything has ever been saved: the annotation belongs on the
	// field from the first look at it, not only once a value exists.
	fresh, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(fresh.RestartRequired, "githubHost") || !contains(fresh.RestartRequired, "githubInsecureHttp") {
		t.Fatalf("restartRequired = %v on an unconfigured deployment, want both GitHub host settings", fresh.RestartRequired)
	}
	if len(fresh.PendingRestart) != 0 {
		t.Fatalf("pendingRestart = %v with nothing stored, want none", fresh.PendingRestart)
	}

	// And the settings a running daemon does pick up on its own are
	// deliberately absent: this list is what the UI annotates, so
	// anything named here is a promise that it cannot be applied live.
	for _, live := range []string{"pollInterval", "maxConcurrent", "geminiModel", "claudeModel",
		"maxAgentTurns", "agentFramework", "gcpProject", "sandboxCpus", "sandboxMemoryMb"} {
		if contains(fresh.RestartRequired, live) {
			t.Errorf("restartRequired names %q, which the daemon applies without a restart", live)
		}
	}

	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}
	saved, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(saved.RestartRequired, "githubHost") {
		t.Fatalf("restartRequired = %v once configured, want githubHost", saved.RestartRequired)
	}
}

func TestSettingsReportsARestartOnlyChangeAsPending(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}
	// What the daemon is actually running with: what firstSettings just
	// stored, which is the state right after a restart.
	runningConfig(c, model.Config{GitHubHost: "github.com"})

	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.PendingRestart) != 0 {
		t.Fatalf("pendingRestart = %v with stored and running agreeing, want none", settings.PendingRestart)
	}

	// Now change one, the way an operator does. The update's own
	// response has to carry it: this is the moment the UI puts the
	// "saved, but not applied" warning up.
	host := "github.internal"
	updated, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{GitHubHost: &host})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(updated.PendingRestart, "githubHost") {
		t.Fatalf("pendingRestart = %v right after changing githubHost, want it named", updated.PendingRestart)
	}
	if contains(updated.PendingRestart, "githubInsecureHttp") {
		t.Fatalf("pendingRestart = %v, want only the setting that actually changed", updated.PendingRestart)
	}

	// And changing it back clears it again -- a pending restart is a
	// comparison, not a flag something has to remember to unset.
	back := "github.com"
	restored, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{GitHubHost: &back})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.PendingRestart) != 0 {
		t.Fatalf("pendingRestart = %v after putting githubHost back, want none", restored.PendingRestart)
	}
}

// A UI with no daemon to speak for (`grain demo`, or any client built
// without Config.RunningConfig) reports nothing pending rather than
// comparing against a zero model.Config, which would call every setting
// changed.
func TestSettingsReportsNoPendingRestartWithoutARunningConfig(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.PendingRestart) != 0 {
		t.Fatalf("pendingRestart = %v with no RunningConfig configured, want none", settings.PendingRestart)
	}
	if len(settings.RestartRequired) == 0 {
		t.Fatal("restartRequired is empty; the annotation does not depend on having a running config to compare with")
	}
}
