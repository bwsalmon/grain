package staterepo_test

// Is what reaches the repository one state of the database, or several?
//
// Every other test here writes rows, exports, and reads them back with
// nothing else happening. A deployment is not like that: the export runs
// on a timer inside the daemon that is also filing tasks, starting runs
// and stamping observations, and it reads one table per statement. These
// tests are about the seam that opens between two of those statements.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// A dump has to be a snapshot of one database, not a series of reads
// taken while it moved.
//
// The tables are read in name order, so `task` is read before
// `task_run`: a task filed and dispatched in the milliseconds between
// the two used to produce a dump whose task_run.json named a task
// task.json did not have. Nothing rejected it -- this schema declares no
// foreign keys today -- so it arrived as a restore with runs belonging
// to tasks that were not there.
func TestAnExportIsASnapshotOfOneDatabase(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("seed-%03d", i)
		if err := store.PutTask(ctx, task(id)); err != nil {
			t.Fatalf("putting: %v", err)
		}
		if err := store.StartRun(ctx, model.Run{
			ID: id + "-1", TaskID: id, Sandbox: "grain-" + id, Attempt: 1, StartedAt: now,
		}, model.Limits{}); err != nil {
			t.Fatalf("starting a run: %v", err)
		}
	}

	// A writer doing what the daemon does: filing a task and dispatching
	// it, as fast as it can, for as long as the exports take.
	stop := make(chan struct{})
	failed := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("live-%05d", i)
			if err := store.PutTask(ctx, task(id)); err != nil {
				select {
				case failed <- err:
				default:
				}
				return
			}
			if err := store.StartRun(ctx, model.Run{
				ID: id + "-1", TaskID: id, Sandbox: "grain-" + id, Attempt: 1, StartedAt: now,
			}, model.Limits{}); err != nil {
				select {
				case failed <- err:
				default:
				}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for attempt := 0; attempt < 10; attempt++ {
		dir := t.TempDir()
		if err := staterepo.Export(ctx, db, dir); err != nil {
			t.Fatalf("exporting: %v", err)
		}
		tasks := columnValues(t, filepath.Join(dir, staterepo.TablesDir, "task.json"), "id")
		runs := columnValues(t, filepath.Join(dir, staterepo.TablesDir, "task_run.json"), "task_id")
		for taskID := range runs {
			if !tasks[taskID] {
				t.Fatalf("export %d is not a snapshot: task_run.json names task %q, "+
					"which task.json does not have", attempt, taskID)
			}
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-failed:
		t.Fatalf("the concurrent writer failed, so the exports above raced nothing: %v", err)
	default:
	}
}

// And the export must not do that by locking the database: the daemon
// goes on writing while it runs. pkg/model/sqlite opens the store with
// _txlock=immediate, so a transaction begun without saying it is
// read-only takes SQLite's write lock -- and an export of every
// transcript grain has stored would hold it for as long as that takes.
func TestAnExportDoesNotBlockTheDaemonFromWriting(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	// Enough rows, with transcripts on them, that the export is doing
	// real work rather than finishing before the writer has started.
	for i := 0; i < 150; i++ {
		id := fmt.Sprintf("seed-%03d", i)
		if err := store.PutTask(ctx, task(id)); err != nil {
			t.Fatalf("putting: %v", err)
		}
		if err := store.StartRun(ctx, model.Run{
			ID: id + "-1", TaskID: id, Sandbox: "grain-" + id, Attempt: 1, StartedAt: now,
		}, model.Limits{}); err != nil {
			t.Fatalf("starting a run: %v", err)
		}
		if err := store.SetRunTranscript(ctx, id+"-1", randomText(8*1024)); err != nil {
			t.Fatalf("writing a transcript: %v", err)
		}
	}

	var written atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.PutTask(ctx, task(fmt.Sprintf("during-%04d", i))); err != nil {
				return
			}
			written.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	// Let the writer get going, so that "nothing landed" below means the
	// export blocked it rather than that it had not started, and then
	// measure how fast it actually goes on *this* machine with nothing in
	// its way.
	//
	// Measured rather than assumed, because the assumption was wrong. The
	// threshold used to be one write per ten milliseconds of export, on
	// the reasoning that a writer sleeping a millisecond between tasks
	// manages far more than that. It does on an idle machine and it does
	// not on a busy one: under `make test`'s own `go test -race ./...`,
	// with every other package's tests running beside it, one PutTask can
	// take the whole ten milliseconds by itself, and the test fails
	// reporting that the export holds the write lock when the export is
	// holding nothing at all. A rate compared against a constant is a
	// test of how fast the machine is; compared against the same writer's
	// own rate a moment earlier, it is a test of what the export did to
	// it, which is the thing this is about.
	time.Sleep(20 * time.Millisecond)
	baselineStart := time.Now()
	baselineFrom := written.Load()
	time.Sleep(200 * time.Millisecond)
	baseline := rate(written.Load()-baselineFrom, time.Since(baselineStart))

	before := written.Load()
	start := time.Now()
	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	took := time.Since(start)
	during := written.Load() - before
	close(stop)
	<-done

	if baseline == 0 {
		t.Fatalf("no write landed in %s with nothing in the writer's way, so this machine "+
			"cannot say anything about what the export did to it", 200*time.Millisecond)
	}
	// A tenth of the writer's own unobstructed rate. Well under what an
	// export that blocks nobody leaves -- it competes for the same disk
	// and the same cores, so some slowdown is honest -- and well over
	// what an export holding the write lock leaves behind, which is the
	// single write that was blocked on it and landed as it let go.
	got := rate(during, took)
	want := baseline / 10
	t.Logf("%d writes landed during an export that took %s: %.0f/s against the writer's own "+
		"%.0f/s a moment earlier (wanted at least %.0f/s)",
		during, took.Round(time.Millisecond), got, baseline, want)
	if got < want {
		t.Fatalf("the writer managed %.0f writes/s during the export and %.0f/s just before it: "+
			"the export is holding SQLite's write lock and the daemon is waiting on it",
			got, baseline)
	}
}

// rate is writes per second, for the two windows the test above compares
// against each other. Zero for a window in which nothing landed, and for
// one of no duration -- neither says anything about a rate, and the
// caller checks for it rather than dividing by zero here.
func rate(writes int64, over time.Duration) float64 {
	if writes <= 0 || over <= 0 {
		return 0
	}
	return float64(writes) / over.Seconds()
}

// A task and the repos it may read are written in one transaction and
// belong in one tier. They were not: task_read sat in the churn tier on
// the strength of its name, so an ordinary sync exported the task within
// thirty seconds and its read scope up to an hour later.
//
// What that costs is not tidiness. A restore taken in between gives the
// task back with no read scope at all, and a run dispatched from it is
// refused the very repositories somebody filed it to read.
func TestATaskReachesTheRepositoryWithItsReadScope(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	// A task filed after the seed, with a read scope, and one ordinary
	// sync -- the churn tier is an hour away and must not be what carries
	// it.
	tk := task("a1b2")
	tk.Reads = []model.RepoRef{{Owner: "owner", Name: "docs"}, {Owner: "owner", Name: "protos"}}
	tk.Grants = []model.Grant{{Capability: "githubtoken"}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing: %v", err)
	}

	// The restore: an empty database, and only what the sync wrote.
	other, otherDB := openDB(t)
	if err := staterepo.Import(ctx, otherDB, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	got, err := other.GetTask(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("reading the restored task: %v %v", got, err)
	}
	if len(got.Reads) != 2 {
		t.Fatalf("the restored task has %d read repos, want 2: %+v", len(got.Reads), got.Reads)
	}
	if len(got.Grants) != 1 {
		t.Fatalf("the restored task has %d grants, want 1: %+v", len(got.Grants), got.Grants)
	}
}

// The general form of it: every table PutTask writes in one transaction
// is exported on one clock. A later build that adds another child table
// to that write and leaves it out of the state tier gets a failure here
// rather than a restore that half-remembers a task.
func TestATasksOwnTablesShareItsTier(t *testing.T) {
	for _, table := range []string{"task_read", "task_grant", "task_link", "task_tag"} {
		if staterepo.TierOf(table) != staterepo.TierOf("task") {
			t.Errorf("%s is written in the same transaction as task and exported on a different clock", table)
		}
	}
}

// randomText is filler of about the right size and the right
// compressibility for a transcript: English-shaped rather than random
// bytes, since what it is standing in for is somebody's terminal.
func randomText(n int) string {
	words := []string{"the", "agent", "ran", "make", "test", "and", "it", "failed", "with",
		"an", "error", "in", "pkg", "model", "store", "so", "I", "changed", "the", "query"}
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(words[i%len(words)])
		b.WriteByte(' ')
	}
	return b.String()
}

// columnValues reads one column out of a dumped table file, as a set.
func columnValues(t *testing.T, path, column string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, row := range rows {
		if s, ok := row[column].(string); ok {
			out[s] = true
		}
	}
	return out
}
