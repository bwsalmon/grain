package agent_test

import (
	"slices"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
)

// The flags a forked mcpserver needs before it serves the self-debug
// capability's own tools: the fact of the grant, and where grain's own
// source is.
func TestSelfDebugArgsCarriesTheGrantAndTheSourceDir(t *testing.T) {
	args := agent.SelfDebugArgs(agent.RunConfig{SelfDebug: true, GrainSourceDir: "/usr/local/share/grain/src"})
	want := []string{"-self-debug", "-grain-src-dir", "/usr/local/share/grain/src"}
	if !slices.Equal(args, want) {
		t.Fatalf("SelfDebugArgs = %v, want %v", args, want)
	}
}

// A deployment with no checkout of grain's source still gets the
// capability's tools -- they say on the call that there is nothing to
// read -- so the grant is passed on its own rather than with an empty
// directory beside it.
func TestSelfDebugArgsOmitsAnAbsentSourceDir(t *testing.T) {
	args := agent.SelfDebugArgs(agent.RunConfig{SelfDebug: true})
	if want := []string{"-self-debug"}; !slices.Equal(args, want) {
		t.Fatalf("SelfDebugArgs = %v, want %v", args, want)
	}
}

// An ordinary task passes nothing at all: its run's tool roster is
// exactly what it was before this existed, whatever else its dispatch
// happens to know.
func TestSelfDebugArgsIsNothingWithoutTheGrant(t *testing.T) {
	if args := agent.SelfDebugArgs(agent.RunConfig{GrainSourceDir: "/src"}); len(args) != 0 {
		t.Fatalf("SelfDebugArgs = %v, want none for a task without the grant", args)
	}
}
