package ui_test

// bwsalmon/agents#611: Settings' new "Capabilities" tab needs to answer
// "is this capability actually usable right now, and if not, what's
// missing" for every capability grain ships a provider for -- not just
// the ones DefaultCapabilities offers a human to attach to a task. These
// tests cover GetSettings' new Capabilities field end to end: ready
// without any secrets store colocated (self-debug/self-repair, which
// need none), gated on GCP config (gcp-key/gemini-key), and gated on
// secrets once one is (github-sandbox).

import (
	"reflect"
	"sort"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func capabilityStatus(t *testing.T, statuses []ui.CapabilityStatus, id string) ui.CapabilityStatus {
	t.Helper()
	for _, s := range statuses {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no capability status for %q among %+v", id, statuses)
	return ui.CapabilityStatus{}
}

func TestCapabilitiesListsEveryKnownCapabilityOnAFreshStore(t *testing.T) {
	c, _, ctx := testClient(t)
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got.Capabilities {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	want := []string{"gcp-key", "gemini-key", "github-sandbox", "self-debug", "self-repair"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("capability ids = %v, want %v", ids, want)
	}
}

func TestCapabilitiesSelfDebugAndSelfRepairAreAlwaysReady(t *testing.T) {
	c, _, ctx := testClient(t)
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"self-debug", "self-repair"} {
		status := capabilityStatus(t, got.Capabilities, id)
		if !status.Ready {
			t.Fatalf("%s: Ready = false, want true (status: %+v)", id, status)
		}
		if len(status.MissingConfig) != 0 || len(status.MissingSecrets) != 0 {
			t.Fatalf("%s: expected nothing missing, got %+v", id, status)
		}
	}
}

func TestCapabilitiesGcpKeyAndGeminiKeyAreNotReadyWithoutAGcpProject(t *testing.T) {
	c, _, ctx := testClient(t)
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}

	gcpKey := capabilityStatus(t, got.Capabilities, "gcp-key")
	if gcpKey.Ready {
		t.Fatalf("gcp-key: Ready = true with no GCP project configured, want false")
	}
	want := []string{"GCP project", "GCP service account email"}
	if !reflect.DeepEqual(gcpKey.MissingConfig, want) {
		t.Fatalf("gcp-key: MissingConfig = %v, want %v", gcpKey.MissingConfig, want)
	}

	geminiKey := capabilityStatus(t, got.Capabilities, "gemini-key")
	if geminiKey.Ready {
		t.Fatalf("gemini-key: Ready = true with no GCP project configured, want false")
	}
	if !reflect.DeepEqual(geminiKey.MissingConfig, []string{"GCP project"}) {
		t.Fatalf("gemini-key: MissingConfig = %v, want [GCP project]", geminiKey.MissingConfig)
	}
}

func TestCapabilitiesGeminiKeyIsReadyOnceAGcpProjectAndItsMinterSecretAreSet(t *testing.T) {
	c, _, ctx := testClient(t)
	c.Config.Secrets = secrets.New(t.TempDir())
	if err := c.Config.Secrets.Set(gcpkey.DefaultMinterCredential, "value", []byte("shh")); err != nil {
		t.Fatal(err)
	}

	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}
	project := "acme-project"
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{GCPProject: &project}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}

	geminiKey := capabilityStatus(t, got.Capabilities, "gemini-key")
	if !geminiKey.Ready {
		t.Fatalf("gemini-key: Ready = false, want true (status: %+v)", geminiKey)
	}

	// gcp-key still needs the service account email, and its own minter
	// secret was never set under a different name -- unaffected by
	// gemini-key becoming ready.
	gcpKey := capabilityStatus(t, got.Capabilities, "gcp-key")
	if gcpKey.Ready {
		t.Fatalf("gcp-key: Ready = true, want false (status: %+v)", gcpKey)
	}
}

func TestCapabilitiesGithubSandboxFlagsMissingSecretsWhenColocatedWithAStore(t *testing.T) {
	c, _, ctx := testClient(t)
	c.Config.Secrets = secrets.New(t.TempDir())

	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := capabilityStatus(t, got.Capabilities, "github-sandbox")
	if status.Ready {
		t.Fatalf("github-sandbox: Ready = true with no secrets set, want false")
	}
	want := []string{githubsandbox.DefaultAppIDCredential, githubsandbox.DefaultPrivateKeyCredential}
	sort.Strings(want)
	got2 := append([]string(nil), status.MissingSecrets...)
	sort.Strings(got2)
	if !reflect.DeepEqual(got2, want) {
		t.Fatalf("github-sandbox: MissingSecrets = %v, want %v", status.MissingSecrets, want)
	}

	if err := c.Config.Secrets.Set("github-app", "app-id", []byte("123")); err != nil {
		t.Fatal(err)
	}
	if err := c.Config.Secrets.Set("github-app", "private-key", []byte("-----BEGIN KEY-----")); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status = capabilityStatus(t, got.Capabilities, "github-sandbox")
	if !status.Ready {
		t.Fatalf("github-sandbox: Ready = false once both secrets are set (status: %+v)", status)
	}
}

func TestCapabilitiesReportNothingMissingWithNoSecretsStoreColocated(t *testing.T) {
	// The default testClient -- Config.Secrets left nil, the same "not
	// colocated" case TestSettingsSkipsTheCredentialCheckWithNoCredentialsConfigured
	// covers for targetRepos.
	c, _, ctx := testClient(t)
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := capabilityStatus(t, got.Capabilities, "github-sandbox")
	if len(status.MissingSecrets) != 0 {
		t.Fatalf("github-sandbox: MissingSecrets = %v, want none reported with no store to check", status.MissingSecrets)
	}
}
