package ui_test

// grain/task-117: a deployment's extra named GitHub tokens are
// capabilities like any other -- offered by the picker, attachable to a
// task, reportable on Settings' Capabilities tab -- and the point of
// giving each one an id rather than building a second, token-shaped
// pane is that none of the machinery in between has to know they exist.
// These tests are that claim, from the offered row through to the
// override the git proxy reads back off the task.

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// withTokenCapabilities is testClient with the picker listing a real
// deployment would have on top of the fixed rows: one per named token
// cmd/grain/daemon.go found under secrets/github.
func withTokenCapabilities(t *testing.T, names ...string) (*ui.Client, *model.Store, context.Context) {
	t.Helper()
	c, store, ctx := testClient(t)
	c.Config.Capabilities = append(ui.OfferedCapabilities(), ui.GitHubTokenCapabilities(names)...)
	return c, store, ctx
}

func TestGitHubTokenCapabilitiesIsOneRowPerNamedToken(t *testing.T) {
	got := ui.GitHubTokenCapabilities([]string{"release-bot", "", "docs-bot"})
	want := []ui.Capability{
		{ID: "github-credential:release-bot", Name: "GitHub token: release-bot",
			Description: got[0].Description},
		{ID: "github-credential:docs-bot", Name: "GitHub token: docs-bot",
			Description: got[1].Description},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHubTokenCapabilities = %+v, want %+v (an empty name is skipped)", got, want)
	}
	for _, capability := range got {
		if capability.Description == "" {
			t.Errorf("%s: no description for the picker to show", capability.ID)
		}
	}
}

func TestGitHubTokenCapabilitiesAreNoneWithoutExtraTokens(t *testing.T) {
	if got := ui.GitHubTokenCapabilities(nil); len(got) != 0 {
		t.Errorf("GitHubTokenCapabilities(nil) = %+v, want none -- a deployment with only a default token offers no rows", got)
	}
}

// The whole path a human takes: tick the token on a task, and the git
// proxy resolving that task's sandbox is told to use it in place of the
// owner/repo ladder.
func TestAttachingATokenCapabilityOverridesTheSandboxCredential(t *testing.T) {
	c, store, ctx := withTokenCapabilities(t, "release-bot")
	task := create(t, c, ctx)

	if err := c.SetCapability(ctx, task.ID, "github-credential:release-bot", true); err != nil {
		t.Fatalf("attaching the token capability: %v", err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "sandbox-0", Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	name, ok, err := store.GitCredentialOverride(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "release-bot" {
		t.Fatalf("GitCredentialOverride = %q, %v, want %q true", name, ok, "release-bot")
	}

	// Detaching puts the sandbox back on the default ladder, the same as
	// any other capability coming off a task.
	if err := c.SetCapability(ctx, task.ID, "github-credential:release-bot", false); err != nil {
		t.Fatalf("detaching the token capability: %v", err)
	}
	if _, ok, err := store.GitCredentialOverride(ctx, "sandbox-0"); err != nil || ok {
		t.Fatalf("GitCredentialOverride ok=%v err=%v, want false, nil after detaching", ok, err)
	}
}

// A token this deployment does not have is rejected the same way any
// other unknown capability id is -- the picker listing is still the one
// gate every grant passes through.
func TestAttachingAnUnconfiguredTokenIsRejected(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")
	task := create(t, c, ctx)
	if err := c.SetCapability(ctx, task.ID, "github-credential:not-configured", true); err == nil {
		t.Fatal("expected attaching a token this deployment has no credential for to be rejected")
	}
}

func TestSettingsReportsEachNamedTokenAsAReadyCapability(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")

	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := capabilityStatus(t, got.Capabilities, "github-credential:release-bot")
	if !status.Ready || !status.Grantable {
		t.Fatalf("release-bot: Ready=%t Grantable=%t, want both true (status: %+v)",
			status.Ready, status.Grantable, status)
	}
	if status.Name != "GitHub token: release-bot" {
		t.Errorf("Name = %q, want the same name the picker shows", status.Name)
	}
	if status.Default {
		t.Errorf("Default = true, want false -- nothing has defaulted this token (status: %+v)", status)
	}
}

// Defaulting one deployment-wide goes through the same validation and
// the same reporting as every other capability id.
func TestANamedTokenCanBeADeploymentDefault(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	defaults := []string{"github-credential:release-bot"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{DefaultCapabilities: &defaults})
	if err != nil {
		t.Fatalf("defaulting a named token: %v", err)
	}
	if !slices.Equal(got.DefaultCapabilities, defaults) {
		t.Fatalf("defaultCapabilities = %v, want %v", got.DefaultCapabilities, defaults)
	}
	if status := capabilityStatus(t, got.Capabilities, "github-credential:release-bot"); !status.Default {
		t.Errorf("Default = false on a token every new task is filed with (status: %+v)", status)
	}

	// And a task filed afterwards really does carry it, which is what
	// makes the git proxy use that token for its pushes.
	task := create(t, c, ctx)
	if !slices.Contains(task.Capabilities, "github-credential:release-bot") {
		t.Fatalf("a task filed with the token defaulted holds %v", task.Capabilities)
	}
}
