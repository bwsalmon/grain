package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestHealthFromClosedStateReadsClosedRegardlessOfChecks(t *testing.T) {
	failure := "failure"
	got := healthFrom(github.PullRequestDetail{State: "closed"}, []github.CheckRun{
		{Status: "completed", Conclusion: &failure},
	}, true)
	if got != model.PrClosed {
		t.Fatalf("got %q, want closed", got)
	}
}

func TestHealthFromUnknownMergeabilityWithNoFailingChecksIsUnknown(t *testing.T) {
	got := healthFrom(github.PullRequestDetail{State: "open"}, nil, true)
	if got != model.PrUnknown {
		t.Fatalf("got %q, want unknown", got)
	}
}

func TestHealthFromNotMergeableIsConflicted(t *testing.T) {
	no := false
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &no}, nil, true)
	if got != model.PrConflicted {
		t.Fatalf("got %q, want conflicted", got)
	}
}

func TestHealthFromAFailedCompletedCheckIsFailing(t *testing.T) {
	yes := true
	failure := "failure"
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "completed", Conclusion: &failure},
	}, true)
	if got != model.PrFailing {
		t.Fatalf("got %q, want failing", got)
	}
}

func TestHealthFromAnInProgressCheckIsNotYetFailing(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "in_progress"},
	}, true)
	if got != model.PrClean {
		t.Fatalf("got %q, want clean (an in-progress check is not a failure)", got)
	}
}

func TestHealthFromMergeableWithNoFailingChecksIsClean(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "completed", Conclusion: strPtr("success")},
	}, true)
	if got != model.PrClean {
		t.Fatalf("got %q, want clean", got)
	}
}

func strPtr(s string) *string { return &s }

// --- checks the credential cannot read ----------------------------------

// The dangerous reading of an unreadable Checks API is "no checks came
// back, so nothing is failing" -- identical, at this function, to a
// genuinely green PR. A deployment on a scoped PAT gets that answer for
// every PR forever, so reading it as clean would auto-merge PRs with CI
// red.
func TestHealthFromUnknownChecksIsNeverClean(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, nil, false)
	if got == model.PrClean {
		t.Fatal("unreadable check runs read as clean: a PR with failing CI would be auto-merged")
	}
	if got != model.PrUnknown {
		t.Fatalf("got %q, want unknown", got)
	}
}

// Both facts healthFrom reads straight off the PR stay authoritative
// without checks: neither needs the Checks API, and a deployment that
// cannot reach it must still close out merged PRs and notice conflicts.
func TestHealthFromClosedAndConflictedSurviveUnknownChecks(t *testing.T) {
	if got := healthFrom(github.PullRequestDetail{State: "closed"}, nil, false); got != model.PrClosed {
		t.Errorf("closed PR with unreadable checks = %q, want closed", got)
	}
	no := false
	if got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &no}, nil, false); got != model.PrConflicted {
		t.Errorf("conflicted PR with unreadable checks = %q, want conflicted", got)
	}
}

func TestCheckRunsForReportsAForbiddenReadAsUnknownNotAnError(t *testing.T) {
	client := &checkRunsClient{err: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch")
	if err != nil {
		t.Fatalf("a 403 must not fail the sync: %v", err)
	}
	if known {
		t.Error("checks reported known after a 403")
	}
	if checks != nil {
		t.Errorf("checks = %v, want nil", checks)
	}
}

// ChecksUnavailable is what lets pkg/ui warn an operator that Submit will
// never actually merge anything on this deployment (bwsalmon/agents#483)
// -- it has to flip alongside the log line checkRunsFor already prints on
// a 403, or that warning would never appear either.
func TestChecksUnavailableReflectsAForbiddenRead(t *testing.T) {
	client := &checkRunsClient{err: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}}
	if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch"); err != nil {
		t.Fatalf("a 403 must not fail the sync: %v", err)
	}
	if !ChecksUnavailable() {
		t.Error("ChecksUnavailable() = false after a 403 from ListCheckRuns, want true")
	}
}

// Only the one permission GitHub offers no way to hold is tolerated.
// Anything else -- a 404, a 500, a transport failure -- is still a real
// error, and swallowing it would hide a broken deployment behind the
// same silent "unknown".
func TestCheckRunsForStillFailsOnEveryOtherError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not found", &github.Error{Status: 404}},
		{"server error", &github.Error{Status: 500}},
		{"transport", errors.New("dial tcp: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &checkRunsClient{err: tc.err}
			if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch"); err == nil {
				t.Fatal("expected the error to propagate")
			}
		})
	}
}

func TestCheckRunsForPassesASuccessfulReadThrough(t *testing.T) {
	client := &checkRunsClient{checks: []github.CheckRun{{Name: "build", Status: "completed"}}}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch")
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Error("a successful read must report the checks as known")
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Errorf("checks = %v, want the one build run", checks)
	}
}

func testPullRequestRef() model.PullRequestRef {
	return model.PullRequestRef{
		Repo:   model.RepoRef{Owner: "owner", Name: "repo"},
		Number: 1,
	}
}

// checkRunsClient is a github.Client that only ListCheckRuns is ever
// called on -- every other method is embedded and would panic on a nil
// interface, which is the point: checkRunsFor must touch nothing else.
type checkRunsClient struct {
	github.Client
	checks []github.CheckRun
	err    error
}

func (c *checkRunsClient) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.checks, nil
}

// --- what a no-action run reports --------------------------------------

func TestToolCallSummary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls []agent.ToolCall
		want  string
	}{
		// The most informative case: an agent that called nothing did not
		// fail to act so much as never start.
		{"no calls at all", nil, ""},
		{"one call", []agent.ToolCall{{Name: "run_command"}}, " [run_command x1]"},
		{"repeats are counted", []agent.ToolCall{
			{Name: "run_command"}, {Name: "run_command"}, {Name: "read_file"},
		}, " [read_file x1, run_command x2]"},
		// An erroring tool is a different signal from a working one --
		// four runs of read_file(error) says the sandbox is wrong, not the
		// model.
		{"errors are distinguished", []agent.ToolCall{
			{Name: "read_file"}, {Name: "read_file", IsError: true},
		}, " [read_file x1, read_file(error) x1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCallSummary(&agent.Result{ToolCalls: tc.calls})
			if got != tc.want {
				t.Errorf("toolCallSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// FinalText is model output and a sandbox can hold a .git-credentials, so
// the bound is not advisory.
func TestTruncateBoundsModelOutput(t *testing.T) {
	if got := truncate("", 10); got != "<empty>" {
		t.Errorf("empty final text = %q, want <empty>", got)
	}
	if got := truncate("   \n ", 10); got != "<empty>" {
		t.Errorf("whitespace-only final text = %q, want <empty>", got)
	}
	if got := truncate("short", 10); got != `"short"` {
		t.Errorf("short text = %q, want it quoted whole", got)
	}
	long := strings.Repeat("x", 40)
	got := truncate(long, 10)
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("long text = %q, want it marked as truncated", got)
	}
	if len(got) > 10+len(`""... (truncated)`) {
		t.Errorf("long text = %q, longer than the bound plus its marker", got)
	}
}

// --- re-checking a branch that is not there yet ------------------------

type branchClient struct {
	github.Client
	answers []bool
	err     error
	calls   int
	// repoErr, when set, is what the repository-visibility probe on the
	// negative path returns -- the "this client cannot see the repo at
	// all, so its 404s mean nothing" case.
	repoErr   error
	repoCalls int
}

func (b *branchClient) DefaultBranch(owner, repo string) (string, error) {
	b.repoCalls++
	if b.repoErr != nil {
		return "", b.repoErr
	}
	return "main", nil
}

func (b *branchClient) BranchExists(owner, repo, branch string) (bool, error) {
	b.calls++
	if b.err != nil {
		return false, b.err
	}
	if b.calls-1 < len(b.answers) {
		return b.answers[b.calls-1], nil
	}
	return false, nil
}

func withNoSleep(t *testing.T) {
	t.Helper()
	prev := branchExistsSleep
	branchExistsSleep = func(time.Duration) {}
	t.Cleanup(func() { branchExistsSleep = prev })
}

// The case this exists for: the push landed, GitHub had not caught up on
// the first read. Believing that first answer records no_action and
// abandons work already on the remote.
func TestBranchExistsSettledReChecksANegative(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{answers: []bool{false, false, true}}
	exists, err := branchExistsSettled(c, "o", "r", "grain/task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("gave up on a branch that appeared on the third read")
	}
	if c.calls != 3 {
		t.Errorf("calls = %d, want 3", c.calls)
	}
}

// A positive costs nothing extra: it is taken at once.
func TestBranchExistsSettledReturnsAPositiveImmediately(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{answers: []bool{true}}
	if _, err := branchExistsSettled(c, "o", "r", "b"); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 -- a positive must not be re-checked", c.calls)
	}
}

// A branch that genuinely is not there still comes back false, bounded.
func TestBranchExistsSettledGivesUpBounded(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{}
	exists, err := branchExistsSettled(c, "o", "r", "b")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("reported a branch that never appeared")
	}
	if c.calls != branchExistsRetries {
		t.Errorf("calls = %d, want %d", c.calls, branchExistsRetries)
	}
}

// An error is the caller's to report, not something to sit on: 404 is
// already handled inside BranchExists, so anything reaching here is a
// real failure.
func TestBranchExistsSettledDoesNotRetryAnError(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{err: errors.New("500 from github")}
	if _, err := branchExistsSettled(c, "o", "r", "b"); err == nil {
		t.Fatal("expected the error to propagate")
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 -- an error must not be retried", c.calls)
	}
}

// --- what the no_action detail records ---------------------------------

// outcomeOf already distinguishes "called tools and achieved nothing"
// from "made no tool calls at all", and the no_action overwrite used to
// replace both with one fixed sentence -- destroying the more useful of
// the two on its way past.
func TestNoActionDetailKeepsTheDiagnosis(t *testing.T) {
	got := noActionDetail(&agent.Result{})
	if !strings.Contains(got, "no tool calls at all") {
		t.Errorf("detail = %q, want it to say the run made no tool calls", got)
	}

	got = noActionDetail(&agent.Result{ToolCalls: []agent.ToolCall{
		{Name: "run_command"}, {Name: "run_command"}, {Name: "read_file"},
	}})
	for _, want := range []string{"3 tool call", "run_command x2", "read_file x1"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail = %q, want it to contain %q", got, want)
		}
	}
}

// Both forms still say what did not happen -- that is the part a reader
// is looking for when a task shows no pull request.
func TestNoActionDetailStillNamesWhatIsMissing(t *testing.T) {
	for _, r := range []*agent.Result{
		{},
		{ToolCalls: []agent.ToolCall{{Name: "run_command"}}},
	} {
		if got := noActionDetail(r); !strings.Contains(got, "without pushing a branch") {
			t.Errorf("detail = %q, want it to still name the missing ending", got)
		}
	}
}

// A negative is only believed once the repository itself reads back. A
// client that cannot see the repo 404s identically to one looking at a
// repo with no such branch, and the caller ends the task on that
// difference -- see branchExistsSettled's own comment for the live
// failure (a REST transport aimed at github.com rather than
// api.github.com) this catches.
func TestBranchExistsSettledRefusesToBelieveAnUnreadableRepo(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{repoErr: &github.Error{Status: 404, Body: []byte("Not Found")}}
	exists, err := branchExistsSettled(c, "o", "r", "grain/task-1")
	if err == nil {
		t.Fatal("a branch absent from a repo the client cannot read must not come back as a plain negative")
	}
	if exists {
		t.Error("exists = true")
	}
	if !strings.Contains(err.Error(), "o/r") {
		t.Errorf("error = %v, want it to name the repo it could not read", err)
	}
}

// The probe costs one call, on the negative path only.
func TestBranchExistsSettledDoesNotProbeTheRepoOnAPositive(t *testing.T) {
	withNoSleep(t)
	c := &branchClient{answers: []bool{true}}
	if _, err := branchExistsSettled(c, "o", "r", "b"); err != nil {
		t.Fatal(err)
	}
	if c.repoCalls != 0 {
		t.Errorf("repo probes = %d, want 0 -- a branch that is there needs no confirmation the repo is", c.repoCalls)
	}
}
