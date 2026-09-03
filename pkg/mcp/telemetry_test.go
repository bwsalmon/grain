package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
)

// Every verdict, driven through the real wait and read back off the real
// report. Asserting against a hand-written string would only prove that
// two literals in this file match each other; what has to hold is that
// the answer a run actually gets still classifies, so each case here runs
// the waiter and hands its whole output to ReadCheckWait.
func TestReadCheckWaitClassifiesEveryVerdictTheWaiterProduces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		client     *scriptedChecks
		want       string
		wantWaited time.Duration
	}{
		{
			name: "passed",
			client: &scriptedChecks{head: pushed(), rounds: [][]github.CheckRun{
				{running("tests")},
				{done("tests", "success")},
			}},
			want:       WaitVerdictPassed,
			wantWaited: 15 * time.Second,
		},
		{
			name: "failed, with the failing job's own log appended",
			client: &scriptedChecks{head: pushed(), rounds: [][]github.CheckRun{
				{running("tests")},
				{done("tests", "failure")},
			}, jobLogs: []github.JobLog{{
				Name: "tests",
				// A log that quotes another verdict's wording, which is
				// the one case the marker order in ReadCheckWait exists
				// for: this is still a failure.
				Log: "the wait timed out with 3 check(s) still running\n",
			}}},
			want:       WaitVerdictFailed,
			wantWaited: 15 * time.Second,
		},
		{
			name: "timed out",
			client: &scriptedChecks{head: pushed(), rounds: [][]github.CheckRun{
				{running("tests")},
			}},
			want:       WaitVerdictTimedOut,
			wantWaited: 30 * time.Second,
		},
		{
			// The wait's own clock runs out before the grace period does,
			// and an empty check list at that point is still reported as
			// "nobody looked" rather than as a timeout.
			name:       "no checks at all",
			client:     &scriptedChecks{head: pushed()},
			want:       WaitVerdictNoChecks,
			wantWaited: 30 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{}
			text, err := waiterFor(tc.client, clock).wait(context.Background(), 30*time.Second)
			if err != nil {
				t.Fatalf("wait: %v", err)
			}
			got, ok := ReadCheckWait(text)
			if !ok {
				t.Fatalf("ReadCheckWait found no verdict in:\n%s", text)
			}
			if got.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q, in:\n%s", got.Verdict, tc.want, text)
			}
			if !got.WaitedKnown || got.Waited != tc.wantWaited {
				t.Errorf("waited = %v (known %v), want %v, in:\n%s",
					got.Waited, got.WaitedKnown, tc.wantWaited, text)
			}
		})
	}
}

// A timeout the run's own clock caused says something different in its
// closing paragraph -- there is no larger timeout_seconds to ask for --
// and it is still a timeout. The two halves reached this file from
// different branches, so this is the case where a verdict would most
// easily go unread: a deployment whose runs keep outliving their CI
// would otherwise measure that as having never waited at all.
func TestReadCheckWaitReadsATimeoutTheRunsOwnClockCaused(t *testing.T) {
	waiter := waiterFor(&scriptedChecks{head: pushed(), rounds: [][]github.CheckRun{
		{running("tests")},
	}}, &fakeClock{})
	waiter.deadlineClamped = true

	text, err := waiter.wait(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	got, ok := ReadCheckWait(text)
	if !ok || got.Verdict != WaitVerdictTimedOut {
		t.Errorf("ReadCheckWait = %+v, %v; want a timed-out verdict, in:\n%s", got, ok, text)
	}
	if !got.WaitedKnown || got.Waited != 30*time.Second {
		t.Errorf("waited = %v (known %v), want 30s, in:\n%s", got.Waited, got.WaitedKnown, text)
	}
}

// The clamp note waitForChecksTimeout puts in front of a report, and the
// deadline notice a Registry appends to the back of one, both leave the
// verdict readable: neither is part of the report, and a reader that only
// looked at the first or last line would lose one of them.
func TestReadCheckWaitSurvivesTheNoticesAroundAReport(t *testing.T) {
	clock := &fakeClock{}
	text, err := waiterFor(&scriptedChecks{head: pushed(), rounds: [][]github.CheckRun{
		{done("tests", "success")},
	}}, clock).wait(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := withDeadlineNotice("(You asked for 27h46m39s; 1h0m0s is the longest wait allowed, "+
		"so that is what I waited.)\n\n"+text, runDeadlineNotice(4*time.Minute))
	got, ok := ReadCheckWait(wrapped)
	if !ok || got.Verdict != WaitVerdictPassed {
		t.Errorf("ReadCheckWait(%q) = %+v, %v; want a passed verdict", wrapped, got, ok)
	}
	if !got.WaitedKnown {
		t.Errorf("the wait's own duration was lost behind the notices:\n%s", wrapped)
	}
}

// An answer that is not a verdict is counted as none of the four. A run
// that called wait_for_checks before pushing anything did not learn that
// its CI said nothing; it learned that it had not pushed, and folding
// that into "no checks registered" would report a CI problem that does
// not exist.
func TestReadCheckWaitRefusesAnswersThatAreNotVerdicts(t *testing.T) {
	clock := &fakeClock{}
	text, err := waiterFor(&scriptedChecks{head: nil}, clock).wait(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "does not exist") {
		t.Fatalf("expected the not-pushed answer, got:\n%s", text)
	}
	if got, ok := ReadCheckWait(text); ok {
		t.Errorf("ReadCheckWait(%q) = %+v, want no verdict", text, got)
	}
	if got, ok := ReadCheckWait("exit=0\nstdout:\nhello\nstderr:\n"); ok {
		t.Errorf("a run_command answer read as the CI verdict %+v", got)
	}
}

// The timeout notice is recognised through everything that can be around
// it -- the command's own output above, the deadline notice below -- and
// is not recognised in output that merely quotes it, which is the whole
// reason only grain's own trailing notices are searched.
func TestRunCommandTimedOutReadsTheBoundsOwnNotice(t *testing.T) {
	bound := runCommandBound{d: 300 * time.Second}
	timedOut := formatRunCommandResult(-1, "building...\n", "", bound.timedOutNotice())
	if !RunCommandTimedOut(timedOut) {
		t.Errorf("a bounded-out command did not read as one:\n%s", timedOut)
	}
	if !RunCommandTimedOut(withDeadlineNotice(timedOut, runDeadlineNotice(3*time.Minute))) {
		t.Error("the deadline notice hid the bound's own notice")
	}

	finished := formatRunCommandResult(0, "ok\n", "", "")
	if RunCommandTimedOut(finished) {
		t.Errorf("a command that finished read as timed out:\n%s", finished)
	}
	// A run reading an earlier answer back, or grepping for the notice:
	// the sentence is in the command's own output, not in a notice.
	quoted := formatRunCommandResult(0, "grep found: "+bound.timedOutNotice()+"\n", "", "")
	if RunCommandTimedOut(quoted) {
		t.Errorf("output quoting the notice read as a timeout:\n%s", quoted)
	}
	// The two notices deliberately not counted: 137 is hedged between the
	// bound and the OOM killer, and a stalled transport is a sandbox that
	// stopped answering rather than a command that ran long.
	if RunCommandTimedOut(formatRunCommandResult(137, "", "", bound.killedNotice())) {
		t.Error("exit 137 counted as a timeout, though it may be the OOM killer")
	}
	if RunCommandTimedOut(formatRunCommandResult(-1, "", "", bound.transportStalledNotice())) {
		t.Error("a stalled guest counted as a command timeout")
	}
}
