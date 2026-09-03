// metrics.go is "grain metrics": the throughput and latency report
// (pkg/metrics, served as GET /api/metrics) printed at a terminal --
// including the daemon's own reconcile tick, which is what says whether
// a queue wait in that report was contention or scheduling.
//
// It is a task verb like every other one in main.go rather than a mode of
// its own -- it asks a running daemon over REST, holds no store, and
// takes -server and -json from the same global flags -- so `grain metrics
// -json | jq` is how a number here gets into anything else.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/ui"
)

func cmdMetrics(ctx context.Context, c *ui.HTTPClient, out *printer, args []string) error {
	fs := flag.NewFlagSet("grain metrics", flag.ContinueOnError)
	window := fs.String("window", "", "how far back to measure: a duration like 36h, or a count of days or weeks like 7d (default 7d)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Parsed here as well as by the daemon, so a typo is a message from
	// the command the operator actually ran rather than a 400 relayed
	// back from a server.
	if _, err := ui.ParseMetricsWindow(*window); err != nil {
		return err
	}
	report, err := c.Metrics(ctx, *window)
	if err != nil {
		return err
	}
	out.metrics(report)
	return nil
}

func (p *printer) metrics(rep ui.MetricsReport) {
	if p.json {
		p.encode(rep)
		return
	}
	fmt.Printf("window: %s -> %s (%s)\n",
		rep.Since.Format(time.RFC3339), rep.Until.Format(time.RFC3339),
		seconds(rep.WindowSeconds))

	t := rep.Throughput
	fmt.Println("\nthroughput")
	fmt.Printf("  tasks filed              %6d  (%.1f/day)\n", t.TasksFiled, t.FiledPerDay)
	fmt.Printf("  tasks completed          %6d  (%.1f/day)\n", t.TasksCompleted, t.CompletedPerDay)
	fmt.Printf("  tasks closed             %6d\n", t.TasksClosed)
	fmt.Printf("  attempts started         %6d\n", t.RunsStarted)
	fmt.Printf("  attempts finished        %6d  (%.1f/day)\n", t.RunsFinished, t.RunsFinishedPerDay)
	if outcomes := outcomeSummary(rep.Runs.Outcomes); outcomes != "" {
		fmt.Printf("  attempt outcomes         %s\n", outcomes)
	}
	fmt.Printf("  attempts per completion  %6.2f\n", rep.Runs.AttemptsPerCompletion)

	fmt.Println("\ncapacity")
	if rep.Runs.MaxConcurrent > 0 {
		fmt.Printf("  mean concurrent runs     %6.2f of %d  (%.0f%% of the limit)\n",
			rep.Runs.MeanConcurrent, rep.Runs.MaxConcurrent, rep.Runs.Utilization*100)
	} else {
		fmt.Printf("  mean concurrent runs     %6.2f\n", rep.Runs.MeanConcurrent)
	}
	fmt.Printf("  live now                 %6d\n", rep.Runs.Live)

	// Printed right after capacity and before the latency stages, so
	// "approved -> attempt started" below is read with both of its causes
	// already on screen: the utilization above says whether the
	// deployment was full, and this says what the tick itself cost
	// regardless of that.
	printCycles(rep.Cycles)

	// Latency's own header says what a window means for it: these are the
	// samples that *ended* inside it, so a task filed last month and
	// completed this morning is in here at its full lead time.
	fmt.Println("\nlatency (stages that ended inside the window)")
	fmt.Printf("  %-40s %5s %10s %10s %10s\n", "stage", "n", "p50", "p90", "max")
	for _, s := range rep.Latency {
		fmt.Printf("  %-40s %5d %10s %10s %10s\n", s.Label, s.N,
			seconds(s.P50Seconds), seconds(s.P90Seconds), seconds(s.MaxSeconds))
	}

	if len(rep.Backlog.ByState) > 0 {
		fmt.Println("\nbacklog (right now, not over the window)")
		states := make([]string, 0, len(rep.Backlog.ByState))
		for state, n := range rep.Backlog.ByState {
			states = append(states, fmt.Sprintf("%s=%d", state, n))
		}
		sort.Strings(states)
		fmt.Printf("  %s\n", strings.Join(states, "  "))
		if rep.Backlog.OldestQueuedTaskID != "" {
			fmt.Printf("  oldest queued: task %s, waiting %s\n",
				rep.Backlog.OldestQueuedTaskID, seconds(rep.Backlog.OldestQueuedSeconds))
		}
	}
}

// printCycles renders the daemon's own tick -- the section that says
// whether the queue_wait stage printed below it was capacity or
// scheduling.
//
// The line the whole section builds to is the scheduling floor:
// tick-to-tick p50 plus dispatch p50 is what a task pays for grain's own
// scheduling with no contention at all, so a queue_wait near it is the
// tick and a queue_wait far above it is the deployment being full. The
// per-reconciler table under it is what says which reconciler to look at
// when the tick itself is the problem, since they run in sequence and a
// slow one delays every decision behind it.
//
// Nothing is printed for a report whose daemon had no ticks to speak
// for: `grain metrics` against a deployment serving a UI without a
// reconcile loop (Enabled false), or against one that has only just
// restarted (n == 0). An empty table there would read as "the tick takes
// no time", which is the opposite of "nobody measured it".
func printCycles(c ui.MetricsCycles) {
	if !c.Enabled || c.N == 0 {
		return
	}
	fmt.Println("\nreconcile tick (this daemon, since it started -- not stored, so not over the window)")
	fmt.Printf("  ticks measured           %6d", c.N)
	if c.Truncated {
		fmt.Printf("  (of %d run; older ones forgotten)", c.Observed)
	}
	fmt.Println()
	fmt.Printf("  %-24s %10s %10s %10s\n", "", "p50", "p90", "max")
	fmt.Printf("  %-24s %10s %10s %10s\n", "tick duration",
		preciseSeconds(c.Tick.P50Seconds), preciseSeconds(c.Tick.P90Seconds), preciseSeconds(c.Tick.MaxSeconds))
	fmt.Printf("  %-24s %10s %10s %10s\n", "tick to tick",
		preciseSeconds(c.Interval.P50Seconds), preciseSeconds(c.Interval.P90Seconds), preciseSeconds(c.Interval.MaxSeconds))
	fmt.Printf("  %-24s %10s %10s %10s\n", "cycle start -> dispatch",
		preciseSeconds(c.DispatchWait.P50Seconds), preciseSeconds(c.DispatchWait.P90Seconds), preciseSeconds(c.DispatchWait.MaxSeconds))
	fmt.Printf("  scheduling floor: %s  (tick-to-tick p50 + dispatch p50 -- the queue wait a task pays\n"+
		"    for grain's own scheduling with no contention at all)\n",
		preciseSeconds(c.Interval.P50Seconds+c.DispatchWait.P50Seconds))

	if len(c.Reconcilers) > 0 {
		fmt.Printf("\n  %-16s %10s %10s %10s %8s\n", "reconciler", "wait p50", "p50", "p90", "failed")
		for _, r := range c.Reconcilers {
			fmt.Printf("  %-16s %10s %10s %10s %8d\n", r.Name,
				preciseSeconds(r.Wait.P50Seconds), preciseSeconds(r.Duration.P50Seconds),
				preciseSeconds(r.Duration.P90Seconds), r.Failures)
		}
	}
}

// preciseSeconds renders one of the cycles section's own second counts.
// It is seconds() with a finer hand: a healthy tick is measured in
// milliseconds, and rounding to the whole second the way every other
// number in this report does would print it as "0s" -- which is the one
// answer this section must never give, since "too fast to matter" and
// "nobody measured it" would then look the same.
func preciseSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

// seconds renders one of the report's own second counts as a duration,
// rounded to the second: a p50 printed as "1h4m12s" is read at a glance,
// where "3852.4" is arithmetic homework.
func seconds(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}

// outcomeSummary renders the outcome counts in a stable order -- by count
// descending, so the ending that dominates a window is first, and by name
// where counts tie so two runs of the same report never disagree.
func outcomeSummary(outcomes map[string]int) string {
	if len(outcomes) == 0 {
		return ""
	}
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if outcomes[names[i]] != outcomes[names[j]] {
			return outcomes[names[i]] > outcomes[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, outcomes[name]))
	}
	return strings.Join(parts, " ")
}
