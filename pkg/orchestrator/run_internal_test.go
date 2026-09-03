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

// A run whose tool call errored still says what it reached for first.
// Without this, a run that opened its pull request and then tripped over
// an unrelated tool error counts as never having opened one at all.
func TestOutcomeOfNamesTheToolsBehindAFailingCall(t *testing.T) {
	outcome, detail := outcomeOf(&agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "open_pull_request"},
		{Name: "run_command", Text: "no such file", IsError: true},
	}})
	if outcome != "failed" {
		t.Fatalf("outcome = %q, want %q", outcome, "failed")
	}
	// The failure itself is still the headline -- that is what a human
	// reading `grain get` came for.
	if !strings.Contains(detail, "no such file") {
		t.Errorf("detail = %q, want it to still lead with the tool error", detail)
	}
	if !strings.Contains(detail, "open_pull_request x1") {
		t.Errorf("detail = %q, want it to name the call the run made before failing", detail)
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
