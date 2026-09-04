package main

// `grain state format` and `grain state ci` end to end: a directory in,
// files out, with no data directory, no daemon and no store -- the same
// shape `grain state check` has, and for the same reason. An operator
// running these has a clone of a repository and nothing else yet.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/staterepo"
)

func TestStateFormatLaysOutAnEmptyRepositoryWithNoDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := runState([]string{"format", dir}); err != nil {
		t.Fatalf("formatting an empty directory: %v", err)
	}
	for _, name := range []string{staterepo.ReadmeFile, staterepo.IgnoreFile, staterepo.WorkflowFile} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
	// Formatting commits nothing and needs no repository: what it
	// produces is files for whoever ran it to commit themselves, with a
	// credential that may write workflows.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("formatting created a git repository; it is meant to run inside a clone the operator made")
	}
}

// The name says "format", and a directory with a deployment's state in
// it is not something to format. `grain state ci` is the command for
// that case, and the refusal has to say so.
func TestStateFormatRefusesARepositoryThatAlreadyHoldsADump(t *testing.T) {
	dir := exportedState(t)
	err := runState([]string{"format", dir})
	if err == nil {
		t.Fatal("formatting a repository that already holds a dump was allowed")
	}
	if !strings.Contains(err.Error(), "grain state ci") {
		t.Errorf("the refusal does not point at the command that does work here: %v", err)
	}
	// And it changed nothing on its way to refusing.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(staterepo.WorkflowFile))); err == nil {
		t.Error("the refused format wrote the workflow anyway")
	}
}

// The other half: a repository a deployment has been pushing to since
// before this workflow existed gets the CI step without being formatted,
// and the dump it holds still checks out afterwards -- a workflow file
// is not something `grain state check` should have an opinion about.
func TestStateCIAddsTheCheckToARepositoryThatIsAlreadyInUse(t *testing.T) {
	dir := exportedState(t)
	if err := runState([]string{"ci", dir}); err != nil {
		t.Fatalf("adding the CI step: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(staterepo.WorkflowFile)))
	if err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}
	if !strings.Contains(string(body), "state check /state") {
		t.Errorf("the workflow does not run the check:\n%s", body)
	}
	if err := runState([]string{"check", dir}); err != nil {
		t.Fatalf("a repository with the CI step in it no longer checks out: %v", err)
	}
}

func TestStateCIAcceptsAnImageToPinTheCheckTo(t *testing.T) {
	dir := exportedState(t)
	const image = "ghcr.io/bwsalmon/grain/grain:sha-abc1234"
	if err := runState([]string{"ci", "-image", image, dir}); err != nil {
		t.Fatalf("adding the CI step: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(staterepo.WorkflowFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), image) {
		t.Errorf("-image did not reach the workflow:\n%s", body)
	}
}
