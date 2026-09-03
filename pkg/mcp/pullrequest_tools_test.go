package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/github"
)

// fakePullRequests is a scripted PullRequestReader: every method answers
// from a field, and records the arguments the tool asked with, so a test
// can assert both what the agent is told and that the tool never looked
// anywhere but its own scope.
type fakePullRequests struct {
	head    *github.BranchHead
	headErr error

	pr    *github.PullRequest
	prErr error

	detail    github.PullRequestDetail
	detailErr error

	checks    []github.CheckRun
	checksErr error

	workflows    []github.CheckRun
	workflowsErr error

	jobLogs    []github.JobLog
	jobLogsErr error
	// logReads counts the calls to FailedJobLogs, since "did not read the
	// logs at all" is as much a property worth pinning as what they said.
	logReads int

	// Every (owner, repo) pair any method was called with, as
	// "owner/repo", plus every ref a CI read named.
	repos []string
	refs  []string
}

func (f *fakePullRequests) note(owner, repo string) { f.repos = append(f.repos, owner+"/"+repo) }

func (f *fakePullRequests) GetBranchHead(owner, repo, branch string) (*github.BranchHead, error) {
	f.note(owner, repo)
	f.refs = append(f.refs, branch)
	return f.head, f.headErr
}

func (f *fakePullRequests) FindOpenPullRequestForBranch(owner, repo, branch string) (*github.PullRequest, error) {
	f.note(owner, repo)
	return f.pr, f.prErr
}

func (f *fakePullRequests) GetPullRequest(owner, repo string, number int) (github.PullRequestDetail, error) {
	f.note(owner, repo)
	return f.detail, f.detailErr
}

func (f *fakePullRequests) ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error) {
	f.note(owner, repo)
	f.refs = append(f.refs, ref)
	return f.checks, f.checksErr
}

func (f *fakePullRequests) ListWorkflowRuns(owner, repo, headSHA string) ([]github.CheckRun, error) {
	f.note(owner, repo)
	f.refs = append(f.refs, headSHA)
	return f.workflows, f.workflowsErr
}

func (f *fakePullRequests) FailedJobLogs(owner, repo, headSHA string) ([]github.JobLog, error) {
	f.note(owner, repo)
	f.refs = append(f.refs, headSHA)
	f.logReads++
	return f.jobLogs, f.jobLogsErr
}

func conclusion(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

var testScope = PullRequestScope{Owner: "acme", Repo: "widgets", Branch: "grain/task-9"}

// call runs pull_request_status the way a client would and returns the
// single Result.
func call(t *testing.T, client PullRequestReader, scope PullRequestScope) Result {
	t.Helper()
	return namedTool(t, NewPullRequestTools(client, scope), "pull_request_status").
		Handler(context.Background(), map[string]any{})
}

// namedTool picks one tool out of a constructor's roster by name, since
// NewPullRequestTools returns the whole watching-your-own-pull-request
// vocabulary rather than one tool -- and fails the test if the name is
// not there at all, which is the failure worth catching.
func namedTool(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no %s among %+v", name, tools)
	return Tool{}
}

// A failing check has to be named, and named as failing: the whole point
// of the tool is that a run can act on it, and "1 check did not pass" is
// exactly the sentence orchestrator.healthReason already refuses to
// settle for.
func TestPullRequestStatusNamesFailingChecks(t *testing.T) {
	client := &fakePullRequests{
		head: &github.BranchHead{SHA: "0123456789abcdef", Message: "wire it up\n\nlonger body"},
		pr:   &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pull/42"},
		// The number and the link are printed off the same lookup, `pr`,
		// so one blank field cannot silently drop half the reference.
		// This fixture carries html_url on both reads because GitHub
		// does, which means it cannot tell the two sources apart --
		// TestPullRequestStatusTakesTheLinkOffTheLookupThatNamedTheNumber
		// is the one that can.
		detail: github.PullRequestDetail{Number: 42, HTMLURL: "https://example.test/pull/42", State: "open", BaseRef: "main", Mergeable: boolPtr(true)},
		checks: []github.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: conclusion("success")},
			{Name: "unit tests", Status: "completed", Conclusion: conclusion("failure")},
			{Name: "integration", Status: "in_progress"},
		},
	}

	res := call(t, client, testScope)
	if res.IsError {
		t.Fatalf("IsError = true, want a plain answer: %s", res.Text)
	}
	for _, want := range []string{
		"0123456", `"wire it up"`, "#42", "https://example.test/pull/42",
		"FAILING", "unit tests", "1 failing", "git push origin grain/task-9",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("answer does not mention %q:\n%s", want, res.Text)
		}
	}
	// The commit body must not follow the subject into the answer.
	if strings.Contains(res.Text, "longer body") {
		t.Errorf("answer carries the whole commit message, not just its subject:\n%s", res.Text)
	}
}

// ...and what that failing job printed comes with the name. A check run
// says only that a job called "go" did not pass, which is enough to know
// the build is red and not enough to fix it, so the run would otherwise
// spend its next turns guessing at the failure and trying to reproduce it
// in a sandbox that is not the runner -- the same gap fileFixTask already
// closes for the fix task the merge queue files.
func TestPullRequestStatusQuotesTheFailingJobsLog(t *testing.T) {
	client := &fakePullRequests{
		head:   &github.BranchHead{SHA: "0123456789abcdef", Message: "wire it up"},
		checks: []github.CheckRun{{Name: "go", Status: "completed", Conclusion: conclusion("failure")}},
		jobLogs: []github.JobLog{{
			Name: "go",
			URL:  "https://github.com/acme/widgets/actions/runs/42/job/7",
			Log: "2026-01-02T03:04:05.1234567Z --- FAIL: TestThing (0.00s)\n" +
				"2026-01-02T03:04:05.1234567Z     a_test.go:12: got 3, want 4\n",
			Truncated: true,
		}},
	}

	res := call(t, client, testScope)
	if res.IsError {
		t.Fatalf("IsError = true, want a plain answer: %s", res.Text)
	}
	for _, want := range []string{
		"FAILING", "--- FAIL: TestThing", "a_test.go:12: got 3, want 4",
		"https://github.com/acme/widgets/actions/runs/42/job/7",
		"````", "the tail of the log",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("answer does not carry %q:\n%s", want, res.Text)
		}
	}
	// Actions stamps every line with the same timestamp, which says
	// nothing about the failure and costs a quarter of each line.
	if strings.Contains(res.Text, "2026-01-02T03:04:05") {
		t.Errorf("answer carries Actions' per-line timestamps:\n%s", res.Text)
	}
	// The logs are read against the commit the checks were read against,
	// never widened to the branch.
	for _, ref := range client.refs {
		if ref != testScope.Branch && ref != "0123456789abcdef" {
			t.Errorf("a read named %q, want the branch's tip commit", ref)
		}
	}
}

// Nothing failed, so nothing is read: the logs cost three GitHub calls
// (github.FailedJobLogs walks runs, then jobs, then a log) and a green
// commit has none of them to find.
func TestPullRequestStatusReadsNoLogsForAGreenBuild(t *testing.T) {
	client := &fakePullRequests{
		head: &github.BranchHead{SHA: "0123456789abcdef", Message: "wire it up"},
		checks: []github.CheckRun{
			{Name: "go", Status: "completed", Conclusion: conclusion("success")},
			{Name: "docs", Status: "in_progress"},
		},
	}

	res := call(t, client, testScope)
	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if client.logReads != 0 {
		t.Errorf("read the failing jobs' logs %d times for a commit with no failing job", client.logReads)
	}
}

// A credential without Actions read degrades to the answer this tool gave
// before the logs existed -- the checks, with the failing ones named --
// rather than turning a report of a red build into an error about logs.
// The same call checkRunsForCommit's own fallback makes.
func TestPullRequestStatusStillReportsAFailureWhenTheLogsAreUnreadable(t *testing.T) {
	denied := &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}
	if !github.IsPermissionDenied(denied) {
		t.Fatal("test fixture is not a permission denial; github.IsPermissionDenied changed")
	}
	res := call(t, &fakePullRequests{
		head:       &github.BranchHead{SHA: "0123456789abcdef", Message: "wire it up"},
		checks:     []github.CheckRun{{Name: "go", Status: "completed", Conclusion: conclusion("failure")}},
		jobLogsErr: denied,
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true, want the red build reported anyway: %s", res.Text)
	}
	if !strings.Contains(res.Text, "FAILING") || !strings.Contains(res.Text, "1 failing") {
		t.Errorf("answer stopped naming the failing check:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "may not read Actions job logs") {
		t.Errorf("answer does not say why there is no log under the failure:\n%s", res.Text)
	}
}

// An Actions log that is simply not there -- a third-party CI's check, or
// a log past GitHub's retention window -- is said out loud too. A reader
// told a job failed and shown nothing under it cannot otherwise tell "no
// log" from "this tool forgot".
func TestPullRequestStatusSaysWhenThereIsNoJobLogAtAll(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0123456789abcdef", Message: "wire it up"},
		checks: []github.CheckRun{{Name: "buildkite/tests", Status: "completed", Conclusion: conclusion("failure")}},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no failed job with a readable log") {
		t.Errorf("answer does not account for the missing log:\n%s", res.Text)
	}
}

// The link is printed off the lookup that named the number, not off the
// detail read. Every other fixture here mirrors GitHub, which returns
// html_url on both, so none of them can tell the two apart -- and neither
// can the end-to-end test, since githubsim serves the field on its list
// and detail responses alike. This one gives it to the search result
// only, which is the exact shape a live run found rendering "Pull
// request #42 () is open": an empty pair of parentheses where the link
// belongs. It is what goes red if the field moves back to `detail`.
//
// The converse -- html_url on the detail read and none on the search
// result -- is deliberately not pinned; pullRequestStatus's own comment
// records why the answer does not fall back for it.
func TestPullRequestStatusTakesTheLinkOffTheLookupThatNamedTheNumber(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		pr:     &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pull/42"},
		detail: github.PullRequestDetail{Number: 42, State: "open", BaseRef: "main", Mergeable: boolPtr(true)},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "Pull request #42 (https://example.test/pull/42) is open") {
		t.Errorf("answer does not print the link the lookup carried:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "#42 ()") {
		t.Errorf("answer renders empty parentheses where the link belongs:\n%s", res.Text)
	}
}

// An unfinished check is not a passing one. healthFrom makes the same
// call for the merge gate; this is the agent-facing half of it, and the
// failure it prevents is a run that pushes, sees three queued jobs, and
// declares itself done.
func TestPullRequestStatusDoesNotCallPendingChecksPassing(t *testing.T) {
	client := &fakePullRequests{
		head: &github.BranchHead{SHA: "abcdef0123456", Message: "wip"},
		checks: []github.CheckRun{
			{Name: "build", Status: "queued"},
		},
	}

	res := call(t, client, testScope)
	if res.IsError {
		t.Fatalf("IsError = true, want a plain answer: %s", res.Text)
	}
	if !strings.Contains(res.Text, "0 failing") || !strings.Contains(res.Text, "1 not finished") {
		t.Errorf("answer does not count the unfinished check as unfinished:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "no verdict yet") {
		t.Errorf("answer does not warn that an unfinished check has no verdict:\n%s", res.Text)
	}
}

// A branch that was never pushed is a different message from a branch
// whose CI said nothing, and the two are otherwise indistinguishable --
// both are an empty check list.
func TestPullRequestStatusSeparatesNotPushedFromNoChecks(t *testing.T) {
	notPushed := call(t, &fakePullRequests{}, testScope)
	if notPushed.IsError {
		t.Fatalf("IsError = true, want a plain answer: %s", notPushed.Text)
	}
	if !strings.Contains(notPushed.Text, "does not exist") ||
		!strings.Contains(notPushed.Text, "git push origin grain/task-9") {
		t.Errorf("answer does not say the branch was never pushed:\n%s", notPushed.Text)
	}

	pushedNoChecks := call(t, &fakePullRequests{
		head: &github.BranchHead{SHA: "feedfacefeedface", Message: "done"},
	}, testScope)
	if pushedNoChecks.IsError {
		t.Fatalf("IsError = true, want a plain answer: %s", pushedNoChecks.Text)
	}
	if strings.Contains(pushedNoChecks.Text, "does not exist") {
		t.Errorf("a pushed branch with no checks reads as unpushed:\n%s", pushedNoChecks.Text)
	}
	if !strings.Contains(pushedNoChecks.Text, "no checks at all") {
		t.Errorf("answer does not say CI has reported nothing:\n%s", pushedNoChecks.Text)
	}
}

// The tool is registered even when nothing configured it, so its answer
// has to explain that rather than reading as a repo with clean CI.
func TestPullRequestStatusWithoutAScopeSaysSo(t *testing.T) {
	for name, tc := range map[string]struct {
		client PullRequestReader
		scope  PullRequestScope
	}{
		"no client": {nil, testScope},
		"no scope":  {&fakePullRequests{}, PullRequestScope{}},
	} {
		t.Run(name, func(t *testing.T) {
			res := call(t, tc.client, tc.scope)
			if !res.IsError {
				t.Errorf("IsError = false, want an error result: %s", res.Text)
			}
			if !strings.Contains(res.Text, "no GitHub repository configured") {
				t.Errorf("answer does not explain why there is nothing to report:\n%s", res.Text)
			}
		})
	}
}

// A credential that cannot read the Checks API but can read Actions gets
// the Actions answer, exactly as orchestrator.checkRunsFor does -- and
// the fallback is scoped to the commit, never widened to the branch.
func TestPullRequestStatusFallsBackToWorkflowRuns(t *testing.T) {
	denied := &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by personal access token"}`)}
	if !github.IsPermissionDenied(denied) {
		t.Fatal("test fixture is not a permission denial; github.IsPermissionDenied changed")
	}
	client := &fakePullRequests{
		head:      &github.BranchHead{SHA: "1234567890abcdef", Message: "try again"},
		checksErr: denied,
		workflows: []github.CheckRun{{Name: "CI", Status: "completed", Conclusion: conclusion("failure")}},
	}

	res := call(t, client, testScope)
	if res.IsError {
		t.Fatalf("IsError = true, want the Actions answer: %s", res.Text)
	}
	if !strings.Contains(res.Text, "FAILING") || !strings.Contains(res.Text, "CI") {
		t.Errorf("answer does not carry the workflow run's verdict:\n%s", res.Text)
	}
	for _, ref := range client.refs {
		if ref == testScope.Branch {
			continue // GetBranchHead's own argument
		}
		if ref != "1234567890abcdef" {
			t.Errorf("a CI read named %q, want the branch's tip commit", ref)
		}
	}
}

// Neither CI API readable is an error here, unlike in SyncPullRequests
// where health simply goes unknown: an agent that asked out loud must
// not be told "no checks" when the truth is "I may not look."
func TestPullRequestStatusErrorsWhenCIIsUnreadable(t *testing.T) {
	denied := &github.Error{Status: 403, Body: []byte(`{"message":"Resource not accessible by integration"}`)}
	res := call(t, &fakePullRequests{
		head:         &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		checksErr:    denied,
		workflowsErr: denied,
	}, testScope)

	if !res.IsError {
		t.Fatalf("IsError = false, want an error rather than a silent all-clear: %s", res.Text)
	}
	if strings.Contains(res.Text, "none of them failed") {
		t.Errorf("an unreadable CI reads as passing:\n%s", res.Text)
	}
}

// An ordinary (non-permission) failure is reported, not swallowed into
// an empty check list that would read as a clean build.
func TestPullRequestStatusReportsOrdinaryGitHubErrors(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:      &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		checksErr: errors.New("connection refused"),
	}, testScope)

	if !res.IsError {
		t.Fatalf("IsError = false, want the error reported: %s", res.Text)
	}
	if !strings.Contains(res.Text, "connection refused") {
		t.Errorf("answer loses GitHub's own error:\n%s", res.Text)
	}
}

// The scope is fixed at construction and no argument can move it: a run
// reads CI for its own branch or nothing (the tool's own doc comment).
func TestPullRequestStatusOnlyEverReadsItsOwnScope(t *testing.T) {
	client := &fakePullRequests{head: &github.BranchHead{SHA: "0011223344556677", Message: "x"}}
	status := namedTool(t, NewPullRequestTools(client, testScope), "pull_request_status")
	res := status.Handler(context.Background(), map[string]any{
		"owner": "attacker", "repo": "secrets", "branch": "main",
	})
	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	for _, repo := range client.repos {
		if repo != "acme/widgets" {
			t.Errorf("read %q, want only acme/widgets", repo)
		}
	}
	if len(client.repos) == 0 {
		t.Error("the tool read nothing at all, so this proves nothing")
	}
}

// A pull request GitHub has not finished judging says so, rather than
// implying either answer -- the same tri-state
// github.PullRequestDetail.Mergeable documents.
func TestPullRequestStatusReportsUnknownMergeability(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		pr:     &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pull/7"},
		detail: github.PullRequestDetail{Number: 7, State: "open", BaseRef: "main"},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "has not finished working out") {
		t.Errorf("answer does not report mergeability as still unknown:\n%s", res.Text)
	}
}

// The other three mergeability answers, of which the conflict one is the
// most actionable thing this tool produces: a run told its branch
// conflicts can rebase and push inside its own turn budget, and a run
// wrongly told it merges cleanly finishes on work nobody can land. Merged
// wins over Mergeable because GitHub leaves the tri-state set to whatever
// it last computed on a PR that is already in.
func TestPullRequestStatusReportsMergeability(t *testing.T) {
	for name, tc := range map[string]struct {
		detail  github.PullRequestDetail
		want    string
		notWant string
	}{
		"conflicts": {
			detail:  github.PullRequestDetail{Number: 7, State: "open", BaseRef: "main", Mergeable: boolPtr(false)},
			want:    "is open and conflicts with main",
			notWant: "merges cleanly",
		},
		"merges cleanly": {
			detail:  github.PullRequestDetail{Number: 7, State: "open", BaseRef: "release-2", Mergeable: boolPtr(true)},
			want:    "is open and merges cleanly into release-2",
			notWant: "conflicts with",
		},
		"merged": {
			detail:  github.PullRequestDetail{Number: 7, State: "closed", BaseRef: "main", Merged: true, Mergeable: boolPtr(false)},
			want:    "is closed and merged",
			notWant: "conflicts with",
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := call(t, &fakePullRequests{
				head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
				pr:     &github.PullRequest{Number: 7, HTMLURL: "https://example.test/pull/7"},
				detail: tc.detail,
			}, testScope)

			if res.IsError {
				t.Fatalf("IsError = true: %s", res.Text)
			}
			if !strings.Contains(res.Text, tc.want) {
				t.Errorf("answer does not say %q:\n%s", tc.want, res.Text)
			}
			if strings.Contains(res.Text, tc.notWant) {
				t.Errorf("answer says %q, which is the opposite verdict:\n%s", tc.notWant, res.Text)
			}
		})
	}
}

// checkFailed's conclusions, including the one it deliberately does not
// share with orchestrator.failingChecks. "cancelled" is not a failure
// here: nothing broke, so telling a run to "reproduce those failures"
// would send it hunting a bug that does not exist -- but it is still
// listed by name, with GitHub's own word, for somebody who might rerun
// it. That divergence is what checkFailed's doc comment records, so it is
// the line a later edit is most likely to "tidy" back into agreement.
func TestPullRequestStatusTreatsTimeoutsAndStartupFailuresAsFailing(t *testing.T) {
	for _, conc := range []string{"failure", "timed_out", "startup_failure"} {
		t.Run(conc, func(t *testing.T) {
			res := call(t, &fakePullRequests{
				head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
				checks: []github.CheckRun{{Name: "unit tests", Status: "completed", Conclusion: conclusion(conc)}},
			}, testScope)

			if res.IsError {
				t.Fatalf("IsError = true: %s", res.Text)
			}
			if !strings.Contains(res.Text, "FAILING") || !strings.Contains(res.Text, "1 failing") {
				t.Errorf("a %q check does not read as failing:\n%s", conc, res.Text)
			}
			if !strings.Contains(res.Text, conc) {
				t.Errorf("answer drops GitHub's own conclusion %q:\n%s", conc, res.Text)
			}
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		res := call(t, &fakePullRequests{
			head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
			checks: []github.CheckRun{{Name: "unit tests", Status: "completed", Conclusion: conclusion("cancelled")}},
		}, testScope)

		if res.IsError {
			t.Fatalf("IsError = true: %s", res.Text)
		}
		if strings.Contains(res.Text, "FAILING") || !strings.Contains(res.Text, "0 failing") {
			t.Errorf("a cancelled check reads as a failure to reproduce:\n%s", res.Text)
		}
		if !strings.Contains(res.Text, "unit tests (cancelled)") {
			t.Errorf("answer does not name the cancelled check and say it was cancelled:\n%s", res.Text)
		}
	})
}

// A check that says it completed and reports no conclusion at all is not
// something GitHub is supposed to produce, so the answer says exactly
// that rather than inventing a verdict for it. It counts toward
// "otherwise done" -- deliberately not "passing", which is the word the
// summary line pointedly does not use -- because the one thing that can
// be said about it is that it did not fail, and the alternative,
// counting it as pending, would tell a run to call back in a minute or
// two for a check that is never going to report anything more.
func TestPullRequestStatusDoesNotInventAVerdictForACheckThatReportedNone(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		checks: []github.CheckRun{{Name: "mystery", Status: "completed"}},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "mystery (no conclusion reported)") {
		t.Errorf("answer does not say the check reported no conclusion:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "0 failing, 0 not finished, 1 otherwise done") {
		t.Errorf("a completed check with no conclusion is miscounted:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "no verdict yet") {
		t.Errorf("a finished check is reported as still running:\n%s", res.Text)
	}
}

// A check with no name still gets a line. Dropping it, or printing an
// empty column, would leave a run reading "1 failing" with nothing
// underneath it that names anything.
func TestPullRequestStatusLabelsAnUnnamedCheck(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		checks: []github.CheckRun{{Status: "completed", Conclusion: conclusion("failure")}},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "FAILING  (unnamed check) (failure)") {
		t.Errorf("answer does not stand in for the missing check name:\n%s", res.Text)
	}
}

// The commit subject is bounded and never empty: a message with a novel
// in it cannot flood the answer past the part the run needs to read, and
// a commit with no message at all says so rather than rendering an empty
// pair of quotes that reads like the tool lost the message.
func TestPullRequestStatusBoundsTheCommitSubject(t *testing.T) {
	t.Run("long subject", func(t *testing.T) {
		subject := strings.Repeat("a", 72) + strings.Repeat("b", 40)
		res := call(t, &fakePullRequests{
			head: &github.BranchHead{SHA: "0011223344556677", Message: subject},
		}, testScope)

		if res.IsError {
			t.Fatalf("IsError = true: %s", res.Text)
		}
		if !strings.Contains(res.Text, `"`+strings.Repeat("a", 72)+`..."`) {
			t.Errorf("answer does not truncate the oversized subject:\n%s", res.Text)
		}
		if strings.Contains(res.Text, "bbbb") {
			t.Errorf("answer carries the subject past its bound:\n%s", res.Text)
		}
	})

	t.Run("no message", func(t *testing.T) {
		res := call(t, &fakePullRequests{
			head: &github.BranchHead{SHA: "0011223344556677", Message: "  \n\n"},
		}, testScope)

		if res.IsError {
			t.Fatalf("IsError = true: %s", res.Text)
		}
		if !strings.Contains(res.Text, "(no commit message)") {
			t.Errorf("answer does not say the commit has no message:\n%s", res.Text)
		}
		if strings.Contains(res.Text, `""`) {
			t.Errorf("answer renders an empty quoted subject:\n%s", res.Text)
		}
	})
}

// The two GitHub reads either side of the check reads fail the same way
// the CI read already does: reported, with GitHub's own error and enough
// context to say which call broke. Swallowing either would hand a run a
// confident-looking answer with the pull request silently missing from
// it.
func TestPullRequestStatusReportsPullRequestReadErrors(t *testing.T) {
	t.Run("looking for the pull request", func(t *testing.T) {
		res := call(t, &fakePullRequests{
			head:  &github.BranchHead{SHA: "0011223344556677", Message: "x"},
			prErr: errors.New("connection refused"),
		}, testScope)

		if !res.IsError {
			t.Fatalf("IsError = false, want the error reported: %s", res.Text)
		}
		for _, want := range []string{"open pull request for grain/task-9", "connection refused"} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("answer does not mention %q:\n%s", want, res.Text)
			}
		}
	})

	t.Run("reading the pull request", func(t *testing.T) {
		res := call(t, &fakePullRequests{
			head:      &github.BranchHead{SHA: "0011223344556677", Message: "x"},
			pr:        &github.PullRequest{Number: 42, HTMLURL: "https://example.test/pull/42"},
			detailErr: errors.New("connection refused"),
		}, testScope)

		if !res.IsError {
			t.Fatalf("IsError = false, want the error reported: %s", res.Text)
		}
		for _, want := range []string{"reading pull request #42", "connection refused"} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("answer does not mention %q:\n%s", want, res.Text)
			}
		}
	})
}

// A pushed branch with no pull request on it yet is grain working as
// designed -- the orchestrator opens one after the run finishes -- so the
// answer has to say so. Without that sentence a run reads "no pull
// request" as grain having failed to open one and goes looking for a
// problem that is not there, or worse, tries to open one itself.
func TestPullRequestStatusExplainsWhyNoPullRequestIsOpenYet(t *testing.T) {
	res := call(t, &fakePullRequests{
		head:   &github.BranchHead{SHA: "0011223344556677", Message: "x"},
		checks: []github.CheckRun{{Name: "lint", Status: "completed", Conclusion: conclusion("success")}},
	}, testScope)

	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	if !strings.Contains(res.Text, "No pull request is open for it yet") ||
		!strings.Contains(res.Text, "once this run finishes") {
		t.Errorf("answer does not explain that grain opens the pull request later:\n%s", res.Text)
	}
	// The checks the push triggered directly are still reported.
	if !strings.Contains(res.Text, "lint") {
		t.Errorf("answer drops the checks that ran without a pull request:\n%s", res.Text)
	}
}
