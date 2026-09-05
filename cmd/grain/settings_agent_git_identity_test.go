package main

// grain/task-14's commit identity, from the shell rather than the
// Settings pane: pointing a deployment's commits at the bot account its
// operator wants on pull requests is part of standing that deployment up,
// so `grain settings -agent-git-name/-agent-git-email` has to set it,
// print what is actually in effect, and let it be cleared back to grain's
// own. Same helpers, and the same real embedded store,
// settings_sandbox_shape_test.go already uses.

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCmdSettingsSetsAndClearsTheAgentGitIdentity(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	if err := cmdSettings(ctx, c, &printer{}, []string{
		"-agent-git-name", "acme bot", "-agent-git-email", "bot@acme.example",
	}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.AgentGitName != "acme bot" || settings.AgentGitEmail != "bot@acme.example" {
		t.Fatalf("agent git identity = %q/%q, want %q/%q",
			settings.AgentGitName, settings.AgentGitEmail, "acme bot", "bot@acme.example")
	}

	// An unrelated flag leaves both alone -- the same fs.Visit
	// nil-means-unchanged contract every other setting here gets.
	if err := cmdSettings(ctx, c, &printer{}, []string{"-max-workers", "3"}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.AgentGitName != "acme bot" || settings.AgentGitEmail != "bot@acme.example" {
		t.Fatalf("agent git identity after an unrelated save = %q/%q, want it untouched",
			settings.AgentGitName, settings.AgentGitEmail)
	}

	// And an empty one puts grain's own identity back, the way an empty
	// -time-zone restores the default clock: there is no such thing as a
	// deployment whose commits have no author.
	if err := cmdSettings(ctx, c, &printer{}, []string{"-agent-git-name", ""}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.AgentGitName != "" {
		t.Fatalf("AgentGitName after clearing = %q, want empty", settings.AgentGitName)
	}
}

// Printed as what is in effect rather than as the blank that is stored:
// an operator reading this listing to find out what their agents commit
// as should not have to know which constant in which package answers it.
func TestCmdSettingsPrintsTheAgentGitIdentityInEffect(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	out := captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	for _, want := range []string{
		"agent git name: " + mcp.DefaultGitIdentityName + " (grain default, unset)",
		"agent git email: " + mcp.DefaultGitIdentityEmail + " (grain default, unset)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`grain settings` printed %q, which does not contain %q", out, want)
		}
	}

	if err := cmdSettings(ctx, c, &printer{}, []string{
		"-agent-git-name", "acme bot", "-agent-git-email", "bot@acme.example",
	}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	out = captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	for _, want := range []string{"agent git name: acme bot", "agent git email: bot@acme.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("`grain settings` printed %q, which does not contain %q", out, want)
		}
	}
	// And stops calling a chosen identity a default, the way the sandbox
	// shape does once it is set.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "agent git ") && strings.Contains(line, "grain default") {
			t.Errorf("`grain settings` printed %q, which still calls a chosen identity a default", line)
		}
	}
}
