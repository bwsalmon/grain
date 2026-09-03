package mcp

// Reading a tool's own answer back, for the run census.
//
// orchestrator records what each run did with its tools
// (model.RunTelemetry), and two of the facts worth recording are only
// visible in the text a tool returned: whether a run_command was ended by
// its bound rather than by the command, and which of its four verdicts a
// wait_for_checks call came back with. Neither is in the tool's name, its
// arguments or its error flag -- a bounded-out command is an error like
// any other, and every wait_for_checks answer is a success.
//
// The readers live here, beside the writers, because that is the only
// arrangement in which they cannot drift apart. Each matches a marker
// that the sentence it is reading is *built from*, so a reworded report
// changes the marker and both halves move together; result_size.go's cap
// and run_deadline.go's notice may be appended to the same text without
// disturbing either. What a reader cannot recognise it reports as
// nothing at all rather than as a guess -- the same rule pkg/metrics
// holds to for a moment that was never recorded.

import (
	"strings"
	"time"
)

// The four verdicts a wait_for_checks call can end in, as the words
// task_run_check_wait.verdict stores. They are this package's vocabulary
// rather than the store's: what a wait can conclude is a fact about this
// tool, and a fifth verdict would be added here first.
const (
	// WaitVerdictFailed: at least one check failed, and the wait
	// returned without waiting for the rest.
	WaitVerdictFailed = "failed"
	// WaitVerdictPassed: every check finished and none failed. The one
	// green light.
	WaitVerdictPassed = "passed"
	// WaitVerdictTimedOut: checks were still running when the wait's own
	// clock ran out. A deployment where most waits end this way has
	// DefaultWaitForChecksTimeout set wrong for its CI.
	WaitVerdictTimedOut = "timed_out"
	// WaitVerdictNoChecks: GitHub reported no checks at all against the
	// commit -- either this repo has no CI, or nothing had registered any
	// yet.
	WaitVerdictNoChecks = "no_checks"
)

// CheckWait is one wait_for_checks answer, read back off its own text.
//
// Waited is how long the call blocked; WaitedKnown is false when the
// header it comes from could not be parsed, which is the difference
// between "this wait took no time" and "nothing here says how long it
// took". A caller that stores it anyway (orchestrator does, as zero) is
// safe: pkg/metrics drops a non-positive duration rather than counting it
// as a zero-second wait.
type CheckWait struct {
	Verdict     string
	Waited      time.Duration
	WaitedKnown bool
}

// ReadCheckWait reads a wait_for_checks result back as the verdict it
// reported, and reports whether the text was a verdict at all.
//
// Not every answer is one: a wait against a branch that has not been
// pushed yet, and a wait that could not read CI at all, are both real
// answers to the agent and neither is a verdict about a build. Those
// return false and are counted nowhere, since folding them into any of
// the four would misreport a run that had not pushed as a run whose CI
// said something.
//
// The order the markers are tried in matters for exactly one of them:
// only the failed report carries text grain did not write (each failing
// job's own log excerpt, failing_job_logs.go), so it is matched first and
// a log that happens to quote another verdict's wording cannot displace
// it.
func ReadCheckWait(text string) (CheckWait, bool) {
	var verdict string
	switch {
	case strings.Contains(text, waitFailedMarker):
		verdict = WaitVerdictFailed
	case strings.Contains(text, waitNoChecksMarker):
		verdict = WaitVerdictNoChecks
	case strings.Contains(text, waitTimedOutMarker):
		verdict = WaitVerdictTimedOut
	case strings.Contains(text, waitPassedMarker):
		verdict = WaitVerdictPassed
	default:
		return CheckWait{}, false
	}
	waited, known := waitedFrom(text)
	return CheckWait{Verdict: verdict, Waited: waited, WaitedKnown: known}, true
}

// waitedFrom reads the "Waited 4m12s for CI on ..." header every report
// opens with. The clamp note waitForChecksTimeout can put in front of it
// is why this searches rather than reading a prefix.
func waitedFrom(text string) (time.Duration, bool) {
	start := strings.Index(text, waitHeaderPrefix)
	if start < 0 {
		return 0, false
	}
	rest := text[start+len(waitHeaderPrefix):]
	end := strings.Index(rest, waitHeaderSuffix)
	if end < 0 {
		return 0, false
	}
	waited, err := time.ParseDuration(rest[:end])
	if err != nil || waited < 0 {
		return 0, false
	}
	return waited, true
}

// RunCommandTimedOut reports whether a run_command answer is one the
// bound ended rather than the command -- runCommandBound.timedOutNotice,
// which both transports append (a local context deadline, or the guest's
// own `timeout` exiting 124).
//
// The other two notices a bound can produce are deliberately not counted
// here. killedNotice's exit 137 is hedged on purpose -- 128+SIGKILL is
// also what the kernel's OOM killer leaves behind -- and
// transportStalledNotice is grain giving up on a guest that never
// answered, which is a sandbox that stopped talking rather than a command
// that ran long. Counting either as a timeout would put a number on the
// timeout rate that is not one.
//
// Only the trailing paragraphs are searched, because that is where grain
// appends its own notices: a command whose *output* happens to contain
// this sentence -- a run reading back an earlier result, a grep over this
// file -- is not a command that timed out.
func RunCommandTimedOut(text string) bool {
	for _, para := range trailingParagraphs(text, 2) {
		// Trimmed because a stream that ended in its own newline leaves
		// the notice's paragraph starting with one.
		if strings.HasPrefix(strings.TrimSpace(para), runCommandKilledMarker) {
			return true
		}
	}
	return false
}

// trailingParagraphs is the last n blank-line-separated blocks of text.
// Two is enough for every answer grain builds: the bound's own notice,
// and the run-deadline notice that may follow it (withDeadlineNotice).
func trailingParagraphs(text string, n int) []string {
	paras := strings.Split(text, "\n\n")
	if len(paras) > n {
		paras = paras[len(paras)-n:]
	}
	return paras
}
