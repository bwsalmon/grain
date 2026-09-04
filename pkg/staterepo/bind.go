package staterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// ErrNoLocalCopy is the one unreachable remote a Load will not carry on
// from: a fetch that failed against a working tree with no commits in it
// at all.
//
// The difference is what grain would be running on. A tree with commits
// in it was cloned or fetched at some point, so the dump under it is
// this repository's own content and the database beside it was loaded
// from that -- the remote being unreachable now costs nothing but the
// news, and the next tick asks again. A tree with no commits has never
// held a byte of this repository, and nothing on this host can say
// whether the remote holds a whole deployment's state or nothing at all.
// Carrying on there means Seed: an export committed as a root commit
// sharing no history with the remote's branch, which no push can ever
// fast-forward and every later pull reports as divergence. Better to
// stop and say why, and let the start after this one find the remote
// back.
//
// Deliberately not wrapped around ErrUnreachable, even though that is
// what caused it, so that errors.Is(err, ErrUnreachable) means exactly
// one thing at a call site deciding whether to start: loaded, but out of
// touch with the remote.
var ErrNoLocalCopy = errors.New("the state repository's remote could not be reached and this host " +
	"has no copy of it to fall back on")

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
	"template",
	"template_read",
	"template_grant",
	"template_sequence",
	// Suites and the templates they run, but not suite_run and its
	// own tables: a suite is settings, a run of one is not.
	"suite",
	"suite_item",
	"suite_sequence",
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
//
// A failure to reach the remote does not stop any of that. Everything
// above is decided against the working tree, and a tree this host has
// already loaded is still a tree this host can load again, so a fetch
// that failed leaves the load going ahead on what is on disk and comes
// back as an error marked ErrUnreachable -- loaded, but out of touch,
// which a daemon reports and goes on running. The two exceptions are
// ErrNoLocalCopy, where there is no tree to fall back on, and every
// failure that is about the repository's contents rather than about
// reaching it: a dump at a schema this build cannot read, rows that
// would not import, a history that has diverged. Those come back
// unmarked and are a deployment that must stop -- see cmd/grain's own
// run() for the other half of that decision.
func Load(ctx context.Context, r *Repo, db *sql.DB, version int) error {
	// unreachable, once set, is what this returns at the end of an
	// otherwise successful load: the caller is owed the fact that the
	// repository it is running on is however stale the last fetch left
	// it, even though nothing here failed to do its job.
	var unreachable error
	if _, err := r.Pull(ctx); err != nil {
		if !errors.Is(err, ErrUnreachable) {
			return err
		}
		if empty, _ := r.isEmpty(ctx); empty {
			return fmt.Errorf("%w: %v", ErrNoLocalCopy, err)
		}
		unreachable = err
	}
	if !HasDump(r.Dir()) {
		if err := Seed(ctx, r, db, version); err != nil {
			// A seed ends in a push, and a push cannot land while the
			// remote is out of reach. The commit it made is on disk either
			// way and the sync loop puts it out with the first push that
			// works, so this stays the unreachable remote it started as
			// rather than becoming a failure to seed.
			if unreachable == nil {
				return err
			}
			return fmt.Errorf("%w (seeding it locally got as far as: %v)", unreachable, err)
		}
		return unreachable
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
		return unreachable
	}
	// Which tables the import replaces depends on which of three cases
	// this is, and the markers are what tell them apart.
	//
	// No marker at all is a working tree this host has never loaded --
	// a clone onto a new machine, the restore case -- and there the
	// repository is the only copy of anything, so all of it is imported.
	//
	// A HEAD that RecoverDiverged put there is the opposite extreme. The
	// working tree was reset onto the remote's branch over local export
	// commits that could not be pushed, so the dump under it is older
	// than this database by everything those commits held: importing the
	// state tier would delete every task, comment, branch and release
	// written since the pushes started failing. Only the settings are
	// taken, exactly as Apply takes them on a tick, and the export that
	// follows writes grain's own record back out on top of the merge.
	//
	// Otherwise a marker that disagrees with HEAD is a repository that
	// moved under a host that already had a database: a pull request was
	// merged and fast-forwarded in. That merge is about state -- nobody
	// opens a pull request editing task_run -- and this host's database is
	// ahead of the repository on churn by up to a churn interval, because
	// that is exactly what exporting churn on a slower clock means.
	// Importing all of it would throw that away every time a settings
	// change landed, so only the state tier is replaced and the database
	// stays authoritative about what grain was doing. The next churn
	// export writes it back out.
	switch {
	case marker == "":
		if err := ImportTier(ctx, db, r.Dir(), TierState, TierChurn); err != nil {
			return err
		}
	case r.recoveredHead(ctx) == head:
		if err := ImportTables(ctx, db, r.Dir(), SettingsTables); err != nil {
			return err
		}
	default:
		if err := ImportTier(ctx, db, r.Dir(), TierState); err != nil {
			return err
		}
	}
	if err := r.setLoadedHead(ctx, head); err != nil {
		return err
	}
	return unreachable
}

// Seed writes the database out as the repository's first content: the
// dump, the schema stamp, the README that tells whoever opens the
// repository next what they are looking at, and the CI step that checks
// what they propose to change in it.
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
	if err := r.recordChurnExport(ctx, r.now()); err != nil {
		return err
	}
	if err := r.recordLoadedHead(ctx); err != nil {
		return err
	}
	if err := r.Push(ctx); err != nil {
		return err
	}
	// After the seed's own commit has reached the remote, and as a commit
	// of its own: a remote that will not take a file under
	// .github/workflows (installWorkflow says why that happens) must not
	// be able to take the dump down with it, and the undo that handles
	// that case needs a commit to come back to.
	_, err := r.installWorkflow(ctx)
	return err
}

// ErrRemoteAhead is returned by Sync and SyncAll when the remote holds
// commits this deployment has not taken up -- a merged pull request against grain's
// own settings, which is the mechanism this whole package exists to
// allow.
//
// Apply, above, is what handles that in the ordinary case: a tick pulls
// the merge down and makes its settings live. This is the tick where
// that did not happen -- a history that will not fast-forward, or a
// merge that landed between the pull and the push -- and it is a refusal
// to export rather than a failure to push, which is the whole point:
// exporting would commit this deployment's dump on top of the merge, and
// no amount of retrying afterwards could get that commit onto a remote
// that has moved past it. The database is still the live state and grain
// goes on running against it; what is waiting is loaded at the next
// start, whole, because that import replaces every row and is not
// something to do underneath runs holding ids (Load, above).
var ErrRemoteAhead = errors.New("the state repository has changes this deployment has not loaded; " +
	"restart grain to apply them")

// Sync writes the database out and commits and pushes anything that
// changed, reporting whether there was anything to commit.
//
// Everything a person or an agent reads goes out on every call. grain's
// own churn -- runs, leases, observations, read marks (tier.go) -- goes
// out only when Config.ChurnInterval has elapsed since it last did, and
// that is the whole of what keeps this repository from growing with the
// clock instead of with the data: a busy deployment with no settings
// changes commits a couple of dozen times a day rather than 2,880 times,
// and each of those commits carries an hour of run history rather than
// thirty seconds of it.
//
// The commit message is deliberately plain and identical every time.
// What changed is in the diff, which is the whole reason the dump is
// text; a message that tried to summarise it would be a second, worse
// answer that could disagree with the first.
//
// A push that failed is not the last word on it either: every call
// pushes whatever this host holds that the remote has not got, whether
// or not it had anything of its own to commit, so an outage costs the
// remote the delay and nothing else. The reported bool stays what it
// says -- a call that only carried an earlier commit out to the remote
// committed nothing and reports false -- and a push that goes on failing
// comes back as an error every time rather than once.
func Sync(ctx context.Context, r *Repo, db *sql.DB, version int) (bool, error) {
	return sync(ctx, r, db, version, false)
}

// SyncAll is Sync with the churn tier written out whether or not it is
// due: what a human means by `grain state sync` or the pane's own Sync
// button, and what a clean shutdown owes the repository on its way out.
// Asking for a sync explicitly is asking for all of it.
func SyncAll(ctx context.Context, r *Repo, db *sql.DB, version int) (bool, error) {
	return sync(ctx, r, db, version, true)
}

func sync(ctx context.Context, r *Repo, db *sql.DB, version int, forceChurn bool) (bool, error) {
	// One ls-remote, before anything is written, answering both of this
	// cycle's questions about the remote (remoteState, staterepo.go): is
	// it ahead of us, which decides whether we may export at all, and are
	// we ahead of it, which decides whether we push at the end.
	//
	// Asked before anything is written so that a merged change is never
	// committed over -- see ErrRemoteAhead. An unreachable remote answers
	// "nothing is known" and the push below reports the network in its
	// own words. It guards both entry points: asking for a sync
	// explicitly (SyncAll) is no more a reason to commit over a merge
	// than the timer's own tick is.
	remote := r.remoteState(ctx)
	if remote.ahead {
		return false, ErrRemoteAhead
	}
	// The CI step, before anything is exported and as a commit and a push
	// of its own (installWorkflow, format.go). Every sync, not only at
	// Seed, for the reason the README below is rewritten every sync: a
	// repository an operator adopted, or one a merge dropped the file out
	// of, has to end up with it anyway. Unlike the README it is written
	// only when it is missing, because it is a file an operator may edit
	// and grain must not fight them for.
	installed, err := r.installWorkflow(ctx)
	if err != nil {
		return installed, err
	}
	// Rewritten on every sync, not only at Seed: a repository an operator
	// adopted, or one whose README a merge dropped, still has to explain
	// itself to whoever opens it next, and this is the cheapest place to
	// make that true rather than a thing that is true only if the
	// repository was created here.
	if err := writeReadme(r.Dir()); err != nil {
		return installed, err
	}
	now := r.now()
	churn := forceChurn || r.churnDue(ctx, now)
	tiers := []Tier{TierState}
	if churn {
		tiers = append(tiers, TierChurn)
	}
	if err := ExportTier(ctx, db, r.Dir(), tiers...); err != nil {
		return installed, err
	}
	if err := WriteSchemaVersion(r.Dir(), version); err != nil {
		return installed, err
	}
	// Recorded whether or not the export produced a commit: what the
	// interval bounds is how often grain re-reads and re-renders every
	// transcript it has ever stored, which is the expensive half even
	// when the answer turns out to be byte-identical to last time.
	if churn {
		if err := r.recordChurnExport(ctx, now); err != nil {
			return installed, err
		}
	}
	// installed rides along in every answer from here on: a sync that
	// committed the CI step and exported nothing has still changed the
	// repository, and a caller that logs "nothing to do" over a commit it
	// just pushed would be describing the wrong tick.
	changed, err := r.Commit(ctx, "Update grain state\n\n"+
		"Written by grain from its own database at "+now.UTC().Format(time.RFC3339)+".")
	if err != nil {
		return installed, err
	}
	if changed {
		// Recorded before the push, not after: the commit exists either
		// way, and a push that fails (an expired credential, an
		// unreachable remote) must not leave the next start believing the
		// repository has moved on without it and importing its own stale
		// dump back over a database that is ahead.
		if err := r.recordLoadedHead(ctx); err != nil {
			return true, err
		}
	}
	// Pushed when this host holds a commit the remote has not got, which
	// is a different question from whether this cycle made one -- and the
	// difference is a commit that never leaves the host.
	//
	// A push that failed used to be retried only by the next cycle that
	// had something of its own to commit. On a busy deployment that is
	// the next one and nobody notices. On an idle one there is no such
	// cycle at all: nothing files a task, nothing changes a setting, the
	// churn tier is not due, so every export from here on is
	// byte-identical, Commit reports nothing to do, and the commit sits
	// here until the deployment next does something -- which may be
	// hours, and is exactly the window in which a merged pull request
	// turns it into the divergence diverge.go exists to clear. The pane
	// went quiet over it too, since a cycle that pushed nothing reported
	// nothing.
	//
	// Asking the remote instead retries on the very next tick, and goes
	// on reporting the failure for as long as it lasts. It costs nothing
	// extra to ask: the answer is the one already fetched at the top of
	// this function.
	if !changed && !remote.behind {
		return installed, nil
	}
	if err := r.Push(ctx); err != nil {
		return installed || changed, err
	}
	r.maintain(ctx)
	return installed || changed, nil
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

Two clocks, not one. Everything a person or an agent reads or changes is
written out within seconds of changing. grain's own running record of
what it did -- ` + "`" + TablesDir + `/task_run.json` + "`" + `, ` + "`" + `task_observation.json` + "`" + `
and ` + "`" + `lease.json` + "`" + ` -- is written out roughly hourly
instead: those rows change on nearly every reconcile cycle, nobody sends
a pull request against them, and committing them every thirty seconds
would rewrite the largest files here 2,880 times a day forever. They are
still exported, so a clone of this repository is still a full restore --
it is just up to an hour behind on runs, and never behind on anything
anybody wrote.

## What is here

- ` + "`" + TablesDir + `/` + "`" + ` -- the database. Settings (templates, suites, repo
  configuration, prompt extensions, schedules), tasks, runs and metrics.
- ` + "`" + SchemaVersionFile + "`" + ` -- the schema the dump was written by. A grain
  that knows a different one refuses to import rather than guessing.

Which tables are settings, and which are grain's own record of what it
did, is the distinction to hold on to before changing anything.
` + "`" + `template` + "`" + `, ` + "`" + `suite` + "`" + ` (with ` + "`" + `suite_item` + "`" + `), ` + "`" + `repo_config` + "`" + `,
` + "`" + `schedule` + "`" + ` and ` + "`" + `grain_config` + "`" + `, plus the ` + "`" + `_read` + "`" + `/` + "`" + `_grant` + "`" + `/` + "`" + `_sequence` + "`" + `
tables belonging to them, are settings. ` + "`" + `task` + "`" + `, ` + "`" + `task_run` + "`" + `,
` + "`" + `task_comment` + "`" + `, ` + "`" + `task_observation` + "`" + `, ` + "`" + `lease` + "`" + `, ` + "`" + `branch` + "`" + `, ` + "`" + `release` + "`" + ` and
their like are observations: grain writes them, and a change to one is
either overwritten by the next export or kept as a record of something
that never happened.

## Changing something

Open a pull request against this repository the way you would against
any other. On merge, grain pulls -- within half a minute, without a
restart -- and the merged files replace what is in its database, so a
deleted row is a deleted row.

That applies live to the settings tables: ` + "`" + `grain_config` + "`" + `,
` + "`" + `repo_config` + "`" + `, the ` + "`" + `template` + "`" + `, ` + "`" + `suite` + "`" + `, ` + "`" + `schedule` + "`" + ` and
` + "`" + `qualification` + "`" + ` tables. The rest -- tasks, runs, metrics: grain's own
record of what it did -- is replaced only when grain starts, because
replacing it underneath a run that is in flight would delete the very
rows that run is working on. Editing one of those here while grain is
running does nothing; the next export writes the database's version back
over your change.

That check already runs on your pull request. ` + "`" + WorkflowFile + "`" + `
runs ` + "`" + `grain state check` + "`" + `, which loads this whole directory into a
throwaway database and reports what breaks, so a red tick means grain
would not have loaded your change -- and a malformed file or a row
missing a required column is caught there rather than when the daemon
next starts, which is the worst place to find out. ` + "`" + `grain state check .` + "`" + `
is the same thing in a terminal.

grain writes that workflow itself, whenever it is not here, and never
rewrites one that is: edit it -- pin the image, change the runner, add a
step -- and what you wrote is what stays. If it is missing and does not
come back, the deployment's credential is not allowed to push files
under ` + "`" + `.github/workflows` + "`" + ` (its journal says so, and
` + "`" + `grain state ci .` + "`" + ` in a clone writes the file for a human to commit),
or the operator has turned it off with ` + "`" + `"noWorkflow": true` + "`" + `.

Do not hand-edit while grain is running unless you mean it: grain is the
only writer, exports on a timer, and a local edit it did not make is
overwritten by the next export.

## Secrets are not here

grain's secrets are encrypted, and they are not in this repository. They
sit beside the private key on the machine grain runs on, under
` + "`" + `<data-dir>/secrets` + "`" + `, which is the directory an operator backs up -- the
key was never here either, so a copy of this repository never could
decrypt anything.

They used to be here, as ` + "`" + SecretsFile + "`" + `. This repository is somewhere
agents are dispatched to work now, and everything a sandbox can clone is
everything a sandbox can read; ciphertext an agent can carry off is
still ciphertext an agent can carry off. If that file appears anywhere
in this repository's history, grain refuses to let any sandbox reach it
at all -- see the "State repository" section of grain's README.
`

// EnsureIgnored writes a .gitignore that keeps the things which are not
// state out of the repository, for the case where the working tree
// shares a directory with anything else. Small, and worth having
// written down rather than assumed: a stray editor swap file committed
// into the state repository would be pushed to the remote.
//
// A file that is already there is added to rather than replaced. It used
// to be left exactly as it was, which was fine while this list never
// changed; it does now (secrets.enc joined it), and a repository created
// by an earlier build would otherwise never learn the new line. Whatever
// an operator added themselves stays.
func EnsureIgnored(dir string) error {
	const body = "# Written by grain.\n" +
		"*.swp\n" +
		".DS_Store\n" +
		// The private key never belongs here, and naming it makes an
		// operator who drops one in this directory by mistake fail to
		// commit it rather than push it to a remote.
		"*.key\n" +
		"secrets.key\n" +
		// Nor does the encrypted file any more: this repository is
		// somewhere agents are dispatched to work now, and everything a
		// sandbox can clone is everything a sandbox can read. The
		// ciphertext lives beside the key under <data-dir>/secrets --
		// see SecretsFile -- and this line is what keeps a copy left
		// behind by an older build, or by a hand that put one here, from
		// being committed back.
		SecretsFile + "\n"
	path := filepath.Join(dir, IgnoreFile)
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return writeFileIfChanged(path, []byte(body))
	}
	if err != nil {
		return fmt.Errorf("staterepo: reading %s: %w", path, err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if !have[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	updated := string(existing)
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	return writeFileIfChanged(path, []byte(updated+strings.Join(missing, "\n")+"\n"))
}
