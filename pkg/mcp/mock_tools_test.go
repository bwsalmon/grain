package mcp

import (
	"context"
	"strings"
	"testing"
)

// escapeHatchText returns each escape hatch's description and the
// confirmation its handler answers a well-formed call with -- the two
// pieces of text a dispatched run actually reads about these tools.
func escapeHatchText(t *testing.T) map[string]struct{ description, confirmation string } {
	t.Helper()
	args := map[string]map[string]any{
		"ask_question":       {"question": "which config file?"},
		"request_secret":     {"secret": "stripe-api-key", "reason": "the deploy script authenticates with it"},
		"comment_on_issue":   {"comment": "the answer is 4"},
		"propose_task":       {"title": "follow-up", "body": "the rest of it"},
		"add_review_comment": {"body": "this loop is quadratic"},
	}
	out := map[string]struct{ description, confirmation string }{}
	for _, tool := range NewMockTools(&MockSink{}) {
		call, ok := args[tool.Name]
		if !ok {
			t.Fatalf("NewMockTools registered %q, which this test has no call for", tool.Name)
		}
		res := tool.Handler(context.Background(), call)
		if res.IsError {
			t.Fatalf("%s answered a well-formed call with an error: %s", tool.Name, res.Text)
		}
		out[tool.Name] = struct{ description, confirmation string }{tool.Description, res.Text}
	}
	if len(out) != len(args) {
		t.Fatalf("NewMockTools returned %d tools, want %d", len(out), len(args))
	}
	return out
}

// TestEscapeHatchTextDescribesTheRelayRatherThanTheSink is the
// regression test for what these tools told every production run for as
// long as there was nothing downstream of them: that the call had been
// mocked, that no comment was posted, that a proposal needed a trigger
// label on a GitHub issue. orchestrator.ProcessResult relays
// ask_question, comment_on_issue and propose_task for real off
// agent.Result.ToolCalls, and has since tasks became rows, so every one
// of those sentences was false where it was read.
//
// It is pinned as a test because the falsehood is invisible from inside
// this package: MockSink really does mock, `grain mcpserver` really does
// build one and throw it away, and only the caller two packages over
// makes the text wrong. Nothing else fails when this drifts back -- the
// cost lands on a run that believes it (docs/agent-ergonomics.md,
// finding 1), which no test of grain's own behaviour would ever see.
func TestEscapeHatchTextDescribesTheRelayRatherThanTheSink(t *testing.T) {
	// Each of these is a sentence one of these tools used to carry, or
	// the vocabulary of the v1 workflow they used to describe: an issue
	// per task, a label to re-apply, a label to file a proposal under.
	// None of it survives in v2, where a task is a row and its
	// conversation is in grain's own UI.
	stale := []string{"mocked", "trigger label", "/lgtm", "new GitHub issue", "issue number"}
	for name, text := range escapeHatchText(t) {
		for _, phrase := range stale {
			if strings.Contains(strings.ToLower(text.description), strings.ToLower(phrase)) {
				t.Errorf("%s's description still says %q", name, phrase)
			}
			if strings.Contains(strings.ToLower(text.confirmation), strings.ToLower(phrase)) {
				t.Errorf("%s's confirmation still says %q", name, phrase)
			}
		}
	}
}

// TestRelayedEscapeHatchesConfirmWhatGrainDoesWithTheCall holds each of
// the three relayed hatches to saying, in its confirmation, the thing
// the agent has to know at the moment it calls: that the words go
// somewhere, when, and what happens to the task as a result. A
// confirmation is the only one of the two texts a run reads *after*
// deciding to call, and it is the one that decides what it does next --
// ask again, carry on, or stop.
func TestRelayedEscapeHatchesConfirmWhatGrainDoesWithTheCall(t *testing.T) {
	text := escapeHatchText(t)
	for _, tt := range []struct {
		tool string
		want []string
	}{
		// A question is relayed into the conversation, parks the task,
		// and ends the turn -- the contract ProcessResult's ordering
		// depends on, and the one thing here that is unchanged from v1.
		{"ask_question", []string{"relays", "conversation", "replies", "no further actions"}},
		// A secret request parks the task the same way, and its
		// confirmation has one more thing to be unambiguous about: the
		// value is not coming back. A run told otherwise waits for it,
		// or asks for it again in a question -- which is exactly the
		// plain-text comment this tool exists to avoid.
		{"request_secret", []string{"conversation", "parks", "never comes back to you", "no further actions"}},
		// A comment is relayed whatever else the run did, and completes
		// the task when it is all the run did.
		{"comment_on_issue", []string{"relays", "conversation", "closing note"}},
		// A proposal becomes a real task in the queue, and a human's
		// approval -- not a label -- is what lets it ever be dispatched.
		{"propose_task", []string{"real task", "unapproved", "approves"}},
	} {
		for _, want := range tt.want {
			if !strings.Contains(text[tt.tool].confirmation, want) {
				t.Errorf("%s's confirmation = %q, want it to say %q",
					tt.tool, text[tt.tool].confirmation, want)
			}
		}
	}
}

// TestRequestSecretRecordsTheNameAndNeverAValue pins the two halves of
// the tool that make it worth having at all: what it records (a name and
// a reason, which is everything grain relays) and what it refuses (a
// name nothing could be stored under, caught in the turn that made the
// call rather than by parking the task on a request no box can be built
// for).
func TestRequestSecretRecordsTheNameAndNeverAValue(t *testing.T) {
	sink := &MockSink{}
	tool := toolNamed(t, NewMockTools(sink), "request_secret")

	res := tool.Handler(context.Background(), map[string]any{
		"secret": "github-app/app-id", "reason": "the deploy needs to authenticate",
	})
	if res.IsError {
		t.Fatalf("well-formed call errored: %s", res.Text)
	}
	got := sink.SecretRequest()
	if got == nil || got.Secret != "github-app/app-id" || got.Reason != "the deploy needs to authenticate" {
		t.Fatalf("SecretRequest() = %+v, want the name and reason as given", got)
	}

	// First call wins, matching what orchestrator.ProcessResult relays
	// off agent.Result.ToolCalls.
	tool.Handler(context.Background(), map[string]any{"secret": "other-key", "reason": "second thoughts"})
	if sink.SecretRequest().Secret != "github-app/app-id" {
		t.Fatalf("SecretRequest() = %+v, want the first call's own name", sink.SecretRequest())
	}

	for _, bad := range []map[string]any{
		{"reason": "no name at all"},
		{"secret": "stripe-api-key"},
		{"secret": "stripe api key", "reason": "a name with a space in it"},
		{"secret": "../etc/passwd", "reason": "a path, not a name"},
		{"secret": "stripe//key", "reason": "an empty key"},
	} {
		if res := tool.Handler(context.Background(), bad); !res.IsError {
			t.Errorf("Handler(%v) = %q, want an error", bad, res.Text)
		}
	}
}

// toolNamed picks one tool out of a constructor's roster, failing rather
// than returning a zero Tool whose nil Handler would panic further down.
func toolNamed(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q in %d registered", name, len(tools))
	return Tool{}
}

// TestAddReviewCommentSaysItGoesNowhere is the other half, and the
// inverse problem: this one is registered on every run, promises a draft
// review a human will open, and is relayed by nothing at all
// (ProcessResult's own doc comment). Until a review dispatch exists to
// attach one to, both its texts have to say where the feedback really
// lands and name the tool that does reach a human, so a run can spend
// its turns on the one that works.
func TestAddReviewCommentSaysItGoesNowhere(t *testing.T) {
	text := escapeHatchText(t)["add_review_comment"]
	for _, where := range []struct{ what, s string }{
		{"description", text.description},
		{"confirmation", text.confirmation},
	} {
		if !strings.Contains(strings.ToLower(where.s), "nothing relays") {
			t.Errorf("add_review_comment's %s = %q, want it to say nothing relays these",
				where.what, where.s)
		}
		if !strings.Contains(where.s, "comment_on_issue") {
			t.Errorf("add_review_comment's %s = %q, want it to point at comment_on_issue",
				where.what, where.s)
		}
	}
}
