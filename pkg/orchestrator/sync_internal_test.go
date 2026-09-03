package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
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

func TestHealthFromClosedAndMergedReadsMerged(t *testing.T) {
	got := healthFrom(github.PullRequestDetail{State: "closed", Merged: true}, nil, true)
	if got != model.PrMerged {
		t.Fatalf("got %q, want merged", got)
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

// The merge gate's whole point: a check that has not finished is not a
// check that passed. Reading an in-progress run as clean is how a merge
// queue lands a change before its tests have said anything at all about
// it, so this must read pending -- an answer syncEntry neither merges on
// nor files a fix for, and one that resolves itself a cycle or two later
// when CI finishes.
func TestHealthFromAnInProgressCheckIsPending(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "build", Status: "in_progress"},
	}, true)
	if got != model.PrPending {
		t.Fatalf("got %q, want pending (an unfinished check must never read as clean)", got)
	}
}

// A queued check has not even started, which is if anything further from
// "passed" than an in-progress one -- and it is the state a check spends
// its first seconds in, exactly when a freshly opened PR is first synced.
func TestHealthFromAQueuedCheckIsPendingEvenAlongsideAPassingOne(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("success")},
		{Name: "tests", Status: "queued"},
	}, true)
	if got != model.PrPending {
		t.Fatalf("got %q, want pending: one green check says nothing about the one still queued", got)
	}
}

// Pending outranks failing on purpose. The queue files exactly one
// automatic fix per pull request, so waiting a cycle for the rest of CI
// buys that one fix task the whole verdict to work from instead of
// whichever job happened to go red first.
func TestHealthFromAFailedCheckAlongsideARunningOneIsStillPending(t *testing.T) {
	yes := true
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "tests", Status: "in_progress"},
	}, true)
	if got != model.PrPending {
		t.Fatalf("got %q, want pending until every check has reported", got)
	}
}

// A conflict is read off the PR itself and needs no CI at all, so it
// still answers straight away -- a PR that cannot merge into its base
// will not start merging because its tests eventually go green.
func TestHealthFromConflictedBeatsPending(t *testing.T) {
	no := false
	got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &no}, []github.CheckRun{
		{Name: "tests", Status: "in_progress"},
	}, true)
	if got != model.PrConflicted {
		t.Fatalf("got %q, want conflicted", got)
	}
}

// A check that ran out of time, or a workflow too broken to start at all
// (the Actions fallback's own conclusion for that), is a check that ran
// and did not pass -- the same thing a fix task exists to repair as an
// outright "failure".
func TestHealthFromATimedOutOrUnstartableCheckIsFailing(t *testing.T) {
	for _, conclusion := range []string{"failure", "timed_out", "startup_failure"} {
		t.Run(conclusion, func(t *testing.T) {
			yes := true
			got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
				{Name: "tests", Status: "completed", Conclusion: strPtr(conclusion)},
			}, true)
			if got != model.PrFailing {
				t.Fatalf("got %q, want failing", got)
			}
		})
	}
}

// The other completed conclusions are not failures, and reading them as
// failures would have the queue file a fix task for something no agent
// can repair: a merge queue's own "cancelled, will retry" check, a
// workflow waiting on a human's approval, or a job deliberately skipped.
func TestHealthFromTheNonFailureConclusionsStayClean(t *testing.T) {
	for _, conclusion := range []string{"success", "neutral", "skipped", "cancelled", "action_required", "stale"} {
		t.Run(conclusion, func(t *testing.T) {
			yes := true
			got := healthFrom(github.PullRequestDetail{State: "open", Mergeable: &yes}, []github.CheckRun{
				{Name: "tests", Status: "completed", Conclusion: strPtr(conclusion)},
			}, true)
			if got != model.PrClean {
				t.Fatalf("got %q, want clean", got)
			}
		})
	}
}

// The fix task's body is the whole brief an agent gets, so it names the
// jobs that went red rather than leaving it to go and find them.
func TestHealthReasonNamesTheFailingChecks(t *testing.T) {
	checks := []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("success")},
		{Name: "unit tests", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "e2e", Status: "completed", Conclusion: strPtr("timed_out")},
	}
	got := healthReason(model.PrFailing, github.PullRequestDetail{BaseRef: "main"}, checks)
	for _, want := range []string{"unit tests", "e2e"} {
		if !strings.Contains(got, want) {
			t.Errorf("healthReason = %q, want it to name %q", got, want)
		}
	}
	if strings.Contains(got, "lint") {
		t.Errorf("healthReason = %q, want it to leave the check that passed out", got)
	}
}

// The general sentence stays as the floor. Nothing reaches it today --
// syncEntry passes healthReason the very checks health was computed from
// -- but a fix task whose body said only "its checks are failing ()"
// would read as a bug to whoever picked it up, so the empty list keeps
// the sentence it had before check names went in.
func TestHealthReasonStillExplainsFailingWithNoNamedChecks(t *testing.T) {
	got := healthReason(model.PrFailing, github.PullRequestDetail{BaseRef: "main"}, nil)
	if !strings.Contains(got, "checks") {
		t.Fatalf("healthReason = %q, want it to still say the checks are failing", got)
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

// --- closing a PR whose head branch is already gone --------------------

// A merged PR's head branch is routinely deleted by the time this runs
// (GitHub's own "automatically delete head branches" setting, or a human
// tidying up after closing without merging), which leaves ListCheckRuns
// unable to resolve it. syncEntry must still close the task out: it does
// not need checks to know a closed PR is done, so it must never even ask
// for them once detail.State already says "closed" -- see the comment in
// syncEntry itself for why asking anyway used to leave the task never
// closing, cycle after cycle.
func TestSyncEntryClosesAPullRequestEvenWhenItsHeadBranchIsGone(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	completedAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	task := model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "fix the thing", Body: "please",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human},
		Target:   &repo,
		Binding:  model.BindingDirective,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task: %v", err)
	}
	obs := model.Observation{TaskID: task.ID, CompletedAt: &completedAt}
	if err := store.Observe(ctx, obs); err != nil {
		t.Fatalf("recording completion: %v", err)
	}

	entry := queueEntry{
		task: task, obs: &obs,
		ref: model.PullRequestRef{Repo: repo, Number: 7},
	}
	client := &deletedBranchClient{}

	if err := syncEntry(ctx, store, client, entry, map[string]string{}, completedAt); err != nil {
		t.Fatalf("syncEntry: %v", err)
	}
	if client.checkRunsCalled {
		t.Error("syncEntry read check runs for a PR already known to be closed")
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed", st)
	}
}

// deletedBranchClient stands in for a real GitHub credential once a PR's
// head branch is gone: GetPullRequest still answers fine (a closed PR
// still exists), but any ref-scoped read like ListCheckRuns 404s because
// there is no longer a branch or commit that name resolves to.
type deletedBranchClient struct {
	github.Client
	checkRunsCalled bool
}

func (c *deletedBranchClient) GetPullRequest(owner, repo string, number int) (github.PullRequestDetail, error) {
	return github.PullRequestDetail{State: "closed", HeadRef: "grain/t1", BaseRef: "main"}, nil
}

func (c *deletedBranchClient) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	c.checkRunsCalled = true
	return nil, &github.Error{Status: 404, Body: []byte("Not Found")}
}

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
	client := &checkRunsClient{err: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}, workflowErr: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef")
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
	client := &checkRunsClient{err: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}, workflowErr: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}}
	if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef"); err != nil {
		t.Fatalf("a 403 must not fail the sync: %v", err)
	}
	if !ChecksUnavailable() {
		t.Error("ChecksUnavailable() = false after both CI reads 403'd, want true")
	}
}

// Only the one permission a deployment can be configured without is
// tolerated. Anything else -- a 404, a 500, a transport failure -- is
// still a real error, and swallowing it would hide a broken deployment
// behind the same silent "unknown".
//
// The 403s below are the reason IsPermissionDenied reads the body rather
// than the status. Each one clears -- on its own, or with a change the
// operator can make -- but checksUnavailable never clears within a
// process, so classifying one as a missing permission would switch
// auto-merge off until the next restart over a condition that had
// already resolved. A propagated error costs one retried cycle instead.
func TestCheckRunsForStillFailsOnEveryOtherError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not found", &github.Error{Status: 404}},
		{"server error", &github.Error{Status: 500}},
		{"transport", errors.New("dial tcp: connection refused")},
		{
			"a rate-limited 403",
			&github.Error{Status: 403, Body: []byte(`{"message":"API rate limit exceeded for user ID 1."}`)},
		},
		{
			"a secondary rate limit's 403",
			&github.Error{Status: 403, Body: []byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)},
		},
		{
			"a SAML-enforcement 403",
			&github.Error{Status: 403, Body: []byte(`{"message":"Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."}`)},
		},
		{
			"an IP-allow-list 403",
			&github.Error{Status: 403, Body: []byte(`{"message":"Although you appear to have the correct authorization credentials, the organization has an IP allow list enabled, and your IP address is not permitted to access this resource."}`)},
		},
		{"a 403 with a body that cannot be read", &github.Error{Status: 403, Body: []byte("<html>403 Forbidden</html>")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &checkRunsClient{err: tc.err, workflowErr: tc.err}
			if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef"); err == nil {
				t.Fatal("expected the error to propagate")
			}
		})
	}
}

// The whole point of the fallback: a fine-grained PAT cannot be granted
// the Checks permission at all (GitHub withdrew it and has said only
// GitHub Apps may use that API), so without this a deployment on one has
// no CI signal and auto-merge never fires. With "Actions" read it does.
func TestCheckRunsForFallsBackToWorkflowRunsWhenChecksAreForbidden(t *testing.T) {
	client := &checkRunsClient{
		err:          &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)},
		workflowRuns: []github.CheckRun{{Name: "tests", Status: "completed"}},
	}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef")
	if err != nil {
		t.Fatalf("the fallback must not fail the sync: %v", err)
	}
	if !known {
		t.Fatal("a successful workflow-run read must report the checks as known")
	}
	if len(checks) != 1 || checks[0].Name != "tests" {
		t.Fatalf("checks = %v, want the one workflow run", checks)
	}
}

// The fallback reads the commit the PR points at, not its branch. A
// branch-scoped read could return a run of an older commit, and reading
// that as this commit's pass is what would auto-merge untested code.
func TestCheckRunsForScopesTheFallbackToTheHeadSHA(t *testing.T) {
	client := &checkRunsClient{err: &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}}
	if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if client.workflowSHASaw != "deadbeef" {
		t.Errorf("the fallback read %q, want the head sha deadbeef", client.workflowSHASaw)
	}
}

// GitHub computes the head sha asynchronously the same way it computes
// mergeable, so a PR read can legitimately arrive without one. With no
// commit to scope to, the fallback is skipped rather than widened to a
// branch-scoped read -- health goes unknown for a cycle instead.
func TestCheckRunsForSkipsTheFallbackWithNoHeadSHA(t *testing.T) {
	client := &checkRunsClient{
		err:          &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)},
		workflowRuns: []github.CheckRun{{Name: "tests", Status: "completed"}},
	}
	_, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "")
	if err != nil {
		t.Fatal(err)
	}
	if known {
		t.Error("checks reported known from a fallback that had no commit to scope to")
	}
	if client.workflowSHASaw != "" {
		t.Errorf("the fallback ran anyway, against %q", client.workflowSHASaw)
	}
}

// The fallback gets the same treatment as the read it stands in for: a
// permission error is a fact about the credential, anything else is a
// real error. A 500 from the Actions API must not read as "no CI".
func TestCheckRunsForPropagatesANonPermissionFallbackFailure(t *testing.T) {
	client := &checkRunsClient{
		err:         &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)},
		workflowErr: &github.Error{Status: 500, Body: []byte("upstream is unwell")},
	}
	if _, _, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef"); err == nil {
		t.Fatal("a 500 from the fallback must propagate, not report unknown")
	}
}

// A credential that can read checks directly must never pay for the
// fallback -- the Checks API sees every provider's checks, the Actions
// API only sees Actions, so preferring it would narrow what CI grain can
// see on a deployment that had no problem to begin with.
func TestCheckRunsForDoesNotReachForTheFallbackWhenChecksWork(t *testing.T) {
	client := &checkRunsClient{
		checks:       []github.CheckRun{{Name: "buildkite", Status: "completed"}},
		workflowRuns: []github.CheckRun{{Name: "tests", Status: "completed"}},
	}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef")
	if err != nil || !known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if len(checks) != 1 || checks[0].Name != "buildkite" {
		t.Fatalf("checks = %v, want the Checks API's own answer", checks)
	}
	if client.workflowSHASaw != "" {
		t.Error("the fallback ran even though the Checks read succeeded")
	}
}

func TestCheckRunsForPassesASuccessfulReadThrough(t *testing.T) {
	client := &checkRunsClient{checks: []github.CheckRun{{Name: "build", Status: "completed"}}}
	checks, known, err := checkRunsFor(client, testPullRequestRef(), "head-branch", "deadbeef")
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

// checkRunsClient is a github.Client that only the two CI reads are ever
// called on -- every other method is embedded and would panic on a nil
// interface, which is the point: checkRunsFor must touch nothing else.
//
// workflowErr defaults to nothing, so a test that sets only err is
// asking for "the Checks read failed and the Actions read succeeded".
// The tests that mean "no CI signal at all" set both.
type checkRunsClient struct {
	github.Client
	checks []github.CheckRun
	err    error

	workflowRuns   []github.CheckRun
	workflowErr    error
	workflowSHASaw string
}

func (c *checkRunsClient) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.checks, nil
}

func (c *checkRunsClient) ListWorkflowRuns(owner, repo, headSHA string) ([]github.CheckRun, error) {
	c.workflowSHASaw = headSHA
	if c.workflowErr != nil {
		return nil, c.workflowErr
	}
	return c.workflowRuns, nil
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
