package staterepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// ErrDatabaseEmpty is the export this package will not do: writing a
// database with nothing in it over a repository that holds a whole
// deployment.
//
// Nothing further up stops that on its own. The remote is not ahead --
// this host really is the one that wrote the commit the dump sits on --
// so RemoteAhead has no objection, the commit is a fast-forward, and the
// push lands. The off-host copy of the deployment is then deleted by the
// deployment, in one commit, by the very loop that exists to keep it.
//
// The way in is a store that was lost while its working tree survived:
// the file deleted, a volume restored from a backup that did not include
// it, sqlite.Open handed a path that has never held one. A *start* in
// that state now puts itself right rather than reaching this at all --
// Load imports the whole repository into a database that has nothing to
// lose by it, which is the restore the repository exists for. This is
// what is left over: a database that went away underneath a daemon
// already running, and a `grain state sync` run against one. There the
// answer is to export nothing, say so where an operator looks (the UI's
// State pane reports it, cmd/grain's stateManager), and leave the
// restore to the restart that can do it safely -- an import that
// replaces every row is not something to do underneath a reconcile loop
// holding the ids it would delete.
var ErrDatabaseEmpty = errors.New("grain's database holds nothing and its state repository holds a " +
	"deployment: refusing to export an empty database over it -- restart grain to restore from the " +
	"repository, or point it at the store it was running on")

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
	// A database with nothing in it is not one to make a settings change
	// live in, and refusing here is what keeps the export below honest
	// rather than a nicety. The settings tables are a small part of the
	// dump; importing them into an empty database would leave a database
	// that holds templates and no tasks -- populated enough that sync's
	// own refusal (ErrDatabaseEmpty) no longer applies, and the very next
	// export would commit an empty task.json over a repository that holds
	// them. Reported as ErrNotApplied so cmd/grain's cycle stops before
	// that export, exactly as it does for a dump it could not read.
	//
	// The whole repository is what such a database wants, and a start is
	// where it gets it (Load). Not here: Apply runs underneath a live
	// reconcile loop, and replacing every row is not something to do with
	// runs in flight holding the ids.
	empty, err := databaseIsEmpty(ctx, db)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	if empty {
		if table, has := dumpHasRows(r.Dir()); has {
			return false, fmt.Errorf("%w: %w (%s/%s.json holds rows this database does not)",
				ErrNotApplied, ErrDatabaseEmpty, TablesDir, table)
		}
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
//     loaded it. Import all of it -- which is what makes "clone the
//     repository onto a new machine" a complete restore.
//   - HEAD is not the commit we recorded: something arrived. Import the
//     settings out of it, exactly as Apply does on a tick, and leave the
//     database's own record of what grain did alone.
//   - otherwise: the repository is exactly where we left it, so the
//     database is authoritative and nothing is imported. The next sync
//     exports whatever has happened since.
//
// There is a second question underneath all of that, and the marker
// cannot answer it: whether there is a database here at all. The marker
// is a fact about the repository and this host's agreement with it, and
// it lives in the git directory -- so a store that went away takes none
// of it with it. Deleted, or on a volume restored from a backup that did
// not include it, or simply opened at a path that has never held one: the
// working tree is untouched, HEAD is exactly the commit this host last
// wrote, and the answer above is "nothing to import" onto an empty
// database. The sync after that would export it and push a commit
// deleting the deployment from its own off-host copy, and no check
// further out would object, because the remote is not ahead -- this host
// wrote the commit under it.
//
// So a database with nothing in it takes the whole repository, marker or
// no marker. It is the same case the missing marker already is, arrived
// at from the other side: there is no database ahead of the dump to
// protect, so importing all of it can cost nothing and gains back
// everything the dump holds -- the restore this repository exists to
// make possible, done by the deployment rather than by an operator who
// has to know to move the working tree aside first. "Nothing in it"
// means exactly that: no row in any table, bar the schema stamp every
// database has from the moment it is created (databaseIsEmpty). A
// database with a single task in it is a database this host is
// responsible for, and it takes the ordinary path.
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
	// Whether there is a database here to protect, asked before the
	// marker is allowed to answer anything: a host can agree with the
	// repository exactly and have nothing to run on, because the marker
	// survives the store it was written beside (this function's own doc
	// comment, and databaseIsEmpty).
	empty, err := databaseIsEmpty(ctx, db)
	if err != nil {
		return err
	}
	// The ordinary restart: the repository is exactly where this host left
	// it, and this host has a database of its own to run on. Nothing is
	// imported, and the next sync exports whatever has happened since.
	if marker != "" && marker == head && !empty {
		return unreachable
	}
	// Which tables the import replaces depends on which of three cases
	// this is. The marker tells the first two apart; the third is the one
	// it cannot see.
	//
	// No marker at all is a working tree this host has never loaded -- a
	// clone onto a new machine, the restore case -- and there the
	// repository is the only copy of anything, so all of it is imported,
	// churn included. That case is what makes a clone a whole deployment
	// rather than a settings file, and nothing below applies to it: there
	// is no database ahead of the dump to protect.
	//
	// A database with nothing in it is that same case with the marker
	// still in place: a store that was lost while its working tree
	// survived. It takes the whole repository too, and for the identical
	// reason -- an empty database is not ahead of anything -- which is why
	// it is decided here rather than left to the marker, whose answer
	// ("this repository has not moved, import nothing") would come up on
	// an empty database and then export it back over the dump.
	//
	// A marker that disagrees with HEAD is a repository that moved under a
	// host that already has a database -- a merged pull request, or a
	// working tree RecoverDiverged reset onto the remote's branch -- and
	// there only the settings are taken, exactly as Apply takes them on a
	// tick.
	//
	// This used to replace the whole state tier, and that lost data every
	// time. The dump is whatever the last export wrote, so the database is
	// ahead of it on the state tier by up to an export interval as a
	// matter of design; replacing that tier deletes every task, comment,
	// attachment, grant, branch and release written since. The window is
	// half a minute in the ordinary case and however long the pushes had
	// been failing in the recovered one, and an operator lands in it by
	// habit rather than by accident: merge a settings pull request,
	// restart grain to make it take effect, lose whatever was filed in the
	// half minute before the stop.
	//
	// Nothing worth having is given up by narrowing it. A merged edit to
	// task or task_run no longer takes effect at a start -- but it never
	// took effect reliably, because the tick that pulled it down (Apply)
	// imported the settings out of it and recorded the commit as loaded,
	// so whether an edit to grain's own record survived came down to
	// whether the process happened to restart inside the same half minute.
	// A deterministic no-op the README can state is a better answer than a
	// coin toss that eats rows, and it is the answer the rest of this
	// package already gives: the database is authoritative for what grain
	// itself did, the repository is authoritative for settings, and the
	// next export writes the database's version back out over a merged
	// change to anything else. The import a restart does is now the import
	// a tick does.
	if marker == "" || empty {
		if err := Import(ctx, db, r.Dir()); err != nil {
			return err
		}
	} else if err := ImportTables(ctx, db, r.Dir(), SettingsTables); err != nil {
		return err
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
	if err := writeReadme(r.Dir(), deploymentName(ctx, db)); err != nil {
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
	// The other export this must not do, and the one no check on the
	// remote can catch: a database with nothing in it, written over a
	// repository that holds a deployment. The remote is not ahead in that
	// case -- this host wrote the commit under the dump -- so the commit
	// fast-forwards and the push lands, and the off-host copy is gone. See
	// ErrDatabaseEmpty for how a host gets into that state and why the
	// answer here is to stop rather than to restore: the restore is a
	// start's to do (Load), because it replaces every row.
	//
	// Both halves of the question are needed and the cheap half comes
	// first. An empty database is not on its own a reason to refuse: a
	// deployment nobody has configured yet, seeded into a repository of
	// its own, is empty at both ends and has to go on exporting. What is
	// refused is the pair -- nothing here, something there.
	empty, err := databaseIsEmpty(ctx, db)
	if err != nil {
		return false, err
	}
	if empty {
		if table, has := dumpHasRows(r.Dir()); has {
			return false, fmt.Errorf("%w (%s/%s.json holds rows this database does not)",
				ErrDatabaseEmpty, TablesDir, table)
		}
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
	//
	// Read from the database on every sync for the same reason it is
	// written on every sync: a deployment that has just been named, or
	// renamed, says so in the README on the next tick rather than at the
	// next restart. The name only ever changes through a settings change,
	// which is a commit of its own anyway, so this costs no churn.
	if err := writeReadme(r.Dir(), deploymentName(ctx, db)); err != nil {
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

// deploymentName is what this deployment calls itself, for the README:
// grain_config.environment_name, read out of the very database the dump
// is written from.
//
// Every way of not having an answer is "" -- the unnamed deployment,
// whose README is the one grain has always written. A build whose schema
// predates the column, a deployment with no config row because nobody
// has opened Settings yet, a NULL left by a hand-edited dump, a read
// that lost to something else holding the database: none of those is a
// reason to fail an export over a line of documentation, and each of
// them means the same thing to a reader anyway.
//
// Queried rather than taken through model.Store because this package
// deliberately knows the database as tables and columns rather than as
// grain's types (dump.go) -- and because Seed and sync are handed the
// *sql.DB and nothing else.
func deploymentName(ctx context.Context, db *sql.DB) string {
	var name string
	if err := db.QueryRowContext(ctx,
		"SELECT `environment_name` FROM `grain_config` WHERE `id` = 1").Scan(&name); err != nil {
		return ""
	}
	return name
}

// deploymentNameFromDump is deploymentName for the caller that has no
// database to ask: `grain state format` runs in a clone (format.go),
// where the config row is a file like every other table.
//
// Without this, formatting a clone of a named deployment's repository
// would write the unnamed README over the named one -- a diff for a
// human to commit, and grain putting it straight back on its next sync
// -- and Format's own promise that a directory already formatted comes
// out byte-identical would stop being true there. A directory with no
// dump in it (the bootstrap case: format an empty repository, then point
// a deployment at it) has nothing to name, and the seed that follows
// writes the name in.
func deploymentNameFromDump(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, TablesDir, "grain_config.json"))
	if err != nil {
		return ""
	}
	var rows []struct {
		EnvironmentName string `json:"environment_name"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0].EnvironmentName
}

func writeReadme(dir, deployment string) error {
	return writeFileIfChanged(filepath.Join(dir, ReadmeFile), []byte(readme(deployment)))
}

// readme is the README's text for a deployment called deployment, which
// is model.Config.EnvironmentName -- free text an operator sets, and
// what the pane already labels itself with.
//
// "" is the unnamed deployment, and renders the README grain has always
// written, byte for byte. That matters more here than the naming line
// does: this file is rewritten on every sync, so a rendering that moved
// so much as a space for a deployment with nothing to say would be a
// commit and a push on the next tick of every installation that upgrades
// past this build.
func readme(deployment string) string {
	return readmeIntro + deploymentParagraph(deployment) + readmeBody
}

// deploymentParagraph names the deployment whose database this is, or
// says nothing at all.
//
// It is what nothing else in the tree says. The dump is the same files
// with the same tables in the same shape whoever wrote it, so a clone,
// or a checkout handed to an agent that never saw the deployment it came
// from, cannot tell one installation's state from another's -- which is
// the reading a human is most likely to get wrong on a fleet of them,
// and the one an agent has no way of checking. A run dispatched by grain
// itself is told separately (orchestrator's settingsRepoSection); this
// is for every other way somebody ends up looking at these files.
//
// The name is quoted rather than pasted in, and both bounds below are
// about what can reach this function. environment_name is validated on
// the way through the UI (ui.UpdateSettings: trimmed, 32 runes, no line
// breaks or tabs), but grain_config is a settings table, so a merged
// pull request against this very repository can put anything at all in
// that column and grain will write it back out here on the next sync.
// strconv.Quote is what keeps a newline or a control character in that
// value from turning one line of this file into several, and the
// truncation is what keeps a pasted paragraph from taking the README
// over. Neither is markdown-aware and neither needs to be: what is left
// is a quoted string on a line of its own, where the worst a stray
// backtick or asterisk can do is italicise some prose.
func deploymentParagraph(deployment string) string {
	name := strings.TrimSpace(deployment)
	if name == "" {
		return ""
	}
	if rs := []rune(name); len(rs) > maxDeploymentNameRunes {
		name = string(rs[:maxDeploymentNameRunes])
	}
	return `
This is the deployment called ` + strconv.Quote(name) + `.
` + "`grain_config.environment_name`" + ` is where that name comes from -- the
same name the deployment's own pane shows. Nothing else in this tree
says whose rows these are: every deployment exports the same files,
holding the same tables in the same shape, so a clone of this repository
-- or a checkout of it handed to somebody who did not clone it -- is
otherwise indistinguishable from any other deployment's.
`
}

// maxDeploymentNameRunes bounds what deploymentParagraph will print,
// deliberately the same bound ui.maxEnvironmentNameLen applies on the
// way in rather than a second opinion about how long a name may be: a
// value longer than this did not come through the UI, and a README is
// no better a place for a paragraph than the sidebar badge is.
const maxDeploymentNameRunes = 32

const readmeIntro = `# grain state

This repository is grain's database, written out as text.
`

const readmeBody = `
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

That applies to the settings tables, and to them only: ` + "`" + `grain_config` + "`" + `,
` + "`" + `repo_config` + "`" + `, the ` + "`" + `template` + "`" + `, ` + "`" + `suite` + "`" + `, ` + "`" + `schedule` + "`" + ` and
` + "`" + `qualification` + "`" + ` tables. The rest -- tasks, runs, metrics: grain's own
record of what it did -- is never replaced from here, at a restart no
more than on a tick. The database is the live copy of those rows and is
always ahead of these files by however long it is since the last export,
so importing them would delete whatever grain had written since. Editing
one of them here does nothing; the next export writes the database's
version back over your change.

The exception is a restore, and it is the only one: a start with nothing
to lose brings back every table -- a clone of this repository onto a host
that has never loaded it, or a start on a host whose database has gone
away and left this directory behind, which is the same situation from the
other side. That is what makes this a whole deployment to restore from
rather than a settings file. grain will not do the reverse: an export
from a database with nothing in it, over a dump that holds rows, is
refused and reported instead of committed.

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
