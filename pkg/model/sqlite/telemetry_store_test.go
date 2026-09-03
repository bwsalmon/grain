package sqlite_test

// The per-run tool census, against a real database: what a run recorded
// about its own tool use has to survive the round trip, a re-record has to
// replace rather than double, and a store that predates these tables has
// to gain them rather than fail (model/schema.go's own note on why
// neither needs a migration rung).

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func TestRunTelemetryRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	seedFinishedRun(t, store, ctx, "r1", "a1b2")

	sizes := model.SizeHistogram{}
	for _, n := range []int64{40, 40, 90000} {
		sizes.Add(n)
	}
	err := store.RecordRunTelemetry(ctx, "r1", model.RunTelemetry{
		Tools: []model.RunToolUse{
			{Tool: "run_command", Calls: 3, Errored: 1, TimedOut: 1,
				ResultBytes: 90080, MaxResultBytes: 90000, Sizes: sizes},
			{Tool: "edit_file", Calls: 2, Errored: 2, ResultBytes: 60,
				MaxResultBytes: 40, Sizes: model.SizeHistogram{6: 2}},
		},
		CheckWaits: []model.RunCheckWait{
			{Seq: 0, Verdict: "failed", Waited: 4 * time.Minute, PushesBefore: 1},
			{Seq: 1, Verdict: "passed", Waited: 9 * time.Minute, PushesBefore: 2},
		},
	})
	if err != nil {
		t.Fatalf("RecordRunTelemetry: %v", err)
	}

	uses, err := store.RunToolUses(ctx)
	if err != nil {
		t.Fatalf("RunToolUses: %v", err)
	}
	if len(uses) != 2 {
		t.Fatalf("read %d census rows, want 2: %+v", len(uses), uses)
	}
	// Ordered by tool within a run, so edit_file comes first.
	if got := uses[0]; got.RunID != "r1" || got.Tool != "edit_file" || got.Calls != 2 || got.Errored != 2 {
		t.Errorf("edit_file row = %+v", got)
	}
	cmd := uses[1]
	if cmd.Tool != "run_command" || cmd.Calls != 3 || cmd.Errored != 1 || cmd.TimedOut != 1 {
		t.Errorf("run_command row = %+v", cmd)
	}
	if cmd.ResultBytes != 90080 || cmd.MaxResultBytes != 90000 {
		t.Errorf("run_command sizes = %d total, %d max", cmd.ResultBytes, cmd.MaxResultBytes)
	}
	if cmd.Sizes.Total() != 3 || cmd.Sizes[model.SizeBucket(90000)] != 1 {
		t.Errorf("run_command histogram = %v, want the three results it recorded", cmd.Sizes)
	}

	waits, err := store.RunCheckWaits(ctx)
	if err != nil {
		t.Fatalf("RunCheckWaits: %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("read %d CI waits, want 2: %+v", len(waits), waits)
	}
	if got := waits[0]; got.Seq != 0 || got.Verdict != "failed" || got.Waited != 4*time.Minute || got.PushesBefore != 1 {
		t.Errorf("first wait = %+v", got)
	}
	if got := waits[1]; got.Seq != 1 || got.Verdict != "passed" || got.Waited != 9*time.Minute || got.PushesBefore != 2 {
		t.Errorf("second wait = %+v", got)
	}
}

// A second record for the same run replaces the first. Every count in
// these tables is summed by a report, so a retried write that added
// instead would double a run's whole census.
func TestRecordRunTelemetryReplacesWhatWasThereBefore(t *testing.T) {
	store, _, ctx := openStore(t)
	seedFinishedRun(t, store, ctx, "r1", "a1b2")

	first := model.RunTelemetry{
		Tools:      []model.RunToolUse{{Tool: "run_command", Calls: 3}},
		CheckWaits: []model.RunCheckWait{{Seq: 0, Verdict: "passed"}},
	}
	if err := store.RecordRunTelemetry(ctx, "r1", first); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunTelemetry(ctx, "r1", first); err != nil {
		t.Fatal(err)
	}
	uses, err := store.RunToolUses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].Calls != 3 {
		t.Errorf("after two records, census = %+v, want one row of 3 calls", uses)
	}
	waits, err := store.RunCheckWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 {
		t.Errorf("after two records, %d CI waits, want 1", len(waits))
	}
}

// RunTimings carries the run's own id and detail, which is what joins a
// census row to the run it belongs to and what tells a run cancelled by
// the wall-clock cap from one whose task a human closed.
func TestRunTimingsCarryTheIDAndDetailAnEndingIsReadFrom(t *testing.T) {
	store, _, ctx := openStore(t)
	seedFinishedRun(t, store, ctx, "r1", "a1b2")
	if err := store.SetRunOutcome(ctx, "r1", "cancelled", model.RuntimeCapDetail(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err := store.RunTimings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("read %d runs, want 1", len(runs))
	}
	if runs[0].RunID != "r1" {
		t.Errorf("RunID = %q, want r1", runs[0].RunID)
	}
	if got := model.EndingOf(runs[0].Outcome, runs[0].Detail); got != model.EndingRuntimeCap {
		t.Errorf("ending = %q, want %q", got, model.EndingRuntimeCap)
	}
}

// A store created before these two tables existed gains them on Init and
// reads back empty, rather than failing every report with "no such
// table".
func TestInitCreatesTheTelemetryTablesOnAnOlderStore(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	// The shape of an older database: the tables simply are not there.
	for _, table := range []string{"task_run_tool", "task_run_check_wait"} {
		if _, err := db.ExecContext(ctx, "DROP TABLE `"+table+"`"); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("re-applying schema: %v", err)
	}
	if uses, err := store.RunToolUses(ctx); err != nil || len(uses) != 0 {
		t.Errorf("RunToolUses = %v, %v; want an empty table", uses, err)
	}
	if waits, err := store.RunCheckWaits(ctx); err != nil || len(waits) != 0 {
		t.Errorf("RunCheckWaits = %v, %v; want an empty table", waits, err)
	}
}

func seedFinishedRun(t *testing.T, store *model.Store, ctx context.Context, runID, taskID string) {
	t.Helper()
	if err := store.PutTask(ctx, task(taskID, true)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: runID, TaskID: taskID, Sandbox: "s1", Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, runID, now.Add(time.Hour), "succeeded", "the run made 5 tool call(s)"); err != nil {
		t.Fatal(err)
	}
}
