package mcp

// The tail of each failing job's own log, hung off whichever CI answer
// named that job as failing.
//
// pull_request_status and wait_for_checks both used to stop at the name:
// "FAILING go (failure)". That is enough for a run to know its build is
// red and not enough to do anything about it, so its next move was always
// to guess at what broke and try to reproduce it -- in a sandbox that is
// not the runner and may not be able to run the failing suite at all.
// github.JobLog's own doc comment is where that gap is written down
// ("a check run says only that a job called 'go' did not pass"), and
// orchestrator's requeueForRepair already closes it for the other reader of a
// red build: the fix task the merge queue files carries the end of each
// failing job's log in its body rather than a job name and an invitation
// to go and find out what it said. A run watching its own CI now gets
// the same thing, rendered the same way and bounded the same way --
// github.JobLogTailBytes over the wire, github.JobLogExcerpt on the page.
//
// Read only once something has failed, never on the way there. This is
// three extra GitHub reads (github.FailedJobLogs walks a commit's runs,
// a failed run's jobs, then each failed job's log), and there is nothing
// for them to find until a job has actually gone red: a wait_for_checks
// call that fetched logs on every fifteen-second poll would spend its
// whole budget reading nothing.
//
// Best effort, deliberately, and for the same reason failingJobLogs is
// in the orchestrator and checkRunsForCommit falls back rather than
// failing: the logs are a credential-dependent bonus on top of an answer
// that already stands up without them. A fine-grained token without
// "Actions" read, or a repo whose CI is Buildkite rather than Actions,
// has to degrade to the answer this tool gave before -- the checks, with
// the failing ones named -- rather than turn a report of a red build
// into an error about logs.

import (
	"fmt"
	"strings"

	"github.com/bwsalmon/grain/pkg/github"
)

// failingJobLogs renders what the failed jobs against sha printed, as a
// block to append to a report that has already named them. It always
// returns something to append: either the logs, or the one sentence
// explaining why there are none, since a reader who has just been told a
// job failed and shown nothing under it cannot tell "no log" from "this
// tool forgot".
func failingJobLogs(client PullRequestReader, scope PullRequestScope, sha string) string {
	logs, err := client.FailedJobLogs(scope.Owner, scope.Repo, sha)
	if err != nil {
		return "\n\n" + logsUnreadable(err)
	}
	if len(logs) == 0 {
		return "\n\nGitHub Actions has no failed job with a readable log against this commit, " +
			"so the check names above are all there is: the failing check may be posted by " +
			"CI that is not Actions (which has no job log to fetch), or its log may have " +
			"expired. Reproduce the failure in your checkout instead."
	}

	var b strings.Builder
	b.WriteString("\n\nWhat those jobs printed -- the end of each failing job's own log, " +
		"copied here because your sandbox is not the runner that produced it:\n")
	for _, l := range logs {
		fmt.Fprintf(&b, "\n### %s\n", l.Name)
		if l.URL != "" {
			fmt.Fprintf(&b, "\n%s\n", l.URL)
		}
		// Four backticks, so a log that itself contains a fenced block --
		// any Go test printing three backticks does -- cannot close this
		// one early. requeueForRepair fences the same way, for the same reason.
		fmt.Fprintf(&b, "\n````\n%s\n````\n", github.JobLogExcerpt(l.Log))
		if l.Truncated {
			b.WriteString("\n(the tail of the log, not all of it -- the job's own page above " +
				"has the whole thing)\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// logsUnreadable is the sentence that replaces the logs when GitHub would
// not give them up. A refused read is named as a property of this
// deployment's credential rather than of the build, so a run does not
// read it as "the failure has no output" and go looking for one.
func logsUnreadable(err error) string {
	if github.IsPermissionDenied(err) {
		return "This grain deployment's GitHub credential may not read Actions job logs, so " +
			"the check names above are all there is -- an operator has to grant it before " +
			"the failing job's output can travel with this answer. Reproduce the failure " +
			"in your checkout instead."
	}
	return fmt.Sprintf(
		"I could not read the failing jobs' logs, so the check names above are all there "+
			"is; reproduce the failure in your checkout instead. GitHub said: %v", err)
}
