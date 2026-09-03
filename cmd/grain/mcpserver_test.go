package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/mcp"
)

// pull_request_status has to be registered whatever the flags said, so
// that a run whose task has no repo gets the tool's own explanation
// rather than "unknown tool pull_request_status" -- which reads like
// grain is broken rather than like a task with nothing to look at.
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
			if len(tools) != 1 || tools[0].Name != "pull_request_status" {
				t.Fatalf("pullRequestTools = %+v, want exactly pull_request_status", tools)
			}
			res := tools[0].Handler(context.Background(), map[string]any{})
			if !res.IsError || !strings.Contains(res.Text, "no GitHub repository configured") {
				t.Errorf("handler answered %q (isError=%v), want the "+
					"nothing-configured explanation", res.Text, res.IsError)
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
