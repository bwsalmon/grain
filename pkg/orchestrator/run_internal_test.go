package orchestrator

import (
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
)

// --- what a finished run's detail records about its tool calls ---------
//
// The question these exist for is not "is the sentence nicely worded".
// It is whether a deployment can answer, from its own store, whether a
// tool it built and named in the prompt is being called at all -- see
// outcomeOf's own doc comment. agent.Result is never persisted and
// Result.Transcript is optional prose, so task_run.detail is the whole
// record, and a successful run used to leave it blank.

// The successful ending is the one that matters most here: a run that
// pushed, read CI, repaired and pushed again is a run that succeeds, so
// the path that recorded nothing was exactly the path worth measuring.
func TestOutcomeOfRecordsWhatASuccessfulRunCalled(t *testing.T) {
	outcome, detail := outcomeOf(&agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "run_command"},
		{Name: "open_pull_request"},
		{Name: "pull_request_status"},
		{Name: "run_command"},
		{Name: "pull_request_status"},
	}})
	if outcome != "succeeded" {
		t.Fatalf("outcome = %q, want %q", outcome, "succeeded")
	}
	for _, want := range []string{
		"5 tool call",
		"open_pull_request x1",
		"pull_request_status x2",
		"run_command x2",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}
}

// An errored tool call is a result the agent reads and works around, not
// a broken run -- pkg/mcp's run_command sets IsError for any non-zero
// exit status, so a grep that matched nothing used to fail a run that
// went on to open its pull request. See outcomeOf's own doc comment for
// what that cost: task_streak counts anything other than "succeeded", so
// those bogus failures backed a healthy task off and eventually stopped
// dispatching it altogether.
func TestOutcomeOfDoesNotFailARunOverAnErroringToolCall(t *testing.T) {
	outcome, detail := outcomeOf(&agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "open_pull_request"},
		{Name: "run_command", Text: "exit=1\nstdout:\n\nstderr:\n", IsError: true},
	}})
	if outcome != "succeeded" {
		t.Fatalf("outcome = %q, want %q", outcome, "succeeded")
	}
	// The errors are still on the record -- both which tool they came
	// from and how many of the run's calls did not land.
	if !strings.Contains(detail, "run_command(error) x1") {
		t.Errorf("detail = %q, want it to name the tool that errored", detail)
	}
	if !strings.Contains(detail, "1 of them returned an error") {
		t.Errorf("detail = %q, want it to count the errored calls", detail)
	}
	if !strings.Contains(detail, "open_pull_request x1") {
		t.Errorf("detail = %q, want it to name every call the run made", detail)
	}
}

// The counterpart: a run with nothing wrong says nothing about errors,
// rather than reporting zero of them.
func TestOutcomeOfSaysNothingAboutErrorsWhenThereWereNone(t *testing.T) {
	_, detail := outcomeOf(&agent.Result{ToolCalls: []agent.ToolCall{{Name: "run_command"}}})
	if strings.Contains(detail, "returned an error") {
		t.Errorf("detail = %q, want no mention of errors on a run that had none", detail)
	}
}

// The one ending with nothing to summarise keeps its own diagnosis: an
// agent that never called a tool did not fail at the work so much as
// never attempt it, and a count of zero says that worse than the
// sentence does.
func TestOutcomeOfStillDistinguishesARunThatNeverStarted(t *testing.T) {
	outcome, detail := outcomeOf(&agent.Result{})
	if outcome != "failed" {
		t.Fatalf("outcome = %q, want %q", outcome, "failed")
	}
	if !strings.Contains(detail, "no tool calls at all") {
		t.Errorf("detail = %q, want it to say the agent never called a tool", detail)
	}
}

// --- what the prompt says about the repo's setup command ---------------
//
// A run reads its prompt once, at turn 1, and there is nothing in the
// sandbox that says a setup command was ever run in it. So these pin the
// two facts a run has to be given: that the setup happened at all, and
// -- the case this whole field exists for -- that it failed, before the
// run reads the first broken build as its own doing.

func TestSetupSectionSaysNothingWhenTheRepoHasNoSetupCommand(t *testing.T) {
	if got := setupSection(nil); got != "" {
		t.Errorf("setupSection(nil) = %q, want nothing at all", got)
	}
}

func TestSetupSectionNamesASucceededSetupSoItIsNotRunAgain(t *testing.T) {
	got := setupSection(&setupResult{Command: "make deps", ExitCode: 0, Output: "exit=0"})
	for _, want := range []string{"make deps", "succeeded"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt section = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "FAILED") {
		t.Errorf("prompt section = %q, want nothing said about a failure", got)
	}
}

func TestSetupSectionTellsTheRunAFailedSetupIsNotItsChange(t *testing.T) {
	got := setupSection(&setupResult{
		Command:  "make deps",
		ExitCode: 2,
		Output:   "exit=2\nstderr:\nno such package: widgetlib",
	})
	// The exit status, the command, and the tail -- and the sentence that
	// makes the difference between a run working around the failure and a
	// run "fixing" the code until the broken tree stops complaining.
	for _, want := range []string{"make deps", "exit 2", "no such package: widgetlib", "not your change"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt section = %q, want it to mention %q", got, want)
		}
	}
}
