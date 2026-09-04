package staterepo_test

// The store is gone and the working tree is not.
//
// It is the half of "this host and this repository have got out of step"
// that the loaded-head marker cannot see. That marker lives inside the
// git directory and answers one question -- has the repository moved
// since this host last agreed with it -- so a store file that was
// deleted, or restored from a backup that did not include it, or opened
// at a path that has never held one, leaves the marker saying "we agree,
// import nothing" over a database with nothing in it. The export after
// that used to commit the empty dump and push it, and nothing objected:
// the remote was not ahead, because this host wrote the commit under it.
//
// The paired case has an answer already -- scripts/setup.sh moves the
// working tree aside alongside the store on a schema bump, so the daemon
// re-seeds -- and this is the unpaired one: store gone, tree kept.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// A start with the tree intact and the store lost is a restore, and does
// it by itself. There is nothing in the database to be ahead of the dump,
// so importing all of it -- runs and tasks included, exactly as a fresh
// clone does -- can cost nothing and brings the deployment back.
func TestAStartWithTheStoreLostRestoresFromTheWorkingTree(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	// A deployment that has been running a while: settings somebody
	// configured, and grain's own record of what it did.
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "tpl-1", Title: "Run the nightly sweep", Body: "as configured", CreatedAt: now,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if err := store.PutTask(ctx, task("filed-before-the-store-went")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// The store file is gone and the working tree is untouched, which is
	// the shape the whole case turns on: the marker still names HEAD, so
	// the question the marker answers has the answer "nothing arrived".
	// (openDB is a database in a directory of its own, which is what
	// sqlite.Open handed a fresh path amounts to.)
	head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	marker := strings.TrimSpace(read(t, filepath.Join(dir, ".git", "grain-loaded-head")))
	if marker != head {
		t.Fatalf("this test means to start from a marker that agrees with HEAD; it reads %q against %q",
			marker, head)
	}
	restored, freshDB := openDB(t)
	reopened, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if err := staterepo.Load(ctx, reopened, freshDB, model.SchemaVersion); err != nil {
		t.Fatalf("starting with the store lost: %v", err)
	}

	// The deployment is back: the whole repository, not the settings half
	// of it, because a database with nothing in it is the restore case.
	if got, err := restored.GetTask(ctx, "filed-before-the-store-went"); err != nil || got == nil {
		t.Fatalf("the start did not bring the tasks back: %v %v", got, err)
	}
	if got, err := restored.GetTemplate(ctx, "tpl-1"); err != nil || got == nil {
		t.Fatalf("the start did not bring the settings back: %v %v", got, err)
	}

	// And the sync that follows exports the restored database rather than
	// committing nothing over the deployment. Nothing left in the working
	// tree either: a dump the export rewrote and did not commit is a
	// revert waiting for the next push.
	if _, err := staterepo.SyncAll(ctx, reopened, freshDB, model.SchemaVersion); err != nil {
		t.Fatalf("syncing after the restore: %v", err)
	}
	if status := strings.TrimSpace(git(t, dir, "status", "--porcelain")); status != "" {
		t.Fatalf("the working tree is not clean after the sync:\n%s", status)
	}
	for _, where := range []string{
		read(t, filepath.Join(dir, staterepo.TablesDir, "task.json")),
		git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json"),
	} {
		if !strings.Contains(where, "filed-before-the-store-went") {
			t.Fatalf("the sync after the restore emptied the dump:\n%s", where)
		}
	}
}

// The same start, with a merge waiting on the remote. The import is
// still the whole repository -- the merge arrives with it -- and what
// must not happen is the settings half alone, which would leave a
// database holding templates and no tasks for the export to write out.
func TestAStartWithTheStoreLostTakesTheWholeRepositoryMergeAndAll(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "tpl-1", Title: "Run the nightly sweep", Body: "as configured", CreatedAt: now,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if err := store.PutTask(ctx, task("filed-before-the-store-went")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	mergeATemplateTitle(t, remote, "tpl-1", "Retitled by a pull request")

	restored, freshDB := openDB(t)
	reopened, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if err := staterepo.Load(ctx, reopened, freshDB, model.SchemaVersion); err != nil {
		t.Fatalf("starting with the store lost and a merge waiting: %v", err)
	}
	got, err := restored.GetTemplate(ctx, "tpl-1")
	if err != nil || got == nil {
		t.Fatalf("reading the restored template: %v %v", got, err)
	}
	if got.Title != "Retitled by a pull request" {
		t.Fatalf("the restore did not take up the merged change: the template reads %q", got.Title)
	}
	if task, err := restored.GetTask(ctx, "filed-before-the-store-went"); err != nil || task == nil {
		t.Fatalf("the restore took the settings and left the tasks behind: %v %v", task, err)
	}
}

// A tick, rather than a start: the database went away underneath a
// daemon that is already running, and a merge is waiting. Applying the
// settings out of it is refused, because a database with nothing in it
// wants the whole repository and that is a start's to do -- an import
// that replaces every row cannot run underneath a reconcile loop holding
// the ids. Reported as ErrNotApplied, which is what stops the export in
// cmd/grain's own cycle.
func TestATickWillNotApplyAMergeIntoAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTemplate(ctx, model.Template{
		ID: "tpl-1", Name: "tpl-1", Title: "Run the nightly sweep", Body: "as configured", CreatedAt: now,
	}); err != nil {
		t.Fatalf("putting a template: %v", err)
	}
	if err := store.PutTask(ctx, task("filed-before-the-store-went")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	mergeATemplateTitle(t, remote, "tpl-1", "Retitled by a pull request")

	emptied, freshDB := openDB(t)
	_, err = staterepo.Apply(ctx, repo, freshDB, model.SchemaVersion)
	if !errors.Is(err, staterepo.ErrDatabaseEmpty) {
		t.Fatalf("applying a merge into an empty database: got %v, want an ErrDatabaseEmpty", err)
	}
	if !errors.Is(err, staterepo.ErrNotApplied) {
		t.Fatalf("the refusal has to be an ErrNotApplied, or cmd/grain exports over it anyway: %v", err)
	}
	// Nothing was imported: a database holding the settings and none of
	// the rest is one the export below would think worth writing out.
	if got, err := emptied.GetTemplate(ctx, "tpl-1"); err != nil || got != nil {
		t.Fatalf("the refused apply imported the settings anyway: %v %v", got, err)
	}
	// And the export does not run either, from the other side: asked
	// directly, it refuses for itself.
	if _, err := staterepo.Sync(ctx, repo, freshDB, model.SchemaVersion); !errors.Is(err, staterepo.ErrDatabaseEmpty) {
		t.Fatalf("exporting an empty database: got %v, want an ErrDatabaseEmpty", err)
	}
}

// The export refusal on its own, which is the last thing between a
// database that has gone away and a repository with nothing left in it.
// No commit, no push, and the repository as it was.
func TestASyncRefusesToExportAnEmptyDatabaseOverADeployment(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := store.PutTask(ctx, task("filed-before-the-store-went")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	before := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	// No Load first, deliberately: this is the database going away under
	// a process that is already running, where the restore is not this
	// cycle's to do.
	_, freshDB := openDB(t)
	changed, err := staterepo.SyncAll(ctx, repo, freshDB, model.SchemaVersion)
	if !errors.Is(err, staterepo.ErrDatabaseEmpty) {
		t.Fatalf("exporting an empty database: got %v, want an ErrDatabaseEmpty", err)
	}
	if changed {
		t.Fatal("the refused export reported a commit")
	}
	// The message is what an operator reads in the State pane, so it has
	// to name what stopped it.
	if !strings.Contains(err.Error(), staterepo.TablesDir+"/") {
		t.Errorf("the refusal names no file for an operator to look at: %v", err)
	}
	if after := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")); after != before {
		t.Fatalf("the refused export committed anyway: %s became %s", before, after)
	}
	if status := strings.TrimSpace(git(t, dir, "status", "--porcelain")); status != "" {
		t.Fatalf("the refused export left files behind for the next commit to carry:\n%s", status)
	}
	for _, where := range []string{
		read(t, filepath.Join(dir, staterepo.TablesDir, "task.json")),
		git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json"),
	} {
		if !strings.Contains(where, "filed-before-the-store-went") {
			t.Fatalf("the deployment was emptied out of its own repository:\n%s", where)
		}
	}
}

// The refusal is about the pair and not about the database alone: a
// deployment nobody has configured yet is empty at both ends, and has to
// go on exporting. Getting this wrong would put an error in the State
// pane of every fresh install and stop it writing its first row out.
func TestASyncStillExportsAnEmptyDatabaseIntoAnEmptyRepository(t *testing.T) {
	ctx := context.Background()
	store, db := openDB(t)
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "state")
	repo, err := staterepo.Open(ctx, staterepo.Config{Dir: dir, Remote: remote})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	// A brand new install: the schema is applied and nothing else has
	// happened yet.
	if err := staterepo.Load(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("seeding an empty deployment: %v", err)
	}
	if _, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil {
		t.Fatalf("syncing an empty deployment: %v", err)
	}
	// And the first thing anybody files goes out the ordinary way.
	if err := store.PutTask(ctx, task("the-first-task")); err != nil {
		t.Fatalf("putting: %v", err)
	}
	if changed, err := staterepo.SyncAll(ctx, repo, db, model.SchemaVersion); err != nil || !changed {
		t.Fatalf("syncing the first task of a deployment: %v %v", changed, err)
	}
	if out := git(t, remote, "show", "main:"+staterepo.TablesDir+"/task.json"); !strings.Contains(out, "the-first-task") {
		t.Fatalf("the first task never reached the remote:\n%s", out)
	}
}

// mergeATemplateTitle is a pull request against the repository, merged:
// one template retitled, pushed to the remote for the deployment to pull
// down on its next cycle.
func mergeATemplateTitle(t *testing.T, remote, id, title string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "pull-request")
	git(t, "", "clone", "--quiet", remote, work)
	path := filepath.Join(work, staterepo.TablesDir, "template.json")
	rows := readTemplateRows(t, path)
	found := false
	for _, row := range rows {
		if row["id"] == id {
			row["title"] = title
			found = true
		}
	}
	if !found {
		t.Fatalf("the repository has no template %q to retitle:\n%s", id, read(t, path))
	}
	writeTemplateRows(t, path, rows)
	git(t, work, "add", "--all", ".")
	git(t, work, "-c", "user.email=someone@example.com", "-c", "user.name=someone",
		"commit", "-m", "A merged pull request: a template retitled")
	git(t, work, "push", "--quiet", "origin", "main")
}
