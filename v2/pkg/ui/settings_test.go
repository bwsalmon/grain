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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
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
