package mcp

// pull_request_status is the one tool that lets a run see what GitHub
// thinks of the work it has pushed: the pull request open for its branch,
// if there is one yet, and every CI check reported against the commit at
// the branch's tip.
//
// It exists because a dispatched run had no way to close the loop on its
// own tests. An agent could commit and `git push origin <branch>` from the
// first turn -- the git proxy authorizes a push to the task's own target
// (gitproxy/authorize.go) and ConfigureGitCredentials leaves a usable
// identity and credential helper behind -- but what CI made of that push
// was invisible until long after the run had finished, when
// orchestrator.SyncPullRequests read the checks and the merge queue filed
// a whole separate fix task for them (orchestrator/sync.go's
// fileFixTask). Repairing a red build therefore always cost a second
// dispatch, starting from a cold sandbox, to fix something the run that
// caused it was still sitting there able to fix. With this, the run can
// push, look, repair and push again inside its own turn budget.
//
// It does not reopen docs/design.md's split surface ("Sandboxes: git
// transport only. No REST, no GraphQL"). This tool is served by the
// `grain mcpserver` process, which runs on the controller alongside the
// daemon and reads GitHub with the controller's own credential -- the
// identical shape the ask_question escape hatch already has, and the same
// reason that one was acceptable: what crosses into the sandbox is a
// rendered answer, never a credential and never a general-purpose API
// call. The sandbox gains no way to reach GitHub it did not have before.
//
// The scope is fixed at process start (PullRequestScope, written by
// cmd/grain/mcpserver.go's flags from the run's own task) and no argument
// can move it, mirroring the "only place it's named" discipline
// agent/claude's own doc comment describes for the sandbox root: a run
// reads CI for its own branch or nothing.

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/grain/pkg/github"
)

// PullRequestScope is the one branch pull_request_status reports on --
// this run's own target repo and the branch model.BranchName gave it.
// Every field is required; an incomplete scope is a task with no repo
// attached, which the tool says plainly rather than guessing.
type PullRequestScope struct {
	Owner  string
	Repo   string
	Branch string
}

func (s PullRequestScope) complete() bool {
	return s.Owner != "" && s.Repo != "" && s.Branch != ""
}

// PullRequestReader is the read-only slice of github.Client this tool
// needs. Narrowed to five methods rather than taking the whole interface
// so that what an agent can reach through here is legible at a glance:
// there is no mutation in this set at all, and nothing that names a repo
// the caller did not already pin in PullRequestScope.
type PullRequestReader interface {
	GetBranchHead(owner, repo, branch string) (*github.BranchHead, error)
	FindOpenPullRequestForBranch(owner, repo, branch string) (*github.PullRequest, error)
	GetPullRequest(owner, repo string, number int) (github.PullRequestDetail, error)
	ListCheckRuns(owner, repo, ref string) ([]github.CheckRun, error)
	ListWorkflowRuns(owner, repo, headSHA string) ([]github.CheckRun, error)
}

// NewPullRequestTools returns the tools a run gets for watching its own
// pull request: pull_request_status, and nothing else.
//
// client may be nil and scope may be incomplete -- a deployment that
// configured no GitHub access, or a task with no repo attached. The tool
// is registered either way, so tools/list does not change shape with a
// deployment's configuration and an agent that calls it gets a sentence
// explaining why there is nothing to report instead of "unknown tool
// pull_request_status", which reads like a bug in grain rather than a
// fact about this run.
func NewPullRequestTools(client PullRequestReader, scope PullRequestScope) []Tool {
	return []Tool{pullRequestStatusTool(client, scope)}
}

func pullRequestStatusTool(client PullRequestReader, scope PullRequestScope) Tool {
	return Tool{
		Name: "pull_request_status",
		Description: "Show what GitHub currently says about the branch you are " +
			"pushing to: the pull request open for it, if there is one " +
			"yet, and every CI check reported against your latest pushed " +
			"commit, with the failing ones named. Use this to find out " +
			"whether the tests actually pass -- commit, `git push` your " +
			"branch, then call this to read CI's verdict on that commit, " +
			"and repeat until it is green. Checks are reported per commit, " +
			"so a result only reflects work you have already pushed, and a " +
			"check that has not finished carries no verdict at all. This " +
			"only reads GitHub; it pushes nothing and changes nothing.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		Handler: func(_ context.Context, _ map[string]any) Result {
			if client == nil || !scope.complete() {
				return Result{
					Text: "There is no pull request to report on: this run has no GitHub " +
						"repository configured for it. Do the work in the sandbox and " +
						"report what you found instead.",
					IsError: true,
				}
			}
			text, err := pullRequestStatus(client, scope)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			return Result{Text: text}
		},
	}
}

// pullRequestStatus renders the whole answer: where the branch is, which
// pull request (if any) carries it, and what CI made of its tip commit.
//
// The branch is read first and on its own, because "you have not pushed
// anything yet" is a completely different message from "your tests are
// failing" and the two are otherwise easy to confuse -- an empty check
// list looks the same either way.
func pullRequestStatus(client PullRequestReader, scope PullRequestScope) (string, error) {
	head, err := client.GetBranchHead(scope.Owner, scope.Repo, scope.Branch)
	if err != nil {
		return "", fmt.Errorf("reading %s/%s's %s branch: %w", scope.Owner, scope.Repo, scope.Branch, err)
	}
	if head == nil {
		return fmt.Sprintf(
			"Branch %s does not exist on %s/%s yet, so there is no pull request and no CI "+
				"to report. Commit your work and `git push origin %s`, then call this "+
				"again to see what CI makes of it.",
			scope.Branch, scope.Owner, scope.Repo, scope.Branch), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Branch %s is at %s (%s).\n", scope.Branch, shortSHA(head.SHA), firstLine(head.Message))

	pr, err := client.FindOpenPullRequestForBranch(scope.Owner, scope.Repo, scope.Branch)
	if err != nil {
		return "", fmt.Errorf("looking for an open pull request for %s: %w", scope.Branch, err)
	}
	if pr == nil {
		b.WriteString("No pull request is open for it yet -- grain opens one for you once this " +
			"run finishes. Any checks below are the ones your push triggered directly.\n")
	} else {
		detail, err := client.GetPullRequest(scope.Owner, scope.Repo, pr.Number)
		if err != nil {
			return "", fmt.Errorf("reading pull request #%d: %w", pr.Number, err)
		}
		// The link comes off `pr`, the same lookup the number does.
		// Taking one field from each read split a single reference
		// across two responses, and wherever the detail read came back
		// without html_url this line rendered "Pull request #42 () is
		// open" -- an empty pair of parentheses where the link belongs.
		// Both reads carry html_url, so the copy already in hand was
		// always enough.
		//
		// The converse is deliberately left alone: a caller whose `pr`
		// is the one missing html_url gets those same empty parentheses
		// rather than a fallback to detail.HTMLURL. A fallback would put
		// the line back on two reads to cover a shape no live GitHub
		// response produces, and the empty parentheses are what made the
		// original blank visible enough to fix -- quietly printing the
		// number alone would not have been. Only the source is pinned
		// (pullrequest_tools_test.go's
		// TestPullRequestStatusTakesTheLinkOffTheLookupThatNamedTheNumber);
		// nothing pins the blank rendering, so a caller that really does
		// arrive holding only the detail's copy can add the fallback.
		fmt.Fprintf(&b, "Pull request #%d (%s) is %s%s.\n",
			pr.Number, pr.HTMLURL, detail.State, mergeableClause(detail))
	}

	checks, err := checkRunsForCommit(client, scope, head.SHA)
	if err != nil {
		return "", err
	}
	b.WriteString("\n")
	b.WriteString(renderChecks(checks, scope.Branch, shortSHA(head.SHA)))
	return b.String(), nil
}

// checkRunsForCommit reads the checks against sha, falling back to the
// Actions API exactly the way orchestrator.checkRunsFor does and for the
// same reason: a fine-grained token granted "Actions" read but not
// "Checks" can see workflow runs and nothing else, and a deployment on
// one would otherwise get a permanent permission error here rather than
// the CI signal it does have.
//
// Unlike that function, a credential that can read neither is an error
// rather than a silent "unknown". This has one caller, answering a
// question an agent asked out loud, and telling it "no checks" when the
// truth is "I am not allowed to look" would send it off to merge-ready
// confidence on a build it never saw.
func checkRunsForCommit(client PullRequestReader, scope PullRequestScope, sha string) ([]github.CheckRun, error) {
	checks, err := client.ListCheckRuns(scope.Owner, scope.Repo, sha)
	if err == nil {
		return checks, nil
	}
	if !github.IsPermissionDenied(err) {
		return nil, fmt.Errorf("reading check runs for %s: %w", shortSHA(sha), err)
	}

	runs, fallbackErr := client.ListWorkflowRuns(scope.Owner, scope.Repo, sha)
	if fallbackErr == nil {
		return runs, nil
	}
	if !github.IsPermissionDenied(fallbackErr) {
		return nil, fmt.Errorf("reading workflow runs for %s: %w", shortSHA(sha), fallbackErr)
	}
	return nil, fmt.Errorf(
		"this grain deployment's GitHub credential may read neither check runs nor Actions "+
			"workflow runs, so CI is invisible from here -- an operator has to grant it "+
			"before this tool can tell you anything. GitHub said: %v", err)
}

// checkFailed reports whether a completed check counts as broken.
//
// Deliberately the same three conclusions orchestrator.failingChecks
// treats as failing, deliberately not shared with it: that one gates a
// merge and this one writes a sentence to an agent, and the two are free
// to diverge -- a "cancelled" run is worth mentioning to somebody who
// might rerun it and worth ignoring at the merge gate. github.CheckRun's
// own doc comment is where the decision not to narrow GitHub's enum in
// that package is recorded.
func checkFailed(c github.CheckRun) bool {
	if c.Status != "completed" || c.Conclusion == nil {
		return false
	}
	switch *c.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// renderChecks lists every check against the pushed commit and says what
// to do about them. The per-check lines carry GitHub's own status and
// conclusion words verbatim, since narrowing them is exactly how an agent
// ends up told that a "skipped" job passed.
func renderChecks(checks []github.CheckRun, branch, sha string) string {
	if len(checks) == 0 {
		return fmt.Sprintf(
			"GitHub reports no checks at all against %s. Either CI has not registered them "+
				"yet -- they usually appear within a minute or two of a push -- or this "+
				"repo has no CI configured. Call this again shortly to tell the two apart.",
			sha)
	}

	var b strings.Builder
	var failing, pending, passed int
	fmt.Fprintf(&b, "Checks against %s:\n", sha)
	for _, c := range checks {
		name := c.Name
		if name == "" {
			name = "(unnamed check)"
		}
		switch {
		case c.Status != "completed":
			pending++
			fmt.Fprintf(&b, "  %-8s %s (%s)\n", "running", name, c.Status)
		case checkFailed(c):
			failing++
			fmt.Fprintf(&b, "  %-8s %s (%s)\n", "FAILING", name, *c.Conclusion)
		default:
			passed++
			conclusion := "no conclusion reported"
			if c.Conclusion != nil {
				conclusion = *c.Conclusion
			}
			fmt.Fprintf(&b, "  %-8s %s (%s)\n", "ok", name, conclusion)
		}
	}

	fmt.Fprintf(&b, "\n%d failing, %d not finished, %d otherwise done.\n", failing, pending, passed)
	switch {
	case failing > 0:
		fmt.Fprintf(&b, "Reproduce those failures in your checkout, fix them, commit, and "+
			"`git push origin %s` -- each push reruns CI against the new commit, and "+
			"calling this again afterwards tells you whether the fix took.", branch)
	case pending > 0:
		b.WriteString("Nothing has failed, but the unfinished checks carry no verdict yet. " +
			"Call this again in a minute or two rather than treating them as passing.")
	default:
		fmt.Fprintf(&b, "Every check that reported against %s is done and none of them failed.", sha)
	}
	return b.String()
}

// mergeableClause renders GitHub's tri-state Mergeable as a clause to
// hang off the pull request's state, including the nil case: GitHub
// computes mergeability asynchronously, so a read seconds after a push
// legitimately does not know yet (github.PullRequestDetail.Mergeable's
// own doc comment), and saying so beats implying either answer.
func mergeableClause(detail github.PullRequestDetail) string {
	switch {
	case detail.Merged:
		return " and merged"
	case detail.Mergeable == nil:
		return ", and GitHub has not finished working out whether it merges cleanly"
	case *detail.Mergeable:
		return " and merges cleanly into " + detail.BaseRef
	default:
		return " and conflicts with " + detail.BaseRef
	}
}

// shortSHA abbreviates a commit for reading, leaving anything already
// short (or empty) alone rather than slicing a string that has no seven
// characters to give.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// firstLine is a commit message's subject, quoted, bounded so a message
// with a novel in it cannot flood the answer.
func firstLine(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	if len(line) > 72 {
		line = line[:72] + "..."
	}
	if line == "" {
		return "no commit message"
	}
	return `"` + line + `"`
}
