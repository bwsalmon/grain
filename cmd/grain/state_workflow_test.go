package main

// What the State pane is told about a deployment that could not install
// the check that runs on pull requests against its own state.
//
// The refusal is deliberately not a sync failure -- pkg/staterepo undoes
// the commit GitHub would not take and goes on pushing the database,
// because a deployment must not stop syncing over a file worth one CI
// step -- so nothing about it ever reaches the manager's lastErr. That
// is what makes this worth a test of its own: without it a repository
// whose changes nothing validates is indistinguishable, from the pane an
// operator actually looks at, from one whose check runs on every pull
// request.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// refuseWorkflowPushes makes a bare remote answer a push that carries a
// file under .github/workflows the way GitHub answers one made with a
// credential that has no workflows permission. The wording matters: it
// is all git hands back, and so all grain has to recognise the refusal
// by.
func refuseWorkflowPushes(t *testing.T, remote string) {
	t.Helper()
	hook := "#!/bin/sh\n" +
		"while read -r old new ref; do\n" +
		"  if [ \"$old\" = \"0000000000000000000000000000000000000000\" ]; then\n" +
		"    files=$(git ls-tree -r --name-only \"$new\")\n" +
		"  else\n" +
		"    files=$(git diff --name-only \"$old\" \"$new\")\n" +
		"  fi\n" +
		"  case \"$files\" in\n" +
		"    *.github/workflows/*)\n" +
		"      echo 'refusing to allow a GitHub App to create or update workflow " +
		"`.github/workflows/grain-state-check.yml` without `workflows` permission' >&2\n" +
		"      exit 1\n" +
		"      ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatalf("installing the pre-receive hook: %v", err)
	}
}

func TestTheStatePaneSaysWhenGrainCouldNotInstallTheCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	remote := bareRemote(t)
	refuseWorkflowPushes(t, remote)
	_, db, err := openStore(dataDir)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer db.Close()
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: stateRepoDir(dataDir), Remote: remote})
	if err != nil {
		t.Fatalf("opening the state repository: %v", err)
	}
	manager := newStateManager(dataDir, db, repo, openSecrets(dataDir), nil)

	// Seeding pushes the database and is refused the workflow, which is
	// the whole of what happens on a deployment whose credential may not
	// write workflows: it works, and one file is missing.
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding against a remote that refuses workflows: %v", err)
	}

	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatalf("reading the pane's status: %v", err)
	}
	if !status.WorkflowRefused {
		t.Fatalf("the pane says nothing about a check that was never installed: %+v", status)
	}
	if status.WorkflowFile != staterepo.WorkflowFile {
		t.Errorf("the pane does not name the file to install: %q", status.WorkflowFile)
	}
	if status.WorkflowRefusedAt == nil {
		t.Error("the pane cannot say when grain was last refused")
	}
	// Reported as a condition, not as a broken sync: this deployment is
	// pushing its database perfectly happily, and an Error here would
	// have the pane claim otherwise.
	if status.Error != "" || status.Diverged || status.RemoteAhead {
		t.Errorf("a refused workflow was reported as a failed sync: %+v", status)
	}

	// And it stops saying so once the file is there, however it got
	// there -- `grain state ci` in somebody's clone, committed with a
	// credential that may push it, is what the pane sends an operator off
	// to do.
	if _, err := staterepo.EnsureWorkflow(repo.Dir(), staterepo.DefaultCheckImage, false); err != nil {
		t.Fatalf("installing the workflow by hand: %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatalf("reading the pane's status: %v", err)
	}
	if status.WorkflowRefused {
		t.Errorf("the pane still reports the check as missing with the file in the tree: %+v", status)
	}
}
