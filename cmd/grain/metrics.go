// metrics.go is "grain metrics": the throughput and latency report
// (pkg/metrics, served as GET /api/metrics) printed at a terminal.
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
