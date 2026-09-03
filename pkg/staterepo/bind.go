package staterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrSchemaTooNew is returned when the repository was written by a build
// that knows a later schema than this one. It is pkg/model's own
// ErrSchemaTooNew argument, one layer out: importing rows shaped for a
// schema this build does not have produces a database that is wrong
// rather than one that refuses to start.
var ErrSchemaTooNew = errors.New("state repository was written by a newer build of grain")

// ErrSchemaTooOld is returned when the repository holds a dump from an
// earlier schema. Nothing here migrates one: a state repository is a
// dump, not a database, and the supported move is to wipe it and let a
// fresh Seed write today's schema (README.md's own note on the state
// repository being destructive across schema changes).
var ErrSchemaTooOld = errors.New("state repository was written by an older build of grain")

// ErrNotApplied marks a pull that arrived and could not be made live:
// the working tree holds commits the database has not taken up. It is
// returned wrapped around whatever the actual failure was, so a caller
// can tell that case apart from a fetch that never got off the ground --
// which matters, because exporting the database over a working tree in
// that state commits a revert of somebody's merged pull request and
// pushes it. The daemon's own sync cycle stops there rather than doing
// that (cmd/grain/statemanager.go).
var ErrNotApplied = errors.New("state repository holds changes grain could not apply")

// SettingsTables names the tables Apply imports into a running daemon.
//
// The list is the whole of the "which rows may change underneath a live
// deployment" decision, so it is here, once, rather than implied by a
// filter somewhere. Two properties put a table on it. It has to be
// something a human or an agent proposes a change to -- a template, a
// suite, a schedule, a repo's configuration, the deployment's own
// config row -- because a table nothing proposes a change to has
// nothing to apply. And it has to be something the daemon does not
// write for itself while it runs, because Import is a replacement:
// clearing and refilling a table the reconcile loop is inserting into
// would delete rows written since the dump was made.
//
// That second property is what keeps task, task_run, lease, the metrics
// and the release and qualification *runs* off the list. Those are
// grain's own record of what it did, the database is authoritative for
// them, and they stay with the startup import (Load), which happens
// before anything is live. A merged change to one of them is therefore
// not applied and is written back out by the next export -- see the
// README's own note on the direction of travel.
var SettingsTables = []string{
	// The deployment's own configuration row, prompt extension included.
	"grain_config",
	// Per-repo configuration: default capabilities, prompt extension,
	// setup command.
	"repo_config",
	// A repo's qualification plan -- the config and its items, not the
	// runs the reconciler creates from them.
	"qualification_config",
	"qualification_item",
	"qualification_item_depends_on",
	// Templates, with the repos they read and the capabilities they
	// grant.
	"task_template",
	"task_template_read",
	"task_template_grant",
	"task_template_sequence",
	// Suites and the templates they run, but not task_suite_run and its
	// own tables: a suite is settings, a run of one is not.
	"task_suite",
	"task_suite_item",
	"task_suite_sequence",
	// Schedules, likewise with their reads and grants. next_run_at and
	// last_run_at live on this row and are written by the firing loop,
	// which is the one place a settings table is also grain's own record
	// -- a schedule imported here can therefore lose a firing time to
	// whatever the dump had. That is the same answer a restart gives
	// today, and it costs at most one early or late firing; the
	// alternative is not being able to change a schedule at all without
	// one.
	"schedule",
	"schedule_read",
	"schedule_grant",
	"schedule_sequence",
}

// Apply pulls, and makes what arrived live in a database that is already
// running: a settings change an agent got merged takes effect on the
// next tick rather than on the next restart (bwsalmon/grain#184).
//
// Only SettingsTables are imported, and that restriction is the whole
// design. Load's whole-database Import is exactly what a merged deletion
// needs and exactly what cannot run here: it clears task and task_run
// along with everything else, underneath a reconcile loop holding the
// ids it would delete. Within the settings tables the replacement is the
// same one, so a merge that deleted a template deletes it here too.
//
// The commit is recorded as loaded even though only part of it was
// imported. That is deliberate: the alternative is a restart re-importing
// the whole of a commit whose settings are already live, which would
// throw away every task filed since it arrived. What the database holds
// for its own tables wins, and the next export writes it back out.
//
// What it imports is decided by the same marker Load reads -- the commit
// this host last loaded or wrote -- rather than by whether this
// particular Pull brought something down. The two differ exactly when it
// matters: an import that failed on one tick (the database was busy, a
// merged row would not insert) has left the working tree at a commit
// nothing has taken up, and the next tick's Pull, having nothing further
// to fetch, would report no news and let the export write over it.
// Against the marker, that tick tries the import again.
//
// Reports whether anything was imported. Nothing to apply is
// (false, nil), which is the ordinary case on almost every tick.
func Apply(ctx context.Context, r *Repo, db *sql.DB, version int) (bool, error) {
	if _, err := r.Pull(ctx); err != nil {
		return false, err
	}
	head, err := r.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	marker, err := r.loadedHead(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	// head == marker is the ordinary tick: the repository is exactly
	// where this host left it. An empty head is a repository with no
	// commits yet, and an empty marker is a working tree Load has not
	// decided about -- neither is Apply's to import, and Load is where
	// both are answered.
	if head == "" || marker == "" || marker == head {
		return false, nil
	}
	// A repository with no dump in it has nothing to say about the
	// database -- an initial commit holding only a README, say. Left
	// unrecorded on purpose: the next Sync exports over it and records
	// the commit it makes, which is the same seeding path a fresh
	// repository already takes.
	if !HasDump(r.Dir()) {
		return false, nil
	}
	found, err := ReadSchemaVersion(r.Dir())
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	switch {
	case found > version:
		return false, fmt.Errorf("%w: %w: repository is at schema %d, this build knows %d",
			ErrNotApplied, ErrSchemaTooNew, found, version)
	case found < version:
		return false, fmt.Errorf("%w: %w: repository is at schema %d, this build knows %d",
			ErrNotApplied, ErrSchemaTooOld, found, version)
	}
	if err := ImportTables(ctx, db, r.Dir(), SettingsTables); err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	if err := r.setLoadedHead(ctx, head); err != nil {
		return true, err
	}
	return true, nil
}

// Load makes the repository and the database agree, in whichever
// direction has something to say.
//
// Which direction that is, is the whole of this function, and getting it
// wrong loses data. The repository is the source of truth for changes
// made *to the repository* -- a merged pull request against a template,
// a commit someone pushed while grain was down -- and importing one is
// how those take effect. But the database is the source of truth for
// everything grain itself did, and it is routinely ahead: the export
// runs on a timer, so every write since the last tick exists only in the
// database. A process that imported unconditionally at start would
// therefore undo whatever the previous one did in its last few seconds,
// which is exactly what a container stopped shortly after a task was
// filed looks like.
//
// So the question is not "does the repository hold a dump" but "has the
// repository moved since this host last agreed with it". The commit grain
// last loaded or wrote is recorded beside the repository (loadedHead),
// inside the git directory so it is never committed and never travels to
// the remote:
//
//   - no dump at all: seed the repository from the database. A brand new
//     install, or a running grain pointed at a fresh empty repository.
//   - no marker: this working tree was cloned onto a host that has never
//     loaded it. Import -- which is what makes "clone the repository onto
//     a new machine" a complete restore.
//   - HEAD is not the commit we recorded: something arrived. Import.
//   - otherwise: the repository is exactly where we left it, so the
//     database is authoritative and nothing is imported. The next sync
//     exports whatever has happened since.
func Load(ctx context.Context, r *Repo, db *sql.DB, version int) error {
	if _, err := r.Pull(ctx); err != nil {
		return err
	}
	if !HasDump(r.Dir()) {
		return Seed(ctx, r, db, version)
	}
	// Checked before anything else, and whether or not this ends up
	// importing: a dump at a schema this build does not know is a
	// deployment that must stop and say so, not one that quietly exports
	// today's schema over yesterday's rows -- or, in the other direction,
	// over a newer build's.
	found, err := ReadSchemaVersion(r.Dir())
	if err != nil {
		return err
	}
	switch {
	case found > version:
		return fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooNew, found, version)
	case found < version:
		return fmt.Errorf("%w: repository is at schema %d, this build knows %d", ErrSchemaTooOld, found, version)
	}
	head, err := r.Head(ctx)
	if err != nil {
		return err
	}
	marker, err := r.loadedHead(ctx)
	if err != nil {
		return err
	}
	if marker != "" && marker == head {
		return nil
	}
	if err := Import(ctx, db, r.Dir()); err != nil {
		return err
	}
	return r.setLoadedHead(ctx, head)
}

// Seed writes the database out as the repository's first content: the
// dump, the schema stamp, and the README that tells whoever opens the
// repository next what they are looking at.
func Seed(ctx context.Context, r *Repo, db *sql.DB, version int) error {
	if err := writeReadme(r.Dir()); err != nil {
		return err
	}
	if err := Export(ctx, db, r.Dir()); err != nil {
		return err
	}
	if err := WriteSchemaVersion(r.Dir(), version); err != nil {
		return err
	}
	if _, err := r.Commit(ctx, "Initialise grain's state repository\n\n"+
		"grain exports its database here as JSON, one file per table, so its\n"+
		"settings can be read and changed through pull requests like anything\n"+
		"else. See README.md."); err != nil {
		return err
	}
	if err := r.recordLoadedHead(ctx); err != nil {
		return err
	}
	return r.Push(ctx)
}

// Sync writes the database out and commits and pushes anything that
// changed, reporting whether there was anything to commit.
//
// The commit message is deliberately plain and identical every time.
// What changed is in the diff, which is the whole reason the dump is
// text; a message that tried to summarise it would be a second, worse
// answer that could disagree with the first.
func Sync(ctx context.Context, r *Repo, db *sql.DB, version int) (bool, error) {
	// Rewritten on every sync, not only at Seed: a repository an operator
	// adopted, or one whose README a merge dropped, still has to explain
	// itself to whoever opens it next, and this is the cheapest place to
	// make that true rather than a thing that is true only if the
	// repository was created here.
	if err := writeReadme(r.Dir()); err != nil {
		return false, err
	}
	if err := Export(ctx, db, r.Dir()); err != nil {
		return false, err
	}
	if err := WriteSchemaVersion(r.Dir(), version); err != nil {
		return false, err
	}
	changed, err := r.Commit(ctx, "Update grain state\n\n"+
		"Written by grain from its own database at "+time.Now().UTC().Format(time.RFC3339)+".")
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	// Recorded before the push, not after: the commit exists either way,
	// and a push that fails (an expired credential, an unreachable
	// remote) must not leave the next start believing the repository has
	// moved on without it and importing its own stale dump back over a
	// database that is ahead.
	if err := r.recordLoadedHead(ctx); err != nil {
		return true, err
	}
	return true, r.Push(ctx)
}

func writeReadme(dir string) error {
	return writeFileIfChanged(filepath.Join(dir, ReadmeFile), []byte(readme))
}

const readme = `# grain state

This repository is grain's database, written out as text.

grain runs against an embedded SQLite database, and exports every table
here after each change: ` + "`" + TablesDir + `/<table>.json` + "`" + `, one file per table,
rows sorted by primary key and columns in the table's declared order so
that an unchanged database always produces byte-identical files.

## What is here

- ` + "`" + TablesDir + `/` + "`" + ` -- the database. Settings (templates, suites, repo
  configuration, prompt extensions, schedules), tasks, runs and metrics.
- ` + "`" + SchemaVersionFile + "`" + ` -- the schema the dump was written by. A grain
  that knows a different one refuses to import rather than guessing.
- ` + "`" + SecretsFile + "`" + ` -- every secret grain holds, encrypted to a public key
  whose private half only the operator has. Nothing else in this
  repository is encrypted, and nothing but secrets goes in this file.

## Changing something

Open a pull request against this repository the way you would against
any other. On merge, grain pulls -- within half a minute, without a
restart -- and the merged files replace what is in its database, so a
deleted row is a deleted row.

That applies live to the settings tables: ` + "`" + `grain_config` + "`" + `,
` + "`" + `repo_config` + "`" + `, the ` + "`" + `task_template` + "`" + `, ` + "`" + `task_suite` + "`" + `, ` + "`" + `schedule` + "`" + ` and
` + "`" + `qualification` + "`" + ` tables. The rest -- tasks, runs, metrics: grain's own
record of what it did -- is replaced only when grain starts, because
replacing it underneath a run that is in flight would delete the very
rows that run is working on. Editing one of those here while grain is
running does nothing; the next export writes the database's version back
over your change.

Do not hand-edit while grain is running unless you mean it: grain is the
only writer, exports on a timer, and a local edit it did not make is
overwritten by the next export.

## Secrets

` + "`" + SecretsFile + "`" + ` is an encrypted blob. Agents never read it: a run gets a
secret only through the secret input a human asked for, exactly as
before. Losing the private key means losing every secret in here -- the
file cannot be recovered from grain, which holds no copy of the key
beyond the one file the operator manages.
`

// EnsureIgnored writes a .gitignore that keeps the things which are not
// state out of the repository, for the case where the working tree
// shares a directory with anything else. Small, and worth having
// written down rather than assumed: a stray editor swap file committed
// into the state repository would be pushed to the remote.
func EnsureIgnored(dir string) error {
	const body = "# Written by grain.\n" +
		"*.swp\n" +
		".DS_Store\n" +
		// The private key never belongs here, and naming it makes an
		// operator who drops one in this directory by mistake fail to
		// commit it rather than push it to a remote.
		"*.key\n" +
		"secrets.key\n"
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeFileIfChanged(path, []byte(body))
}
