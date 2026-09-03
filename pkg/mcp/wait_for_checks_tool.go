package mcp

// wait_for_checks is pull_request_status with the polling loop moved
// inside grain.
//
// pull_request_status answers "what does CI say right now", which is the
// honest answer to a question nobody actually asks: a run that has just
// pushed wants to know how CI *ended*, and the only way to get that out
// of a status read is to call it, be told "nothing has failed, but the
// unfinished checks carry no verdict yet", spend a turn sleeping, and
// call it again. Every one of those turns costs a model round trip and
// burns the run's turn budget on waiting rather than on work, and a run
// that guesses the interval wrong either wastes turns or -- worse --
// reads one queued check, decides it has waited long enough, and
// finishes on a build it never saw a verdict for. This tool blocks
// instead: one call, one answer, and the answer is always a verdict
// (failed, passed, nothing registered, or "still running when the clock
// ran out") rather than a snapshot.
//
// It returns the moment the outcome is settled, which is not the same as
// the moment CI is finished:
//
//   - any check failing ends the wait immediately, without waiting for
//     the rest. There is nothing left for the agent to learn by watching
//     a build it already has to fix, and the sooner it is told the more
//     of its turn budget is left to fix it in.
//   - every check completing with none failing ends it too. That is the
//     one green light in here.
//
// Everything else is bounded by the timeout, and by one shorter clock:
// GitHub reports no checks at all for a commit both when CI has not
// registered them yet and when the repo has no CI, and the two are
// indistinguishable in the moment. Blocking the full timeout on a repo
// with no CI would spend fifteen minutes learning nothing, so the empty
// answer is given firstCheckGrace to become non-empty and then reported
// as what it most likely is.
//
// The scope is fixed at process start exactly as pull_request_status's
// is (pullrequest_tools.go's doc comment): a run waits on CI for its own
// branch or for nothing. The commit is pinned too -- the branch head is
// read once, at the start, and the whole wait is about that SHA, so an
// answer can never be a mix of verdicts from two different pushes.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
)

// How long a wait can run for, and the knobs that are not an agent's to
// set. The default is sized for this repo's own CI, which builds
// container images and takes minutes rather than seconds; the maximum
// exists because a tool call that blocks for hours is indistinguishable
// from a wedged run to everything watching it.
//
// MaxWaitForChecksTimeout is exported because a client with a tool-call
// timeout of its own has to be told a number larger than this one, or it
// kills the call before the tool's own clock can produce the report that
// is the entire point (agent/claude's mcpToolTimeout).
const (
	DefaultWaitForChecksTimeout = 15 * time.Minute
	MaxWaitForChecksTimeout     = 60 * time.Minute
	minWaitForChecksTimeout     = 30 * time.Second
)

// checkPollInterval is how often the wait re-reads CI, and
// firstCheckGrace is how long an entirely empty check list is given to
// become a real one before it is reported as "this repo has no CI".
//
// Fifteen seconds is fast enough that the report lands within a turn of
// the build finishing and slow enough that a full-length wait costs a
// couple of hundred reads against a rate limit measured in thousands per
// hour. Neither is an argument: an agent choosing its own poll interval
// can only get this wrong, and both are only variables so a test can
// shrink them.
var (
	checkPollInterval = 15 * time.Second
	firstCheckGrace   = 3 * time.Minute
)

// checkReadAttempts is how many *consecutive* failed reads of CI end the
// wait. A single 500 or a dropped connection in the middle of a
// fifteen-minute wait is not a reason to throw away the wait -- the next
// poll is fifteen seconds away and will most likely succeed -- but a
// credential that cannot see checks at all fails every read, and a wait
// that retried that forever would block until the deadline and then
// report a timeout, hiding the real cause behind a wrong one.
const checkReadAttempts = 3

// NewWaitForChecksTools returns the blocking half of the CI loop:
// wait_for_checks, and nothing else. Registered on the same terms as
// pull_request_status, including the nil-client case -- see
// NewPullRequestTools.
func NewWaitForChecksTools(client PullRequestReader, scope PullRequestScope) []Tool {
	return []Tool{waitForChecksTool(client, scope)}
}

func waitForChecksTool(client PullRequestReader, scope PullRequestScope) Tool {
	return Tool{
		Name: "wait_for_checks",
		// The two numbers are formatted from the constants rather than
		// written out, since a description that disagrees with the
		// clamp teaches an agent a timeout it cannot actually have.
		Description: fmt.Sprintf("Block until CI has an actual verdict on the commit at the tip "+
			"of your branch, then report it -- instead of calling "+
			"pull_request_status over and over and spending turns waiting. "+
			"It returns as soon as any check fails (so you can start fixing "+
			"without waiting for the rest), or once every check has finished "+
			"with none failing, or when it times out. Push first: it reports "+
			"on your latest pushed commit and does not watch for new ones. "+
			"Optional timeout_seconds bounds the wait (default %d, maximum "+
			"%d); a timeout is reported, not an error, along with what each "+
			"check was doing when the clock ran out. This only reads GitHub; "+
			"it pushes nothing and changes nothing.",
			int(DefaultWaitForChecksTimeout.Seconds()), int(MaxWaitForChecksTimeout.Seconds())),
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"timeout_seconds": map[string]any{
					"type": "number",
					"description": fmt.Sprintf("How long to wait before giving up and reporting "+
						"what CI was doing at that moment. Defaults to %d; values outside "+
						"%d..%d are clamped into that range.",
						int(DefaultWaitForChecksTimeout.Seconds()),
						int(minWaitForChecksTimeout.Seconds()),
						int(MaxWaitForChecksTimeout.Seconds())),
				},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) Result {
			if client == nil || !scope.complete() {
				return Result{
					Text: "There is no CI to wait for: this run has no GitHub repository " +
						"configured for it. Do the work in the sandbox and report what " +
						"you found instead.",
					IsError: true,
				}
			}
			timeout, note := waitForChecksTimeout(args)
			text, err := newCheckWaiter(client, scope).wait(ctx, timeout)
			if err != nil {
				return Result{Text: err.Error(), IsError: true}
			}
			return Result{Text: note + text}
		},
	}
}

// waitForChecksTimeout reads the one argument, clamped, and returns the
// sentence to put in front of the report when the number used is not the
// number asked for.
//
// Clamping rather than refusing: an agent that asks for two hours has
// made a judgement about its own CI, not a mistake worth failing a call
// over, and the answer it gets after an hour is still the answer. Saying
// so on the report is what keeps the clamp from silently looking like a
// CI that finished early.
func waitForChecksTimeout(args map[string]any) (time.Duration, string) {
	seconds, ok := argFloat(args, "timeout_seconds")
	if !ok {
		if _, present := args["timeout_seconds"]; present {
			return DefaultWaitForChecksTimeout, fmt.Sprintf(
				"(timeout_seconds was not a number, so I waited up to %s.)\n\n",
				DefaultWaitForChecksTimeout)
		}
		return DefaultWaitForChecksTimeout, ""
	}
	requested := time.Duration(seconds * float64(time.Second))
	switch {
	case requested > MaxWaitForChecksTimeout:
		return MaxWaitForChecksTimeout, fmt.Sprintf(
			"(You asked for %s; %s is the longest wait allowed, so that is what I waited.)\n\n",
			roundDuration(requested), MaxWaitForChecksTimeout)
	case requested < minWaitForChecksTimeout:
		return minWaitForChecksTimeout, fmt.Sprintf(
			"(You asked for %s; %s is the shortest wait allowed, so that is what I waited.)\n\n",
			roundDuration(requested), minWaitForChecksTimeout)
	}
	return requested, ""
}

// checkWaiter is the loop itself. now and sleep are fields rather than
// direct calls into the time package so a test can run a whole
// fifteen-minute wait instantly and deterministically -- there is no
// sleeping to be done in a test, and a test that really slept would
// either be slow or would be testing the clock.
type checkWaiter struct {
	client PullRequestReader
	scope  PullRequestScope

	poll  time.Duration
	grace time.Duration
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newCheckWaiter(client PullRequestReader, scope PullRequestScope) checkWaiter {
	return checkWaiter{
		client: client,
		scope:  scope,
		poll:   checkPollInterval,
		grace:  firstCheckGrace,
		now:    time.Now,
		sleep:  sleepUntilCancelled,
	}
}

// sleepUntilCancelled waits out d, or returns early with ctx's error if
// the call is cancelled first. Cancellation has to reach in here: this
// is the only place a wait_for_checks call spends any time at all, so a
// cancelled run whose sleep ignored ctx would sit here for the rest of
// its timeout after everything else had been torn down.
func sleepUntilCancelled(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// wait blocks until CI on the branch's tip commit has a verdict, timeout
// elapses, or ctx is cancelled, and renders whichever of those happened.
//
// The returned error is for the cases where the tool could not do its
// job at all: an unreadable branch, an unreadable CI, a cancelled run.
// A build that failed, or one that never finished, is a perfectly good
// answer to the question asked and comes back as text.
func (w checkWaiter) wait(ctx context.Context, timeout time.Duration) (string, error) {
	// Read once, up front, and for the same reason pullRequestStatus
	// does: "you have not pushed anything yet" is a different answer
	// from anything CI could say, and blocking for fifteen minutes on a
	// branch that does not exist would be a particularly slow way to
	// not say it.
	head, err := w.client.GetBranchHead(w.scope.Owner, w.scope.Repo, w.scope.Branch)
	if err != nil {
		return "", fmt.Errorf("reading %s/%s's %s branch: %w", w.scope.Owner, w.scope.Repo, w.scope.Branch, err)
	}
	if head == nil {
		return fmt.Sprintf(
			"Branch %s does not exist on %s/%s yet, so there is no CI to wait for. "+
				"Commit your work and `git push origin %s`, then call this again to "+
				"wait for the checks that push triggers.",
			w.scope.Branch, w.scope.Owner, w.scope.Repo, w.scope.Branch), nil
	}
	sha := shortSHA(head.SHA)

	started := w.now()
	deadline := started.Add(timeout)
	var (
		seen      []github.CheckRun
		everSeen  bool
		failures  int
		lastError error
	)
	for {
		checks, err := checkRunsForCommit(w.client, w.scope, head.SHA)
		if err != nil {
			failures++
			lastError = err
			if failures >= checkReadAttempts {
				return "", fmt.Errorf("gave up after %d consecutive failed reads of CI for %s: %w",
					failures, sha, err)
			}
		} else {
			failures, lastError = 0, nil
			seen = checks
			everSeen = everSeen || len(checks) > 0
			tally := tallyChecks(checks)
			switch {
			case tally.failing > 0:
				return w.report(sha, w.now().Sub(started), tally, waitVerdictFailed), nil
			case len(checks) > 0 && tally.pending == 0:
				return w.report(sha, w.now().Sub(started), tally, waitVerdictPassed), nil
			case !everSeen && w.now().Sub(started) >= w.grace:
				return w.report(sha, w.now().Sub(started), tally, waitVerdictNoChecks), nil
			}
		}

		remaining := deadline.Sub(w.now())
		if remaining <= 0 {
			verdict := waitVerdictTimedOut
			if !everSeen {
				verdict = waitVerdictNoChecks
			}
			text := w.report(sha, w.now().Sub(started), tallyChecks(seen), verdict)
			if lastError != nil {
				text += fmt.Sprintf("\n\nThe last read of CI also failed, which may be why "+
					"there is nothing newer to report: %v", lastError)
			}
			return text, nil
		}
		// The last nap is only as long as there is left: sleeping a whole
		// interval past the deadline would report a verdict up to a poll
		// interval staler than the one that was asked for.
		nap := w.poll
		if remaining < nap {
			nap = remaining
		}
		if err := w.sleep(ctx, nap); err != nil {
			return "", fmt.Errorf("the wait for CI on %s was cancelled after %s: %w",
				sha, roundDuration(w.now().Sub(started)), err)
		}
	}
}

// The four ways a wait ends, each with its own closing paragraph: what
// happened, and what the agent should do next.
type waitVerdict int

const (
	waitVerdictFailed waitVerdict = iota
	waitVerdictPassed
	waitVerdictTimedOut
	waitVerdictNoChecks
)

// report renders one whole answer: how long it waited, what every check
// was doing when it stopped waiting, and what that means.
func (w checkWaiter) report(sha string, waited time.Duration, tally checkTally, verdict waitVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Waited %s for CI on %s at %s.\n\n", roundDuration(waited), w.scope.Branch, sha)

	if verdict == waitVerdictNoChecks {
		fmt.Fprintf(&b, "GitHub still reports no checks at all against %s after %s. Either this "+
			"repo has no CI configured, or its checks are registered by something slower "+
			"than a push -- a workflow that only runs on pull requests, for instance, has "+
			"nothing to report until one is open. Nothing here says your change is good; "+
			"it says nobody looked.", sha, roundDuration(waited))
		return b.String()
	}

	fmt.Fprintf(&b, "Checks against %s:\n%s", sha, tally.lines)
	fmt.Fprintf(&b, "\n%d failing, %d not finished, %d otherwise done.\n\n", tally.failing, tally.pending, tally.passed)

	switch verdict {
	case waitVerdictFailed:
		fmt.Fprintf(&b, "CI has failed, so I stopped waiting")
		if tally.pending > 0 {
			fmt.Fprintf(&b, " -- the %d unfinished check(s) above are still running and may "+
				"fail too", tally.pending)
		}
		fmt.Fprintf(&b, ". Reproduce those failures in your checkout, fix them, commit, and "+
			"`git push origin %s` -- each push reruns CI against the new commit, and "+
			"calling this again afterwards waits for the verdict on the fix.", w.scope.Branch)
	case waitVerdictPassed:
		fmt.Fprintf(&b, "Every check against %s finished and none of them failed. Note that a "+
			"green build is not the same as a mergeable branch: call pull_request_status "+
			"if you also need to know whether the branch still merges into its base.", sha)
	case waitVerdictTimedOut:
		fmt.Fprintf(&b, "The wait timed out with %d check(s) still running, so none of this is a "+
			"verdict yet -- an unfinished check has not passed. Call this again to keep "+
			"waiting (with a larger timeout_seconds if this CI is simply slow), rather "+
			"than finishing on a build you have not seen the end of.", tally.pending)
	}
	return b.String()
}

// roundDuration renders a wait the way a human reads one: whole seconds,
// since nothing here is measured finely enough for a nanosecond tail to
// mean anything.
func roundDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}
