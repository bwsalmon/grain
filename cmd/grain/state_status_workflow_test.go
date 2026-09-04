package main

// What `grain state status` says about a deployment that could not
// install the check that runs on pull requests against its own state.
//
// The State pane already says it (state_workflow_test.go); this is the
// same question asked from a terminal by an operator on the host, who
// has no pane in front of them. It matters here for the reason it
// matters there: the refusal is deliberately not a sync failure, so
// every other line this command prints -- the remote, the head, the
// schema, the dispatch verdict -- is exactly what a repository whose
// check runs on every pull request prints, and a repository nothing
// validates is otherwise indistinguishable from a healthy one.

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

func TestStateStatusSaysWhenTheCheckCouldNotBeInstalled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	remote := bareRemote(t)
	refuseWorkflowPushes(t, remote)
	if err := staterepo.SaveSettings(dataDir, staterepo.Settings{Remote: remote}); err != nil {
		t.Fatalf("writing the state settings: %v", err)
	}
	_, db, err := openStore(dataDir)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer db.Close()
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		t.Fatalf("opening the state repository: %v", err)
	}
	// Seeding pushes the database and is refused the workflow, which is
	// the whole of what happens on a deployment whose credential may not
	// write workflows: it works, and one file is missing.
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding against a remote that refuses workflows: %v", err)
	}

	var out bytes.Buffer
	if err := stateStatus(ctx, dataDir, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{
		// The file that is missing, so an operator holding this output
		// knows what to go and commit...
		staterepo.WorkflowFile,
		// ...that grain has been refused it rather than never offered it,
		// and when it last tried...
		"last tried",
		// ...and both ways out, which are the two the pane names: install
		// it by hand, or tell grain to stop offering it.
		"grain state ci",
		`"noWorkflow": true`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("`grain state status` never mentions %q:\n%s", want, out.String())
		}
	}

	// And it stops saying so once the file is there, however it got
	// there -- `grain state ci` in somebody's clone, committed with a
	// credential that may push it, is what this output sends an operator
	// off to do, and a command that went on complaining afterwards would
	// be telling them to do it twice.
	if _, err := staterepo.EnsureWorkflow(repo.Dir(), staterepo.DefaultCheckImage, false); err != nil {
		t.Fatalf("installing the workflow by hand: %v", err)
	}
	out.Reset()
	if err := stateStatus(ctx, dataDir, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out.String(), "grain state ci") {
		t.Errorf("`grain state status` still reports the check as missing with the file in the tree:\n%s", out.String())
	}
}
