package main

// Which repository this deployment's settings live in, as the reconcile
// loop asks for it: stateManager.settingsRepo, which is what every
// dispatched run is told through orchestrator.Config.StateRepo so an
// agent can tell the settings repo of the environment it is working in
// from any other repository -- including another grain's state dump,
// which looks identical in a checkout.

import (
	"context"
	"os/exec"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

func TestSettingsRepoNamesTheRepositoryThisDeploymentReads(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: stateRepoDir(dataDir)})
	if err != nil {
		t.Fatalf("opening a state repository: %v", err)
	}
	manager := newStateManager(dataDir, nil, repo, openSecrets(dataDir), nil)

	// Local-only: there is no repository to name, and a run told one
	// would be told to open a pull request against something that does
	// not exist.
	if got := manager.settingsRepo(); got != (model.RepoRef{}) {
		t.Fatalf("a local-only installation named %v as its settings repo, want none", got)
	}

	if err := repo.SetRemote(ctx, "https://github.com/acme/grain-state.git"); err != nil {
		t.Fatalf("pointing the repository at a remote: %v", err)
	}
	want := model.RepoRef{Owner: "acme", Name: "grain-state"}
	if got := manager.settingsRepo(); got != want {
		t.Fatalf("settingsRepo() = %v, want %v", got, want)
	}
}

// The answer is read per call, not snapshotted: adopting a different
// repository has to reach the next run dispatched rather than the next
// restart, the same rule the git proxy's own forbidden set follows.
func TestSettingsRepoFollowsAnAdoptWithoutARestart(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: stateRepoDir(dataDir)})
	if err != nil {
		t.Fatalf("opening a state repository: %v", err)
	}
	manager := newStateManager(dataDir, nil, repo, openSecrets(dataDir), nil)
	if err := repo.SetRemote(ctx, "https://github.com/acme/grain-state.git"); err != nil {
		t.Fatalf("pointing the repository at a remote: %v", err)
	}
	if err := repo.SetRemote(ctx, "git@github.com:acme/other-state.git"); err != nil {
		t.Fatalf("pointing the repository somewhere else: %v", err)
	}
	want := model.RepoRef{Owner: "acme", Name: "other-state"}
	if got := manager.settingsRepo(); got != want {
		t.Fatalf("settingsRepo() after the remote changed = %v, want %v", got, want)
	}
}
