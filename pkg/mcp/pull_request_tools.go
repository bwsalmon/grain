package mcp

import (
	"context"
	"fmt"
	"strings"
)

// CheckReport is one CI check on a pull request's head, as
// open_pull_request reports it back to the agent: GitHub's own name,
// status ("queued", "in_progress", "completed") and, once completed, its
// conclusion ("success", "failure", ...) verbatim. Nothing here narrows
// that vocabulary -- an agent reading "failure" against a check it
// recognizes knows more about what to do next than this package could
// encode.
type CheckReport struct {
	Name       string
	Status     string
	Conclusion string
}

// PullRequestReport is what one open_pull_request call learned: the pull
// request now open for this run's own branch, and what the repo's CI has
// said about it so far.
type PullRequestReport struct {
	Repo   string
	Number int
	URL    string
	Checks []CheckReport
	// ChecksAvailable is false when the deployment cannot read checks at
	// all, as opposed to a head nothing has reported on yet -- the two are
	// the same empty Checks otherwise, and only one of them is worth
	// waiting on.
	ChecksAvailable bool
	// ChecksError is a failure to read the checks of a pull request that
	// was nonetheless opened. The pull request above is real either way.
	ChecksError string
}

// PullRequestOpener opens (or finds) the pull request for the one task
// this server's run belongs to, and reports its current checks.
//
// It is an interface here, and a narrow one, because this package must
// not know how that actually happens: an mcpserver process holds no
// GitHub credential (pkg/gitproxy's whole shape is that grain reaches
// GitHub, never the agent), so the real implementation -- cmd/grain/
// mcpserver.go's own -- asks the running daemon over its REST API and
// lets the daemon decide, from the task's own record, which repo and
// which branch that means. Nothing an agent can put in a tool call
// changes any of it: this call takes no arguments at all.
type PullRequestOpener interface {
	OpenPullRequest(ctx context.Context) (PullRequestReport, error)
}

// NewPullRequestTools returns the one tool a run gets when its dispatch
// can open its own pull request: open_pull_request.
//
// Unlike NewMockTools' four escape hatches, this one is not deferred
// until the run ends and then applied by the controller -- it happens
// while the agent waits, because the entire point is what comes back.
// grain has always opened a pull request for a run's branch, but only
// after the run had already exited, which meant the agent never saw its
// own CI: a change that compiles locally and fails the repo's own build
// was a fact nobody learned until a human read the pull request. A run
// that opens it early can read the checks, fix what they say, push again,
// and call this tool again to see the next round.
//
// opener nil returns the tool anyway, refusing every call: that is what
// lets a caller enumerate the tool names this package registers (each
// agent framework's own allowedTools does exactly that) without holding a
// live opener, and a server that genuinely has none should simply not
// register these tools at all.
func NewPullRequestTools(opener PullRequestOpener) []Tool {
	return []Tool{openPullRequestTool(opener)}
}

func openPullRequestTool(opener PullRequestOpener) Tool {
	return Tool{
		Name: "open_pull_request",
		Description: "Open the pull request for the branch you were told to push, " +
			"without ending your turn -- and report the state of its CI checks. " +
			"Use it once you have pushed a change you believe is finished: it is " +
			"the only way to see what the repo's own checks make of your branch " +
			"while you still have turns left to fix them. This is the same pull " +
			"request grain opens for you when your run ends, so calling it does " +
			"not commit you to anything and not calling it loses you nothing but " +
			"the checks. Call it again after pushing more commits, or after " +
			"waiting for checks that were still running, to see the current " +
			"state -- it never opens a second pull request. It takes no " +
			"arguments: which repo, branch, base and title are grain's to decide, " +
			"not yours.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		Handler: func(ctx context.Context, _ map[string]any) Result {
			if opener == nil {
				return Result{
					Text: "This run cannot open its own pull request (no route to grain's " +
						"GitHub client). Finish your work and push your branch: a pull " +
						"request is opened for it when your run ends.",
					IsError: true,
				}
			}
			report, err := opener.OpenPullRequest(ctx)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			return Result{Text: renderPullRequestReport(report)}
		},
	}
}

// renderPullRequestReport is what the agent actually reads back. Plain
// lines rather than JSON, matching every other tool in this package, and
// explicit about the difference between "no checks have reported yet"
// (wait and call again) and "checks cannot be read here" (waiting will
// never help) -- an agent that cannot tell those apart will either poll
// forever or dismiss a real failure it never saw.
func renderPullRequestReport(r PullRequestReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pull request %s#%d is open", r.Repo, r.Number)
	if r.URL != "" {
		fmt.Fprintf(&b, ": %s", r.URL)
	}
	b.WriteString("\n")

	switch {
	case r.ChecksError != "":
		fmt.Fprintf(&b, "Its checks could not be read: %s\n", r.ChecksError)
	case !r.ChecksAvailable:
		b.WriteString("This deployment's GitHub credential cannot read checks, so there is " +
			"nothing to wait for here -- calling again will not show you CI.\n")
	case len(r.Checks) == 0:
		b.WriteString("No checks have reported on this commit yet. If this repo has CI, " +
			"give it a minute and call this tool again.\n")
	default:
		b.WriteString("Checks on its head commit right now:\n")
		for _, c := range r.Checks {
			if c.Conclusion != "" {
				fmt.Fprintf(&b, "- %s: %s (%s)\n", c.Name, c.Status, c.Conclusion)
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Status)
		}
		b.WriteString("A check that is not \"completed\" yet is still running: call this " +
			"tool again later to see how it ended.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
