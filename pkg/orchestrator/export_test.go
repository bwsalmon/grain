package orchestrator

import "time"

// DisableBranchExistsSleep makes branchExistsSettled's own backoff
// (branchExistsRetries/branchExistsBackoff) stop actually sleeping, and
// returns the function that puts it back.
//
// sync_internal_test.go's withNoSleep already does exactly this, but it
// lives in this package and so can only reach the tests written here.
// Most of this package's tests are in orchestrator_test, and a dispatch
// driven from there runs the real thing: ProcessResult -> salvagePushedBranch
// -> branchExistsSettled, which re-reads a negative branchExists answer
// branchExistsRetries times with branchExistsBackoff between attempts.
// Against a simulated GitHub that answers instantly and honestly, every
// one of those waits is dead time -- seconds per dispatch, paid by every
// external test that finishes a run, with nothing about the retry
// behaviour being asserted (the internal test above owns that).
//
// This exists so TestMain can spend that time nowhere. Any test that does
// want the real backoff can restore it with the returned function.
func DisableBranchExistsSleep() func() {
	prev := branchExistsSleep
	branchExistsSleep = func(time.Duration) {}
	return func() { branchExistsSleep = prev }
}

// FixTaskDeadline is defaultFixTaskDeadline, for the merge-queue tests in
// orchestrator_test.
//
// The other two merge-queue clocks have setters (SetCheckStallDeadline,
// SetCheckRegistrationWindow) because their sightings live in memory and
// a test has no other way to reach them. This one is measured off the fix
// task's own CreatedAt, so a test drives it by moving the clock it hands
// SyncPullRequests -- and only needs to know how far to move it.
const FixTaskDeadline = defaultFixTaskDeadline
