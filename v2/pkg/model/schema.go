package model

import "strconv"

// The model as DDL, and the derivations as views.
//
// Two things here are not merely a translation of task.go, and both are
// why a schema'd store earns its place:
//
// task_state is a view. TaskState is derived from approval plus grain's
// own observations and never written, so as a view there is no column to
// write: no finish path can write one, no migration can add one by
// accident, and "a task is in exactly one state" stops being a rule each
// code path upholds and becomes a property of the store.
//
// Declaration and observation are separate tables, not columns on one
// row. They answer to different records — a human authors the first,
// grain writes the second.
//
// On naming: every table is backtick-quoted. SQLite accepts backticks as
// a MySQL-compatibility identifier quote, which is what lets this SQL
// read the same regardless of which engine is behind Store -- no
// identifier here needs it against SQLite's own much smaller reserved
// word list, but keeping the quoting uniform means nothing has to
// remember which columns happen to need it.
//
// On types: enums are TEXT rather than a constrained type. Keeping the
// vocabulary in task.go rather than the schema is what lets a new
// LinkKind ship without a migration; the cost is that the database will
// not reject an unknown value, which the Valid methods and schema_test.go
// cover instead. Timestamps keep DATETIME rather than MySQL's DATETIME(6):
// SQLite has no timestamp storage class of its own, and this is what
// tells modernc.org/sqlite (the driver pkg/model/sqlite opens) to hand a
// column back as a time.Time rather than the TEXT it stores under the
// hood -- store.go's sql.NullTime scanning and time.Time binding are
// otherwise unchanged from what they were against Dolt. Booleans are
// INTEGER (0/1) -- SQLite has no separate boolean storage class either,
// and database/sql converts either direction without help from this
// package (pinned by schema_test.go).

// SchemaVersion is bumped whenever Tables or Views change in a way an
// existing database cannot simply be re-created into. Open records this
// and refuses a database written by a newer build, rather than failing
// later with a confusing missing column.
const SchemaVersion = 13

// Tables is the DDL, in dependency order.
var Tables = []string{
	`CREATE TABLE IF NOT EXISTS ` + "`task`" + ` (
  ` + "`id`" + `                    TEXT    NOT NULL,
  ` + "`intent`" + `                TEXT    NOT NULL,
  ` + "`title`" + `                 TEXT    NOT NULL,
  ` + "`body`" + `                  TEXT    NOT NULL,

  ` + "`origin_actor_kind`" + `     TEXT    NOT NULL,
  ` + "`origin_actor_id`" + `       TEXT    NOT NULL,
  ` + "`origin_behalf_kind`" + `    TEXT    NULL,
  ` + "`origin_behalf_id`" + `      TEXT    NULL,
  ` + "`origin_reason`" + `         TEXT    NOT NULL,

  ` + "`approval_actor_kind`" + `   TEXT    NULL,
  ` + "`approval_actor_id`" + `     TEXT    NULL,
  ` + "`approval_behalf_kind`" + `  TEXT    NULL,
  ` + "`approval_behalf_id`" + `    TEXT    NULL,
  ` + "`approved_at`" + `           DATETIME NULL,

  ` + "`target_owner`" + `          TEXT    NULL,
  ` + "`target_name`" + `           TEXT    NULL,
  ` + "`binding`" + `               TEXT    NOT NULL,
  ` + "`base`" + `                  TEXT    NULL,
  ` + "`folder`" + `                TEXT    NULL,

  ` + "`auto_merge`" + `            INTEGER  NOT NULL,
  ` + "`created_at`" + `            DATETIME NULL,
  ` + "`order_key`" + `             REAL     NOT NULL DEFAULT 0,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// Read targets get a table rather than a JSON column: "which tasks
	// read this repo?" is worth being able to ask, and keeping reads
	// structurally distinct from task.target is what stops a read target
	// becoming a capability-laundering channel by accident.
	`CREATE TABLE IF NOT EXISTS ` + "`task_read`" + ` (
  ` + "`task_id`" + ` TEXT NOT NULL,
  ` + "`owner`" + `   TEXT NOT NULL,
  ` + "`name`" + `    TEXT NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_grant`" + ` (
  ` + "`task_id`" + `    TEXT NOT NULL,
  ` + "`capability`" + ` TEXT NOT NULL,
  ` + "`via`" + `        TEXT NOT NULL,
  ` + "`folder`" + `     TEXT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`capability`" + `)
)`,

	// blocks is stored rather than recomputed in SQL so task_blocked can
	// be a plain join instead of hard-coding the kind vocabulary in two
	// places where it could drift.
	`CREATE TABLE IF NOT EXISTS ` + "`task_link`" + ` (
  ` + "`task_id`" + ` TEXT    NOT NULL,
  ` + "`kind`" + `    TEXT    NOT NULL,
  ` + "`target`" + `  TEXT    NOT NULL,
  ` + "`blocks`" + `  INTEGER NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`kind`" + `, ` + "`target`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_tag`" + ` (
  ` + "`task_id`" + ` TEXT NOT NULL,
  ` + "`tag`" + `     TEXT NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`tag`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_observation`" + ` (
  ` + "`task_id`" + `                     TEXT     NOT NULL,
  ` + "`closed_at`" + `                   DATETIME NULL,
  ` + "`completed_at`" + `                DATETIME NULL,
  ` + "`pending_question_comment_id`" + ` INTEGER  NULL,
  ` + "`baseline_comment_id`" + `         INTEGER  NULL,
  ` + "`merge_queue_blocked_at`" + `      DATETIME NULL,
  ` + "`observed_at`" + `                 DATETIME NULL,
  ` + "`retry_requested_at`" + `          DATETIME NULL,
  ` + "`pr_opened_at`" + `                DATETIME NULL,
  ` + "`pr_merged_at`" + `                DATETIME NULL,
  ` + "`pr_closed_at`" + `                DATETIME NULL,
  PRIMARY KEY (` + "`task_id`" + `)
)`,

	// detail is a short human-readable reason behind outcome -- see
	// model.Run.Detail's own doc comment on why it exists at all.
	//
	// transcript is the full narrative record of what the agent said and
	// did over the run -- model.Run.Transcript's own doc comment explains
	// why it is worth a column of its own rather than folding into detail
	// (bwsalmon/agents#446). It is written once, by SetRunTranscript,
	// after FinishRun has already recorded outcome/detail -- the same
	// after-the-fact shape SetRunOutcome already uses -- and stays NULL
	// for a run still in flight, or one whose framework never populated
	// agent.Result.Transcript at all.
	`CREATE TABLE IF NOT EXISTS ` + "`task_run`" + ` (
  ` + "`id`" + `          TEXT     NOT NULL,
  ` + "`task_id`" + `     TEXT     NOT NULL,
  ` + "`slot`" + `        TEXT     NOT NULL,
  ` + "`sandbox`" + `     TEXT     NOT NULL,
  ` + "`unit`" + `        TEXT     NULL,
  ` + "`attempt`" + `     INTEGER  NOT NULL,
  ` + "`started_at`" + `  DATETIME NOT NULL,
  ` + "`finished_at`" + ` DATETIME NULL,
  ` + "`outcome`" + `     TEXT     NULL,
  ` + "`detail`" + `      TEXT     NULL,
  ` + "`transcript`" + `  TEXT     NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// At most one open (finished_at IS NULL) run per slot -- the DB-level
	// backstop bwsalmon/agents#434 asks for. dispatch.Cycle reads
	// OccupiedSlots and Ready outside of any single transaction and then
	// issues one StartRun per free slot it found, so nothing in Go stops
	// two overlapping Cycle calls (nothing makes one today -- cycle.go's
	// own doc comments -- but nothing enforces that either) from both
	// seeing the same slot as free and both dispatching onto it. Paired
	// with startRun's own INSERT (its doc comment), the second StartRun
	// now fails outright on this index instead of landing a second live
	// run on a slot the first dispatch already claimed. A partial index
	// rather than a plain UNIQUE column, since the same slot legitimately
	// appears in many finished rows over a store's lifetime and only the
	// currently-open one must be unique.
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + "`task_run_open_slot`" + ` ON ` + "`task_run`" + ` (` + "`slot`" + `) WHERE ` + "`finished_at`" + ` IS NULL`,

	`CREATE TABLE IF NOT EXISTS ` + "`lease`" + ` (
  ` + "`run_id`" + `     TEXT     NOT NULL,
  ` + "`capability`" + ` TEXT     NOT NULL,
  ` + "`resource`" + `   TEXT     NOT NULL,
  ` + "`minted_by`" + `  TEXT     NOT NULL,
  ` + "`issued_at`" + `  DATETIME NOT NULL,
  ` + "`expires_at`" + ` DATETIME NULL,
  PRIMARY KEY (` + "`run_id`" + `, ` + "`capability`" + `, ` + "`resource`" + `)
)`,

	// The conversation, as grain's own rows rather than a GitHub issue's
	// comment thread. INTEGER PRIMARY KEY is SQLite's own rowid alias, the
	// AUTOINCREMENT that role needs: the id is both the ordering and the
	// thing task_observation.pending_question_comment_id names, so a
	// caller that has just written a question needs its id back to record
	// which question is outstanding.
	`CREATE TABLE IF NOT EXISTS ` + "`task_comment`" + ` (
  ` + "`id`" + `                 INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`task_id`" + `            TEXT    NOT NULL,
  ` + "`author_kind`" + `        TEXT    NOT NULL,
  ` + "`author_id`" + `          TEXT    NOT NULL,
  ` + "`author_behalf_kind`" + ` TEXT     NULL,
  ` + "`author_behalf_id`" + `   TEXT     NULL,
  ` + "`body`" + `               TEXT     NOT NULL,
  ` + "`created_at`" + `         DATETIME NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`task_comment_task`" + ` ON ` + "`task_comment`" + ` (` + "`task_id`" + `)`,

	// Task identity, allocated here rather than borrowed from a GitHub
	// issue number. A task used to be identified by the issue it was filed
	// from ("owner/name/7"), which meant nothing could file a task without
	// first creating an issue -- the coupling that made GitHub the input.
	//
	// INTEGER PRIMARY KEY rather than a counter column somebody reads and
	// writes back: allocation has to stay correct with a daemon, a UI and
	// a CLI all writing at once (README's "Single writer, many openers"),
	// and an INSERT that lets SQLite assign the rowid is atomic where
	// read-modify-write is a race. The row it leaves behind is also a
	// record of when each id was handed out, which a bare counter throws
	// away.
	`CREATE TABLE IF NOT EXISTS ` + "`task_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
)`,

	// One row per schedule (bwsalmon/agents#376), not one per firing --
	// task_sequence's own reasoning applies again for a firing's identity,
	// but the schedule itself needs exactly one durable row to carry
	// next_run_at/last_run_at and the enabled switch a UI toggles.
	// recurrence_kind/every_n_hours/time_of_day_minutes/weekday/day_of_month
	// are model.Recurrence's fields (bwsalmon/agents#464) -- only the
	// subset matching recurrence_kind is ever non-NULL for a given row
	// (model.Recurrence.Validate's own doc comment has the mapping), the
	// same "column per case, most left NULL" shape task's own
	// approval_actor_kind columns already use for a different sum type.
	// This replaces the original bare interval_ms (every N hours since it
	// last fired, with no wall-clock alignment); a database from before
	// that widening is migrated by ensureScheduledTaskRecurrenceColumns.
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task`" + ` (
  ` + "`id`" + `                    TEXT     NOT NULL,
  ` + "`title`" + `                 TEXT     NOT NULL,
  ` + "`body`" + `                  TEXT     NOT NULL,
  ` + "`target_owner`" + `          TEXT     NOT NULL,
  ` + "`target_name`" + `           TEXT     NOT NULL,
  ` + "`base`" + `                  TEXT     NULL,
  ` + "`auto_merge`" + `            INTEGER  NOT NULL,
  ` + "`template_id`" + `           TEXT     NULL,
  ` + "`recurrence_kind`" + `       TEXT     NOT NULL,
  ` + "`every_n_hours`" + `         INTEGER  NULL,
  ` + "`time_of_day_minutes`" + `   INTEGER  NULL,
  ` + "`weekday`" + `               INTEGER  NULL,
  ` + "`day_of_month`" + `          INTEGER  NULL,
  ` + "`enabled`" + `               INTEGER  NOT NULL,
  ` + "`next_run_at`" + `           DATETIME NOT NULL,
  ` + "`last_run_at`" + `           DATETIME NULL,
  ` + "`created_at`" + `            DATETIME NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// task_sequence's own doc comment gives the reasoning for a dedicated
	// allocator rather than a counter column: an INSERT that lets SQLite
	// assign the rowid is atomic where read-modify-write is a race.
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
)`,

	// task_read's own doc comment gives the reasoning for a table rather
	// than a JSON column, applied again here now that a schedule's firing
	// carries read-only repos too (bwsalmon/agents#464).
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task_read`" + ` (
  ` + "`scheduled_task_id`" + ` TEXT NOT NULL,
  ` + "`owner`" + `             TEXT NOT NULL,
  ` + "`name`" + `              TEXT NOT NULL,
  PRIMARY KEY (` + "`scheduled_task_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	// task_grant's own shape, for a schedule's own capabilities
	// (bwsalmon/agents#464) -- via is always 'label' here (a human chose
	// it in the schedule's own form, same as grantsFor does for a task),
	// but the column exists anyway so scanning stays identical to
	// task_grant's.
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task_grant`" + ` (
  ` + "`scheduled_task_id`" + ` TEXT NOT NULL,
  ` + "`capability`" + `        TEXT NOT NULL,
  ` + "`via`" + `               TEXT NOT NULL,
  ` + "`folder`" + `            TEXT NULL,
  PRIMARY KEY (` + "`scheduled_task_id`" + `, ` + "`capability`" + `)
)`,

	// One row per template (bwsalmon/agents#516) -- task_sequence's own
	// "identity allocated here, not borrowed" reasoning applies again:
	// name/title/body/target/base/auto_merge are exactly the fields a
	// schedule already carried inline, now given a row of their own so
	// more than one schedule (scheduled_task.template_id) can point at
	// the same one instead of repeating it.
	`CREATE TABLE IF NOT EXISTS ` + "`task_template`" + ` (
  ` + "`id`" + `           TEXT     NOT NULL,
  ` + "`name`" + `         TEXT     NOT NULL,
  ` + "`title`" + `        TEXT     NOT NULL,
  ` + "`body`" + `         TEXT     NOT NULL,
  ` + "`target_owner`" + ` TEXT     NOT NULL,
  ` + "`target_name`" + `  TEXT     NOT NULL,
  ` + "`base`" + `         TEXT     NULL,
  ` + "`auto_merge`" + `   INTEGER  NOT NULL,
  ` + "`created_at`" + `   DATETIME NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// scheduled_task_sequence's own doc comment gives the reasoning for a
	// dedicated allocator rather than a counter column.
	`CREATE TABLE IF NOT EXISTS ` + "`task_template_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
)`,

	// scheduled_task_read's own doc comment gives the reasoning for a
	// table rather than a JSON column, ported onto a template's own id.
	`CREATE TABLE IF NOT EXISTS ` + "`task_template_read`" + ` (
  ` + "`task_template_id`" + ` TEXT NOT NULL,
  ` + "`owner`" + `            TEXT NOT NULL,
  ` + "`name`" + `             TEXT NOT NULL,
  PRIMARY KEY (` + "`task_template_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	// scheduled_task_grant's own shape, for a template's own capabilities.
	`CREATE TABLE IF NOT EXISTS ` + "`task_template_grant`" + ` (
  ` + "`task_template_id`" + ` TEXT NOT NULL,
  ` + "`capability`" + `       TEXT NOT NULL,
  ` + "`via`" + `              TEXT NOT NULL,
  ` + "`folder`" + `           TEXT NULL,
  PRIMARY KEY (` + "`task_template_id`" + `, ` + "`capability`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`grain_schema`" + ` (
  ` + "`id`" + `      INTEGER NOT NULL,
  ` + "`version`" + ` INTEGER NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// A deployment's tunable, non-secret configuration -- see config.go's
	// own doc comment on Config for what belongs here and what does not.
	// One row, like grain_schema: there is exactly one of these per
	// deployment, not one per something else, so there is nothing for a
	// second row to key against.
	//
	// target_repos was missing here from grain_config's very first
	// version (schema version 6, bwsalmon/agents#320) even though
	// Config.TargetRepos was already meant to be store-backed like every
	// other field: store.go's scanConfig/PutConfig never selected or
	// bound it, so a Settings change widening it was echoed back at
	// once but was never durable, silently reverting to unrestricted on
	// the very next read or restart -- bwsalmon/agents#427's actual root
	// cause, not merely a PAT-scope/credential-ladder mismatch. CREATE
	// TABLE IF NOT EXISTS does not add a column to a table that already
	// exists, so an already-created grain_config gets this one from
	// Store.Init's own migration step (store.go's
	// ensureConfigTargetReposColumn) instead of from this DDL.
	//
	// max_concurrent replaced a slots column (a comma-separated list of
	// operator-chosen concurrency-slot names) here for bwsalmon/agents#461:
	// Config.MaxConcurrent is a plain count now, and dispatch.Cycle's own
	// slot identifiers are generated from it (model.SlotNames) rather than
	// configured. The same CREATE TABLE IF NOT EXISTS limitation applies,
	// so an already-created grain_config gets max_concurrent added, and
	// backfilled from however many names its old slots column held, by
	// Store.Init's own ensureConfigMaxConcurrentColumn, which also drops
	// that now-unused column.
	`CREATE TABLE IF NOT EXISTS ` + "`grain_config`" + ` (
  ` + "`id`" + `                         INTEGER NOT NULL,
  ` + "`poll_interval_ms`" + `           INTEGER NOT NULL,
  ` + "`max_concurrent`" + `              INTEGER NOT NULL,
  ` + "`gemini_model`" + `                TEXT    NOT NULL,
  ` + "`max_agent_turns`" + `             INTEGER NOT NULL,
  ` + "`github_host`" + `                 TEXT    NOT NULL,
  ` + "`github_insecure_http`" + `        INTEGER NOT NULL,
  ` + "`gcp_project`" + `                 TEXT    NOT NULL,
  ` + "`gcp_service_account_email`" + `   TEXT    NOT NULL,
  ` + "`target_repos`" + `                TEXT    NOT NULL,
  ` + "`newest_first`" + `                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// A repo's release settings -- bwsalmon/agents#398's prod/rc branch
	// names, the prefix a cut or promoted branch is named under, and the
	// hand-edited major version an RC's own label starts from. One row per
	// repo, keyed the way task_read and every other repo-scoped table
	// already is (owner, name), rather than a single deployment-wide row
	// the way grain_config is: a deployment can run release management for
	// more than one repo at once.
	`CREATE TABLE IF NOT EXISTS ` + "`release_config`" + ` (
  ` + "`owner`" + `                 TEXT    NOT NULL,
  ` + "`name`" + `                  TEXT    NOT NULL,
  ` + "`prod_branch`" + `           TEXT    NOT NULL,
  ` + "`rc_branch`" + `             TEXT    NOT NULL,
  ` + "`release_branch_prefix`" + ` TEXT    NOT NULL,
  ` + "`major_version`" + `         INTEGER NOT NULL,
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `)
)`,

	// One cut release candidate. INTEGER PRIMARY KEY AUTOINCREMENT for the
	// same reason task_sequence uses it: cutting a new candidate has to
	// stay correct with more than one writer, and letting SQLite assign
	// the rowid is atomic where a read-then-increment column would race.
	//
	// major_version, number and version together are the candidate's own
	// label (CandidateLabel) -- captured here at cut time rather than
	// re-read from release_config, so a later hand-edit to major_version
	// never renames a candidate already cut under the old one. status is
	// TEXT rather than a constrained type for the reason schema.go's own
	// doc comment gives for every other enum column here: the vocabulary
	// lives in release.go, not the schema. last_error is the reconciler's
	// own report of why a cut or promotion it attempted did not land yet
	// -- cleared the moment the attempt that follows succeeds.
	`CREATE TABLE IF NOT EXISTS ` + "`release_candidate`" + ` (
  ` + "`id`" + `             INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`owner`" + `          TEXT     NOT NULL,
  ` + "`name`" + `           TEXT     NOT NULL,
  ` + "`major_version`" + `  INTEGER  NOT NULL,
  ` + "`number`" + `         INTEGER  NOT NULL,
  ` + "`version`" + `        INTEGER  NOT NULL,
  ` + "`branch`" + `         TEXT     NOT NULL,
  ` + "`release_branch`" + ` TEXT     NULL,
  ` + "`status`" + `         TEXT     NOT NULL,
  ` + "`created_at`" + `     DATETIME NOT NULL,
  ` + "`promoted_at`" + `    DATETIME NULL,
  ` + "`last_error`" + `     TEXT     NULL
)`,

	// What CurrentCandidate and the releases reconciler both need: every
	// candidate for one repo, newest first.
	`CREATE INDEX IF NOT EXISTS ` + "`release_candidate_repo`" + ` ON ` + "`release_candidate`" + ` (` + "`owner`" + `, ` + "`name`" + `, ` + "`id`" + `)`,

	// A repo's qualification setup -- bwsalmon/agents#518's two switches:
	// require_approval gates every task a run instantiates behind a
	// human's own bulk approval (Store.ApproveQualificationRun) rather
	// than landing pre-approved the way a schedule's own firing does, and
	// auto_promote is what lets the qualifications reconciler promote a
	// candidate itself the moment its run succeeds instead of leaving
	// that to a human. One row per repo, the same key release_config
	// already uses.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_config`" + ` (
  ` + "`owner`" + `            TEXT    NOT NULL,
  ` + "`name`" + `             TEXT    NOT NULL,
  ` + "`require_approval`" + ` INTEGER NOT NULL,
  ` + "`auto_promote`" + `     INTEGER NOT NULL,
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `)
)`,

	// One entry in a repo's qualification plan -- a task_template
	// (bwsalmon/agents#516) this plan schedules, referenced by id rather
	// than copied: content lives on the template, and
	// CreateQualificationRun resolves it fresh every time a candidate is
	// qualified, the same "not a stale copy" discipline
	// fireScheduledTask already holds a schedule's own TemplateID to.
	// repeat_count is model.QualificationItem's own Repeat; order_key is
	// display order only -- unlike task.order_key it decides nothing
	// about dispatch, since an item's actual scheduling order comes from
	// the dependency graph below, not from this column.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_item`" + ` (
  ` + "`owner`" + `        TEXT    NOT NULL,
  ` + "`name`" + `         TEXT    NOT NULL,
  ` + "`template_id`" + `  TEXT    NOT NULL,
  ` + "`repeat_count`" + ` INTEGER NOT NULL,
  ` + "`order_key`" + `    REAL    NOT NULL,
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `, ` + "`template_id`" + `)
)`,

	// One dependency edge between two items in the same plan --
	// depends_on_template_id names another item's own template_id, never
	// a task: the graph lives entirely among a plan's items, and
	// CreateQualificationRun is what turns an edge here into real
	// depends-on links between the task instances each side produced.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_item_depends_on`" + ` (
  ` + "`owner`" + `                  TEXT NOT NULL,
  ` + "`name`" + `                   TEXT NOT NULL,
  ` + "`template_id`" + `             TEXT NOT NULL,
  ` + "`depends_on_template_id`" + ` TEXT NOT NULL,
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `, ` + "`template_id`" + `, ` + "`depends_on_template_id`" + `)
)`,

	// One qualification run per candidate -- CreateQualificationRun's own
	// INSERT, guarded by the unique index below so a reconciler that
	// finds the same active candidate ready twice in a race can only ever
	// create one. INTEGER PRIMARY KEY AUTOINCREMENT for the same reason
	// release_candidate uses it: more than one writer has to stay correct.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_run`" + ` (
  ` + "`id`" + `           INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`owner`" + `        TEXT     NOT NULL,
  ` + "`name`" + `         TEXT     NOT NULL,
  ` + "`candidate_id`" + ` INTEGER  NOT NULL,
  ` + "`created_at`" + `   DATETIME NOT NULL
)`,

	`CREATE UNIQUE INDEX IF NOT EXISTS ` + "`qualification_run_candidate`" + ` ON ` + "`qualification_run`" + ` (` + "`candidate_id`" + `)`,

	// One task instance a qualification run instantiated from a
	// template. template_name is a snapshot of the resolved template's
	// own Name at the moment this instance was created --
	// model.QualificationTaskStatus's own doc comment on why -- and
	// instance_index/repeat_count together are what lets a UI show "2 of
	// 3" against an item whose Repeat is greater than one. task_id is
	// unique: a task belongs to at most one run, ever.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_task`" + ` (
  ` + "`id`" + `             INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`run_id`" + `        INTEGER NOT NULL,
  ` + "`task_id`" + `       TEXT    NOT NULL,
  ` + "`template_id`" + `   TEXT    NOT NULL,
  ` + "`template_name`" + ` TEXT    NOT NULL,
  ` + "`instance_index`" + ` INTEGER NOT NULL,
  ` + "`repeat_count`" + `  INTEGER NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`qualification_task_run`" + ` ON ` + "`qualification_task`" + ` (` + "`run_id`" + `)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + "`qualification_task_task`" + ` ON ` + "`qualification_task`" + ` (` + "`task_id`" + `)`,
}

// Views is the derivations, each a (name, DDL) pair so Init can drop and
// recreate one by name -- SQLite has no CREATE OR REPLACE VIEW, unlike
// the MySQL dialect Dolt spoke. Order matters: each view here may only
// name a table or an earlier view in this same slice, since Init applies
// them in order and a CREATE VIEW referencing something not yet created
// fails outright. task_state reads task_streak, and task_ready reads
// task_state and task_blocked -- all three appear before their reader.
var Views = []View{
	// How many of a task's most recent runs, in a row, ended without
	// succeeding -- zero the moment any of them succeeds, or once a human
	// asks for a retry (task_observation.retry_requested_at), whichever
	// is later. task_state reads this to know when to stop calling a task
	// 'queued' at all (bwsalmon/agents#403); it is its own view, not a
	// branch of task_state's own CASE, because task_state must be able to
	// join it the same way it already joins task_run and
	// task_observation -- a CASE branch has no name of its own to join
	// against a second time the way a WHEN condition needs here.
	//
	// The two correlated subqueries below stand in for a window function
	// (ROW_NUMBER/PARTITION BY, which SQLite does support): both express
	// "the most recent boundary after which every run counts", one for a
	// success, one for a retry request, and COALESCE'ing each against a
	// date no real row predates is what makes "never happened" fall out
	// of the same comparison as "happened, but a while ago" rather than
	// needing its own branch.
	{"task_streak", `
SELECT
  ` + "`tr`.`task_id`" + ` AS ` + "`task_id`" + `,
  COUNT(*) AS ` + "`streak`" + `
FROM ` + "`task_run`" + ` AS ` + "`tr`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`o`" + ` ON ` + "`o`.`task_id`" + ` = ` + "`tr`.`task_id`" + `
WHERE ` + "`tr`.`finished_at`" + ` IS NOT NULL
  AND ` + "`tr`.`outcome`" + ` != 'succeeded'
  AND ` + "`tr`.`started_at`" + ` > COALESCE((
        SELECT MAX(` + "`started_at`" + `) FROM ` + "`task_run`" + ` AS ` + "`tr2`" + `
        WHERE ` + "`tr2`.`task_id`" + ` = ` + "`tr`.`task_id`" + ` AND ` + "`tr2`.`outcome`" + ` = 'succeeded'
      ), '0001-01-01 00:00:00')
  AND ` + "`tr`.`started_at`" + ` > COALESCE(` + "`o`.`retry_requested_at`" + `, '0001-01-01 00:00:00')
GROUP BY ` + "`tr`.`task_id`" + ``},

	// The invariant, as a view. Precedence top to bottom, matching
	// StateOf exactly — see state_test.go on what holds them together.
	// 'failed' sits between 'running' and 'proposed': a task with a live
	// run is running whatever its streak says (task_ready cannot have
	// dispatched a capped task in the first place, but a run started
	// before this cutoff shipped, or before a human lowered
	// MaxConsecutiveFailures, should still be let to finish), and a
	// proposed task can carry no streak at all -- dispatch never ran an
	// unapproved task -- so the two branches never compete in practice.
	{"task_state", `
SELECT
  ` + "`t`.`id`" + ` AS ` + "`task_id`" + `,
  CASE
    WHEN ` + "`o`.`closed_at`" + `    IS NOT NULL THEN 'closed'
    WHEN ` + "`o`.`completed_at`" + ` IS NOT NULL THEN 'completed'
    WHEN ` + "`o`.`pending_question_comment_id`" + ` IS NOT NULL THEN 'awaiting_reply'
    WHEN ` + "`r`.`id`" + ` IS NOT NULL THEN 'running'
    WHEN COALESCE(` + "`st`.`streak`" + `, 0) >= ` + strconv.Itoa(MaxConsecutiveFailures) + ` THEN 'failed'
    WHEN ` + "`t`.`approval_actor_kind`" + ` IS NULL THEN 'proposed'
    ELSE 'queued'
  END AS ` + "`state`" + `
FROM ` + "`task`" + ` AS ` + "`t`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`o`" + ` ON ` + "`o`.`task_id`" + ` = ` + "`t`.`id`" + `
LEFT JOIN ` + "`task_run`" + ` AS ` + "`r`" + `
       ON ` + "`r`.`task_id`" + ` = ` + "`t`.`id`" + ` AND ` + "`r`.`finished_at`" + ` IS NULL
LEFT JOIN ` + "`task_streak`" + ` AS ` + "`st`" + ` ON ` + "`st`.`task_id`" + ` = ` + "`t`.`id`" + ``},

	// Blocked is an annotation on queued, never a state of its own, so it
	// gets its own view rather than another branch of the CASE above. A
	// link whose target is not a task at all — a review thread, a pull
	// request — never blocks, which falls out of the join rather than
	// needing a rule.
	{"task_blocked", `
SELECT
  ` + "`l`.`task_id`" + ` AS ` + "`task_id`" + `,
  COUNT(*)      AS ` + "`open_blockers`" + `
FROM ` + "`task_link`" + ` AS ` + "`l`" + `
JOIN ` + "`task`" + ` AS ` + "`bt`" + ` ON ` + "`bt`.`id`" + ` = ` + "`l`.`target`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`bo`" + ` ON ` + "`bo`.`task_id`" + ` = ` + "`l`.`target`" + `
WHERE ` + "`l`.`blocks`" + ` = TRUE AND ` + "`bo`.`closed_at`" + ` IS NULL
GROUP BY ` + "`l`.`task_id`" + ``},

	// What is dispatchable right now: approved, not running, nothing open
	// in front of it. The whole dispatch query, as one view.
	{"task_ready", `
SELECT ` + "`s`.`task_id`" + ` AS ` + "`task_id`" + `
FROM ` + "`task_state`" + ` AS ` + "`s`" + `
LEFT JOIN ` + "`task_blocked`" + ` AS ` + "`b`" + ` ON ` + "`b`.`task_id`" + ` = ` + "`s`.`task_id`" + `
WHERE ` + "`s`.`state`" + ` = 'queued'
  AND COALESCE(` + "`b`.`open_blockers`" + `, 0) = 0`},

	// Every lease still outstanding, with the credential that minted it —
	// which makes "what would I break by rotating this?" a query.
	{"lease_live", `
SELECT
  ` + "`l`.`run_id`" + `     AS ` + "`run_id`" + `,
  ` + "`r`.`task_id`" + `    AS ` + "`task_id`" + `,
  ` + "`l`.`capability`" + ` AS ` + "`capability`" + `,
  ` + "`l`.`resource`" + `   AS ` + "`resource`" + `,
  ` + "`l`.`minted_by`" + `  AS ` + "`minted_by`" + `,
  ` + "`l`.`issued_at`" + `  AS ` + "`issued_at`" + `,
  ` + "`l`.`expires_at`" + ` AS ` + "`expires_at`" + `
FROM ` + "`lease`" + ` AS ` + "`l`" + `
JOIN ` + "`task_run`" + ` AS ` + "`r`" + ` ON ` + "`r`.`id`" + ` = ` + "`l`.`run_id`" + `
WHERE ` + "`r`.`finished_at`" + ` IS NULL`},
}

// View is one derivation: a name, and the SELECT that defines it.
type View struct {
	Name string
	DDL  string // a SELECT statement, with no CREATE VIEW/AS around it
}

// Statements is the order Open applies things in: tables, then views
// (each dropped first, so a changed definition always wins over
// whatever an older build left behind -- a view has no data of its own
// to lose), a view referencing a missing table being an error.
func Statements() []string {
	out := make([]string, 0, len(Tables)+len(Views)*2)
	out = append(out, Tables...)
	for _, v := range Views {
		out = append(out,
			"DROP VIEW IF EXISTS `"+v.Name+"`",
			"CREATE VIEW `"+v.Name+"` AS"+v.DDL)
	}
	return out
}
