package main

// grain/task-69's deployment label, from the shell rather than the
// Settings pane: naming a deployment is something a scripted setup wants
// to do as it brings one up, so `grain settings -environment-name=...`
// has to set it, print it back, and let it be cleared again. Same
// helpers, and the same real embedded store, settings_sandbox_shape_test.go
// already uses.

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCmdSettingsSetsAndClearsTheEnvironmentName(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	if err := cmdSettings(ctx, c, &printer{}, []string{"-environment-name", "staging"}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.EnvironmentName != "staging" {
		t.Fatalf("EnvironmentName = %q, want %q", settings.EnvironmentName, "staging")
	}

	// An unrelated flag leaves it alone -- the same fs.Visit
	// nil-means-unchanged contract every other setting here gets.
	if err := cmdSettings(ctx, c, &printer{}, []string{"-max-workers", "3"}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.EnvironmentName != "staging" {
		t.Fatalf("EnvironmentName after an unrelated save = %q, want it untouched", settings.EnvironmentName)
	}

	// And an empty one clears it, the way an empty -target-repos clears
	// the allowlist: an operator who named a deployment by mistake has to
	// be able to unname it.
	if err := cmdSettings(ctx, c, &printer{}, []string{"-environment-name", ""}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	settings, err = c.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.EnvironmentName != "" {
		t.Fatalf("EnvironmentName after clearing = %q, want empty", settings.EnvironmentName)
	}
}

func TestCmdSettingsPrintsTheEnvironmentName(t *testing.T) {
	ctx := context.Background()
	srv := settingsTestServer(t)
	c := ui.NewHTTPClient(srv.URL)

	// Printed even when unset: a CLI pointed at the wrong -server has no
	// sidebar badge to give it away, so "which deployment is this" is
	// worth an answer either way.
	out := captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	if !strings.Contains(out, "environment:    unnamed") {
		t.Errorf("`grain settings` printed %q, which does not name the deployment as unnamed", out)
	}

	if err := cmdSettings(ctx, c, &printer{}, []string{"-environment-name", "staging"}); err != nil {
		t.Fatalf("cmdSettings: %v", err)
	}
	out = captureStdout(t, func() {
		if err := cmdSettings(ctx, c, &printer{}, nil); err != nil {
			t.Errorf("cmdSettings: %v", err)
		}
	})
	if !strings.Contains(out, "environment:    staging") {
		t.Errorf("`grain settings` printed %q, which does not contain the environment name", out)
	}
}
