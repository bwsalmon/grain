package orchestrator

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

// errUsageLimit is context.Cause's own report, by identity, for a run
// Pause cancelled because the deployment's agent has no budget left in
// its current window -- the third way a run here ends without having
// failed, alongside errTaskClosed and errRunTimedOut, and read the same
// way (errors.Is against context.Cause) by RunDispatch.
var errUsageLimit = errors.New("orchestrator: the agent's usage limit was reached while this run was live")

// How long dispatch waits before trying again, when the provider's own
// answer needs bounding:
//
//   - defaultUsageLimitPause is the wait when the refusal named no reset
//     at all, which is the common shape of a bare 429. Long enough that
//     the deployment is not hammering a provider that just said no,
//     short enough that a limit which has in fact already lifted costs
//     one idle quarter of an hour rather than an evening.
//   - minUsageLimitPause is the floor under a provider that named a very
//     short delay. The point of pausing is to stop the queue walking
//     into the same wall run after run, and a few seconds does not.
//   - maxUsageLimitPause is the ceiling over one that named a very long
//     one -- or over a reset instant this code misread. A deployment
//     idling for a wrong six hours is recoverable; one idling until 2286
//     because an epoch was parsed in the wrong unit is not something
//     anybody would think to look for.
//
// Retrying once the pause lifts is not a promise the limit is over: a
// window that has not actually reset produces one more refusal, one more
// pause, and no further traffic. That is the property these numbers are
// chosen for, rather than for guessing any particular provider's window.
const (
	defaultUsageLimitPause = 15 * time.Minute
	minUsageLimitPause     = time.Minute
	maxUsageLimitPause     = 6 * time.Hour
)

// Pause is the deployment-wide "stop dispatching, there is no budget
// left" gate: what an agent's own usage limit
// (agent.UsageLimitError) turns into for the rest of the deployment.
//
// A usage limit is not a fact about the run that met it. Every other run
// in flight is spending the same credential, and every task still queued
// will meet the same refusal the moment it is dispatched -- each one
// burning an attempt, a sandbox, and a place in its own failure streak
// on an outage none of them caused. So the run that meets one pauses
// dispatch until the window resets (Begin), and the runs already in
// flight are cancelled rather than left to grind through the same wall
// (register, which is how Begin can reach them at all). Every run ended
// either way records model.PausedOutcome -- its own word rather than
// "failed" or "cancelled", and one that neither task_streak nor
// Store.FailureStreak counts, so an outage does not spend the retry
// budget of every task that happened to be running when it began.
//
// It is in-process state, not a store row, for the same reason
// CycleTimes is: it describes what this daemon is doing right now, and a
// restart is entitled to find out for itself. A daemon restarted inside
// a paused window dispatches one run, meets the same limit, and pauses
// again -- one wasted attempt, against a durable row that would have to
// be reconciled with a credential that may by then have changed
// entirely.
//
// The zero value is ready to use, and every method tolerates a nil
// receiver: a caller that wires no Pause (a test, a one-shot cycle) gets
// a deployment that reports a usage limit on the run that met it and
// pauses nothing -- see Config.Pause.
//
// Every method is safe for concurrent callers.
type Pause struct {
	mu sync.Mutex
	// until is when dispatch may resume; zero when nothing is paused.
	until time.Time
	// reason is what the provider said, kept for the log line and for
	// Until's own caller.
	reason string
	// live is every run currently cancellable by a pause, keyed by run
	// ID -- RunDispatch registers its own runCtx's cancel for as long as
	// its agent is running, and no longer.
	live map[string]context.CancelCauseFunc
}

// register makes runID cancellable by a pause and returns the function
// that stops it being so, which RunDispatch calls the moment its
// framework returns.
//
// A run registering while a pause is already in force is cancelled on
// the spot. That is not the ordinary path -- reconcileDispatch does not
// dispatch while paused -- but a run dispatched by the cycle that was
// already in flight when the pause began has nowhere else to be caught,
// and it would otherwise spend a whole agent run discovering the limit
// for itself.
func (p *Pause) register(runID string, now time.Time, cancel context.CancelCauseFunc) func() {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	if p.live == nil {
		p.live = map[string]context.CancelCauseFunc{}
	}
	p.live[runID] = cancel
	paused := !p.until.IsZero() && now.Before(p.until)
	p.mu.Unlock()

	if paused {
		cancel(errUsageLimit)
	}
	return func() {
		p.mu.Lock()
		delete(p.live, runID)
		p.mu.Unlock()
	}
}

// Begin records that a run met limit at now: dispatch is paused until
// the window that limit names resets, and every run still in flight is
// cancelled. It returns the instant dispatch resumes, which is what the
// caller records in the run's own detail.
//
// An already-paused deployment keeps the later of the two instants. Two
// runs meeting the same limit within a second of each other is the
// ordinary case -- they were in flight together -- and the second must
// not be able to shorten the wait the first established, while a genuine
// second refusal naming a later reset should extend it.
func (p *Pause) Begin(now time.Time, limit *agent.UsageLimitError) time.Time {
	if p == nil {
		return time.Time{}
	}
	until := resumeAt(now, limit)
	reason := ""
	if limit != nil {
		reason = limit.Error()
	}

	p.mu.Lock()
	extended := until.After(p.until)
	if extended {
		p.until, p.reason = until, reason
	} else {
		until = p.until
	}
	cancels := make(map[string]context.CancelCauseFunc, len(p.live))
	for runID, cancel := range p.live {
		cancels[runID] = cancel
	}
	p.mu.Unlock()

	cancelled := make([]string, 0, len(cancels))
	for runID, cancel := range cancels {
		cancelled = append(cancelled, runID)
		cancel(errUsageLimit)
	}
	sort.Strings(cancelled)
	if extended {
		log.Printf("orchestrator: %s -- dispatch paused until %s; cancelling %d live run(s) %v",
			reason, until.Format(time.RFC3339), len(cancelled), cancelled)
	}
	return until
}

// Blocked reports whether dispatch is paused at now, and until when. A
// pause whose instant has passed is cleared here, by the first caller to
// ask after it expired, which is what makes resuming a thing that
// happens on its own rather than something a later failure has to
// notice.
//
// The reconciler that calls this is level-triggered like every other
// (Reconciler's own doc comment), so "has the window reset yet?" is
// answered by asking again next tick and not by a timer this would
// otherwise have to own.
func (p *Pause) Blocked(now time.Time) (until time.Time, reason string, blocked bool) {
	if p == nil {
		return time.Time{}, "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until.IsZero() {
		return time.Time{}, "", false
	}
	if !now.Before(p.until) {
		log.Printf("orchestrator: the agent usage-limit pause set until %s has expired; dispatch resumes",
			p.until.Format(time.RFC3339))
		p.until, p.reason = time.Time{}, ""
		return time.Time{}, "", false
	}
	return p.until, p.reason, true
}

// Lift clears a pause before its window has run out, and reports whether
// there was one to clear. It is the operator's own override: someone who
// has just topped a plan up, or switched the deployment to the other
// agent framework, is holding information this process cannot have --
// the credential behind the refusal is not the credential the next run
// would spend -- and has no reason to sit out the remainder of a window
// that no longer applies (grain/task-132, over the daemon's own
// DELETE /api/pause).
//
// It stops dispatch being gated and nothing more. Runs Begin cancelled
// stay cancelled -- they are over, and their attempts are recorded as
// model.PausedOutcome, which no streak counts -- so what a lift buys is
// the next tick dispatching rather than skipping. If the limit is in
// fact still in force, the run that follows meets it again and pauses
// again, which is the same self-correcting shape as a pause that expires
// on its own into a window that has not really reset.
func (p *Pause) Lift() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until.IsZero() {
		return false
	}
	log.Printf("orchestrator: the agent usage-limit pause set until %s was lifted by hand; dispatch resumes",
		p.until.Format(time.RFC3339))
	p.until, p.reason = time.Time{}, ""
	return true
}

// Until is what Blocked reports without the expiry it performs -- for a
// caller that wants to show the current pause rather than act on it, and
// for a test asserting on what Begin recorded. A zero time means nothing
// is paused; an instant already in the past means a pause nothing has
// asked about since it expired.
func (p *Pause) Until() (time.Time, string) {
	if p == nil {
		return time.Time{}, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.until, p.reason
}

// resumeAt turns a provider's own account of when it will serve again
// into the instant this deployment will actually try again, bounded by
// the three constants above. A nil limit -- a caller with no detail at
// all -- is read the same way as a refusal that named no reset.
func resumeAt(now time.Time, limit *agent.UsageLimitError) time.Time {
	until := now.Add(defaultUsageLimitPause)
	if limit != nil {
		if named, ok := limit.ResumeAt(now); ok {
			until = named
		}
	}
	if floor := now.Add(minUsageLimitPause); until.Before(floor) {
		until = floor
	}
	if ceiling := now.Add(maxUsageLimitPause); until.After(ceiling) {
		until = ceiling
	}
	return until.UTC()
}
