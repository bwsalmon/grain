package model

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// State is computed twice on purpose — once as a SQL view so the store
// makes "state is never written" structural, and once in Go so code
// holding a Task needs no database. Two implementations of one rule is a
// drift risk, and this file is what pays for it.

var (
	thenState = regexp.MustCompile(`THEN '(\w+)'`)
	elseState = regexp.MustCompile(`ELSE '(\w+)'`)
)

func stateView() string {
	for _, v := range Views {
		if v.Name == "task_state" {
			return v.DDL
		}
	}
	panic("no task_state view")
}

// viewPrecedence is the state strings in task_state's CASE, in the order
// the CASE tests them — which is the view's precedence.
func viewPrecedence() []string {
	var out []string
	for _, m := range thenState.FindAllStringSubmatch(stateView(), -1) {
		out = append(out, m[1])
	}
	for _, m := range elseState.FindAllStringSubmatch(stateView(), -1) {
		out = append(out, m[1])
	}
	return out
}

// goPrecedence is the same precedence, discovered by turning each
// condition on from the most significant down and seeing what StateOf
// returns.
func goPrecedence() []string {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	id := int64(5)
	approved := task(true)
	unapproved := task(false)
	got := []State{
		StateOf(approved, &Observation{ClosedAt: &now}, true, 0),
		StateOf(approved, &Observation{CompletedAt: &now}, true, 0),
		StateOf(approved, &Observation{PendingQuestionCommentID: &id}, true, 0),
		StateOf(approved, nil, true, 0),
		StateOf(approved, nil, false, MaxConsecutiveFailures),
		StateOf(unapproved, nil, false, 0),
		StateOf(approved, nil, false, 0),
	}
	out := make([]string, len(got))
	for i, s := range got {
		out[i] = string(s)
	}
	return out
}

func task(approved bool) Task {
	t := Task{
		ID: "a1b2", Intent: IntentImplement, Title: "t",
		Origin: Origin{
			Attribution: Attribution{Actor: Principal{PrincipalHuman, "someone"}},
			Reason:      ReasonDirect,
		},
		Binding: BindingDirective,
	}
	if approved {
		t.Approval = &Attribution{Actor: Principal{PrincipalHuman, "someone"}}
	}
	return t
}

func TestViewAndGoDerivationAgreeOnPrecedence(t *testing.T) {
	// If somebody reorders one CASE branch and not the other, this is
	// what says so. The failure is otherwise a task in the wrong state
	// only when two conditions happen to be true at once.
	if got, want := viewPrecedence(), goPrecedence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("view precedence %v, Go precedence %v", got, want)
	}
}

func TestEveryStateIsReachable(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range goPrecedence() {
		seen[s] = true
	}
	for _, s := range []State{StateProposed, StateQueued, StateRunning,
		StateAwaitingReply, StateFailed, StateCompleted, StateClosed} {
		if !seen[string(s)] {
			t.Errorf("state %q is not reachable from the derivation", s)
		}
	}
}

func TestBlockedIsNotAState(t *testing.T) {
	// Blocked is an annotation on queued, derived from links, which is
	// why it lives in its own view.
	for _, s := range viewPrecedence() {
		if s == "blocked" {
			t.Fatal("blocked leaked into the state vocabulary")
		}
	}
}

func TestStateIsAViewAndNotAColumn(t *testing.T) {
	// The invariant the schema exists to make structural: there is no
	// column to write, so no finish path can write one.
	for _, table := range Tables {
		if strings.Contains(table, "CREATE TABLE IF NOT EXISTS `task` ") {
			if strings.Contains(table, "`state`") || strings.Contains(table, "state_since") {
				t.Fatal("task table grew a state column")
			}
		}
	}
}

func TestDeclarationAndObservationAreSeparateTables(t *testing.T) {
	// They answer to different records, and keeping them apart is what
	// would let a declaration change be branched and reviewed while
	// observations keep landing on the trunk.
	var taskTable string
	for _, table := range Tables {
		if strings.Contains(table, "CREATE TABLE IF NOT EXISTS `task` ") {
			taskTable = table
		}
	}
	for _, observed := range []string{"closed_at", "completed_at", "baseline_comment_id", "retry_requested_at"} {
		if strings.Contains(taskTable, observed) {
			t.Errorf("observation column %q leaked into the task table", observed)
		}
	}
}

func TestLinkKindsThatBlock(t *testing.T) {
	// merge-with gates the merge, not the run, which is what lets the
	// members of a coordinated cross-repo change be worked in parallel.
	for kind, want := range map[LinkKind]bool{
		LinkDependsOn: true, LinkChildOf: true,
		LinkMergeWith: false, LinkFixes: false,
		LinkAddresses: false, LinkProposedBy: false,
	} {
		if got := kind.Blocks(); got != want {
			t.Errorf("%s.Blocks() = %v, want %v", kind, got, want)
		}
	}
}

func TestLandingStateIsDecidedByTheActorNotTheReason(t *testing.T) {
	human := Principal{PrincipalHuman, "someone"}
	bot := Principal{PrincipalAutomation, "grain"}
	// Every reason, with a human actor: queued. With automation: not.
	// A sixth reason cannot queue itself because reasons are not read.
	for _, r := range []OriginReason{ReasonDirect, ReasonSchedule, ReasonFix,
		ReasonReview, ReasonProposal} {
		if !LandsQueued(Origin{Attribution{Actor: human}, r}) {
			t.Errorf("human + %s should land queued", r)
		}
		if LandsQueued(Origin{Attribution{Actor: bot, OnBehalfOf: &human}, r}) {
			t.Errorf("automation + %s must not land queued, even on a human's behalf", r)
		}
	}
}

func TestTransitionsCoversProposedThroughCompleted(t *testing.T) {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	approved := created.Add(time.Minute)
	started := approved.Add(time.Minute)
	finished := started.Add(10 * time.Minute)

	tk := task(true)
	tk.CreatedAt, tk.ApprovedAt = &created, &approved
	obs := &Observation{CompletedAt: &finished}
	runs := []Run{{StartedAt: started, FinishedAt: &finished}}

	got := Transitions(tk, obs, runs, nil, nil)
	want := []Transition{
		{StateProposed, created},
		{StateQueued, approved},
		{StateRunning, started},
		{StateCompleted, finished},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transitions = %+v, want %+v", got, want)
	}
}

func TestTransitionsRequeuesBetweenAttemptsButNotAfterTheLastOne(t *testing.T) {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	firstStart := created.Add(time.Minute)
	firstEnd := firstStart.Add(time.Minute)
	secondStart := firstEnd.Add(time.Minute)
	secondEnd := secondStart.Add(time.Minute)

	tk := task(true)
	tk.CreatedAt, tk.ApprovedAt = &created, &firstStart
	obs := &Observation{CompletedAt: &secondEnd}
	runs := []Run{
		{StartedAt: firstStart, FinishedAt: &firstEnd},
		{StartedAt: secondStart, FinishedAt: &secondEnd},
	}

	got := Transitions(tk, obs, runs, nil, nil)
	want := []Transition{
		{StateProposed, created},
		{StateQueued, firstStart},
		{StateRunning, firstStart},
		// The requeue between the two attempts, but nothing after the
		// second's own finish -- that instant is StateCompleted instead.
		{StateQueued, firstEnd},
		{StateRunning, secondStart},
		{StateCompleted, secondEnd},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transitions = %+v, want %+v", got, want)
	}
}

func TestTransitionsShowsFailedOnceTheStreakCapsOut(t *testing.T) {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	tk := task(true)
	tk.CreatedAt, tk.ApprovedAt = &created, &created
	lastFinished := created.Add(time.Hour)
	streak := &FailureStreak{Count: MaxConsecutiveFailures, LastFinishedAt: lastFinished, LastOutcome: "failed"}

	got := Transitions(tk, nil, nil, streak, nil)
	want := []Transition{
		{StateProposed, created},
		{StateQueued, created},
		{StateFailed, lastFinished},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transitions = %+v, want %+v", got, want)
	}
}

// TestTransitionsHidesFailedOnceTheTaskHasCompleted covers
// bwsalmon/agents#502: a run salvaged into a pull request after erroring
// keeps its own outcome "failed" forever (orchestrator.salvagePushedBranch
// never corrects it), so task_streak's count can sit at or above
// MaxConsecutiveFailures for a task that has, in every other respect,
// completed. StateOf already masks that streak the moment
// obs.CompletedAt is set; Transitions has to apply the same precedence
// when reconstructing history, or a completed task's timeline shows a
// bogus "Failed" entry right before "Completed", forever.
func TestTransitionsHidesFailedOnceTheTaskHasCompleted(t *testing.T) {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	tk := task(true)
	tk.CreatedAt, tk.ApprovedAt = &created, &created
	lastFinished := created.Add(time.Hour)
	streak := &FailureStreak{Count: MaxConsecutiveFailures, LastFinishedAt: lastFinished, LastOutcome: "failed"}
	obs := &Observation{CompletedAt: &lastFinished}

	got := Transitions(tk, obs, nil, streak, nil)
	want := []Transition{
		{StateProposed, created},
		{StateQueued, created},
		{StateCompleted, lastFinished},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transitions = %+v, want %+v", got, want)
	}
}

func TestTransitionsShowsAPendingQuestionOnlyWhileOutstanding(t *testing.T) {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	asked := created.Add(time.Hour)
	tk := task(true)
	tk.CreatedAt, tk.ApprovedAt = &created, &created
	obs := &Observation{PendingQuestionCommentID: new(int64)}
	runs := []Run{{StartedAt: created.Add(time.Minute), FinishedAt: &asked}}

	got := Transitions(tk, obs, runs, nil, &asked)
	want := []Transition{
		{StateProposed, created},
		{StateQueued, created},
		{StateRunning, created.Add(time.Minute)},
		{StateAwaitingReply, asked},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transitions = %+v, want %+v", got, want)
	}

	// Once answered, the caller passes no askedAt at all -- the period is
	// gone from the record, not merely unlabelled.
	obs.PendingQuestionCommentID = nil
	got = Transitions(tk, obs, runs, nil, nil)
	for _, tr := range got {
		if tr.State == StateAwaitingReply {
			t.Fatalf("an answered question should leave no awaiting_reply transition: %+v", got)
		}
	}
}

func TestIsBlockedIgnoresNonBlockingLinks(t *testing.T) {
	tk := task(true)
	tk.Links = []Link{
		{Kind: LinkProposedBy, Target: "open-but-irrelevant"},
		{Kind: LinkMergeWith, Target: "also-open"},
	}
	if IsBlocked(tk, map[string]bool{}) {
		t.Error("provenance and merge-group links must not block dispatch")
	}
	tk.Links = append(tk.Links, Link{Kind: LinkDependsOn, Target: "c3d4"})
	if !IsBlocked(tk, map[string]bool{}) {
		t.Error("an open depends-on must block")
	}
	if IsBlocked(tk, map[string]bool{"c3d4": true}) {
		t.Error("a closed depends-on must not block")
	}
}

func TestLeaseExpiry(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	issued := now.Add(-30 * time.Hour)
	l := Lease{IssuedAt: issued}
	if l.Expired(now, 0) {
		t.Error("a lease with no expiry and no backstop is not expired")
	}
	// The backstop exists because materialisation has a window that
	// cannot be closed: a failure between minting and recording leaks a
	// credential nothing knows to revoke.
	if !l.Expired(now, 24*time.Hour) {
		t.Error("the 24h backstop should have caught a 30h-old lease")
	}
	soon := now.Add(time.Hour)
	if (Lease{IssuedAt: issued, ExpiresAt: &soon}).Expired(now, 0) {
		t.Error("a lease expiring later is not expired yet")
	}
}
