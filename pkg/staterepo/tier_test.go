package staterepo_test

// The two clocks a sync runs on (tier.go), guarded cheaply.
//
// growth_test.go measures what these tests assert: a simulated day of a
// busy deployment cost 2,880 commits and 2.9 GiB of .git before this,
// and 84 commits and 9.1 MiB after. What follows is the same property
// stated small enough to run on every build -- how many commits a day of
// pure churn is worth, that a settings change still lands in seconds,
// and that nothing about either has cost the repository its ability to
// restore a host.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// clock is a hand-wound Config.Now: a day of a 30s sync loop passes in a
// few hundred calls rather than in a day.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) tick()          { c.t = c.t.Add(30 * time.Second) }
func newClock() *clock          { return &clock{t: now} }

// openTieredRepo is a local-only repository on a hand-wound clock -- the
// state every test here starts from.
func openTieredRepo(t *testing.T, c *clock) *staterepo.Repo {
	t.Helper()
	repo, err := staterepo.Open(context.Background(), staterepo.Config{
		Dir: filepath.Join(t.TempDir(), "state"), Now: c.now,
	})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	return repo
}

// A day of nothing but grain watching its own pull requests must not be
// a day of 2,880 commits. task_observation is stamped on every reconcile
// cycle whether or not anything moved, so this is what a busy deployment
// with no settings changes at all actually looks like.
func TestChurnCommitsOnItsOwnClockAndNotOnEveryTick(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	c := newClock()
	repo := openTieredRepo(t, c)
	if err := staterepo.Seed(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Four hours of ticks, each one restamping observed_at exactly as a
	// reconcile cycle does.
	const hours = 4
	commits := 0
	for i := 0; i < hours*120; i++ {
		c.tick()
		at := c.t
		if err := store.ObserveField(ctx, "a1b2", at, func(o *model.Observation) {
			o.PrOpenedAt = &at
		}); err != nil {
			t.Fatalf("observing: %v", err)
		}
		changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
		if err != nil {
			t.Fatalf("syncing: %v", err)
		}
		if changed {
			commits++
		}
	}
	if commits != hours {
		t.Fatalf("%d hours of pure churn produced %d commits, want %d -- one per churn interval",
			hours, commits, hours)
	}

	// And it is genuinely there, not merely uncommitted: the last commit
	// carries the latest observation, not the first.
	obs := read(t, filepath.Join(repo.Dir(), staterepo.TablesDir, "task_observation.json"))
	if !strings.Contains(obs, c.t.UTC().Add(-time.Second).Format("2006-01-02T15:04")) &&
		!strings.Contains(obs, c.t.UTC().Format("2006-01-02T15:04")) {
		t.Fatalf("the exported observation is not from the end of the run:\n%s", obs)
	}
}

// The other half of the trade, and the half the repository exists for: a
// settings change is committed on the very next sync, not on the churn
// clock.
func TestSettingsStillReachTheRepositoryImmediately(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	c := newClock()
	repo := openTieredRepo(t, c)
	if err := staterepo.Seed(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	c.tick()
	if err := store.PutRepoConfig(ctx, model.RepoConfig{
		Repo:            model.RepoRef{Owner: "owner", Name: "payments-api"},
		PromptExtension: "always run make test",
	}); err != nil {
		t.Fatalf("configuring a repo: %v", err)
	}
	changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
	if err != nil {
		t.Fatalf("syncing: %v", err)
	}
	if !changed {
		t.Fatal("a settings change did not commit on the next sync")
	}
	got := read(t, filepath.Join(repo.Dir(), staterepo.TablesDir, "repo_config.json"))
	if !strings.Contains(got, "always run make test") {
		t.Fatalf("the settings change is not in the dump:\n%s", got)
	}
}

// The property bwsalmon/grain#174 rests on, restated across a churn
// boundary: an unchanged database produces byte-identical files, so a
// sync with nothing to say commits nothing -- including the sync where
// the churn tier came due and was written out again to the same bytes.
func TestAnUnchangedDatabaseCommitsNothingAcrossAChurnBoundary(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	c := newClock()
	repo := openTieredRepo(t, c)
	if err := staterepo.Seed(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	head, _ := repo.Head(ctx)

	for i := 0; i < 3*120; i++ {
		c.tick()
		changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
		if err != nil {
			t.Fatalf("syncing: %v", err)
		}
		if changed {
			t.Fatalf("a sync with nothing to say committed, at tick %d", i)
		}
	}
	if after, _ := repo.Head(ctx); after != head {
		t.Fatalf("HEAD moved without anything changing: %s -> %s", head, after)
	}
}

// SyncAll is what a human pressing Sync, or a daemon shutting down,
// means: everything now, whether or not the churn clock agrees.
func TestSyncAllWritesChurnOutImmediately(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	c := newClock()
	repo := openTieredRepo(t, c)
	if err := staterepo.Seed(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	c.tick()
	if err := store.StartRun(ctx, model.Run{
		ID: "a1b2-1", TaskID: "a1b2", Sandbox: "grain-a1b2", Attempt: 1, StartedAt: c.t,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err != nil || changed {
		t.Fatalf("an ordinary sync wrote churn out early: changed=%v err=%v", changed, err)
	}
	changed, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion)
	if err != nil {
		t.Fatalf("syncing all: %v", err)
	}
	if !changed {
		t.Fatal("SyncAll committed nothing with a run waiting to be written out")
	}
	if got := read(t, filepath.Join(repo.Dir(), staterepo.TablesDir, "task_run.json")); !strings.Contains(got, "a1b2-1") {
		t.Fatalf("the run is not in the dump:\n%s", got)
	}
}

// A merged pull request against the repository must not roll the
// database's own record of what grain did back to whenever churn was
// last exported. The merge is about settings; the database is ahead on
// runs by up to a churn interval, and that is by design.
func TestAMergedChangeDoesNotRollBackRuns(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	c := newClock()
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote, Now: c.now})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "nightly", Title: "Run the nightly sweep", CreatedAt: now,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}

	// A run happens after the last churn export -- the ordinary case, for
	// the 59 minutes of every hour when one has not just run.
	c.tick()
	if err := store.StartRun(ctx, model.Run{
		ID: "a1b2-1", TaskID: "a1b2", Sandbox: "grain-a1b2", Attempt: 1, StartedAt: c.t,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := store.FinishRun(ctx, "a1b2-1", c.t, "succeeded", "opened a pull request"); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	// Meanwhile a pull request retitling the template is merged and pushed.
	mergeIntoRemote(t, remote, "template.json", "Run the nightly sweep", "Retitled by a pull request")

	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading after the merge: %v", err)
	}
	tpl, err := store.GetTemplate(ctx, "tpl-1")
	if err != nil || tpl == nil {
		t.Fatalf("reading the template: %v %v", tpl, err)
	}
	if tpl.Title != "Retitled by a pull request" {
		t.Fatalf("the merged change did not reach the database: %q", tpl.Title)
	}
	runs, err := store.Runs(ctx, "a1b2")
	if err != nil {
		t.Fatalf("reading the runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("the import rolled the run back: %d runs, want 1", len(runs))
	}
}

// And the case that pays for all of it: a clone onto a machine that has
// never seen this repository imports everything, churn included, because
// there the repository is the only copy there is.
func TestACloneOntoANewHostRestoresRunsToo(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	c := newClock()
	repo, err := staterepo.Open(ctx, staterepo.Config{
		Dir: filepath.Join(t.TempDir(), "state"), Remote: remote, Now: c.now,
	})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "a1b2-1", TaskID: "a1b2", Sandbox: "grain-a1b2", Attempt: 1, StartedAt: c.t,
	}, model.Limits{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := store.FinishRun(ctx, "a1b2-1", c.t, "succeeded", "opened a pull request"); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// A second host, an empty database, and nothing but the remote.
	otherStore, otherDB := openDB(t)
	other, err := staterepo.Open(ctx, staterepo.Config{
		Dir: filepath.Join(t.TempDir(), "restored"), Remote: remote, Now: c.now,
	})
	if err != nil {
		t.Fatalf("cloning: %v", err)
	}
	if err := staterepo.Load(ctx, other, otherDB, model.SchemaVersion); err != nil {
		t.Fatalf("loading the clone: %v", err)
	}
	runs, err := otherStore.Runs(ctx, "a1b2")
	if err != nil {
		t.Fatalf("reading the restored runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("the restore lost the run history: %d runs, want 1", len(runs))
	}
}
