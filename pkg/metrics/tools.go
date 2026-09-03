package metrics

// What happened inside a run, over a window.
//
// Everything else this package measures is about the outside of a run --
// when it started, how long it took, how it was recorded. This is the
// inside: how many tool calls a run makes, how many of them fail, which
// tool the failures are in, how big the answers are, and how the CI loop
// the prompt sends every run around actually goes. Every finding in
// docs/agent-ergonomics.md was argued from code-reading and single
// transcripts because none of this had an aggregate to argue from.
//
// The package's three rules still hold, with one stated exception. The
// census rows this reads are the one thing in the store that had to be
// written rather than derived (model/telemetry.go says why), and given
// them, everything here is derived: no counter, nothing to reset, and a
// report that changes when the rows do. A row belongs to a window when
// its *run* finished inside it -- census rows carry no moment of their
// own, and the run's ending is the moment the measurement was taken. And
// what was never recorded is skipped: a run from before these tables
// existed contributes to nothing rather than to a zero, which is why
// Tools.Runs is reported next to every rate drawn from it.

import (
	"math"
	"sort"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// Counts summarises a series of plain numbers -- calls in a run, pushes
// before a green build. Nearest-rank percentiles over the samples
// themselves, exactly like Distribution, so every number reported is one
// that really happened.
type Counts struct {
	N     int
	Min   int64
	P50   int64
	P90   int64
	P99   int64
	Max   int64
	Mean  float64
	Total int64
}

// Sizes is how big one tool's answers were.
//
// Max and Mean are exact. The percentiles are not, and say so in their
// names: they come from the base-2 histogram each census row carries
// (model.SizeHistogram), so each is the bound of the bucket the p'th
// sample fell in -- "95% of read_file answers were at most 65535 bytes"
// is true, and the real number is somewhere in the octave below it.
//
// That is deliberate rather than a shortcut. An exact percentile needs
// every sample, which means a row per tool call rather than per tool per
// run -- hundreds a run, read whole on every report. A bound within a
// factor of two is enough for the question these exist to answer: what
// cap keeps most answers intact (mcp.maxToolResultBytes, still a guess
// until this is read against a real deployment).
type Sizes struct {
	N         int
	Mean      int64
	Max       int64
	P50AtMost int64
	P95AtMost int64
	P99AtMost int64
}

// ToolUse is one tool's numbers across the window.
type ToolUse struct {
	Name string
	// Runs is how many of the window's runs called this tool at all --
	// the difference between a tool every run uses twice and one that a
	// single run used two hundred times.
	Runs    int
	Calls   int
	Errored int
	// ErrorRate is Errored/Calls. An errored call is an ordinary turn of
	// an agent's loop rather than a broken run (orchestrator.outcomeOf
	// says why at length), so this is never zero for a healthy
	// deployment: what it is for is the trend and the comparison between
	// tools. edit_file's is the "String not found" loop measured;
	// run_command's is the sandbox's own health.
	ErrorRate float64
	// TimedOut is how many calls the tool's own bound cut off rather than
	// the work finishing, and TimeoutRate is that over Calls. Only
	// run_command reports this today (mcp.RunCommandTimedOut); it is zero
	// for every other tool because none of them can say, not because none
	// of them ever ran long.
	TimedOut    int
	TimeoutRate float64
	// ResultBytes is how much text this tool's answers carried back --
	// the run's own context, spent.
	ResultBytes Sizes
}

// Tools is the whole census over the window.
type Tools struct {
	// Runs is how many finished runs in the window recorded a census at
	// all, and is the first thing to read: every rate below is over these
	// runs, not over Throughput.RunsFinished. A run that never reached
	// its agent, one that made no tool calls, and one that finished
	// before these rows were recorded at all each contribute nothing.
	Runs    int
	Calls   int
	Errored int
	// ErroredShare is Errored/Calls across every tool. A handful of
	// errors is the ordinary shape of agentic work; a deployment drifting
	// upward is a deployment whose tools got worse.
	ErroredShare float64
	// CallsPerRun is the distribution of how many calls a run makes.
	CallsPerRun Counts
	// ByTool is every tool the window's runs called, busiest first --
	// which is the order that matters, since a rate over four calls is
	// not a rate.
	ByTool []ToolUse
}

// Checks is the CI loop, measured end to end: BuildPrompt tells every run
// to push, call wait_for_checks, fix and push again, and nothing showed
// how that loop actually goes.
type Checks struct {
	// Waits is how many wait_for_checks calls the window's runs made, and
	// Runs is how many runs made at least one.
	Waits int
	Runs  int
	// Verdicts counts each of the four endings a wait can have
	// (mcp.WaitVerdict*). A deployment where most waits end "timed_out"
	// has mcp.DefaultWaitForChecksTimeout set wrong for its CI, and one
	// where most end "no_checks" is telling every run to wait for CI that
	// does not exist -- neither is visible in any other number here.
	Verdicts map[string]int
	// Blocked is how long a wait blocked for. Read against the verdicts:
	// waits that blocked a long time and then passed are CI being slow,
	// and waits that blocked the full timeout are the timeout being
	// wrong.
	Blocked Distribution
	// PushesToGreen is how many pushes a run had made by the time its
	// first passing wait returned, over the runs that got a pass at all
	// (GreenRuns). 1.0 is a deployment whose runs get CI right first
	// time; the distance above it is the rework the CI loop costs.
	PushesToGreen Counts
	GreenRuns     int
}

// toolsOf and checksOf turn the stored census into the two sections
// above, over the runs that finished inside the window.
func toolsOf(w Window, runs []model.RunTiming, uses []model.RunToolUse) Tools {
	finished := finishedInWindow(w, runs)
	out := Tools{}
	callsPerRun := map[string]int64{}

	type series struct {
		use   ToolUse
		bytes int64
		sizes model.SizeHistogram
	}
	var names []string
	byName := map[string]*series{}

	for _, use := range uses {
		if !finished[use.RunID] {
			continue
		}
		entry, ok := byName[use.Tool]
		if !ok {
			entry = &series{use: ToolUse{Name: use.Tool}, sizes: model.SizeHistogram{}}
			byName[use.Tool] = entry
			names = append(names, use.Tool)
		}
		entry.use.Runs++
		entry.use.Calls += use.Calls
		entry.use.Errored += use.Errored
		entry.use.TimedOut += use.TimedOut
		entry.bytes += use.ResultBytes
		if use.MaxResultBytes > entry.use.ResultBytes.Max {
			entry.use.ResultBytes.Max = use.MaxResultBytes
		}
		entry.sizes.Merge(use.Sizes)

		out.Calls += use.Calls
		out.Errored += use.Errored
		callsPerRun[use.RunID] += int64(use.Calls)
	}

	out.Runs = len(callsPerRun)
	if out.Calls > 0 {
		out.ErroredShare = float64(out.Errored) / float64(out.Calls)
	}
	perRun := make([]int64, 0, len(callsPerRun))
	for _, calls := range callsPerRun {
		perRun = append(perRun, calls)
	}
	out.CallsPerRun = summarizeCounts(perRun)

	for _, name := range names {
		entry := byName[name]
		use := entry.use
		if use.Calls > 0 {
			use.ErrorRate = float64(use.Errored) / float64(use.Calls)
			use.TimeoutRate = float64(use.TimedOut) / float64(use.Calls)
			use.ResultBytes.Mean = entry.bytes / int64(use.Calls)
		}
		use.ResultBytes.N = entry.sizes.Total()
		use.ResultBytes.P50AtMost = sizeQuantile(entry.sizes, 0.50)
		use.ResultBytes.P95AtMost = sizeQuantile(entry.sizes, 0.95)
		use.ResultBytes.P99AtMost = sizeQuantile(entry.sizes, 0.99)
		out.ByTool = append(out.ByTool, use)
	}
	// Busiest first, and by name where two tools tie, so the order is
	// stable across reports of the same window.
	sort.SliceStable(out.ByTool, func(i, j int) bool {
		if out.ByTool[i].Calls == out.ByTool[j].Calls {
			return out.ByTool[i].Name < out.ByTool[j].Name
		}
		return out.ByTool[i].Calls > out.ByTool[j].Calls
	})
	return out
}

func checksOf(w Window, runs []model.RunTiming, waits []model.RunCheckWait) Checks {
	finished := finishedInWindow(w, runs)
	out := Checks{Verdicts: map[string]int{}}
	var blocked []time.Duration
	var pushes []int64
	seenRun := map[string]bool{}
	greenRun := map[string]bool{}

	for _, wait := range waits {
		if !finished[wait.RunID] {
			continue
		}
		out.Waits++
		out.Verdicts[wait.Verdict]++
		if !seenRun[wait.RunID] {
			seenRun[wait.RunID] = true
			out.Runs++
		}
		// A wait whose own header could not be read stored a zero, and a
		// zero-second wait never happened: appendIfPositive drops it
		// rather than reporting a wait that took no time.
		blocked = appendIfPositive(blocked, wait.Waited)
		// The *first* pass in a run is the one that answers "how many
		// pushes did this take": a run that went green, pushed again and
		// went green again did not need the second round to get there.
		if wait.Verdict == waitVerdictPassed && !greenRun[wait.RunID] {
			greenRun[wait.RunID] = true
			pushes = append(pushes, int64(wait.PushesBefore))
		}
	}

	out.Blocked = summarize(blocked)
	out.PushesToGreen = summarizeCounts(pushes)
	out.GreenRuns = len(greenRun)
	return out
}

// waitVerdictPassed is mcp.WaitVerdictPassed, spelled out rather than
// imported: this package reads rows, and a metrics report has no business
// depending on the package that serves an agent's tools. The store's own
// column is the contract between them, and a verdict this build does not
// know still counts under its own name in Verdicts.
const waitVerdictPassed = "passed"

// finishedInWindow is the set of runs whose ending falls inside the
// window -- the same rule every other measurement here follows, applied
// to rows that have no moment of their own to be judged by.
func finishedInWindow(w Window, runs []model.RunTiming) map[string]bool {
	out := make(map[string]bool)
	for _, r := range runs {
		if r.RunID != "" && w.holds(r.FinishedAt) {
			out[r.RunID] = true
		}
	}
	return out
}

// sizeQuantile is the p'th sample's own bucket bound: the smallest size
// bucket whose cumulative count reaches the p'th sample, reported as the
// largest result that bucket can hold. Nearest-rank, like percentile
// above, and an upper bound rather than a value -- see Sizes.
func sizeQuantile(h model.SizeHistogram, p float64) int64 {
	total := h.Total()
	if total == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(total)))
	if rank < 1 {
		rank = 1
	}
	seen := 0
	buckets := h.Buckets()
	for _, bucket := range buckets {
		seen += h[bucket]
		if seen >= rank {
			return model.SizeBucketMax(bucket)
		}
	}
	return model.SizeBucketMax(buckets[len(buckets)-1])
}

// summarizeCounts is summarize for whole numbers. It sorts a copy, like
// its duration twin, and reports N first: a p99 over four samples is the
// maximum wearing a percentile's name here too.
func summarizeCounts(samples []int64) Counts {
	c := Counts{N: len(samples)}
	if c.N == 0 {
		return c
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, s := range sorted {
		c.Total += s
	}
	c.Min, c.Max = sorted[0], sorted[len(sorted)-1]
	c.P50, c.P90, c.P99 = countPercentile(sorted, 0.50), countPercentile(sorted, 0.90), countPercentile(sorted, 0.99)
	c.Mean = float64(c.Total) / float64(c.N)
	return c
}

func countPercentile(sorted []int64, p float64) int64 {
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
