package ui

// GET /api/metrics: what this deployment delivered over a window, and
// where a task's wall-clock time went getting there. pkg/metrics computes
// it; this is the wire shape and the one place a window string is parsed.
//
// The JSON types below are this package's own rather than pkg/metrics'
// types with tags on them, the same choice SandboxSnapshot makes over
// orchestrator.SandboxHealth: a Distribution is a set of time.Durations,
// which marshal as bare nanosecond counts that no consumer can read
// without knowing that, and an API is worth stating in units it says out
// loud. Everything crossing this boundary is seconds, named as such.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/metrics"
	"github.com/bwsalmon/grain/pkg/model"
)

// DefaultMetricsWindow is how far back a report reaches when the caller
// does not say. A week covers a deployment's own weekly rhythm without
// making the first number anybody sees an average over a quarter.
const DefaultMetricsWindow = 7 * 24 * time.Hour

// maxMetricsWindow bounds ?window=. Not a cost limit -- the read is the
// whole table either way (Store.TaskTimings) -- but a sanity one: a
// window measured in centuries says the caller meant something else.
const maxMetricsWindow = 365 * 24 * time.Hour

// ParseMetricsWindow reads a window string: "" for the default, a Go
// duration ("36h", "90m"), or a count of days or weeks ("7d", "2w"),
// which time.ParseDuration itself has no unit for and which is how anyone
// actually asks for a window this long.
func ParseMetricsWindow(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return DefaultMetricsWindow, nil
	}
	var window time.Duration
	switch unit := text[len(text)-1]; unit {
	case 'd', 'w':
		unitName, per := "days", 24*time.Hour
		if unit == 'w' {
			unitName, per = "weeks", 7*24*time.Hour
		}
		n, err := strconv.Atoi(text[:len(text)-1])
		if err != nil {
			return 0, fmt.Errorf("window %q: expected a whole number of %s, like 7%c", text, unitName, unit)
		}
		window = time.Duration(n) * per
	default:
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("window %q: expected a duration like 36h, or a count of days or weeks like 7d", text)
		}
		window = parsed
	}
	if window <= 0 {
		return 0, fmt.Errorf("window %q: must be positive", text)
	}
	if window > maxMetricsWindow {
		return 0, fmt.Errorf("window %q: at most %s", text, maxMetricsWindow)
	}
	return window, nil
}

// MetricsReport is GET /api/metrics' whole body.
type MetricsReport struct {
	Since         time.Time         `json:"since"`
	Until         time.Time         `json:"until"`
	WindowSeconds float64           `json:"windowSeconds"`
	Throughput    MetricsThroughput `json:"throughput"`
	// Latency is a list rather than an object so it arrives in the order
	// a task passes through the stages, which is the order anybody reads
	// them in and the order the CLI prints them.
	Latency []MetricsStage `json:"latency"`
	Runs    MetricsRuns    `json:"runs"`
	Backlog MetricsBacklog `json:"backlog"`
}

// MetricsThroughput is how much crossed each line during the window, and
// the same counts as a daily rate.
type MetricsThroughput struct {
	TasksFiled         int             `json:"tasksFiled"`
	TasksCompleted     int             `json:"tasksCompleted"`
	TasksClosed        int             `json:"tasksClosed"`
	RunsStarted        int             `json:"runsStarted"`
	RunsFinished       int             `json:"runsFinished"`
	FiledPerDay        float64         `json:"filedPerDay"`
	CompletedPerDay    float64         `json:"completedPerDay"`
	RunsFinishedPerDay float64         `json:"runsFinishedPerDay"`
	Buckets            []MetricsBucket `json:"buckets"`
}

// MetricsBucket is one slice of the window's own time series, oldest
// first -- what a trend line is drawn from.
type MetricsBucket struct {
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	Filed        int       `json:"filed"`
	Completed    int       `json:"completed"`
	RunsFinished int       `json:"runsFinished"`
}

// MetricsStage is one latency distribution: what it measures, and the
// shape of the samples that ended inside the window. N is worth reading
// first -- a p99 over four samples is the maximum wearing a percentile's
// name, and stages legitimately carry different sample counts (see
// pkg/metrics on why a missing moment is skipped rather than guessed).
type MetricsStage struct {
	Stage       string  `json:"stage"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	N           int     `json:"n"`
	MinSeconds  float64 `json:"minSeconds"`
	P50Seconds  float64 `json:"p50Seconds"`
	P90Seconds  float64 `json:"p90Seconds"`
	P99Seconds  float64 `json:"p99Seconds"`
	MaxSeconds  float64 `json:"maxSeconds"`
	MeanSeconds float64 `json:"meanSeconds"`
}

// MetricsRuns is how the window's attempts ended, and how much of the
// deployment's dispatch capacity they used.
type MetricsRuns struct {
	Outcomes              map[string]int `json:"outcomes"`
	AttemptsPerCompletion float64        `json:"attemptsPerCompletion"`
	MeanConcurrent        float64        `json:"meanConcurrent"`
	MaxConcurrent         int            `json:"maxConcurrent"`
	Utilization           float64        `json:"utilization"`
	Live                  int            `json:"live"`
}

// MetricsBacklog is what was still in the system when the report was
// taken -- a gauge, not a window. Utilization next to a deep backlog is
// the pair worth reading together: work waiting while capacity sat idle
// is a scheduling problem, not a capacity one.
type MetricsBacklog struct {
	ByState             map[model.State]int `json:"byState"`
	Queued              int                 `json:"queued"`
	OldestQueuedSeconds float64             `json:"oldestQueuedSeconds"`
	OldestQueuedTaskID  string              `json:"oldestQueuedTaskId,omitempty"`
}

// Metrics computes a report over the window ending now. buckets is how
// many points the time series carries; 0 takes pkg/metrics' own default.
//
// It reads the store and derives everything else, so it takes no lock,
// caches nothing, and cannot be stale: a report is exactly what the rows
// said at the moment it was asked for.
func (c *Client) Metrics(ctx context.Context, window time.Duration, buckets int) (MetricsReport, error) {
	if window <= 0 {
		window = DefaultMetricsWindow
	}
	until := c.now()
	in := metrics.Input{
		Window:  metrics.Window{Since: until.Add(-window), Until: until},
		Buckets: buckets,
	}

	var err error
	if in.Tasks, err = c.Store.TaskTimings(ctx); err != nil {
		return MetricsReport{}, fmt.Errorf("reading task timings: %w", err)
	}
	if in.Runs, err = c.Store.RunTimings(ctx); err != nil {
		return MetricsReport{}, fmt.Errorf("reading run timings: %w", err)
	}
	if in.States, err = c.Store.States(ctx); err != nil {
		return MetricsReport{}, fmt.Errorf("reading task states: %w", err)
	}
	// A deployment whose settings have never been written has no
	// concurrency limit to compare occupancy against; the rest of the
	// report is unaffected, so this reports no utilization rather than
	// no report.
	cfg, err := c.Store.GetConfig(ctx)
	if err != nil {
		return MetricsReport{}, fmt.Errorf("reading deployment settings: %w", err)
	}
	if cfg != nil {
		in.MaxConcurrent = cfg.MaxConcurrent
	}

	return metricsReportFrom(metrics.Compute(in)), nil
}

// metricsReportFrom is the whole of the conversion to the wire: durations
// become seconds, and the Latency struct becomes the ordered list a
// reader walks a task's life through.
func metricsReportFrom(rep metrics.Report) MetricsReport {
	out := MetricsReport{
		Since:         rep.Window.Since,
		Until:         rep.Window.Until,
		WindowSeconds: rep.Window.Duration().Seconds(),
		Throughput: MetricsThroughput{
			TasksFiled:         rep.Throughput.TasksFiled,
			TasksCompleted:     rep.Throughput.TasksCompleted,
			TasksClosed:        rep.Throughput.TasksClosed,
			RunsStarted:        rep.Throughput.RunsStarted,
			RunsFinished:       rep.Throughput.RunsFinished,
			FiledPerDay:        rep.Throughput.FiledPerDay,
			CompletedPerDay:    rep.Throughput.CompletedPerDay,
			RunsFinishedPerDay: rep.Throughput.RunsFinishedPerDay,
			Buckets:            make([]MetricsBucket, 0, len(rep.Throughput.Buckets)),
		},
		Runs: MetricsRuns{
			Outcomes:              rep.Runs.Outcomes,
			AttemptsPerCompletion: rep.Runs.AttemptsPerCompletion,
			MeanConcurrent:        rep.Runs.MeanConcurrent,
			MaxConcurrent:         rep.Runs.MaxConcurrent,
			Utilization:           rep.Runs.Utilization,
			Live:                  rep.Runs.Live,
		},
		Backlog: MetricsBacklog{
			ByState:             rep.Backlog.ByState,
			Queued:              rep.Backlog.Queued,
			OldestQueuedSeconds: rep.Backlog.OldestQueuedWait.Seconds(),
			OldestQueuedTaskID:  rep.Backlog.OldestQueuedTaskID,
		},
	}
	for _, b := range rep.Throughput.Buckets {
		out.Throughput.Buckets = append(out.Throughput.Buckets, MetricsBucket{
			Since: b.Since, Until: b.Until,
			Filed: b.Filed, Completed: b.Completed, RunsFinished: b.RunsFinished,
		})
	}
	if out.Runs.Outcomes == nil {
		out.Runs.Outcomes = map[string]int{}
	}

	l := rep.Latency
	for _, s := range []struct {
		stage, label, description string
		d                         metrics.Distribution
	}{
		{"approval_wait", "filed -> approved",
			"how long a proposed task sat before a human accepted it (tasks approved as they were filed are not counted)", l.ApprovalWait},
		{"queue_wait", "approved -> attempt started",
			"the backlog wait: grain's own scheduling, and what more concurrency or a faster tick would shrink", l.QueueWait},
		{"sandbox_setup", "attempt started -> agent's first turn",
			"a sandbox built, a repo cloned, capabilities minted -- the cost a golden image or a warm spare would cut", l.SandboxSetup},
		{"agent_work", "agent's first turn -> attempt finished",
			"what the agent framework itself spent; grain does not control it", l.AgentWork},
		{"attempt", "one whole attempt",
			"setup plus agent work plus the finish path -- the one stage every run has", l.Attempt},
		{"retry_wait", "attempt finished -> next attempt started",
			"dispatch's failure backoff, plus another turn through the queue", l.RetryWait},
		{"time_to_finish", "first attempt started -> completed",
			"how long the work took once it began, across every attempt, question and reply it needed", l.TimeToFinish},
		{"lead_time", "filed -> completed",
			"the whole of what whoever filed the task actually waited", l.LeadTime},
	} {
		out.Latency = append(out.Latency, MetricsStage{
			Stage: s.stage, Label: s.label, Description: s.description,
			N:           s.d.N,
			MinSeconds:  s.d.Min.Seconds(),
			P50Seconds:  s.d.P50.Seconds(),
			P90Seconds:  s.d.P90.Seconds(),
			P99Seconds:  s.d.P99.Seconds(),
			MaxSeconds:  s.d.Max.Seconds(),
			MeanSeconds: s.d.Mean.Seconds(),
		})
	}
	return out
}

// maxMetricsBuckets caps ?buckets= the same way pkg/metrics does its own,
// so an absurd request is a 400 here rather than a silently different
// answer there.
const maxMetricsBuckets = 200

func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	window, err := ParseMetricsWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	buckets := 0
	if raw := r.URL.Query().Get("buckets"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxMetricsBuckets {
			writeError(w, http.StatusBadRequest,
				errors.New("buckets must be a positive integer no greater than "+strconv.Itoa(maxMetricsBuckets)))
			return
		}
		buckets = parsed
	}
	report, err := s.tasks.Metrics(r.Context(), window, buckets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
