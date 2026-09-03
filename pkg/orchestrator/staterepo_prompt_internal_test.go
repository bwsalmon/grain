package orchestrator

// What a run is told when the repo it has been handed is grain's own
// state (pkg/staterepo) rather than source code: that the files are a
// database dump, which of them are settings, and which are grain's own
// record of what it did.
//
// Internal, like checkout_internal_test.go and for the same reason: the
// fact this turns on is one prepareCheckout reads out of the cloned tree,
// and there is nothing exported to reach either half through that would
// not also be running an agent.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// seedStateRemote makes the remote look like a state repository: the
// tables/ directory and the schema-version stamp staterepo.Export
// writes, which together are what prepareCheckout recognises.
func seedStateRemote(t *testing.T, base string, repo model.RepoRef) {
	t.Helper()
	seed := seedRemote(t, base, repo)
	if err := os.MkdirAll(filepath.Join(seed, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "tables", "task_template.json"), []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "schema-version"), []byte("16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "--all", ".")
	git(t, seed, "commit", "--quiet", "-m", "state")
	git(t, seed, "push", "--quiet", "origin", "main")
}

func TestPrepareCheckoutRecognisesAStateRepository(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "grain-state"}
	seedStateRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, "")
	if err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if !prepared.StateRepo {
		t.Fatal("a checkout holding tables/ and schema-version was not recognised as grain's state")
	}
	if prepared.Dir != CheckoutDir {
		t.Fatalf("prepareCheckout returned %q, want %q", prepared.Dir, CheckoutDir)
	}
}

// The probe must not change what an ordinary repo's dispatch looks like,
// and above all must not fail one: it runs under `set -e` in the same
// script as the clone.
func TestPrepareCheckoutLeavesAnOrdinaryRepoAlone(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, "")
	if err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if prepared.StateRepo {
		t.Fatal("a repo with no dump in it was taken for grain's state")
	}
	if got, want := git(t, filepath.Join(root, CheckoutDir), "rev-parse", "--abbrev-ref", "HEAD"),
		model.BranchName("t1"); got != want {
		t.Fatalf("checked-out branch = %q, want %q", got, want)
	}
}

func TestStateRepoSectionIsSilentForAnOrdinaryCheckout(t *testing.T) {
	if got := stateRepoSection(checkout{Dir: CheckoutDir}); got != "" {
		t.Fatalf("an ordinary checkout got a state-repository section: %q", got)
	}
}

// Every fact the section is for: the layout, which tables are settings,
// which are grain's own observations, and the check that keeps an
// unloadable dump out of the deployment.
func TestStateRepoSectionSaysWhatTheLayoutIs(t *testing.T) {
	got := stateRepoSection(checkout{Dir: CheckoutDir, StateRepo: true})
	for _, want := range []string{
		"tables/<name>.json",
		"one object per row",
		"declared order",
		"repo_config",
		"grain_config",
		"task_run",
		"task_observation",
		"grain state check .",
		"secrets.enc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the state-repository section never mentions %q:\n%s", want, got)
		}
	}
	// Named from the list grain itself acts on, so the prompt cannot
	// call a table settings that Apply will not import.
	for _, table := range staterepo.SettingsTables {
		if !strings.Contains(got, table) {
			t.Errorf("the section does not name the settings table %q:\n%s", table, got)
		}
	}
}

// The section reaches a real prompt, not just its own function: the
// checkout is the only thing that knows, and prepareCapabilities is
// where what it knows is turned into words.
func TestPrepareCapabilitiesCarriesTheStateRepoSection(t *testing.T) {
	ctx := context.Background()
	task := model.Task{ID: "t1", Title: "Change a template", Target: &model.RepoRef{Owner: "acme", Name: "grain-state"}}
	cc := model.CapabilityContext{Task: task}
	_, prompt, err := prepareCapabilities(ctx, nil, cc, t.TempDir(), nil, nil, History{}, nil,
		checkout{Dir: CheckoutDir, StateRepo: true}, false, "", DefaultMaxRunRuntime)
	if err != nil {
		t.Fatalf("prepareCapabilities: %v", err)
	}
	if !strings.Contains(prompt, "grain's own state") {
		t.Fatalf("the dispatch prompt never says the repo is grain's state:\n%s", prompt)
	}
}
