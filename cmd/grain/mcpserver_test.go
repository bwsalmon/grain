package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/mcp"
)

// pull_request_status and wait_for_checks have to be registered whatever
// the flags said, so that a run whose task has no repo gets each tool's
// own explanation rather than "unknown tool pull_request_status" -- which
// reads like grain is broken rather than like a task with nothing to
// look at.
func TestPullRequestToolsAlwaysRegisterTheTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dataDir string
		repo    string
		branch  string
	}{
		{name: "nothing configured"},
		{name: "repo but no branch", repo: "acme/widgets"},
		{name: "repo but no data directory", repo: "acme/widgets", branch: "grain/task-9"},
		{name: "unparseable repo", dataDir: t.TempDir(), repo: "not-a-repo", branch: "grain/task-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := pullRequestTools(tc.dataDir, "github.com", false, tc.repo, tc.branch)
			var names []string
			for _, tool := range tools {
				names = append(names, tool.Name)
				res := tool.Handler(context.Background(), map[string]any{})
				if !res.IsError || !strings.Contains(res.Text, "no GitHub repository configured") {
					t.Errorf("%s answered %q (isError=%v), want the "+
						"nothing-configured explanation", tool.Name, res.Text, res.IsError)
				}
			}
			if want := []string{"pull_request_status", "wait_for_checks"}; !slices.Equal(names, want) {
				t.Fatalf("pullRequestTools registered %v, want %v", names, want)
			}
		})
	}
}

// The happy path: a repo, a branch and a data directory produce a real
// reader scoped to exactly that repo and branch. A data directory with
// no secrets/github in it yields an empty credential ladder rather than
// an error (gitproxy.readStringMap) -- that is a deployment reading a
// public repo unauthenticated, a supported shape and not a failure.
func TestPullRequestReaderScopesToTheRunsOwnBranch(t *testing.T) {
	client, scope, err := pullRequestReader(t.TempDir(), "github.com", false, "acme/widgets", "grain/task-9")
	if err != nil {
		t.Fatalf("pullRequestReader: %v", err)
	}
	if client == nil {
		t.Fatal("client = nil, want a GitHub reader")
	}
	want := mcp.PullRequestScope{Owner: "acme", Repo: "widgets", Branch: "grain/task-9"}
	if scope != want {
		t.Errorf("scope = %+v, want %+v", scope, want)
	}
}

// An unset -pr-repo is a task with no repo, not a misconfiguration:
// there is nothing to warn an operator about, so it must not come back
// as an error.
func TestPullRequestReaderTreatsNoRepoAsNoError(t *testing.T) {
	client, scope, err := pullRequestReader("/data", "github.com", false, "", "")
	if err != nil {
		t.Fatalf("pullRequestReader err = %v, want nil for a task with no repo", err)
	}
	if client != nil {
		t.Errorf("client = %+v, want nil", client)
	}
	if scope != (mcp.PullRequestScope{}) {
		t.Errorf("scope = %+v, want the zero scope", scope)
	}
}

// -run-deadline is how a run's wall-clock deadline reaches the tools it
// calls (agent.RunDeadlineArgs on the way in,
// mcp.Registry.AnnounceDeadline on the way out), so it has to survive
// the round trip through the command line as the same instant.
func TestParsedRunDeadlineReadsTheFlagBack(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if got := parsedRunDeadline(at.Format(time.RFC3339)); !got.Equal(at) {
		t.Errorf("parsedRunDeadline = %s, want %s", got, at)
	}
}

// Unset is every caller with no deadline to give -- pkg/mcp's own tests,
// tests/e2e, a `grain mcpserver` run by hand -- and a malformed value is
// an operator's problem, not the run's: neither may cost a run the tools
// that are its only way to touch its sandbox, so both come back as "say
// nothing about time" rather than as a failure.
func TestParsedRunDeadlineIsZeroWithoutAUsableValue(t *testing.T) {
	for _, value := range []string{"", "not-a-time", "2026-03-04 05:06:07"} {
		if got := parsedRunDeadline(value); !got.IsZero() {
			t.Errorf("parsedRunDeadline(%q) = %s, want the zero time", value, got)
		}
	}
}
