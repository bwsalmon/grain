package ui_test

// The measurement surface: Client.Metrics against a real store, and the
// endpoint over it. What a percentile *means* is pkg/metrics' own tests'
// job; what these pin is that a task's real timestamps -- written by
// CreateTask, StartRun, SetRunAgentStarted, FinishRun and Observe, each
// by the path that really writes it -- come back out the other end as the
// stages of that task's life.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestParseMetricsWindow(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: ui.DefaultMetricsWindow},
		{in: "36h", want: 36 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: " 7d ", want: 7 * 24 * time.Hour},
		{in: "banana", wantErr: true},
		{in: "-3d", wantErr: true},
		{in: "0h", wantErr: true},
		{in: "500w", wantErr: true}, // past maxMetricsWindow
	} {
		got, err := ui.ParseMetricsWindow(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMetricsWindow(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMetricsWindow(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMetricsWindow(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMetricsReportsAWholeTaskLifeAsStages(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx) // filed and approved at baseTime

	// One attempt: dispatched half an hour later, five minutes of setup
	// before its agent's first turn, half an hour of agent work, and the
	// completion five minutes after the run finished.
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1", Attempt: 1,
		StartedAt: baseTime.Add(30 * time.Minute),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunAgentStarted(ctx, "r1", baseTime.Add(35*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(65*time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	completed := baseTime.Add(70 * time.Minute)
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &completed}); err != nil {
		t.Fatal(err)
	}

	// The report is taken later than the work it measures, which is the
	// only ordering a real one ever has.
	c.Now = func() time.Time { return baseTime.Add(2 * time.Hour) }
	rep, err := c.Metrics(ctx, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}

	if rep.Throughput.TasksFiled != 1 || rep.Throughput.TasksCompleted != 1 {
		t.Errorf("throughput = %+v, want one task filed and one completed", rep.Throughput)
	}
	if got, want := rep.Runs.Outcomes["succeeded"], 1; got != want {
		t.Errorf("Outcomes[succeeded] = %d, want %d", got, want)
	}
	for _, want := range []struct {
		stage   string
		n       int
		seconds float64
	}{
		{"queue_wait", 1, 30 * 60},
		{"sandbox_setup", 1, 5 * 60},
		{"agent_work", 1, 30 * 60},
		{"attempt", 1, 35 * 60},
		{"time_to_finish", 1, 40 * 60},
		{"lead_time", 1, 70 * 60},
		// Approved in the same instant it was filed, so nobody waited.
		{"approval_wait", 0, 0},
		// One attempt, so no retry.
		{"retry_wait", 0, 0},
	} {
		got := metricsStage(t, rep, want.stage)
		if got.N != want.n || got.P50Seconds != want.seconds {
			t.Errorf("%s = (n=%d, p50=%vs), want (n=%d, p50=%vs)",
				want.stage, got.N, got.P50Seconds, want.n, want.seconds)
		}
	}

	// The backlog is a gauge taken at the report's own moment, and counts
	// unfinished work only: this task has completed by then, so nothing
	// is left in the system at all.
	if rep.Backlog.Queued != 0 || len(rep.Backlog.ByState) != 0 {
		t.Errorf("backlog = %+v, want nothing left once the one task completed", rep.Backlog)
	}
}

func TestMetricsEndpointServesAReportAndRejectsNonsense(t *testing.T) {
	srv, c := testServer(t)
	create(t, c, context.Background())
	c.Now = func() time.Time { return baseTime.Add(time.Hour) }

	rec := do(t, srv, http.MethodGet, "/api/metrics?window=3d&buckets=4", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	rep := decode[ui.MetricsReport](t, rec)
	if got, want := rep.WindowSeconds, float64(3*24*60*60); got != want {
		t.Errorf("WindowSeconds = %v, want %v", got, want)
	}
	if got, want := len(rep.Throughput.Buckets), 4; got != want {
		t.Errorf("got %d buckets, want %d", got, want)
	}
	if len(rep.Latency) == 0 {
		t.Error("the report carried no latency stages at all")
	}
	if rep.Throughput.TasksFiled != 1 {
		t.Errorf("TasksFiled = %d, want the one task filed above", rep.Throughput.TasksFiled)
	}

	for _, query := range []string{"?window=banana", "?window=0h", "?buckets=0", "?buckets=nine"} {
		if rec := do(t, srv, http.MethodGet, "/api/metrics"+query, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /api/metrics%s = %d, want 400", query, rec.Code)
		}
	}
}

// metricsStage picks one stage out of the report by its stable key.
func metricsStage(t *testing.T, rep ui.MetricsReport, stage string) ui.MetricsStage {
	t.Helper()
	for _, s := range rep.Latency {
		if s.Stage == stage {
			return s
		}
	}
	t.Fatalf("no %q stage in the report", stage)
	return ui.MetricsStage{}
}
