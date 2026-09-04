package staterepo_test

// A soak: hundreds of rounds of everything that happens to a state
// repository, in an order nobody chose, with the database and the
// repository held against each other the whole way.
//
// The rest of this package's tests each set one situation up and check
// one thing about it, which is what tests should do and is not enough
// here. The failures this arrangement is prone to are failures of
// *sequence*: an export that committed before a merge arrived, a
// recovery that ran with a stranded commit still in the way, a restart
// that landed between a pull and the import it was supposed to trigger.
// Those need a run long enough for the orderings to collide, which is
// what this is.
//
// What it drives, per round, drawn at random:
//
//   - grain writing: tasks with read scopes and grants, comments, runs
//     starting and finishing with transcripts on them, observations
//     stamped by a reconcile cycle, settings edited through the UI
//   - somebody merging a pull request against the repository: a template
//     retitled, a template added, a template deleted, a file that is not
//     part of the dump
//   - a push that fails, leaving grain's own export stranded on this host
//     -- and so, once a merge lands on top of it, a divergence
//   - a restart, taking the daemon's own startup path
//   - a human pressing Sync, which writes churn out whether or not its
//     slower clock says it is due
//   - the clock crossing a churn interval
//   - a restore onto a new host: a fresh clone, a fresh database, and
//     everything the repository holds imported into it
//
// And what it checks, every round:
//
//   - the working tree is clean after a cycle that reported no error --
//     an export that wrote files it never committed would be a revert
//     waiting to be pushed
//   - grain has made no merge commit, because grain never merges
//   - the dump is internally consistent: every row that names a task
//     names a task the dump has
//   - the settings the daemon is running on are the ones last merged
//   - the state tier of the repository is the state tier of the database
//   - a database restored from the repository re-exports to the same
//     bytes, which is the fixed point the whole arrangement rests on
//
// It is not skipped. Sixty rounds is well under a minute and is worth
// having on every commit; GRAIN_STATEREPO_SOAK_ROUNDS turns it up (a
// thousand is a few minutes) for a change to this package that deserves
// one, and GRAIN_STATEREPO_SOAK_SEED reproduces a run that failed --
// every failure names the seed it came from.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// defaultSoakRounds is what runs without anybody asking for it.
const defaultSoakRounds = 60

func TestSoakTheRepositoryAndTheDatabaseStayInAgreement(t *testing.T) {
	rounds := defaultSoakRounds
	if v := os.Getenv("GRAIN_STATEREPO_SOAK_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("GRAIN_STATEREPO_SOAK_ROUNDS=%q is not a number: %v", v, err)
		}
		rounds = n
	}
	seed := time.Now().UnixNano()
	if v := os.Getenv("GRAIN_STATEREPO_SOAK_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("GRAIN_STATEREPO_SOAK_SEED=%q is not a number: %v", v, err)
		}
		seed = n
	}
	t.Logf("soaking for %d rounds with GRAIN_STATEREPO_SOAK_SEED=%d", rounds, seed)

	d := newSoak(t, seed)
	for round := 1; round <= rounds; round++ {
		d.round = round
		d.act()
		d.cycle()
		d.check()
	}
	d.finish()
}

// soak is the deployment under test: one database, one working tree, one
// bare remote standing in for GitHub, and a clock the churn interval is
// measured against.
type soak struct {
	t      *testing.T
	ctx    context.Context
	rnd    *rand.Rand
	seed   int64
	round  int
	failed bool

	store  *model.Store
	db     *sql.DB
	repo   *staterepo.Repo
	dir    string
	remote string
	clock  *soakClock

	// tasks is every task filed so far, so a later round can pick one to
	// comment on, run or observe.
	tasks []string
	// templates is what the settings half currently holds, as this side
	// believes it: the expectation every check of a merged change is made
	// against.
	templates map[string]string
	nextID    int

	// What the run did, reported at the end: a soak that never diverged
	// or never restarted tested less than it looks like it did.
	counts map[string]int
}

type soakClock struct{ t time.Time }

func (c *soakClock) now() time.Time { return c.t }

func newSoak(t *testing.T, seed int64) *soak {
	t.Helper()
	ctx := context.Background()
	store, db := openDB(t)
	d := &soak{
		t: t, ctx: ctx, rnd: rand.New(rand.NewSource(seed)), seed: seed,
		store: store, db: db, remote: bareRemote(t),
		dir:       filepath.Join(t.TempDir(), "state"),
		clock:     &soakClock{t: now},
		templates: map[string]string{},
		counts:    map[string]int{},
	}
	// A deployment that has been running a while before the soak starts:
	// settings a human configured, and some history behind them.
	d.putTemplate("tpl-0", "Run the nightly sweep")
	d.putTemplate("tpl-1", "Update the dependencies")
	for i := 0; i < 5; i++ {
		d.fileTask()
	}
	d.open()
	if err := staterepo.Load(d.ctx, d.repo, d.db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding the repository: %v", err)
	}
	return d
}

// open opens (or reopens) the working tree the way cmd/grain does.
func (d *soak) open() {
	d.t.Helper()
	repo, err := staterepo.Open(d.ctx, staterepo.Config{
		Dir: d.dir, Remote: d.remote, AuthorName: "grain", AuthorEmail: "grain@a-host",
		ChurnInterval: staterepo.DefaultChurnInterval, Now: d.clock.now,
	})
	if err != nil {
		d.t.Fatalf("round %d: opening the repository: %v", d.round, err)
	}
	if err := staterepo.EnsureIgnored(repo.Dir()); err != nil {
		d.t.Fatalf("round %d: writing .gitignore: %v", d.round, err)
	}
	d.repo = repo
}

func (d *soak) count(what string) { d.counts[what]++ }

// fatalf names the seed on every failure, because a soak that fails
// without one is a soak nobody can look at twice.
func (d *soak) fatalf(format string, args ...any) {
	d.t.Helper()
	d.t.Fatalf("round %d (GRAIN_STATEREPO_SOAK_SEED=%d): "+format,
		append([]any{d.round, d.seed}, args...)...)
}

// -- what happens to the deployment ------------------------------------

// act draws one round's worth of events. The weights are not arbitrary:
// grain writing to its own database is what a deployment does nearly all
// of the time, and the rare things -- a merge, a failed push, a restart
// -- are rare here in roughly the proportion they are rare there, which
// is what makes a long run cover orderings a scripted one would have to
// name in advance.
func (d *soak) act() {
	d.clock.t = d.clock.t.Add(30 * time.Second)
	switch n := d.rnd.Intn(100); {
	case n < 30:
		d.fileTask()
	case n < 45:
		d.startRun()
	case n < 55:
		d.finishRuns()
	case n < 65:
		d.observe()
	case n < 72:
		d.editSettings()
	case n < 80:
		_ = d.mergePullRequest()
	case n < 84:
		d.strandAnExport()
	case n < 88:
		if d.rnd.Intn(2) == 0 {
			// A merge that lands while the process is down, so the start
			// below is the thing that has to notice and import it.
			_ = d.mergePullRequest()
		}
		d.restart()
	case n < 92:
		d.crossTheChurnInterval()
	case n < 96:
		d.restoreOntoANewHost()
	default:
		// A quiet round: nothing at all happened. The cycle that follows
		// has to produce no commit, which is the property that keeps a
		// deployment from pushing noise forever.
		d.count("quiet round")
	}
}

func (d *soak) fileTask() {
	d.t.Helper()
	d.nextID++
	id := fmt.Sprintf("task-%04d", d.nextID)
	at := d.clock.t
	tk := task(id)
	tk.CreatedAt = &at
	tk.Reads = []model.RepoRef{{Owner: "owner", Name: "docs"}}
	tk.Grants = []model.Grant{{Capability: "githubtoken"}}
	if d.rnd.Intn(3) == 0 {
		tk.Reads = append(tk.Reads, model.RepoRef{Owner: "owner", Name: "protos"})
	}
	if err := d.store.PutTask(d.ctx, tk); err != nil {
		d.fatalf("filing %s: %v", id, err)
	}
	if _, err := d.store.AddComment(d.ctx, model.Comment{
		TaskID: id, CreatedAt: at, Body: "why this is being asked for",
		Author: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}},
	}); err != nil {
		d.fatalf("commenting on %s: %v", id, err)
	}
	d.tasks = append(d.tasks, id)
	d.count("tasks filed")
}

func (d *soak) startRun() {
	d.t.Helper()
	if len(d.tasks) == 0 {
		return
	}
	taskID := d.tasks[d.rnd.Intn(len(d.tasks))]
	id := fmt.Sprintf("%s-run-%d", taskID, d.round)
	if err := d.store.StartRun(d.ctx, model.Run{
		ID: id, TaskID: taskID, Sandbox: "grain-" + id,
		Attempt: d.round, StartedAt: d.clock.t,
	}, model.Limits{}); err != nil {
		// One live run per task is a real constraint, and a dispatcher
		// that lost that race would skip too.
		return
	}
	d.count("runs started")
}

func (d *soak) finishRuns() {
	d.t.Helper()
	rows, err := d.db.QueryContext(d.ctx, "SELECT `id` FROM `task_run` WHERE `finished_at` IS NULL")
	if err != nil {
		d.fatalf("listing live runs: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			d.fatalf("listing live runs: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := d.store.FinishRun(d.ctx, id, d.clock.t, "succeeded", "opened a pull request"); err != nil {
			d.fatalf("finishing %s: %v", id, err)
		}
		if err := d.store.SetRunTranscript(d.ctx, id, randomText(4*1024)); err != nil {
			d.fatalf("writing a transcript for %s: %v", id, err)
		}
		d.count("runs finished")
	}
}

func (d *soak) observe() {
	d.t.Helper()
	for i := 0; i < 5 && i < len(d.tasks); i++ {
		id := d.tasks[len(d.tasks)-1-i]
		at := d.clock.t
		if err := d.store.ObserveField(d.ctx, id, at, func(o *model.Observation) {
			o.PrOpenedAt = &at
		}); err != nil {
			d.fatalf("observing %s: %v", id, err)
		}
	}
	d.count("observation sweeps")
}

// editSettings is a human changing something in the UI, which reaches
// the repository the ordinary way: through the database and the export.
func (d *soak) editSettings() {
	d.t.Helper()
	ids := d.templateIDs()
	if len(ids) == 0 {
		return
	}
	id := ids[d.rnd.Intn(len(ids))]
	d.putTemplate(id, fmt.Sprintf("Edited in the UI at round %d", d.round))
	d.count("settings edited here")
}

func (d *soak) putTemplate(id, title string) {
	d.t.Helper()
	if err := d.store.PutTemplate(d.ctx, model.Template{
		ID: id, Name: id, Title: title, Body: "as configured", CreatedAt: now,
	}); err != nil {
		d.fatalf("writing template %s: %v", id, err)
	}
	d.templates[id] = title
}

func (d *soak) templateIDs() []string {
	out := make([]string, 0, len(d.templates))
	for id := range d.templates {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

// mergePullRequest is the mechanism this whole package exists for: a
// change to the settings, made as a diff against the repository, merged
// by a human, and pulled down by the deployment on its next cycle.
func (d *soak) mergePullRequest() bool {
	d.t.Helper()
	work := filepath.Join(d.t.TempDir(), fmt.Sprintf("pr-%d", d.round))
	git(d.t, "", "clone", "--quiet", d.remote, work)
	if !staterepo.HasDump(work) {
		// Nothing has been pushed yet, so there is no dump to send a pull
		// request against.
		return false
	}
	path := filepath.Join(work, staterepo.TablesDir, "template.json")
	rows := readTemplateRows(d.t, path)
	if len(rows) == 0 {
		return false
	}
	what := ""
	switch d.rnd.Intn(4) {
	case 0:
		// Retitle one.
		i := d.rnd.Intn(len(rows))
		id, _ := rows[i]["id"].(string)
		title := fmt.Sprintf("Merged at round %d", d.round)
		rows[i]["title"] = title
		d.templates[id] = title
		what = "a template retitled"
	case 1:
		// Add one.
		d.nextID++
		id := fmt.Sprintf("tpl-merged-%04d", d.nextID)
		title := fmt.Sprintf("Added by a pull request at round %d", d.round)
		row := map[string]any{}
		for k, v := range rows[0] {
			row[k] = v
		}
		row["id"], row["name"], row["title"] = id, id, title
		rows = append(rows, row)
		d.templates[id] = title
		what = "a template added"
	case 2:
		// Delete one, which is the case that makes the import a
		// replacement rather than a merge.
		if len(rows) < 2 {
			return false
		}
		i := d.rnd.Intn(len(rows))
		id, _ := rows[i]["id"].(string)
		rows = append(rows[:i], rows[i+1:]...)
		delete(d.templates, id)
		what = "a template deleted"
	default:
		// A change to something that is not the dump at all, which grain
		// must carry rather than revert.
		if err := os.WriteFile(filepath.Join(work, "NOTES.md"),
			[]byte(fmt.Sprintf("merged at round %d\n", d.round)), 0o644); err != nil {
			d.fatalf("writing NOTES.md: %v", err)
		}
		what = "a file outside the dump"
	}
	if what != "a file outside the dump" {
		writeTemplateRows(d.t, path, rows)
	}
	git(d.t, work, "add", "--all", ".")
	git(d.t, work, "-c", "user.email=someone@example.com", "-c", "user.name=someone",
		"commit", "-m", "A merged pull request: "+what)
	git(d.t, work, "push", "--quiet", "origin", "main")
	d.count("pull requests merged")
	return true
}

// strandAnExport is a push that fails: the commit stays on this host,
// holding rows that exist nowhere else. Half the time a merge lands
// before it can be retried, which is the divergence -- and half of
// those, the process restarts into it, so the recovery is reached from
// a start rather than from a tick.
func (d *soak) strandAnExport() {
	d.t.Helper()
	unreachable := filepath.Join(d.t.TempDir(), "gone.git")
	git(d.t, d.dir, "remote", "set-url", "origin", unreachable)
	// Something to export, or the stranding is a no-op -- and something
	// that exists only in the commit about to be stranded, which is what
	// a recovery has to not throw away.
	d.fileTask()
	_, _ = staterepo.Sync(d.ctx, d.repo, d.db, model.SchemaVersion)
	git(d.t, d.dir, "remote", "set-url", "origin", d.remote)
	d.count("pushes that failed")
	if d.rnd.Intn(2) != 0 {
		return
	}
	if !d.mergePullRequest() {
		return
	}
	d.count("divergences created")
	if d.rnd.Intn(2) == 0 {
		d.restart()
	}
}

// restart is the daemon starting again over the same data directory:
// staterepo.Load, and, if that is a divergence grain made itself, a
// recovery and a second Load. cmd/grain/daemon.go, verbatim.
func (d *soak) restart() {
	d.t.Helper()
	d.open()
	err := staterepo.Load(d.ctx, d.repo, d.db, model.SchemaVersion)
	if err == nil {
		d.count("restarts")
		return
	}
	if !errors.Is(err, staterepo.ErrDiverged) {
		d.fatalf("a start over the state repository failed: %v", err)
	}
	recovered, rerr := d.repo.RecoverDiverged(d.ctx)
	if rerr != nil || !recovered {
		d.fatalf("a start could not recover a divergence made of grain's own exports: %v %v", recovered, rerr)
	}
	if err := staterepo.Load(d.ctx, d.repo, d.db, model.SchemaVersion); err != nil {
		d.fatalf("loading after a recovery at startup: %v", err)
	}
	d.count("restarts that recovered a divergence")
}

// crossTheChurnInterval moves the clock past DefaultChurnInterval, so the
// next export writes grain's own record of what it did out too.
func (d *soak) crossTheChurnInterval() {
	d.clock.t = d.clock.t.Add(staterepo.DefaultChurnInterval + time.Minute)
	d.count("churn intervals crossed")
}

// restoreOntoANewHost is the claim the repository exists to be able to
// make: a clone of it, on a machine that has never seen this deployment,
// is this deployment.
func (d *soak) restoreOntoANewHost() {
	d.t.Helper()
	dir := filepath.Join(d.t.TempDir(), fmt.Sprintf("restore-%d", d.round))
	restored, restoredDB := d.openScratchDB()
	repo, err := staterepo.Open(d.ctx, staterepo.Config{Dir: dir, Remote: d.remote, Now: d.clock.now})
	if err != nil {
		d.fatalf("cloning onto a new host: %v", err)
	}
	if err := staterepo.Load(d.ctx, repo, restoredDB, model.SchemaVersion); err != nil {
		d.fatalf("loading onto a new host: %v", err)
	}
	if !staterepo.HasDump(dir) {
		return
	}
	// The dump the new host arrived with has to be consistent with itself
	// and has to be the fixed point of its own import: a restore that
	// re-exported to different bytes would commit a diff of itself on its
	// first sync, on every host that ever restored.
	d.dumpIsConsistent(dir)
	// Only when the tip is grain's own export. A merge that has not been
	// exported over yet holds JSON somebody else's editor wrote, and
	// "grain re-exports it in its own shape" is what is supposed to
	// happen to that, not a failure.
	if strings.TrimSpace(git(d.t, dir, "log", "-1", "--format=%an")) == "grain" {
		again := filepath.Join(d.t.TempDir(), fmt.Sprintf("reexport-%d", d.round))
		if err := staterepo.Export(d.ctx, restoredDB, again); err != nil {
			d.fatalf("re-exporting a restored database: %v", err)
		}
		sameDump(d.t, dir, again)
	}

	// And every task it restored came back whole -- with the repos it may
	// read, which is the half a tier mistake takes away silently.
	for _, id := range d.tasksIn(dir) {
		got, err := restored.GetTask(d.ctx, id)
		if err != nil {
			d.fatalf("reading restored task %s: %v", id, err)
		}
		if got == nil {
			d.fatalf("the dump names task %s and the restore does not have it", id)
		}
		if len(got.Reads) == 0 {
			d.fatalf("task %s was restored with no read scope", id)
		}
	}
	d.count("restores onto a new host")
}

// openScratchDB opens a throwaway database at this build's schema, for a
// restore or a check to be poured into.
func (d *soak) openScratchDB() (*model.Store, *sql.DB) {
	d.t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(d.t.TempDir()))
	if err != nil {
		d.fatalf("opening a scratch database: %v", err)
	}
	d.t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Init(d.ctx); err != nil {
		d.fatalf("applying the schema to a scratch database: %v", err)
	}
	return store, db
}

// -- one cycle of the daemon's own loop --------------------------------

// cycle is cmd/grain/statemanager.go's cycle: pull and apply what was
// merged, recovering a divergence that is grain's own to recover, then
// export, commit and push. Its error handling is that function's too,
// because the point of driving it here is to drive what the daemon
// actually does.
func (d *soak) cycle() {
	d.t.Helper()
	all := d.rnd.Intn(10) == 0 // a human pressing Sync now and then
	_, applyErr := staterepo.Apply(d.ctx, d.repo, d.db, model.SchemaVersion)
	if errors.Is(applyErr, staterepo.ErrDiverged) {
		recovered, rerr := d.repo.RecoverDiverged(d.ctx)
		if rerr != nil {
			d.fatalf("recovering a divergence made of grain's own exports: %v", rerr)
		}
		if !recovered {
			d.fatalf("a divergence made only of grain's own exports was refused: %v", applyErr)
		}
		d.count("divergences recovered")
		_, applyErr = staterepo.Apply(d.ctx, d.repo, d.db, model.SchemaVersion)
	}
	if applyErr != nil {
		d.fatalf("applying what the remote holds: %v", applyErr)
	}
	export := staterepo.Sync
	if all {
		export = staterepo.SyncAll
		d.count("syncs asked for by hand")
	}
	changed, err := export(d.ctx, d.repo, d.db, model.SchemaVersion)
	if err != nil {
		d.fatalf("exporting: %v", err)
	}
	if changed {
		d.count("commits")
	}
	// An export of a database nothing has touched since the last one has
	// to commit nothing. It is the property the whole cadence rests on --
	// a dump whose row order or formatting wandered would commit and push
	// on every tick forever, burying the changes that matter -- and it is
	// only ever true if it is true here, after whatever this round did.
	if again, err := staterepo.Sync(d.ctx, d.repo, d.db, model.SchemaVersion); err != nil {
		d.fatalf("re-exporting an untouched database: %v", err)
	} else if again {
		d.fatalf("exporting an untouched database committed something:\n%s",
			git(d.t, d.dir, "show", "--stat", "--format=%s", "HEAD"))
	}
}

// -- what is checked, every round --------------------------------------

func (d *soak) check() {
	d.t.Helper()
	// Nothing uncommitted. An export that wrote files and did not commit
	// them leaves the working tree holding a diff nobody made, which the
	// next commit would push as if grain meant it.
	if status := strings.TrimSpace(git(d.t, d.dir, "status", "--porcelain")); status != "" {
		d.fatalf("the working tree is not clean after a cycle:\n%s", status)
	}
	// grain never merges: Pull is fast-forward only, and a merge commit
	// in this history would mean something resolved a conflict in a
	// database dump by guesswork.
	if merges := strings.TrimSpace(git(d.t, d.dir, "log", "--merges", "--format=%H")); merges != "" {
		d.fatalf("the history holds a merge commit grain would never have made:\n%s", merges)
	}
	d.dumpIsConsistent(d.dir)

	// Nothing grain wrote has gone missing. This is the invariant the
	// whole run exists to hold: a task filed is a task the deployment
	// still has, however many merges, failed pushes, recoveries and
	// restarts have happened since. Every path here that imports is a
	// path that replaces rows, and getting the tier or the moment wrong
	// deletes exactly these.
	for _, id := range d.tasks {
		got, err := d.store.GetTask(d.ctx, id)
		if err != nil {
			d.fatalf("reading task %s: %v", id, err)
		}
		if got == nil {
			d.fatalf("task %s was filed and is no longer in the database", id)
		}
		if len(got.Reads) == 0 {
			d.fatalf("task %s has lost the repos it may read", id)
		}
	}
	if n := d.taskCount(); n != len(d.tasks) {
		d.fatalf("the database holds %d tasks and %d were filed", n, len(d.tasks))
	}

	// The settings the deployment is running on are the ones last merged
	// or last edited here, whichever came later -- which is the whole
	// point of the mechanism.
	for id, title := range d.templates {
		got, err := d.store.GetTemplate(d.ctx, id)
		if err != nil {
			d.fatalf("reading template %s: %v", id, err)
		}
		if got == nil {
			d.fatalf("template %s is gone from the database; it should read %q", id, title)
		}
		if got.Title != title {
			d.fatalf("template %s reads %q; it should read %q", id, got.Title, title)
		}
	}
	if n := d.templateCount(); n != len(d.templates) {
		d.fatalf("the database holds %d templates and %d were expected", n, len(d.templates))
	}

	// The repository's own copy of the state tier is the database's. Not
	// the churn tier: that is exported on a slower clock and is behind on
	// purpose.
	//
	// Every fifth round rather than every one: it pours the whole dump
	// into a database of its own, which is the most expensive thing here
	// by a distance and says the same thing whether it runs 60 times or
	// 12. finish() does it once more, against the remote, at the end.
	if d.round%5 != 0 {
		return
	}
	_, mirror := d.openScratchDB()
	if err := staterepo.Import(d.ctx, mirror, d.dir); err != nil {
		d.fatalf("importing the working tree into a scratch database: %v", err)
	}
	sameTables(d.t, d.ctx, d.db, mirror, d.stateTables(), "the working tree against the database")
}

// stateTables is every table the export writes on every sync -- the ones
// the repository is expected to be current on.
func (d *soak) stateTables() []string {
	var out []string
	for _, table := range realTables(d.t, d.ctx, d.db) {
		if staterepo.TierOf(table) == staterepo.TierState {
			out = append(out, table)
		}
	}
	return out
}

// dumpIsConsistent checks that the dump in dir does not refer to rows it
// does not have. It is deliberately asymmetric about the churn tier: a
// churn file is up to an hour older than the state files beside it, so a
// run may be missing from a dump whose task is there, and never the
// other way round.
func (d *soak) dumpIsConsistent(dir string) {
	d.t.Helper()
	tasks := map[string]bool{}
	for _, id := range d.tasksIn(dir) {
		tasks[id] = true
	}
	for _, table := range []string{"task_comment", "task_read", "task_grant", "task_run", "task_observation"} {
		path := filepath.Join(dir, staterepo.TablesDir, table+".json")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		for id := range columnValues(d.t, path, "task_id") {
			if !tasks[id] {
				d.fatalf("%s/%s.json names task %q, which %s/task.json does not have",
					staterepo.TablesDir, table, id, staterepo.TablesDir)
			}
		}
	}
}

func (d *soak) tasksIn(dir string) []string {
	d.t.Helper()
	var out []string
	for id := range columnValues(d.t, filepath.Join(dir, staterepo.TablesDir, "task.json"), "id") {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

func (d *soak) taskCount() int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRowContext(d.ctx, "SELECT COUNT(*) FROM `task`").Scan(&n); err != nil {
		d.fatalf("counting tasks: %v", err)
	}
	return n
}

func (d *soak) templateCount() int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRowContext(d.ctx, "SELECT COUNT(*) FROM `template`").Scan(&n); err != nil {
		d.fatalf("counting templates: %v", err)
	}
	return n
}

// -- the end of the run ------------------------------------------------

// finish leaves the deployment in the state a clean shutdown does -- one
// last sync of everything, churn included -- and then asks the question
// the whole soak was building up to: is a clone of what was pushed the
// database that pushed it?
func (d *soak) finish() {
	d.t.Helper()
	if _, err := staterepo.SyncAll(d.ctx, d.repo, d.db, model.SchemaVersion); err != nil {
		d.fatalf("the last sync of the run: %v", err)
	}
	dir := filepath.Join(d.t.TempDir(), "final-restore")
	_, restoredDB := d.openScratchDB()
	repo, err := staterepo.Open(d.ctx, staterepo.Config{Dir: dir, Remote: d.remote, Now: d.clock.now})
	if err != nil {
		d.fatalf("cloning the remote at the end of the run: %v", err)
	}
	if err := staterepo.Load(d.ctx, repo, restoredDB, model.SchemaVersion); err != nil {
		d.fatalf("restoring the remote at the end of the run: %v", err)
	}
	sameDatabase(d.t, d.ctx, d.db, restoredDB,
		"a database restored from the remote at the end of the run")

	// And the same directory has to pass the check a pull request against
	// this repository runs in CI, which is what a reviewer's green tick
	// means.
	_, checkDB := d.openScratchDB()
	report, err := staterepo.Check(d.ctx, checkDB, dir, model.SchemaVersion)
	if err != nil {
		d.fatalf("`grain state check` on what the run pushed: %v", err)
	}
	if len(report.Warnings) != 0 {
		d.fatalf("`grain state check` warned about what grain itself wrote: %v", report.Warnings)
	}

	var summary []string
	for _, what := range sortedKeys(d.counts) {
		summary = append(summary, fmt.Sprintf("%s: %d", what, d.counts[what]))
	}
	d.t.Logf("%d rounds, %d rows restored: %s", d.round, report.Total(), strings.Join(summary, ", "))
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// readTemplateRows and writeTemplateRows edit a dumped table file the way
// a pull request against the repository does: as JSON, preserving every
// column the file has, since a row that lost one would not import.
func readTemplateRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return rows
}

func writeTemplateRows(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
