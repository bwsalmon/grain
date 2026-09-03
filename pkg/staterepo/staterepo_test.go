package staterepo_test

// The state repository against real git and a real embedded SQLite
// database. Nothing here is mocked: the remote is a bare repository in a
// temporary directory, which is what a push and a fetch actually talk
// to, and the database is the model's own schema, which is what an
// export has to be able to carry.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

var now = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T) (*model.Store, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, db
}

func task(id string) model.Task {
	return model.Task{
		ID: id, Intent: model.IntentImplement, Title: "Rename the endpoint",
		Body: "with a body\nover two lines",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}},
			Reason:      model.ReasonDirect,
		},
		Target:    &model.RepoRef{Owner: "owner", Name: "payments-api"},
		Binding:   model.BindingDirective,
		CreatedAt: &now,
	}
}

func bareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", dir).CombinedOutput(); err != nil {
		t.Fatalf("creating a bare remote: %v: %s", err, out)
	}
	return dir
}

func TestExportRoundTripsThroughFiles(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	want := task("a1b2")
	if err := store.PutTask(ctx, want); err != nil {
		t.Fatalf("putting a task: %v", err)
	}
	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	// A second, empty database imports the files and has to end up with
	// the same task -- which is the whole claim this package makes: the
	// files, not the database file, are the state.
	other, otherDB := openDB(t)
	if err := staterepo.Import(ctx, otherDB, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	got, err := other.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatalf("reading the imported task: %v", err)
	}
	if got == nil {
		t.Fatal("the task did not survive the round trip")
	}
	if got.Title != want.Title || got.Body != want.Body {
		t.Fatalf("round trip changed the task: got %q/%q", got.Title, got.Body)
	}
	if got.Target == nil || got.Target.Name != "payments-api" {
		t.Fatalf("round trip lost the target: %+v", got.Target)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(now) {
		t.Fatalf("round trip changed CreatedAt: %v, want %v", got.CreatedAt, now)
	}
}

func TestExportIsByteStable(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	for _, id := range []string{"c3", "a1", "b2"} {
		if err := store.PutTask(ctx, task(id)); err != nil {
			t.Fatalf("putting %s: %v", id, err)
		}
	}
	first, second := t.TempDir(), t.TempDir()
	if err := staterepo.Export(ctx, db, first); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	if err := staterepo.Export(ctx, db, second); err != nil {
		t.Fatalf("exporting again: %v", err)
	}
	// Byte-identical, not merely equivalent: an export that reordered
	// rows would commit and push on every sync cycle forever.
	a := read(t, filepath.Join(first, staterepo.TablesDir, "task.json"))
	b := read(t, filepath.Join(second, staterepo.TablesDir, "task.json"))
	if a != b {
		t.Fatalf("two exports of one database differ:\n%s\n---\n%s", a, b)
	}
	if !strings.Contains(a, `"id": "a1"`) {
		t.Fatalf("the dump does not name its rows readably:\n%s", a)
	}
	// Sorted by primary key, so a reviewer reads a stable file.
	if strings.Index(a, `"a1"`) > strings.Index(a, `"b2"`) {
		t.Fatalf("rows are not sorted by primary key:\n%s", a)
	}
}

func TestImportReplacesWhatIsThere(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("keep")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	dir := t.TempDir()
	if err := staterepo.Export(ctx, db, dir); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	// A row added after the export is one the dump does not have, and an
	// import is a replacement, not a merge -- that is how a merged pull
	// request deleting a template actually deletes it.
	if err := store.PutTask(ctx, task("added-later")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Import(ctx, db, dir); err != nil {
		t.Fatalf("importing: %v", err)
	}
	if got, err := store.GetTask(ctx, "added-later"); err != nil {
		t.Fatalf("reading: %v", err)
	} else if got != nil {
		t.Fatal("import left behind a row the dump does not have")
	}
	if got, err := store.GetTask(ctx, "keep"); err != nil || got == nil {
		t.Fatalf("import lost a row the dump does have: %v %v", got, err)
	}
}

func TestLocalOnlyRepositoryNeedsNoRemote(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("opening a local-only repository: %v", err)
	}
	if repo.Remote() != "" {
		t.Fatalf("a local-only repository grew a remote: %q", repo.Remote())
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if !staterepo.HasDump(dir) {
		t.Fatal("nothing was written to the repository")
	}
	log := git(t, dir, "log", "--oneline")
	if strings.TrimSpace(log) == "" {
		t.Fatal("nothing was committed")
	}
	// Sync with nothing changed must not produce a commit, or a deployment
	// pushes noise forever.
	changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
	if err != nil {
		t.Fatalf("syncing an unchanged database: %v", err)
	}
	if changed {
		t.Fatal("an unchanged database produced a commit")
	}
}

func TestSyncPushesToTheRemote(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening against a remote: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	changed, err := staterepo.Sync(ctx, repo, db, model.SchemaVersion)
	if err != nil {
		t.Fatalf("syncing: %v", err)
	}
	if !changed {
		t.Fatal("a new task produced no commit")
	}
	// Read it back out of the remote itself rather than trusting the push
	// exited zero.
	out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json")
	if !strings.Contains(out, "a1b2") {
		t.Fatalf("the remote does not hold the task:\n%s", out)
	}
}

func TestPullBringsBackAMergedChange(t *testing.T) {
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

	// Somebody else -- an agent's merged pull request, in the real thing --
	// edits the dump and pushes it.
	work := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", "--quiet", remote, work)
	path := filepath.Join(work, staterepo.TablesDir, "task.json")
	edited := strings.Replace(read(t, path), "Rename the endpoint", "Retitled by a pull request", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("editing the dump: %v", err)
	}
	git(t, work, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-am", "Retitle")
	git(t, work, "push", "--quiet", "origin", "main")

	// grain pulls, and the merged file is what its database now says.
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

func TestAdoptingAnEmptyRemoteSeedsIt(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	// The bootstrap's "start from scratch" path: a repository that exists
	// on GitHub and has nothing in it at all.
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("cloning an empty remote: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	out := git(t, remote, "show", "main:"+staterepo.SchemaVersionFile)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("the empty remote was not seeded")
	}
}

func TestSetRemoteConvertsALocalRepository(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	if err := store.PutTask(ctx, task("a1b2")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	// The bootstrap's "I ran locally first and now want a remote" path.
	remote := bareRemote(t)
	if err := repo.SetRemote(ctx, remote); err != nil {
		t.Fatalf("setting a remote: %v", err)
	}
	if err := repo.Push(ctx); err != nil {
		t.Fatalf("pushing: %v", err)
	}
	if out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json"); !strings.Contains(out, "a1b2") {
		t.Fatalf("the history did not reach the newly attached remote:\n%s", out)
	}
}

// A restart must not undo what the process before it did in the seconds
// after its last export. The export runs on a timer, so the database is
// routinely ahead of the repository, and a Load that imported
// unconditionally would roll those writes back -- which is exactly what a
// container stopped shortly after a task was filed looks like, and what
// tests/container's own "a task survives the container that created it"
// caught.
func TestARestartDoesNotImportOverANewerDatabase(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	// First start: an empty database, exported as an empty dump.
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("first load: %v", err)
	}
	// A task is filed, and the process dies before the next sync tick.
	if err := store.PutTask(ctx, task("filed-just-before-the-stop")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	// Second start, same data directory, same repository.
	reopened, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if err := staterepo.Load(ctx, reopened, db, model.SchemaVersion); err != nil {
		t.Fatalf("second load: %v", err)
	}
	got, err := store.GetTask(ctx, "filed-just-before-the-stop")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got == nil {
		t.Fatal("restarting imported a stale dump over the database and lost the task")
	}
	// And the next sync writes it out, so the repository catches up rather
	// than staying behind forever.
	if _, err := staterepo.Sync(ctx, reopened, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing: %v", err)
	}
	if !strings.Contains(read(t, filepath.Join(dir, staterepo.TablesDir, "task.json")), "filed-just-before-the-stop") {
		t.Fatal("the task never reached the repository")
	}
}

// The other direction of the same decision: a working tree cloned onto a
// host that has never loaded it is a restore, and must import.
func TestACloneOntoANewHostImports(t *testing.T) {
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

	// Somewhere else entirely: a fresh machine, an empty database, a
	// clone of that repository.
	otherStore, otherDB := openDB(t)
	otherDir := filepath.Join(t.TempDir(), "state")
	other, err := staterepo.Open(ctx, staterepo.Config{Dir: otherDir, Remote: remote})
	if err != nil {
		t.Fatalf("cloning: %v", err)
	}
	if err := staterepo.Load(ctx, other, otherDB, model.SchemaVersion); err != nil {
		t.Fatalf("loading the clone: %v", err)
	}
	if got, err := otherStore.GetTask(ctx, "a1b2"); err != nil || got == nil {
		t.Fatalf("a clone onto a new host did not restore the database: %v %v", got, err)
	}
}

func TestARepositoryFromANewerBuildIsRefused(t *testing.T) {
	ctx := context.Background()
	_, db := openDB(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := staterepo.WriteSchemaVersion(dir, model.SchemaVersion+1); err != nil {
		t.Fatalf("stamping: %v", err)
	}
	err = staterepo.Load(ctx, repo, db, model.SchemaVersion)
	if err == nil {
		t.Fatal("a repository from a newer build was imported anyway")
	}
	if !strings.Contains(err.Error(), "newer build") {
		t.Fatalf("the error does not say what is wrong: %v", err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
