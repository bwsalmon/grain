package main

// Which agent framework a deployment dispatches onto, from the shell.
// It was the one field the Settings pane could change and `grain
// settings` could not (found by hand-testing a real deployment, task
// 244) -- which matters because scripts/setup.sh binds the UI to
// loopback, so on a host with no browser tunnelled to it the CLI is the
// whole interface. Same helpers, and the same real embedded store, the
// other settings tests here use.

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCmdSettingsSetsTheAgentFramework(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	if err := cmdSettings(ctx, c, &printer{}, []string{"-agent-framework", model.AgentFrameworkClaude}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("AgentFramework = %q, want %q", settings.AgentFramework, model.AgentFrameworkClaude)
	}

	// An unrelated flag leaves it alone, the same fs.Visit
	// nil-means-unchanged contract every other setting gets.
	if err := cmdSettings(ctx, c, &printer{}, []string{"-max-workers", "2"}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.AgentFramework != model.AgentFrameworkClaude {
		t.Fatalf("AgentFramework after an unrelated save = %q, want it untouched", settings.AgentFramework)
	}
}

func TestCmdSettingsRefusesAFrameworkThatDoesNotExist(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	err := cmdSettings(ctx, c, &printer{}, []string{"-agent-framework", "gpt"})
	if err == nil {
		t.Fatal("cmdSettings accepted a framework nothing can dispatch onto")
	}
	// The vocabulary comes back from the same validation the Settings
	// pane's own field goes through, rather than a second copy in the CLI.
	if !strings.Contains(err.Error(), model.AgentFrameworkAntigravity) {
		t.Errorf("error = %q, want it to name the frameworks that do exist", err)
	}
}

func TestCmdSettingsPrintsTheAgentFramework(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	out := captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	if !strings.Contains(out, "agent framework: "+model.AgentFrameworkAntigravity) {
		t.Errorf("`grain settings` printed %q, which does not say which framework runs", out)
	}
}
