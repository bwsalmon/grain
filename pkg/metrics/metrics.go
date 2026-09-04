// Package metrics measures what a deployment actually delivers: how many
// tasks it finished over a window (throughput), and how long each stage
// of a task's life took to get there (latency).
//
// It exists because grain could not answer either question. The store has
// held every moment needed for both since tasks became rows -- filed,
// approved, dispatched, finished, completed -- and nothing ever read them
// together, so "is this deployment getting slower?" and "where does a
// task's day actually go?" were answerable only by reading a task list
// and doing arithmetic by eye. README's own "What this costs" is the
// sharpest example: a VM boot moved onto the critical path of every
// dispatch, the mitigations for it (a golden image, a warm spare) are
// both unbuilt, and that section says outright that it is "worth
// measuring before reaching for either." This is that measurement, and
// task_run.agent_started_at (schema.go) is the one moment that had to
// start being recorded for it to be possible at all.
//
// Three rules shape everything below.
//
// **Nothing here is stored.** A Report is computed from rows that already
// exist, every time it is asked for, the same way task_state is a view
// rather than a column (docs/data-model.md's "anything derivable is
// derived, never stored"). There is no counter to increment on a hot
// path, nothing to reset, and no way for a metric to disagree with the
// task it describes -- a task edited, retried or closed changes what
// every past report would have said, which is correct, because the record
// changed.
//
// **A window bounds measurements, not rows.** A sample belongs to a
// window when the moment it *ended* falls inside it: a task completed
// this morning contributes its whole lead time even if it was filed last
// month. That makes a report answer "what did this deployment deliver
// during these dates", which is the question, rather than "what happened
// entirely within them", which nothing ever asks.
//
// **A missing moment is skipped, never guessed.** Tasks filed before
// created_at existed, runs recorded before agent_started_at did, a run
// that failed in setup and never reached an agent -- each simply
// contributes to no distribution rather than to a wrong one, which is why
// every Distribution carries its own N. Two stages of the same report
// legitimately have different sample counts.
//
// **One thing measured here is not a row, because it never was.** A
// RunCycle tick -- the daemon's own scheduling pass -- reads the store,
// decides, and returns, leaving nothing behind to derive its duration
// from afterwards. It is also the missing half of Latency.QueueWait: a
// task waiting because the deployment was full and a task waiting
// because the tick was slow to reach dispatch produce the same number.
// So the daemon measures its own ticks into a bounded in-memory ring
// (orchestrator.CycleTimes -- no table, no write per tick, nothing
// stored, and lost on restart, which its own doc comment argues for),
// and hands them here as Input.Cycles. The rules above still hold for it
// as far as they can: it is windowed on the same "the moment it ended"
// rule, it carries its own N, and Report.Cycles says outright how far
// back it actually reaches, which is however long that daemon's ring is
// rather than however long a window was asked for.
//
// This package holds no state and touches no database: pkg/model reads
// the rows (Store.TaskTimings, Store.RunTimings), the daemon reports its
// own ticks, this decides what they mean, and pkg/ui serves the answer.
package metrics

import (
	"math"
	"sort"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// Window is the half-open interval [Since, Until) a Report covers.
type Window struct {
	Since time.Time
	Until time.Time
}

// Duration is how long the window is, or zero for one that is empty or
// inverted -- which is what every rate below divides by, and so what they
// all check before dividing.
func (w Window) Duration() time.Duration {
	if !w.Until.After(w.Since) {
		return 0
	}
	return w.Until.Sub(w.Since)
}

// holds reports whether a moment falls in the window. A nil moment never
// does: "this never happened" and "this happened outside the window" are
// both reasons to leave a sample out, and neither is a reason to invent
// one.
func (w Window) holds(t *time.Time) bool {
	return t != nil && !t.Before(w.Since) && t.Before(w.Until)
}

// DefaultBuckets is how many points a Report's own time series carries
// when the caller does not say -- enough for a sparkline to show a trend
// across the window, few enough that a report stays a small JSON body.
const DefaultBuckets = 12

// maxBuckets caps what a caller can ask for, so a window sliced into a
// bucket per second cannot turn one report into a megabyte of them.
const maxBuckets = 200

// Input is everything a Report is computed from. Tasks and Runs are the
// whole store's worth (see Store.TaskTimings on why they are not
// pre-filtered); States and MaxConcurrent are what the store knows about
// right now rather than about the window, and both are optional -- a
// caller that has neither still gets every throughput and latency number,
// and gets a Backlog and a Utilization of zero.
type Input struct {
	Window        Window
	Tasks         []model.TaskTiming
	Runs          []model.RunTiming
	States        map[string]model.State
	MaxConcurrent int
	Buckets       int
	// Cycles is what the daemon's own reconcile loop measured about
	// itself, and the one input here that does not come from a row --
	// see Cycles below for why it cannot. Optional, like States and
	// MaxConcurrent: a caller with no daemon to ask (the CLI over REST
	// against another process, a test) supplies none and gets an empty
	// Cycles rather than a wrong one.
	Cycles CycleHistory
	// ToolUses and CheckWaits are what each run recorded about its own
	// tool use (Store.RunToolUses, Store.RunCheckWaits) -- the whole
	// tables, like Tasks and Runs, and windowed here against the run each
	// row belongs to. Optional in the same way States is: a caller that
	// supplies neither gets an empty Tools and Checks rather than a wrong
	// one, and every other number is unaffected.
	ToolUses   []model.RunToolUse
	CheckWaits []model.RunCheckWait
}

// CycleHistory is a daemon's own record of its recent RunCycle ticks --
// orchestrator.CycleTimes' contents, converted at the one boundary where
// both types are in scope (cmd/grain).
//
// Samples are oldest first, and are the most recent ones that daemon
// still remembers rather than all of them: Observed is how many it has
// ever run, so a caller can tell a full history from a truncated tail.
type CycleHistory struct {
	Observed int
	Samples  []CycleSample
}

// CycleSample is one RunCycle tick -- orchestrator.CycleTiming, in this
// package's own terms.
type CycleSample struct {
	Start        time.Time
	Duration     time.Duration
	DispatchWait time.Duration
	Reconcilers  []ReconcilerSample
}

// ReconcilerSample is one reconciler's share of one tick. Wait is how far
// into the cycle it started; Duration is how long it then took.
type ReconcilerSample struct {
	Name     string
	Wait     time.Duration
	Duration time.Duration
	Failed   bool
}

// Distribution summarises one latency series. Percentiles are
// nearest-rank over the samples themselves -- no interpolation, so every
// value reported is one that really happened -- and N is how many samples
// there were, which is the first thing to read: a P99 over four samples
// is the maximum wearing a percentile's name.
type Distribution struct {
	N     int
	Min   time.Duration
	P50   time.Duration
	P90   time.Duration
	P99   time.Duration
	Max   time.Duration
	Mean  time.Duration
	Total time.Duration
}

// Throughput is how much work crossed each line during the window, and
// the same counts as a daily rate so two windows of different lengths
// compare.
//
// TasksClosed overlaps TasksCompleted deliberately: a task that completed
// and was closed later counts once in each, because the two lines are
// different events and a report that netted them off could not show the
// second happening at all.
type Throughput struct {
	TasksFiled     int
	TasksCompleted int
	TasksClosed    int
	RunsStarted    int
	RunsFinished   int

	FiledPerDay        float64
	CompletedPerDay    float64
	RunsFinishedPerDay float64

	// Buckets is the same throughput sliced into equal spans across the
	// window, oldest first -- what a trend line is drawn from. The last
	// bucket is the only one that can be partially elapsed.
	Buckets []Bucket
}

// Bucket is one slice of the window's own time series.
type Bucket struct {
	Since        time.Time
	Until        time.Time
	Filed        int
	Completed    int
	RunsFinished int
}

// Latency is where a task's wall-clock time goes, stage by stage. Each
// distribution covers the samples whose own stage *ended* inside the
// window (this package's own doc comment), and each is measured
// independently -- they do not sum to LeadTime, because a task can sit in
// awaiting_reply between them, be retried after a backoff, or wait on a
// dependency, and none of those is a stage anything records the start of.
type Latency struct {
	// ApprovalWait is filed -> approved: how long a proposal sat before a
	// human accepted it. Tasks approved in the same instant they were
	// filed (anything a human files directly, LandsQueued) are left out
	// rather than counted as zero -- nobody waited, and a pile of zeroes
	// would drag the percentiles of the tasks that did.
	ApprovalWait Distribution
	// QueueWait is approved -> the first attempt starting: the backlog
	// wait, and the one stage that is grain's own scheduling rather than
	// anyone's work. It is what a bigger max_concurrent, a faster tick or
	// a warm sandbox would shrink.
	QueueWait Distribution
	// SandboxSetup is an attempt starting -> its agent's first turn: a
	// sandbox built, a repo cloned, capabilities minted and placed. This
	// is the number README's "a VM boot moves onto the critical path of
	// every task" is about, and the one that says whether a golden image
	// or a warm spare would be worth building.
	SandboxSetup Distribution
	// AgentWork is the agent's first turn -> the attempt finishing: what
	// the agent framework actually spent. Grain does not control it, and
	// separating it out is what keeps it from being read as if grain did.
	AgentWork Distribution
	// Attempt is one whole attempt, start to finish -- SandboxSetup plus
	// AgentWork plus the finish path, and reported alongside them because
	// it is the only one of the three every run has (a run that never
	// reached its agent has no setup or agent sample, but it took time
	// all the same).
	Attempt Distribution
	// RetryWait is one attempt finishing -> the next attempt on the same
	// task starting: dispatch's own failure backoff plus another turn
	// through the queue.
	RetryWait Distribution
	// TimeToFinish is the first attempt starting -> the task completing:
	// how long the work itself took once it began, across however many
	// attempts, questions and replies it needed.
	TimeToFinish Distribution
	// LeadTime is filed -> completed, the whole of what somebody who
	// filed a task actually waited. Every other stage above is an answer
	// to "why is this number what it is".
	LeadTime Distribution
}

// Runs is how the window's attempts turned out, and how much of the
// deployment's own capacity they used.
type Runs struct {
	// Outcomes counts task_run.outcome over the runs that finished in the
	// window. The vocabulary is open (schema.go keeps enums out of the
	// DDL): "succeeded", "failed", "cancelled", "setup-failed" today, and
	// whatever a later ending calls itself. A run finished with no
	// outcome recorded counts under "unrecorded".
	Outcomes map[string]int
	// Endings counts the same runs by how they actually ended
	// (model.RunEnding), which is what Outcomes above cannot say. Two of
	// its words each cover two endings with different fixes: "cancelled"
	// is both a human closing the task and the run hitting the
	// wall-clock cap, and "failed" is both a framework that broke and a
	// run that exhausted MaxAgentTurns. Each of those is its own series
	// here -- alongside no_action, a run that had tools, used them and
	// produced nothing, which is the purest measure of the agent-facing
	// surface failing and was previously one key in a map beside
	// "succeeded"; and alongside the runs a provider's usage limit
	// stopped, which are not a fault of the deployment's at all.
	Endings map[model.RunEnding]int
	// AttemptsPerCompletion is how many attempts, on average, each task
	// completed in this window took -- every attempt it ever made, not
	// only those inside the window, since that is what the completion
	// actually cost. 1.0 is a deployment that gets it right first time;
	// the distance above 1.0 is rework.
	AttemptsPerCompletion float64
	// MeanConcurrent is how many runs were live at once on average across
	// the window: every run's overlap with it, summed, over the window's
	// own length. A run still live at Until counts up to Until.
	MeanConcurrent float64
	// MaxConcurrent is the deployment's own configured limit, echoed here
	// so MeanConcurrent has something to be a fraction of, and zero when
	// the caller did not supply one.
	MaxConcurrent int
	// Utilization is MeanConcurrent / MaxConcurrent: the share of the
	// deployment's dispatch capacity that was actually in use. A low
	// number next to a deep Backlog is the signature of a scheduling
	// problem rather than a capacity one -- work waiting while slots sat
	// idle -- and that pair is the reason both numbers are in one report.
	Utilization float64
	// Live is how many runs were still in flight at Until.
	Live int
}

// Backlog is a gauge, not a window: how much work was sitting in the
// system when the report was taken. Throughput without it says how fast
// the deployment goes and not whether that is fast enough -- a completion
// rate matching the filing rate means something entirely different
// depending on whether the queue behind it is empty or a hundred deep.
//
// It is only populated when Input.States is supplied, because the honest
// version of these counts is task_state's own vocabulary: a task that is
// neither completed nor closed may be queued, blocked behind a
// dependency, awaiting a reply, or capped out at MaxConsecutiveFailures,
// and those are four different problems that the timings alone cannot
// tell apart.
type Backlog struct {
	// ByState counts the unfinished states only. Completed, awaiting
	// submit and closed tasks are left out on purpose: they are a census
	// of everything the deployment has ever done, which grows forever and
	// is not work sitting in the system -- how many of them landed *in the
	// window* is what Throughput already answers.
	//
	// StateAwaitingSubmit is on that side of the line because it is a
	// post-run state: the run is over and its lead time is already
	// recorded, and a task nobody ever submits and nobody ever closes
	// accumulates for exactly as long as a completed one does. It is
	// still a queue of a kind -- of human clicks rather than of dispatch
	// -- but not the one Utilization is read against, and counting it
	// here would make a deployment that simply does not use auto-merge
	// look permanently backed up.
	ByState map[model.State]int
	// Queued is ByState[StateQueued] -- the depth of the dispatch queue
	// itself, lifted out because it is the one this report's own
	// Utilization is read against.
	Queued int
	// OldestQueuedWait is how long the longest-queued task has been
	// waiting at Until, and OldestQueuedTaskID is which one it is. A
	// percentile over what finished cannot see a task that has never
	// started; this is what does.
	OldestQueuedWait   time.Duration
	OldestQueuedTaskID string
}

// Cycles is how the daemon's own reconcile loop has been behaving: how
// long a RunCycle tick takes, how often one actually happens, and where
// inside a tick the time goes.
//
// It is here because Latency.QueueWait cannot be attributed without it.
// A queued task waits for two entirely different reasons that produce
// the same number -- the deployment was at max_concurrent and there was
// genuinely no room (which Runs.Utilization near 1.0 shows), or there
// was room the whole time and the task was waiting on the tick. Nothing
// showed the second. A DispatchWait that has quietly grown to minutes
// under a large store looks exactly like a busy deployment from
// QueueWait alone, and the fix for each is the opposite of the fix for
// the other: more concurrency for one, a faster or better-ordered cycle
// for the second.
//
// It is also the one part of a Report not derived from rows, because a
// tick leaves none -- see orchestrator.CycleTimes on why the record is a
// ring in the daemon rather than a table. What that costs is stated
// rather than hidden: this section describes the process serving the
// report, is empty right after a restart, and reaches back only as far
// as that daemon's ring does, however long a window the caller asked
// for.
type Cycles struct {
	// N is how many ticks the window holds, and Observed is how many
	// this daemon has run since it started -- so N below Observed is
	// either a window narrower than the ring or a ring that has
	// forgotten the rest, and Truncated is which.
	N        int
	Observed int
	// Truncated says the daemon has run more ticks than it still
	// remembers: First below is where its ring begins rather than where
	// this deployment's history does. It is a fact about the history
	// supplied, not about the window, which is why it is not simply
	// Observed > N.
	Truncated bool
	// First and Last are the oldest and newest tick this section
	// actually covers -- the real span behind N, which is not the
	// requested window when the ring is shorter than it (the usual
	// case).
	First time.Time
	Last  time.Time
	// Tick is the whole of a RunCycle call: the stored-configuration
	// refresh plus every reconciler.
	Tick Distribution
	// Interval is one tick's start to the next one's -- the loop's real
	// period, which is the -poll-interval only while a tick is fast
	// compared to it. Ticks do not overlap (cmd/grain's reconcile waits
	// for one to return), so this is what the wait for the *next*
	// dispatch decision is really drawn from.
	Interval Distribution
	// DispatchWait is how far into a tick the dispatch decision was
	// reached. Interval plus this is the queue wait a task pays purely
	// for grain's own scheduling, with no contention involved at all:
	// the floor under Latency.QueueWait, and the number to compare it
	// against before concluding a deployment needs more concurrency.
	DispatchWait Distribution
	// Reconcilers is where a tick's time went, in the order the cycle
	// ran them. A single tick duration cannot say which reconciler grew;
	// this can, and the ordering matters because they run in sequence --
	// a pull-request sync that has grown to a minute delays every
	// decision behind it by a minute.
	Reconcilers []ReconcilerCycles
}

// ReconcilerCycles is one reconciler's numbers across the window.
type ReconcilerCycles struct {
	Name string
	// Wait is how far into a cycle this reconciler started -- what
	// everything before it cost it. For the dispatch reconciler this is
	// the same series as Cycles.DispatchWait.
	Wait Distribution
	// Duration is how long it took, and Failures is how many of those
	// ticks it ended in an error. The two are worth reading together: a
	// reconciler that is fast because it fails immediately is not a
	// reconciler that is fast.
	Duration Distribution
	Failures int
}

// Report is one measurement of a deployment. Its gauges (Runs.Live,
// Backlog) describe Window.Until rather than the window as a whole, which
// for the report a caller actually asks for is the moment it was taken.
type Report struct {
	Window     Window
	Throughput Throughput
	Latency    Latency
	Runs       Runs
	Backlog    Backlog
	Cycles     Cycles
	// Tools and Checks are the inside of a run: what its tools cost it,
	// and how the CI loop it was told to go round actually went. Both are
	// empty for a caller that supplied no census (Input.ToolUses,
	// Input.CheckWaits) and for a window whose runs all predate it -- see
	// tools.go.
	Tools  Tools
	Checks Checks
	// PullRequests is the last stretch of that same loop: whether runs
	// take the pull request grain offers them mid-flight, and whether
	// taking it changes how often a red build outlives the run that
	// pushed it. Read off the same census as Tools, and empty on the same
	// input -- see pullrequests.go.
	PullRequests PullRequests
}

// Compute derives a Report. It reads its input and nothing else -- no
// clock beyond Input.Window.Until, no database -- so the same input
// always gives the same report.
func Compute(in Input) Report {
	rep := Report{Window: in.Window}
	w := in.Window

	byTask := runsByTask(in.Runs)

	// --- throughput, and the same counts bucketed ---------------------
	buckets := newBuckets(w, in.Buckets)
	for _, t := range in.Tasks {
		if w.holds(t.CreatedAt) {
			rep.Throughput.TasksFiled++
			addTo(buckets, w, t.CreatedAt, func(b *Bucket) { b.Filed++ })
		}
		if w.holds(t.CompletedAt) {
			rep.Throughput.TasksCompleted++
			addTo(buckets, w, t.CompletedAt, func(b *Bucket) { b.Completed++ })
		}
		if w.holds(t.ClosedAt) {
			rep.Throughput.TasksClosed++
		}
	}
	rep.Runs.Outcomes = map[string]int{}
	rep.Runs.Endings = map[model.RunEnding]int{}
	for _, r := range in.Runs {
		started := r.StartedAt
		if w.holds(&started) {
			rep.Throughput.RunsStarted++
		}
		if w.holds(r.FinishedAt) {
			rep.Throughput.RunsFinished++
			rep.Runs.Outcomes[outcomeOf(r)]++
			rep.Runs.Endings[model.EndingOf(r.Outcome, r.Detail)]++
			addTo(buckets, w, r.FinishedAt, func(b *Bucket) { b.RunsFinished++ })
		}
	}
	rep.Throughput.Buckets = buckets
	if days := w.Duration().Hours() / 24; days > 0 {
		rep.Throughput.FiledPerDay = float64(rep.Throughput.TasksFiled) / days
		rep.Throughput.CompletedPerDay = float64(rep.Throughput.TasksCompleted) / days
		rep.Throughput.RunsFinishedPerDay = float64(rep.Throughput.RunsFinished) / days
	}

	// --- latency ------------------------------------------------------
	var approval, queue, setup, agent, attempt, retry, toFinish, lead []time.Duration
	for _, t := range in.Tasks {
		// Approved strictly after being filed: a task a human filed
		// directly is approved in the same instant, and nobody waited.
		if t.CreatedAt != nil && w.holds(t.ApprovedAt) && t.ApprovedAt.After(*t.CreatedAt) {
			approval = append(approval, t.ApprovedAt.Sub(*t.CreatedAt))
		}
		runs := byTask[t.TaskID]
		if len(runs) > 0 {
			first := runs[0].StartedAt
			if t.ApprovedAt != nil && w.holds(&first) {
				queue = appendIfPositive(queue, first.Sub(*t.ApprovedAt))
			}
			if w.holds(t.CompletedAt) {
				toFinish = appendIfPositive(toFinish, t.CompletedAt.Sub(first))
			}
		}
		if t.CreatedAt != nil && w.holds(t.CompletedAt) {
			lead = appendIfPositive(lead, t.CompletedAt.Sub(*t.CreatedAt))
		}
	}
	for _, runs := range byTask {
		for i, r := range runs {
			if w.holds(r.AgentStartedAt) {
				setup = appendIfPositive(setup, r.AgentStartedAt.Sub(r.StartedAt))
			}
			if w.holds(r.FinishedAt) {
				attempt = appendIfPositive(attempt, r.FinishedAt.Sub(r.StartedAt))
				if r.AgentStartedAt != nil {
					agent = appendIfPositive(agent, r.FinishedAt.Sub(*r.AgentStartedAt))
				}
			}
			if i == 0 {
				continue
			}
			if prev := runs[i-1].FinishedAt; prev != nil {
				next := r.StartedAt
				if w.holds(&next) {
					retry = appendIfPositive(retry, next.Sub(*prev))
				}
			}
		}
	}
	rep.Latency = Latency{
		ApprovalWait: summarize(approval),
		QueueWait:    summarize(queue),
		SandboxSetup: summarize(setup),
		AgentWork:    summarize(agent),
		Attempt:      summarize(attempt),
		RetryWait:    summarize(retry),
		TimeToFinish: summarize(toFinish),
		LeadTime:     summarize(lead),
	}

	// --- capacity -----------------------------------------------------
	var occupied time.Duration
	for _, r := range in.Runs {
		occupied += overlap(w, r)
		if r.FinishedAt == nil && r.StartedAt.Before(w.Until) {
			rep.Runs.Live++
		}
	}
	if d := w.Duration(); d > 0 {
		rep.Runs.MeanConcurrent = float64(occupied) / float64(d)
	}
	rep.Runs.MaxConcurrent = in.MaxConcurrent
	if in.MaxConcurrent > 0 {
		rep.Runs.Utilization = rep.Runs.MeanConcurrent / float64(in.MaxConcurrent)
	}
	var completedTasks, completedAttempts int
	for _, t := range in.Tasks {
		if !w.holds(t.CompletedAt) {
			continue
		}
		completedTasks++
		completedAttempts += len(byTask[t.TaskID])
	}
	if completedTasks > 0 {
		rep.Runs.AttemptsPerCompletion = float64(completedAttempts) / float64(completedTasks)
	}

	// --- backlog ------------------------------------------------------
	if in.States != nil {
		rep.Backlog = backlogOf(in.Tasks, in.States, w.Until)
	}

	// --- the inside of a run ------------------------------------------
	rep.Tools = toolsOf(w, in.Runs, in.ToolUses)
	rep.Checks = checksOf(w, in.Runs, in.CheckWaits)
	rep.PullRequests = pullRequestsOf(w, in.Tasks, in.Runs, in.ToolUses)

	// --- the daemon's own tick ----------------------------------------
	rep.Cycles = cyclesOf(w, in.Cycles)
	return rep
}

// cyclesOf summarises the daemon's own ticks over the window.
//
// A tick belongs to the window on the same rule everything else here
// follows -- the moment it *ended* falls inside -- so a cycle that began
// before Since and was still running at it contributes in full, which is
// the case a report of slow ticks most wants to see.
//
// Interval is measured between consecutive ticks in the history rather
// than between consecutive ticks in the window, and attributed to the
// later of the two: the gap a tick waited is a fact about that tick, and
// dropping the pair whose earlier half fell outside the window would
// silently understate the very first interval in every report.
func cyclesOf(w Window, h CycleHistory) Cycles {
	out := Cycles{Observed: h.Observed, Truncated: h.Observed > len(h.Samples)}
	var tick, interval, dispatch []time.Duration
	// Reconcilers keep the order the cycle ran them in, which is the
	// order that explains them -- so they are collected in a slice, with
	// a map only to find the entry a name already has.
	type series struct {
		wait, duration []time.Duration
		failures       int
	}
	var names []string
	byName := map[string]*series{}

	for i, s := range h.Samples {
		end := s.Start.Add(s.Duration)
		if !w.holds(&end) {
			continue
		}
		out.N++
		if out.First.IsZero() || s.Start.Before(out.First) {
			out.First = s.Start
		}
		if s.Start.After(out.Last) {
			out.Last = s.Start
		}
		tick = appendIfPositive(tick, s.Duration)
		dispatch = appendIfPositive(dispatch, s.DispatchWait)
		if i > 0 {
			interval = appendIfPositive(interval, s.Start.Sub(h.Samples[i-1].Start))
		}
		for _, r := range s.Reconcilers {
			entry, ok := byName[r.Name]
			if !ok {
				entry = &series{}
				byName[r.Name] = entry
				names = append(names, r.Name)
			}
			entry.wait = appendIfPositive(entry.wait, r.Wait)
			entry.duration = appendIfPositive(entry.duration, r.Duration)
			if r.Failed {
				entry.failures++
			}
		}
	}

	out.Tick = summarize(tick)
	out.Interval = summarize(interval)
	out.DispatchWait = summarize(dispatch)
	for _, name := range names {
		entry := byName[name]
		out.Reconcilers = append(out.Reconcilers, ReconcilerCycles{
			Name:     name,
			Wait:     summarize(entry.wait),
			Duration: summarize(entry.duration),
			Failures: entry.failures,
		})
	}
	return out
}

// runsByTask groups attempts by their task, oldest first. The store
// already returns them in that order; sorting here anyway is what lets
// Compute's own "the run before this one" reasoning (RetryWait) hold for
// any caller, including a test that builds its input by hand.
func runsByTask(runs []model.RunTiming) map[string][]model.RunTiming {
	out := make(map[string][]model.RunTiming)
	for _, r := range runs {
		out[r.TaskID] = append(out[r.TaskID], r)
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].StartedAt.Equal(list[j].StartedAt) {
				return list[i].Attempt < list[j].Attempt
			}
			return list[i].StartedAt.Before(list[j].StartedAt)
		})
	}
	return out
}

// backlogOf counts what was still in the system at until, by task_state's
// own vocabulary, and finds the queued task that has been waiting longest.
func backlogOf(tasks []model.TaskTiming, states map[string]model.State, until time.Time) Backlog {
	b := Backlog{ByState: map[model.State]int{}}
	for _, t := range tasks {
		state, ok := states[t.TaskID]
		if !ok || state == model.StateCompleted || state == model.StateAwaitingSubmit || state == model.StateClosed {
			continue
		}
		b.ByState[state]++
		if state != model.StateQueued {
			continue
		}
		b.Queued++
		// Queued since it was approved -- the same moment QueueWait
		// measures from, so "the oldest thing still waiting" and "how long
		// the things that started waited" are the same clock.
		since := t.ApprovedAt
		if since == nil {
			since = t.CreatedAt
		}
		if since == nil {
			continue
		}
		if wait := until.Sub(*since); wait > b.OldestQueuedWait {
			b.OldestQueuedWait, b.OldestQueuedTaskID = wait, t.TaskID
		}
	}
	return b
}

// overlap is how much of one run's life fell inside the window. A run
// still live counts up to the window's end: it really was occupying
// capacity for all of that, and waiting for it to finish before counting
// any of it would make a deployment look idlest exactly when it is
// busiest.
func overlap(w Window, r model.RunTiming) time.Duration {
	start := r.StartedAt
	if start.Before(w.Since) {
		start = w.Since
	}
	end := w.Until
	if r.FinishedAt != nil && r.FinishedAt.Before(end) {
		end = *r.FinishedAt
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// outcomeOf names a finished run's outcome, giving the empty one a word
// of its own rather than letting it count under "".
func outcomeOf(r model.RunTiming) string {
	if r.Outcome == "" {
		return "unrecorded"
	}
	return r.Outcome
}

// appendIfPositive drops a negative or zero sample. Both mean the same
// thing -- two timestamps that cannot really be that far apart in that
// order, from a clock adjustment, a hand-edited row, or a moment recorded
// by two different writers -- and neither is a latency worth reporting.
func appendIfPositive(samples []time.Duration, d time.Duration) []time.Duration {
	if d <= 0 {
		return samples
	}
	return append(samples, d)
}

// newBuckets slices the window into n equal spans, oldest first.
func newBuckets(w Window, n int) []Bucket {
	if w.Duration() <= 0 {
		return nil
	}
	if n <= 0 {
		n = DefaultBuckets
	}
	if n > maxBuckets {
		n = maxBuckets
	}
	span := w.Duration() / time.Duration(n)
	if span <= 0 {
		return nil
	}
	out := make([]Bucket, 0, n)
	for i := 0; i < n; i++ {
		since := w.Since.Add(span * time.Duration(i))
		until := since.Add(span)
		if i == n-1 {
			// The last bucket ends where the window does, whatever the
			// integer division above left over.
			until = w.Until
		}
		out = append(out, Bucket{Since: since, Until: until})
	}
	return out
}

// addTo applies add to whichever bucket holds t. The caller has already
// established that t is inside the window, so a moment landing outside
// every bucket is arithmetic drift rather than a real case, and is
// dropped rather than forced into an end bucket.
func addTo(buckets []Bucket, w Window, t *time.Time, add func(*Bucket)) {
	if t == nil || len(buckets) == 0 {
		return
	}
	span := buckets[0].Until.Sub(buckets[0].Since)
	if span <= 0 {
		return
	}
	i := int(t.Sub(w.Since) / span)
	if i < 0 || i >= len(buckets) {
		return
	}
	add(&buckets[i])
}

// summarize turns a sample set into a Distribution. It sorts a copy: the
// caller's slice is its own, and Compute reuses none of them anyway.
func summarize(samples []time.Duration) Distribution {
	d := Distribution{N: len(samples)}
	if d.N == 0 {
		return d
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, s := range sorted {
		d.Total += s
	}
	d.Min, d.Max = sorted[0], sorted[len(sorted)-1]
	d.P50, d.P90, d.P99 = percentile(sorted, 0.50), percentile(sorted, 0.90), percentile(sorted, 0.99)
	d.Mean = d.Total / time.Duration(d.N)
	return d
}

// percentile is nearest-rank over an ascending slice: the smallest sample
// at or above the p'th of them. No interpolation, so every number a
// report shows is a duration something really took.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
