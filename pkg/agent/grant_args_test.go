package agent_test

import (
	"slices"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
)

// The arguments a forked mcpserver needs before it serves the self-debug
// capability's own tools: the name of the grant, and where grain's own
// source is.
func TestGrantArgsCarriesTheGrantAndTheSourceDir(t *testing.T) {
	args := agent.GrantArgs(agent.RunConfig{
		Grants: []string{"self-debug"}, GrainSourceDir: "/usr/local/share/grain/src",
	})
	want := []string{"-grant", "self-debug", "-grain-src-dir", "/usr/local/share/grain/src"}
	if !slices.Equal(args, want) {
		t.Fatalf("GrantArgs = %v, want %v", args, want)
	}
}

// A deployment with no checkout of grain's source still gets the task
// half of the capability, so the grant is passed on its own rather than
// with an empty directory beside it.
func TestGrantArgsOmitsAnAbsentSourceDir(t *testing.T) {
	args := agent.GrantArgs(agent.RunConfig{Grants: []string{"self-debug"}})
	if want := []string{"-grant", "self-debug"}; !slices.Equal(args, want) {
		t.Fatalf("GrantArgs = %v, want %v", args, want)
	}
}

// Every grant a task holds travels, one -grant pair each -- what makes a
// fourth capability wanting these tools a name at either end and no
// change here. The source directory belongs to self-debug and is written
// beside it, not at the end of the list.
func TestGrantArgsCarriesEveryGrant(t *testing.T) {
	args := agent.GrantArgs(agent.RunConfig{
		Grants:         []string{"self-debug", "bootstrap-playbooks"},
		GrainSourceDir: "/usr/local/share/grain/src",
	})
	want := []string{
		"-grant", "self-debug", "-grain-src-dir", "/usr/local/share/grain/src",
		"-grant", "bootstrap-playbooks",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("GrantArgs = %v, want %v", args, want)
	}
}

// A grant with no source directory to go with it passes on its own, and
// the deployment's own checkout stays out of the arguments of a run that
// was never granted a way to read it.
func TestGrantArgsWithholdsTheSourceDirFromAnotherGrant(t *testing.T) {
	args := agent.GrantArgs(agent.RunConfig{
		Grants: []string{"bootstrap-playbooks"}, GrainSourceDir: "/usr/local/share/grain/src",
	})
	if want := []string{"-grant", "bootstrap-playbooks"}; !slices.Equal(args, want) {
		t.Fatalf("GrantArgs = %v, want %v", args, want)
	}
}

// An ordinary task passes nothing at all: its run's tool roster is
// exactly what it was before this existed, whatever else its dispatch
// happens to know.
func TestGrantArgsIsNothingWithoutAGrant(t *testing.T) {
	if args := agent.GrantArgs(agent.RunConfig{GrainSourceDir: "/src"}); len(args) != 0 {
		t.Fatalf("GrantArgs = %v, want none for a task holding no tool-granting capability", args)
	}
}
