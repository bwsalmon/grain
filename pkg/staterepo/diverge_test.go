package staterepo_test

// Divergence, against a real bare remote and a real second clone: a push
// that failed, a pull request merged before it could be retried, and what
// the deployment does about it on its next sync.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// diverge produces the real thing rather than a hand-built history: an
// export grain committed and could not push -- the remote is pointed at a
// directory that is not there for exactly as long as the push takes --
// followed by somebody else's commit on the remote, which is what a
// merged pull request against the state repository leaves behind.
//
// It returns the two sides so a test can name them: nothing here asserts,
// so a test that wants a divergence somebody else made can build the
// local half itself.
func diverge(t *testing.T, ctx context.Context, repo *staterepo.Repo, remote, dir string, local func()) {
	t.Helper()
	unreachable := filepath.Join(t.TempDir(), "gone.git")
	git(t, dir, "remote", "set-url", "origin", unreachable)
	local()
	git(t, dir, "remote", "set-url", "origin", remote)

	// And the merge that lands in the meantime, pushed from a clone that
	// was made before any of it.
	work := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, "NOTES.md"), []byte("merged by somebody\n"), 0o644); err != nil {
		t.Fatalf("writing the merged file: %v", err)
	}
	git(t, work, "add", "--all", ".")
	git(t, work, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-m", "A merged pull request")
	git(t, work, "push", "--quiet", "origin", "main")
}

// The failure this exists for, end to end: grain's own export is stranded
// on this host, a merge is stranded on the remote, and neither moves
// again until the divergence is cleared. Both directions have to recover
// in the one cycle -- what was merged arrives here, and what only this
// host had reaches the remote.
func TestADivergenceMadeOfGrainsOwnExportsRecovers(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}

	diverge(t, ctx, repo, remote, dir, func() {
		// A row changes and the timer comes round, exactly as it does on
		// any other tick. The commit is made; the push is what fails.
		if err := store.PutTask(ctx, task("a1b2")); err != nil {
			t.Fatalf("putting: %v", err)
		}
		if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err == nil {
			t.Fatal("the push against an unreachable remote reported success")
		}
	})

	// Where the deployment is now, and where it stayed forever before
	// this: a pull that refuses, tick after tick.
	if _, err := staterepo.Apply(ctx, repo, db, model.SchemaVersion); !errors.Is(err, staterepo.ErrDiverged) {
		t.Fatalf("applying over a divergence: got %v, want an ErrDiverged", err)
	}

	recovered, err := repo.RecoverDiverged(ctx)
	if err != nil || !recovered {
		t.Fatalf("recovering a divergence made only of grain's own exports: %v %v", recovered, err)
	}

	// Inbound: the merged commit is in the working tree, and the history
	// is the remote's own rather than a merge of the two.
	if _, err := os.Stat(filepath.Join(dir, "NOTES.md")); err != nil {
		t.Fatalf("the merged change never arrived: %v", err)
	}
	if _, err := staterepo.Apply(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("applying after the recovery: %v", err)
	}

	// Outbound: the task that only ever existed in the reset-away commit
	// is written back out of the database and reaches the remote.
	if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing after the recovery: %v", err)
	}
	out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json")
	if !strings.Contains(out, "a1b2") {
		t.Fatalf("the export never reached the remote:\n%s", out)
	}
	// And what was merged is still there, rather than reverted by the
	// export that followed it.
	if got := git(t, remote, "show", "main:NOTES.md"); !strings.Contains(got, "merged by somebody") {
		t.Fatalf("the export committed over the merge:\n%s", got)
	}
}

// The same recovery, reached from a start rather than from a tick, which
// is the path cmd/grain/daemon.go takes: Load, and if that is a
// divergence grain made itself, recover and Load again.
//
// The rows written while the pushes were failing exist only in the
// database -- the commits that held them are exactly what the recovery
// threw away, on the argument that the database still has them -- so
// this is the one place that argument can be falsified. It was:
// Load read "HEAD is not the commit we recorded" as "a merge arrived"
// and replaced the whole state tier from a dump older than the database
// by however long the remote had been unreachable, deleting every task
// filed in that window.
func TestAStartThatRecoversADivergenceKeepsWhatTheDatabaseHas(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "nightly", Title: "Run the nightly sweep", CreatedAt: now,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing: %v", err)
	}

	diverge(t, ctx, repo, remote, dir, func() {
		// Two ticks' worth of work that got as far as a commit and no
		// further, which is what a remote that goes away for a few minutes
		// leaves behind.
		for _, id := range []string{"filed-first", "filed-second"} {
			if err := store.PutTask(ctx, task(id)); err != nil {
				t.Fatalf("putting: %v", err)
			}
			if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err == nil {
				t.Fatal("the push against an unreachable remote reported success")
			}
		}
	})

	// The process restarts, and takes the daemon's own path.
	reopened, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if err := staterepo.Load(ctx, reopened, db, model.SchemaVersion); err != nil {
		if !errors.Is(err, staterepo.ErrDiverged) {
			t.Fatalf("loading over a divergence: got %v, want an ErrDiverged", err)
		}
		recovered, rerr := reopened.RecoverDiverged(ctx)
		if rerr != nil || !recovered {
			t.Fatalf("recovering a divergence made only of grain's own exports: %v %v", recovered, rerr)
		}
		if err := staterepo.Load(ctx, reopened, db, model.SchemaVersion); err != nil {
			t.Fatalf("loading after the recovery: %v", err)
		}
	}

	for _, id := range []string{"filed-first", "filed-second"} {
		got, err := store.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got == nil {
			t.Fatalf("%s was written while the push was failing and the start that "+
				"recovered the divergence rolled it back", id)
		}
	}
	// What was merged is live all the same -- the point of recovering at
	// all is that both directions move again.
	if _, err := os.Stat(filepath.Join(dir, "NOTES.md")); err != nil {
		t.Fatalf("the merged change never arrived: %v", err)
	}
	// And the export that follows puts the database back on the remote,
	// on top of the merge.
	if _, err := staterepo.Sync(ctx, reopened, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing after the recovery: %v", err)
	}
	out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json")
	for _, id := range []string{"filed-first", "filed-second"} {
		if !strings.Contains(out, id) {
			t.Fatalf("%s never reached the remote:\n%s", id, out)
		}
	}
}

// A merge that arrives the ordinary way -- fast-forwarded, no divergence
// -- still replaces the state tier at a start, which is what makes a
// merged change to anything but the settings take effect at all. The
// marker that makes the recovery above import less must not leak into
// this case.
func TestAnOrdinaryMergeStillLoadsWholeAtAStart(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	// Somebody edits the dump and merges it, with nothing stranded on
	// this side.
	work := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", "--quiet", remote, work)
	path := filepath.Join(work, staterepo.TablesDir, "task.json")
	edited := strings.Replace(read(t, path), "Rename the endpoint", "Retitled by a pull request", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("editing the dump: %v", err)
	}
	git(t, work, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-am", "Retitle")
	git(t, work, "push", "--quiet", "origin", "main")

	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading after the merge: %v", err)
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("reading the task: %v %v", got, err)
	}
	if got.Title != "Retitled by a pull request" {
		t.Fatalf("the merged change did not reach the database: %q", got.Title)
	}
}

// The other half of the rule: a commit that is not grain's own export is
// never reset over, however inconvenient the divergence is. A hand edit
// in the working tree exists nowhere else -- no export will write it back
// -- so this stays a refusal with the commit named, and the deployment
// waits for a human.
func TestADivergenceSomebodyElseMadeIsRefused(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}

	diverge(t, ctx, repo, remote, dir, func() {
		if err := os.WriteFile(filepath.Join(dir, "HAND-EDIT.md"), []byte("by a human\n"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
		git(t, dir, "add", "--all", ".")
		git(t, dir, "-c", "user.email=someone@example.com", "-c", "user.name=someone",
			"commit", "-m", "Something a person did on the host")
	})

	before := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	recovered, err := repo.RecoverDiverged(ctx)
	if recovered {
		t.Fatal("grain reset over a commit it did not write")
	}
	if !errors.Is(err, staterepo.ErrDiverged) {
		t.Fatalf("refusing somebody else's commit: got %v, want an ErrDiverged", err)
	}
	if !strings.Contains(err.Error(), "someone") {
		t.Fatalf("the refusal does not say who wrote the commit in the way: %v", err)
	}
	if after := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")); after != before {
		t.Fatalf("the working tree moved anyway: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "HAND-EDIT.md")); err != nil {
		t.Fatalf("the commit grain refused to reset over was reset over anyway: %v", err)
	}
}

// A deployment that is merely ahead of its remote -- which is every
// deployment between its commit and its push -- has not diverged, and
// must not have its own unpushed export thrown away by anything that
// mistakes the two.
func TestBeingAheadOfTheRemoteIsNotADivergence(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	unreachable := filepath.Join(t.TempDir(), "gone.git")
	git(t, dir, "remote", "set-url", "origin", unreachable)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err == nil {
		t.Fatal("the push against an unreachable remote reported success")
	}
	git(t, dir, "remote", "set-url", "origin", remote)

	recovered, err := repo.RecoverDiverged(ctx)
	if recovered || err != nil {
		t.Fatalf("an unpushed export was treated as a divergence: %v %v", recovered, err)
	}
	// The commit is still here to be pushed, and the next sync that has
	// anything to say pushes it along with its own.
	if err := store.PutTask(ctx, task("c3d4")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if _, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing after the remote came back: %v", err)
	}
	if out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json"); !strings.Contains(out, "a1b2") {
		t.Fatalf("the unpushed export never reached the remote:\n%s", out)
	}
}
