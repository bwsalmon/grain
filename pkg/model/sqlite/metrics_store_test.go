package sqlite_test

// The measurement reads, against a real database: every moment
// pkg/metrics computes a throughput or latency number from has to survive
// a round trip through SQLite, and a moment that never happened has to
// come back absent rather than zero.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func TestTimingsReadEveryMomentAReportMeasuresBetween(t *testing.T) {
	store, _, ctx := openStore(t)

	filed, approved := now, now.Add(30*time.Minute)
	tk := task("a1b2", true)
	tk.CreatedAt = &filed
	tk.ApprovedAt = &approved
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}

	// One attempt that reached its agent and finished, then a second
	// still in flight -- the two shapes RunTimings has to tell apart.
	started, agentStarted, finished := now.Add(time.Hour), now.Add(time.Hour+5*time.Minute), now.Add(90*time.Minute)
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Sandbox: "s1", Attempt: 1, StartedAt: started,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunAgentStarted(ctx, "r1", agentStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", finished, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r2", TaskID: "a1b2", Sandbox: "s2", Attempt: 2, StartedAt: now.Add(2 * time.Hour),
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	completed := now.Add(3 * time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", CompletedAt: &completed}); err != nil {
		t.Fatal(err)
	}

	timings, err := store.TaskTimings(ctx)
	if err != nil {
		t.Fatalf("TaskTimings: %v", err)
	}
	if len(timings) != 1 {
		t.Fatalf("read %d task timings, want 1", len(timings))
	}
	got := timings[0]
	if got.TaskID != "a1b2" || got.Reason != model.ReasonDirect {
		t.Errorf("timing = %+v, want task a1b2 filed directly", got)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(filed) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, filed)
	}
	if got.ApprovedAt == nil || !got.ApprovedAt.Equal(approved) {
		t.Errorf("ApprovedAt = %v, want %v", got.ApprovedAt, approved)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completed) {
		t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, completed)
	}
	if got.ClosedAt != nil {
		t.Errorf("ClosedAt = %v, want nil -- the task was never closed", got.ClosedAt)
	}

	runs, err := store.RunTimings(ctx)
	if err != nil {
		t.Fatalf("RunTimings: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("read %d run timings, want 2", len(runs))
	}
	first, second := runs[0], runs[1]
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Fatalf("attempts came back %d then %d, want oldest first", first.Attempt, second.Attempt)
	}
	if !first.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", first.StartedAt, started)
	}
	if first.AgentStartedAt == nil || !first.AgentStartedAt.Equal(agentStarted) {
		t.Errorf("AgentStartedAt = %v, want %v", first.AgentStartedAt, agentStarted)
	}
	if first.FinishedAt == nil || !first.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", first.FinishedAt, finished)
	}
	if first.Outcome != "succeeded" {
		t.Errorf("Outcome = %q, want succeeded", first.Outcome)
	}

	// The live attempt: started, no agent yet, no ending. All three have
	// to read as absent, since pkg/metrics reports a missing moment by
	// leaving the sample out rather than by measuring against zero.
	if second.AgentStartedAt != nil || second.FinishedAt != nil || second.Outcome != "" {
		t.Errorf("live attempt = %+v, want no agent start, no finish and no outcome", second)
	}
}

// A store written before agent_started_at existed keeps working: Init
// adds the column (Store.ensureTaskRunAgentStartedAtColumn), the runs
// recorded before it read back with no agent start rather than failing
// the query they appear in, and the next run can record one for real --
// the same shape TestInitMigratesAnExistingDatabaseMissingTranscript
// pins for the column before this one.
func TestInitMigratesAnExistingDatabaseMissingAgentStartedAt(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_run`"+` (
  `+"`id`"+`          TEXT     NOT NULL,
  `+"`task_id`"+`     TEXT     NOT NULL,
  `+"`sandbox`"+`     TEXT     NOT NULL,
  `+"`unit`"+`        TEXT     NULL,
  `+"`attempt`"+`     INTEGER  NOT NULL,
  `+"`started_at`"+`  DATETIME NOT NULL,
  `+"`finished_at`"+` DATETIME NULL,
  `+"`outcome`"+`     TEXT     NULL,
  `+"`detail`"+`      TEXT     NULL,
  `+"`transcript`"+`  TEXT     NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the older task_run table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_run` (`id`,`task_id`,`sandbox`,`attempt`,`started_at`,`finished_at`,`outcome`) "+
			"VALUES ('r1','a1b2','s1',1,?,?,'succeeded')", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("seeding a run recorded before the column existed: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task_run.agent_started_at: %v", err)
	}

	runs, err := store.RunTimings(ctx)
	if err != nil {
		t.Fatalf("RunTimings after migrating: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentStartedAt != nil {
		t.Fatalf("runs = %+v, want one attempt with no agent start recorded", runs)
	}
	if err := store.SetRunAgentStarted(ctx, "r1", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("set after migrating: %v", err)
	}
	if runs, err := store.RunTimings(ctx); err != nil ||
		len(runs) != 1 || runs[0].AgentStartedAt == nil || !runs[0].AgentStartedAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("RunTimings after set = (%+v, %v), want the agent start now durable", runs, err)
	}
}

// TaskTiming carries two facts that are not moments -- whether the task
// had a repo to push to, and whether the merge queue ever had to file a
// fix task for it -- because pkg/metrics cannot measure the pull request
// loop without them: the first is who was ever offered that loop, and
// the second is the recorded form of a red build outliving its run.
func TestTaskTimingsReadTargetAndFixTaskLink(t *testing.T) {
	store, _, ctx := openStore(t)

	// One task with a target and a fix task filed against it, one with a
	// target and only an unrelated link, and one with no target at all.
	red := task("a1b2", true)
	red.Links = []model.Link{{Kind: model.LinkFixTask, Target: "f1x0"}}
	green := task("b2c3", true)
	green.Links = []model.Link{{Kind: model.LinkDependsOn, Target: "a1b2"}}
	bare := task("c3d4", true)
	bare.Target = nil
	for _, tk := range []model.Task{red, green, bare} {
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatalf("putting %s: %v", tk.ID, err)
		}
	}

	timings, err := store.TaskTimings(ctx)
	if err != nil {
		t.Fatalf("TaskTimings: %v", err)
	}
	byID := map[string]model.TaskTiming{}
	for _, tm := range timings {
		byID[tm.TaskID] = tm
	}
	if got := byID["a1b2"]; !got.Targeted || !got.FixTaskFiled {
		t.Errorf("a1b2 = %+v, want targeted with a fix task filed", got)
	}
	if got := byID["b2c3"]; !got.Targeted || got.FixTaskFiled {
		t.Errorf("b2c3 = %+v, want targeted with no fix task -- its only link is a dependency", got)
	}
	if got := byID["c3d4"]; got.Targeted || got.FixTaskFiled {
		t.Errorf("c3d4 = %+v, want neither -- it has no repo to push to", got)
	}
}
