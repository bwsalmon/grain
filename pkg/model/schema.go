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
// not reject an unknown value, which the Valid methods and their own
// tests cover instead. Timestamps keep DATETIME rather than MySQL's DATETIME(6):
// SQLite has no timestamp storage class of its own, and this is what
// tells modernc.org/sqlite (the driver pkg/model/sqlite opens) to hand a
// column back as a time.Time rather than the TEXT it stores under the
// hood -- store.go's sql.NullTime scanning and time.Time binding are
// otherwise unchanged from what they were against Dolt. Booleans are
// INTEGER (0/1) -- SQLite has no separate boolean storage class either,
// and database/sql converts either direction without help from this
// package (pinned by pkg/model/sqlite's own store tests).

// SchemaVersion is bumped whenever Tables or Views change in a way an
// existing database cannot simply be re-created into. Open records this
// and refuses a database written by a newer build, rather than failing
// later with a confusing missing column.
//
// 16 is the removal of slots: task_run lost its slot column and its
// one-open-run-per-slot index. Nothing migrates a database across that
// -- Init's own CREATE TABLE IF NOT EXISTS never alters a table that
// already exists, so an older store would keep a slot column still
// declared NOT NULL with no default, which every startRun would then
// fail to satisfy. This bump is what tells ../scripts/setup.sh to move
// such a store aside at deploy instead (cmd/grain/schemaversion.go's own
// doc comment).
const SchemaVersion = 16

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
  ` + "`sandbox_cpus`" + `          INTEGER  NOT NULL DEFAULT 0,
  ` + "`sandbox_memory_mb`" + `     INTEGER  NOT NULL DEFAULT 0,
  ` + "`sandbox_disk_gb`" + `       INTEGER  NOT NULL DEFAULT 0,
  ` + "`interactive`" + `           INTEGER  NOT NULL DEFAULT 0,
  ` + "`configuration`" + `         INTEGER  NOT NULL DEFAULT 0,
  ` + "`agent_framework`" + `       TEXT     NOT NULL DEFAULT '',
  ` + "`prompt_extension`" + `      TEXT     NOT NULL DEFAULT '',
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
  ` + "`merge_queue_refreshed_at`" + `    DATETIME NULL,
  ` + "`merge_queue_repair_at`" + `       DATETIME NULL,
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
	//
	// agent_started_at is the one moment inside a run that nothing else
	// records: when its agent actually got its first turn. started_at is
	// stamped by dispatch, before any sandbox exists (Store.SetRunSandbox's
	// own doc comment), so finished_at - started_at is setup *and* agent
	// work together -- a VM boot, a checkout and a capability mint on one
	// side, whatever the agent then did on the other. Splitting the two is
	// the whole point: README's own "What this costs" says a VM boot moved
	// onto the critical path of every task and is "worth measuring before
	// reaching for" a golden image or a warm spare, and this column is what
	// makes that a query (pkg/metrics' SetupLatency vs AgentLatency).
	//
	// Written once, by SetRunAgentStarted, immediately before
	// orchestrator.RunDispatch hands the run to agent.Framework.Run -- the
	// same after-the-fact shape SetRunOutcome and SetRunTranscript already
	// use. It stays NULL for a run still in setup, and for one that never
	// reached its agent at all (outcome "setup-failed", a checkout that
	// would not clone), which is exactly the distinction a reader wants:
	// no agent latency to report, because no agent ran.
	//
	// prompt is the whole text the agent was actually handed for this run
	// -- the task's own title and body plus everything grain adds to them
	// (orchestrator.BuildPrompt's branch and repo sentences, the
	// conversation so far, the attachments section, and each capability's
	// own prompt section). Nothing else records it: the prompt is
	// assembled per run, from a task that may since have been edited and
	// a conversation that has since grown, so it cannot be reconstructed
	// after the fact from the task alone -- which is exactly why "what
	// were you actually told?" needs a column rather than a re-derivation.
	// It carries no credential material by construction (every capability
	// PromptSection names its credential and hands over none -- see
	// githubtoken and gcpkey's own tests), which is what makes it safe to
	// show in the UI.
	//
	// Written once, by SetRunPrompt, immediately before
	// orchestrator.RunDispatch hands the run to agent.Framework.Run, and
	// so NULL for a run that never reached its agent at all (a checkout
	// that would not clone, a capability that would not mint) as well as
	// for every run recorded before this column existed.
	`CREATE TABLE IF NOT EXISTS ` + "`task_run`" + ` (
  ` + "`id`" + `               TEXT     NOT NULL,
  ` + "`task_id`" + `          TEXT     NOT NULL,
  ` + "`sandbox`" + `          TEXT     NOT NULL,
  ` + "`unit`" + `             TEXT     NULL,
  ` + "`attempt`" + `          INTEGER  NOT NULL,
  ` + "`started_at`" + `       DATETIME NOT NULL,
  ` + "`agent_started_at`" + ` DATETIME NULL,
  ` + "`finished_at`" + `      DATETIME NULL,
  ` + "`outcome`" + `          TEXT     NULL,
  ` + "`detail`" + `           TEXT     NULL,
  ` + "`transcript`" + `       TEXT     NULL,
  ` + "`prompt`" + `           TEXT     NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// At most one open (finished_at IS NULL) run per task -- what is left
	// of the DB-level backstop bwsalmon/agents#434 asked for once slots
	// stopped existing. It used to be one open run per *slot*: dispatch.
	// Cycle read OccupiedSlots and Ready outside any single transaction
	// and then issued one StartRun per free slot it found, so two
	// overlapping Cycle calls could both see the same slot as free and
	// both dispatch onto it, and this index was what made the second one
	// fail loudly instead of quietly landing a second live run on a
	// claimed slot.
	//
	// A sandbox per task removes that race at its source rather than
	// catching it here: startRun now counts live runs and enforces the
	// concurrency limit inside its own transaction (its doc comment), so
	// there is no window between deciding a run may start and recording
	// that it has. What is still worth an index is the invariant that
	// outlived slots -- a task has at most one run in flight at a time,
	// which task_state already assumes when it reads a live run as
	// 'running', and which dispatch.Cycle relies on when it takes
	// task_ready's own order at face value. A partial index rather than a
	// plain UNIQUE column, since the same task legitimately appears in
	// many finished rows over a store's lifetime (one per attempt) and
	// only the currently-open one must be unique.
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + "`task_run_open_task`" + ` ON ` + "`task_run`" + ` (` + "`task_id`" + `) WHERE ` + "`finished_at`" + ` IS NULL`,

	// task_run_tool is the per-run, per-tool census: how many times a run
	// called each tool, how many of those calls came back as errors, how
	// many the tool's own bound cut off, and how big the answers were.
	// telemetry.go's own doc comment argues for storing it at all -- it is
	// the one measurement in this schema that is not derivable from
	// something else, because agent.Result does not survive the run.
	//
	// size_buckets is a base-2 histogram of result sizes
	// (model.SizeHistogram), encoded as "bucket:count" pairs. A column
	// rather than a table because it is only ever read whole, with the row
	// it belongs to, and never joined or queried on.
	//
	// Neither this table nor task_run_check_wait below needs a
	// SchemaVersion bump, and neither needs a rung on store.go's migration
	// ladder: Init's own CREATE TABLE IF NOT EXISTS creates a *missing*
	// table on an existing database perfectly well -- what it cannot do is
	// alter one that is already there, which is what every ensure*Column
	// migration and every bump exists for. An older store simply gains two
	// empty tables and starts filling them from its next run.
	//
	// Written once, by orchestrator.RunDispatch, after FinishRun has
	// recorded the run's own outcome -- the same after-the-fact shape
	// task_run.transcript uses. A run that never reached its agent, or
	// that made no tool calls at all, gets no rows here rather than rows
	// of zeroes: that fact is already in its outcome, and a zero row would
	// only dilute the per-tool rates it was averaged into.
	`CREATE TABLE IF NOT EXISTS ` + "`task_run_tool`" + ` (
  ` + "`run_id`" + `           TEXT     NOT NULL,
  ` + "`tool`" + `             TEXT     NOT NULL,
  ` + "`calls`" + `            INTEGER  NOT NULL,
  ` + "`errored`" + `          INTEGER  NOT NULL,
  ` + "`timed_out`" + `        INTEGER  NOT NULL,
  ` + "`result_bytes`" + `     INTEGER  NOT NULL,
  ` + "`max_result_bytes`" + ` INTEGER  NOT NULL,
  ` + "`size_buckets`" + `     TEXT     NULL,
  PRIMARY KEY (` + "`run_id`" + `, ` + "`tool`" + `)
)`,

	// task_run_check_wait is one row per wait_for_checks call: the CI loop
	// BuildPrompt sends every run around, measured end to end.
	//
	// A row per call rather than a per-run total because the sequence is
	// the question. "How many pushes does a run take before its checks go
	// green?" is read off the first wait that ended
	// mcp.WaitVerdictPassed, and a per-run average could not answer it;
	// nor could it show a deployment where most waits end with the clock
	// running out, which is mcp.DefaultWaitForChecksTimeout set wrong for
	// that CI. A run makes a handful of these calls, not hundreds, so the
	// per-call shape costs a handful of rows.
	`CREATE TABLE IF NOT EXISTS ` + "`task_run_check_wait`" + ` (
  ` + "`run_id`" + `        TEXT     NOT NULL,
  ` + "`seq`" + `           INTEGER  NOT NULL,
  ` + "`verdict`" + `       TEXT     NOT NULL,
  ` + "`waited_ms`" + `     INTEGER  NOT NULL,
  ` + "`pushes_before`" + ` INTEGER  NOT NULL,
  PRIMARY KEY (` + "`run_id`" + `, ` + "`seq`" + `)
)`,

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

	// One row per file (bwsalmon/agents#522), content included -- the same
	// "the store is durable; nothing needs to stay reachable to read it
	// back" reasoning task_comment's own doc comment gives for the
	// conversation itself, rather than a path on disk or in a bucket that
	// this row would have to keep pointing at correctly forever.
	// comment_id is NULL for a file carried by the task's own body, and a
	// task_comment.id for one carried by a later comment -- both share
	// this table rather than two, since a dispatched run materializing
	// them into a sandbox (orchestrator's AttachmentsDir) treats every
	// attachment exactly alike regardless of which one it came from.
	// content_type and size are carried alongside content rather than
	// derived from it on every read: content_type is what a browser's
	// file picker already told the upload (and is not always reliably
	// re-derivable from bytes alone), and size is what a listing needs
	// without paying to read every attachment's full content back out of
	// the database just to report how big it is.
	`CREATE TABLE IF NOT EXISTS ` + "`task_attachment`" + ` (
  ` + "`id`" + `           INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`task_id`" + `      TEXT     NOT NULL,
  ` + "`comment_id`" + `   INTEGER  NULL,
  ` + "`filename`" + `     TEXT     NOT NULL,
  ` + "`content_type`" + ` TEXT     NOT NULL,
  ` + "`size`" + `         INTEGER  NOT NULL,
  ` + "`content`" + `      BLOB     NOT NULL,
  ` + "`created_at`" + `   DATETIME NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`task_attachment_task`" + ` ON ` + "`task_attachment`" + ` (` + "`task_id`" + `)`,

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
	// task_sequence's own reasoning applies again for a firing's
	// identity, but the schedule itself needs exactly one durable row to
	// carry next_run_at/last_run_at and the enabled switch a UI toggles.
	// recurrence_kind/every_n_hours/time_of_day_minutes/weekday/day_of_month
	// are model.Recurrence's fields (bwsalmon/agents#464) -- only the
	// subset matching recurrence_kind is ever non-NULL for a given row
	// (model.Recurrence.Validate's own doc comment has the mapping), the
	// same "column per case, most left NULL" shape task's own
	// approval_actor_kind columns already use for a different sum type.
	// This replaces the original bare interval_ms (every N hours since it
	// last fired, with no wall-clock alignment); a database from before
	// that widening is migrated by ensureScheduleRecurrenceColumns.
	//
	// template_id and suite_id are what a firing actually is: both NULL
	// for a schedule carrying its own inline task content, template_id
	// for one firing a template's content instead, suite_id for one
	// starting a suite run instead of filing a task at all
	// (model.Schedule's own doc comment on why the two are mutually
	// exclusive). A database created before either existed gets the
	// column from
	// ensureScheduleTemplateColumn/ensureScheduleSuiteColumn rather
	// than from this DDL.
	`CREATE TABLE IF NOT EXISTS ` + "`schedule`" + ` (
  ` + "`id`" + `                    TEXT     NOT NULL,
  ` + "`title`" + `                 TEXT     NOT NULL,
  ` + "`body`" + `                  TEXT     NOT NULL,
  ` + "`target_owner`" + `          TEXT     NOT NULL,
  ` + "`target_name`" + `           TEXT     NOT NULL,
  ` + "`base`" + `                  TEXT     NULL,
  ` + "`auto_merge`" + `            INTEGER  NOT NULL,
  ` + "`template_id`" + `           TEXT     NULL,
  ` + "`suite_id`" + `              TEXT     NULL,
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
	`CREATE TABLE IF NOT EXISTS ` + "`schedule_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
)`,

	// task_read's own doc comment gives the reasoning for a table rather
	// than a JSON column, applied again here now that a schedule's firing
	// carries read-only repos too (bwsalmon/agents#464).
	`CREATE TABLE IF NOT EXISTS ` + "`schedule_read`" + ` (
  ` + "`schedule_id`" + ` TEXT NOT NULL,
  ` + "`owner`" + `       TEXT NOT NULL,
  ` + "`name`" + `        TEXT NOT NULL,
  PRIMARY KEY (` + "`schedule_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	// task_grant's own shape, for a schedule's own capabilities
	// (bwsalmon/agents#464) -- via is always 'label' here (a human chose
	// it in the schedule's own form, same as grantsFor does for a task),
	// but the column exists anyway so scanning stays identical to
	// task_grant's.
	`CREATE TABLE IF NOT EXISTS ` + "`schedule_grant`" + ` (
  ` + "`schedule_id`" + ` TEXT NOT NULL,
  ` + "`capability`" + `  TEXT NOT NULL,
  ` + "`via`" + `         TEXT NOT NULL,
  ` + "`folder`" + `      TEXT NULL,
  PRIMARY KEY (` + "`schedule_id`" + `, ` + "`capability`" + `)
)`,

	// One row per template (bwsalmon/agents#516) -- task_sequence's own
	// "identity allocated here, not borrowed" reasoning applies again:
	// name/title/body/auto_merge are exactly the reusable-content
	// fields a schedule already carried inline, now given a row of
	// their own so more than one schedule (schedule.template_id) can
	// point at the same one instead of repeating it. Deliberately no
	// target_owner/target_name/base here (model.Template's own doc
	// comment on why): which repo and branch a firing targets is a
	// property of the caller using this template, not of the template
	// itself.
	`CREATE TABLE IF NOT EXISTS ` + "`template`" + ` (
  ` + "`id`" + `           TEXT     NOT NULL,
  ` + "`name`" + `         TEXT     NOT NULL,
  ` + "`title`" + `        TEXT     NOT NULL,
  ` + "`body`" + `         TEXT     NOT NULL,
  ` + "`auto_merge`" + `   INTEGER  NOT NULL,
  ` + "`created_at`" + `   DATETIME NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// schedule_sequence's own doc comment gives the reasoning for a
	// dedicated allocator rather than a counter column.
	`CREATE TABLE IF NOT EXISTS ` + "`template_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
)`,

	// schedule_read's own doc comment gives the reasoning for a table
	// rather than a JSON column, ported onto a template's own id.
	`CREATE TABLE IF NOT EXISTS ` + "`template_read`" + ` (
  ` + "`template_id`" + ` TEXT NOT NULL,
  ` + "`owner`" + `       TEXT NOT NULL,
  ` + "`name`" + `        TEXT NOT NULL,
  PRIMARY KEY (` + "`template_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	// schedule_grant's own shape, for a template's own capabilities.
	`CREATE TABLE IF NOT EXISTS ` + "`template_grant`" + ` (
  ` + "`template_id`" + ` TEXT NOT NULL,
  ` + "`capability`" + `  TEXT NOT NULL,
  ` + "`via`" + `         TEXT NOT NULL,
  ` + "`folder`" + `      TEXT NULL,
  PRIMARY KEY (` + "`template_id`" + `, ` + "`capability`" + `)
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
	// max_workers and max_mergers are the two halves of this deployment's
	// concurrency (model.Limits, grain/task-63): how many runs of
	// ordinary work may be live, and how much further capacity only the
	// merge queue's own fix tasks may reach.
	//
	// max_workers is the descendant of a slots column (a comma-separated
	// list of operator-chosen concurrency-slot names), which
	// bwsalmon/agents#461 replaced with a plain max_concurrent count, and
	// which grain/task-63 renamed once it stopped being the whole limit.
	// The same CREATE TABLE IF NOT EXISTS limitation applies to both
	// columns, so an already-created grain_config gets max_concurrent
	// added and backfilled from its old slots column by Store.Init's own
	// ensureConfigMaxConcurrentColumn, and then gets max_workers
	// (backfilled from max_concurrent, which is dropped) and max_mergers
	// from ensureConfigWorkerMergerColumns after it. max_mergers DEFAULT
	// 1 is model.DefaultMaxMergers, the same number DefaultConfig carries
	// for every row written from Go rather than defaulted by the engine.
	//
	// Both carry a DEFAULT, unlike the columns beside them that predate
	// the convention, and max_workers' own is what keeps a *downgrade*
	// survivable: a build from before the split writes a settings row
	// naming max_concurrent and not max_workers (its own migration
	// re-adds the column this one dropped), which a NOT NULL column with
	// no default would reject outright. The setting such a write leaves
	// behind is wrong until something sets it again; the alternative is a
	// deployment that cannot save settings at all.
	//
	// agent_framework (bwsalmon/agents#609) is Config.AgentFramework's own
	// column -- DEFAULT 'antigravity' both here and in
	// Store.ensureConfigAgentFrameworkColumn (the same CREATE TABLE IF NOT
	// EXISTS limitation means an already-created grain_config gets it from
	// there instead), naming the framework a deployment that has never
	// chosen one runs. A database written before agent/antigravity
	// replaced the home-grown Gemini runtime may hold the legacy 'gemini'
	// spelling instead; model.NormalizeAgentFramework folds that back in
	// on read rather than a migration rewriting the row.
	//
	// claude_model is Config.ClaudeModel's own column -- agent/claude's
	// counterpart to gemini_model, added the same way (DEFAULT '' both
	// here and in Store.ensureConfigClaudeModelColumn for an
	// already-created grain_config). codex_model is agent/codex's, added
	// the same way again (Store.ensureConfigCodexModelColumn).
	//
	// approved_by_default and auto_merge_by_default (Config.ApprovedByDefault/
	// AutoMergeByDefault) are DEFAULT 1, not 0: both settings are on for a
	// deployment that has never chosen either way, the same default
	// model.DefaultConfig carries for every row written from Go rather
	// than defaulted by the engine. task_defaults_on_backfilled is not a
	// setting at all but the ledger for that change of default: its
	// *presence* records that Store.ensureConfigTaskDefaultsOn has
	// already run against this database, so the one-time backfill it does
	// can never run twice and undo an operator who has since turned
	// either setting off. Its value is never read by anything -- PutConfig
	// does not name it, and REPLACE re-defaults it on every settings save,
	// so the value carries no information to read.
	//
	// default_capabilities is Config.DefaultCapabilities' own column --
	// the capability ids a new task is filed holding -- stored the same
	// comma-separated way target_repos is (store.go's joinCSV/splitCSV; a
	// capability id can no more contain a comma than an owner/name repo
	// can), DEFAULT '' both here and in
	// Store.ensureConfigDefaultCapabilitiesColumn for an already-created
	// grain_config. Empty, the default, means a new task starts with only
	// the capabilities whoever files it asked for -- exactly what every
	// deployment did before this column existed.
	//
	// environment_name is Config.EnvironmentName's own column -- what
	// this deployment is called on screen -- DEFAULT '' both here and in
	// Store.ensureConfigEnvironmentNameColumn for an already-created
	// grain_config. Empty is an unnamed deployment, which is what every
	// deployment was before this column existed.
	//
	// prompt_extension is Config.PromptExtension's own column -- the
	// standing instructions every dispatch's prompt ends with
	// (prompt_extension.go) -- DEFAULT '' both here and in
	// Store.ensureConfigPromptExtensionColumn for an already-created
	// grain_config. Empty adds nothing to a prompt, which is what every
	// deployment did before this column existed. TEXT with no length of
	// its own: it is prose for an agent, the same as task.body beside it.
	`CREATE TABLE IF NOT EXISTS ` + "`grain_config`" + ` (
  ` + "`id`" + `                         INTEGER NOT NULL,
  ` + "`poll_interval_ms`" + `           INTEGER NOT NULL,
  ` + "`max_workers`" + `                 INTEGER NOT NULL DEFAULT 1,
  ` + "`max_mergers`" + `                 INTEGER NOT NULL DEFAULT 1,
  ` + "`gemini_model`" + `                TEXT    NOT NULL,
  ` + "`max_agent_turns`" + `             INTEGER NOT NULL,
  ` + "`github_host`" + `                 TEXT    NOT NULL,
  ` + "`github_insecure_http`" + `        INTEGER NOT NULL,
  ` + "`gcp_project`" + `                 TEXT    NOT NULL,
  ` + "`gcp_service_account_email`" + `   TEXT    NOT NULL,
  ` + "`target_repos`" + `                TEXT    NOT NULL,
  ` + "`newest_first`" + `                INTEGER NOT NULL DEFAULT 0,
  ` + "`sandbox_cpus`" + `                INTEGER NOT NULL DEFAULT 0,
  ` + "`sandbox_memory_mb`" + `            INTEGER NOT NULL DEFAULT 0,
  ` + "`sandbox_disk_gb`" + `              INTEGER NOT NULL DEFAULT 0,
  ` + "`show_closed_by_default`" + `       INTEGER NOT NULL DEFAULT 0,
  ` + "`agent_framework`" + `              TEXT    NOT NULL DEFAULT 'antigravity',
  ` + "`approved_by_default`" + `          INTEGER NOT NULL DEFAULT 1,
  ` + "`auto_merge_by_default`" + `        INTEGER NOT NULL DEFAULT 1,
  ` + "`claude_model`" + `                 TEXT    NOT NULL DEFAULT '',
  ` + "`codex_model`" + `                  TEXT    NOT NULL DEFAULT '',
  ` + "`task_defaults_on_backfilled`" + `  INTEGER NOT NULL DEFAULT 1,
  ` + "`default_capabilities`" + `         TEXT    NOT NULL DEFAULT '',
  ` + "`environment_name`" + `             TEXT    NOT NULL DEFAULT '',
  ` + "`prompt_extension`" + `             TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (` + "`id`" + `)
)`,

	// A repo's release settings -- bwsalmon/agents#398's prod/rc branch
	// One named release branch set (bwsalmon/agents#571) -- a repo may
	// have any number of these at once, each with its own name and its own
	// independent candidate sequence, unlike bwsalmon/agents#398's own
	// release_config, which this table replaces and which held exactly one
	// singleton row per repo. INTEGER PRIMARY KEY AUTOINCREMENT for the
	// same reason release_candidate's own id is: starting a fresh release
	// under a name that already merged has to stay correct with more than
	// one writer. `repo` is the target repo's own name, kept apart from
	// `name` -- the release's own user-given name -- since a release is no
	// longer 1:1 with a repo the way release_config's row was. status is
	// TEXT rather than a constrained type for the reason schema.go's own
	// doc comment gives for every other enum column here: the vocabulary
	// lives in release.go, not the schema. last_error is the reconciler's
	// own report of why provisioning or a requested merge has not landed
	// yet -- cleared the moment the attempt that follows succeeds.
	// merge_note is the opposite of an error and is why it is not kept in
	// last_error: a release that reached `merged` with no pull request of
	// its own, because its prod branch carried nothing the default branch
	// did not already have, says so here (Release.MergeNote).
	`CREATE TABLE IF NOT EXISTS ` + "`release`" + ` (
  ` + "`id`" + `                INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`owner`" + `             TEXT     NOT NULL,
  ` + "`repo`" + `              TEXT     NOT NULL,
  ` + "`name`" + `              TEXT     NOT NULL,
  ` + "`status`" + `            TEXT     NOT NULL,
  ` + "`created_at`" + `        DATETIME NOT NULL,
  ` + "`merged_at`" + `         DATETIME NULL,
  ` + "`pull_request_url`" + `  TEXT     NULL,
  ` + "`last_error`" + `        TEXT     NULL,
  ` + "`merge_note`" + `        TEXT     NULL
)`,

	// What GetRelease and ListReleases both need: every release for one
	// repo, or by one repo and name, newest first.
	`CREATE INDEX IF NOT EXISTS ` + "`release_repo`" + ` ON ` + "`release`" + ` (` + "`owner`" + `, ` + "`repo`" + `, ` + "`name`" + `, ` + "`id`" + `)`,

	// One cut release candidate. INTEGER PRIMARY KEY AUTOINCREMENT for the
	// same reason task_sequence uses it: cutting a new candidate has to
	// stay correct with more than one writer, and letting SQLite assign
	// the rowid is atomic where a read-then-increment column would race.
	//
	// release_id names the release (above) this candidate belongs to;
	// number is scoped to that release alone, starting back at 1 for every
	// fresh release, unlike bwsalmon/agents#398's own candidate number,
	// which never reset for the life of a repo's single release_config.
	// `owner`/`repo` duplicate the release's own target repo, the same
	// "every repo, oldest first" shape PendingCandidates and
	// QualifiableActiveCandidates both need without a join back to
	// `release` for it. There is no release_branch column the way
	// bwsalmon/agents#398's own release_candidate had: promoting moves the
	// release's own prod branch (release.name, unadorned) forward, and
	// that is the only permanent branch a promotion touches now.
	`CREATE TABLE IF NOT EXISTS ` + "`release_candidate`" + ` (
  ` + "`id`" + `             INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`release_id`" + `     INTEGER  NOT NULL,
  ` + "`owner`" + `          TEXT     NOT NULL,
  ` + "`repo`" + `           TEXT     NOT NULL,
  ` + "`number`" + `         INTEGER  NOT NULL,
  ` + "`branch`" + `         TEXT     NOT NULL,
  ` + "`status`" + `         TEXT     NOT NULL,
  ` + "`created_at`" + `     DATETIME NOT NULL,
  ` + "`promoted_at`" + `    DATETIME NULL,
  ` + "`last_error`" + `     TEXT     NULL
)`,

	// What CurrentCandidateForRelease and the releases reconciler both
	// need: every candidate for one release, newest first.
	`CREATE INDEX IF NOT EXISTS ` + "`release_candidate_release`" + ` ON ` + "`release_candidate`" + ` (` + "`release_id`" + `, ` + "`id`" + `)`,

	// One requested branch (bwsalmon/agents#638) -- a repo page's own
	// "create a new branch," recorded here as a declared intent the same
	// way `release` and `release_candidate` are: `status` walks pending ->
	// created, TEXT rather than a constrained type for the same reason
	// every other enum column here is (the vocabulary lives in branch.go,
	// not the schema); `last_error` is the branches reconciler's own
	// report of why a create has not landed yet, cleared the moment it
	// succeeds. There is no uniqueness constraint on (`owner`,`repo`,
	// `name`): whether a name collides with something already on GitHub is
	// a fact only GitHub itself holds, so a collision only ever surfaces
	// as `last_error`, not as a rejected insert.
	`CREATE TABLE IF NOT EXISTS ` + "`branch`" + ` (
  ` + "`id`" + `           INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`owner`" + `        TEXT     NOT NULL,
  ` + "`repo`" + `         TEXT     NOT NULL,
  ` + "`name`" + `         TEXT     NOT NULL,
  ` + "`status`" + `       TEXT     NOT NULL,
  ` + "`created_at`" + `   DATETIME NOT NULL,
  ` + "`last_error`" + `   TEXT     NULL
)`,

	// What the repo page's own branch list needs: every branch ever
	// requested for one repo, newest first.
	`CREATE INDEX IF NOT EXISTS ` + "`branch_repo`" + ` ON ` + "`branch`" + ` (` + "`owner`" + `, ` + "`repo`" + `, ` + "`id`" + `)`,

	// A repo's own configuration -- model.RepoConfig, the per-repo layer
	// of what grain_config already says for the whole deployment
	// (grain/task-24). One row per repo, the same (owner, name) key
	// qualification_config below uses, and a row exists only while the
	// repo has something of its own to say: PutRepoConfig deletes rather
	// than writing a row that says nothing.
	//
	// default_capabilities is stored the comma-separated way
	// grain_config.default_capabilities and target_repos already are
	// (store.go's joinCSV/splitCSV), for the same reason -- a capability
	// id is a bare word with no room for a comma in it. No DEFAULT and no
	// migration: that column is as old as this table, so Init's own
	// CREATE TABLE IF NOT EXISTS creates the pair together on an existing
	// database the same as on a fresh one, and a deployment that upgrades
	// across it starts with no repo saying anything -- exactly what every
	// deployment did before it existed.
	//
	// prompt_extension is RepoConfig.PromptExtension's own column -- this
	// repo's standing instructions, appended to the deployment's for a
	// task that targets it (prompt_extension.go). Unlike
	// default_capabilities it *is* newer than the table, so it carries a
	// DEFAULT '' and Store.ensureRepoConfigPromptExtensionColumn adds it
	// to a repo_config that already exists.
	//
	// setup_command is RepoConfig.SetupCommand's own column -- the shell
	// orchestrator.prepareCheckout runs in the fresh checkout before a
	// run's first turn (grain/task-154). Newer than the table again, so
	// it carries the same DEFAULT '' and the same
	// Store.ensureRepoConfigSetupCommandColumn migration alongside it.
	`CREATE TABLE IF NOT EXISTS ` + "`repo_config`" + ` (
  ` + "`owner`" + `                TEXT NOT NULL,
  ` + "`name`" + `                 TEXT NOT NULL,
  ` + "`default_capabilities`" + ` TEXT NOT NULL,
  ` + "`prompt_extension`" + `     TEXT NOT NULL DEFAULT '',
  ` + "`setup_command`" + `        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `)
)`,

	// A repo's qualification setup -- bwsalmon/agents#518's two switches:
	// require_approval gates every task a run instantiates behind a
	// human's own bulk approval (Store.ApproveQualificationRun) rather
	// than landing pre-approved the way a schedule's own firing does, and
	// auto_promote is what lets the qualifications reconciler promote a
	// candidate itself the moment its run succeeds instead of leaving
	// that to a human. One row per repo, the same (owner, name) key every
	// other repo-scoped, one-row-per-repo table here already uses.
	`CREATE TABLE IF NOT EXISTS ` + "`qualification_config`" + ` (
  ` + "`owner`" + `            TEXT    NOT NULL,
  ` + "`name`" + `             TEXT    NOT NULL,
  ` + "`require_approval`" + ` INTEGER NOT NULL,
  ` + "`auto_promote`" + `     INTEGER NOT NULL,
  PRIMARY KEY (` + "`owner`" + `, ` + "`name`" + `)
)`,

	// One entry in a repo's qualification plan -- a template
	// (bwsalmon/agents#516) this plan schedules, referenced by id
	// rather than copied: content lives on the template, and
	// CreateQualificationRun resolves it fresh every time a candidate
	// is qualified, the same "not a stale copy" discipline
	// fireTaskSchedule already holds a schedule's own TemplateID to.
	// repeat_count is model.QualificationItem's own Repeat; order_key
	// is display order only -- unlike task.order_key it decides nothing
	// about dispatch, since an item's actual scheduling order comes
	// from the dependency graph below, not from this column.
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

	// One suite (bwsalmon/agents#642) -- a saved combination of
	// templates plus how to run them. INTEGER-free TEXT id, the same
	// "own sequence, own prefix" shape template and schedule already
	// use (suite_sequence below), since a suite is created directly
	// rather than cut like a release or a candidate.
	// mode/count/max_passes are model.Suite's own
	// SuiteMode/Count/MaxPasses -- which pair is meaningful depends on
	// mode, model.Suite.Validate's own job to check, not the schema's.
	`CREATE TABLE IF NOT EXISTS ` + "`suite`" + ` (
  ` + "`id`" + `                TEXT     NOT NULL,
  ` + "`name`" + `              TEXT     NOT NULL,
  ` + "`mode`" + `              TEXT     NOT NULL,
  ` + "`count`" + `             INTEGER  NOT NULL,
  ` + "`max_passes`" + `        INTEGER  NOT NULL,
  ` + "`require_approval`" + `  INTEGER  NOT NULL,
  ` + "`auto_merge`" + `        INTEGER  NOT NULL,
  ` + "`created_at`" + `        DATETIME NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`suite_sequence`" + ` (
  ` + "`id`" + `         INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + `  DATETIME NOT NULL
)`,

	// One template a suite runs, in display and firing order (order_key)
	// -- deliberately no uniqueness constraint on (suite_id, template_id)
	// the way qualification_item has on (owner, name, template_id): a
	// suite has no dependency graph to keep acyclic across its items, so
	// nothing here stops the same template from appearing twice in one
	// suite on purpose (e.g. running it once against each of two
	// unrelated concerns a single pass covers).
	`CREATE TABLE IF NOT EXISTS ` + "`suite_item`" + ` (
  ` + "`id`" + `           INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`suite_id`" + `     TEXT    NOT NULL,
  ` + "`template_id`" + `  TEXT    NOT NULL,
  ` + "`order_key`" + `    REAL    NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`suite_item_suite`" + ` ON ` + "`suite_item`" + ` (` + "`suite_id`" + `, ` + "`order_key`" + `)`,

	// One run of a suite against a repo and branch. Every field a run's
	// own behaviour depends on is copied from the suite at creation
	// (model.SuiteRun's own doc comment on why), so this table carries
	// its own mode/count/max_passes/require_approval/auto_merge rather
	// than joining back to suite for them -- a suite edited after a run
	// starts must not change how that run already in flight behaves.
	// suite_id and suite_name are kept anyway, for display and for
	// CreateSuiteRun's own idempotency (a suite deleted after a run
	// starts leaves the run's own history intact). INTEGER PRIMARY KEY
	// AUTOINCREMENT for the same reason release/candidate/
	// qualification_run all use it: starting a run has to stay correct
	// with more than one writer.
	//
	// schedule_id is NULL for a run a human started by hand, and names
	// the schedule that fired it otherwise -- Store.
	// HasActiveRunForSchedule reads it back as the "a previous firing
	// that has not finished suppresses the next one" check a schedule
	// firing a suite needs, exactly what task_tag's own firing tag is
	// for a schedule filing a task. A database created before schedules
	// could fire a suite gets this column from
	// ensureSuiteRunScheduleColumn rather than from this DDL.
	`CREATE TABLE IF NOT EXISTS ` + "`suite_run`" + ` (
  ` + "`id`" + `                INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`suite_id`" + `          TEXT     NOT NULL,
  ` + "`suite_name`" + `        TEXT     NOT NULL,
  ` + "`schedule_id`" + `       TEXT     NULL,
  ` + "`owner`" + `             TEXT     NOT NULL,
  ` + "`repo`" + `              TEXT     NOT NULL,
  ` + "`base`" + `              TEXT     NOT NULL,
  ` + "`mode`" + `              TEXT     NOT NULL,
  ` + "`count`" + `             INTEGER  NOT NULL,
  ` + "`max_passes`" + `        INTEGER  NOT NULL,
  ` + "`require_approval`" + `  INTEGER  NOT NULL,
  ` + "`auto_merge`" + `        INTEGER  NOT NULL,
  ` + "`status`" + `            TEXT     NOT NULL,
  ` + "`last_error`" + `        TEXT     NULL,
  ` + "`created_at`" + `        DATETIME NOT NULL,
  ` + "`completed_at`" + `      DATETIME NULL
)`,

	// What ListSuiteRuns (newest first) and the reconciler's own
	// ActiveSuiteRuns (status only) both need.
	`CREATE INDEX IF NOT EXISTS ` + "`suite_run_status`" + ` ON ` + "`suite_run`" + ` (` + "`status`" + `)`,

	// A run's own snapshot of its suite's items at the moment it was
	// created -- CreateSuiteRun's copy, read back by FireNextPass every
	// time it files another pass, so a suite edited after a run starts
	// cannot change which templates that run's later passes fire
	// (model.SuiteRun.Items's own doc comment).
	`CREATE TABLE IF NOT EXISTS ` + "`suite_run_item`" + ` (
  ` + "`id`" + `           INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`run_id`" + `       INTEGER NOT NULL,
  ` + "`template_id`" + `  TEXT    NOT NULL,
  ` + "`order_key`" + `    REAL    NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`suite_run_item_run`" + ` ON ` + "`suite_run_item`" + ` (` + "`run_id`" + `, ` + "`order_key`" + `)`,

	// One task instance a run's pass instantiated. template_name is a
	// snapshot of the resolved template's own Name at the moment this
	// instance was created, qualification_task's own reasoning for the
	// same column. task_id is unique: a task belongs to at most one
	// suite run, ever.
	`CREATE TABLE IF NOT EXISTS ` + "`suite_run_task`" + ` (
  ` + "`id`" + `             INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`run_id`" + `        INTEGER NOT NULL,
  ` + "`task_id`" + `       TEXT    NOT NULL,
  ` + "`template_id`" + `   TEXT    NOT NULL,
  ` + "`template_name`" + ` TEXT    NOT NULL,
  ` + "`pass_number`" + `   INTEGER NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS ` + "`suite_run_task_run`" + ` ON ` + "`suite_run_task`" + ` (` + "`run_id`" + `)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ` + "`suite_run_task_task`" + ` ON ` + "`suite_run_task`" + ` (` + "`task_id`" + `)`,
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
	//
	// PausedOutcome is excluded alongside 'succeeded', for a different
	// reason than either boundary above: a run stopped because the
	// deployment's agent had no budget left says nothing at all about the
	// task it was given -- see that constant's own doc comment.
	// Store.FailureStreak skips the same word, and state_test.go holds
	// the two to one answer the way it does for every other rule here.
	{"task_streak", `
SELECT
  ` + "`tr`.`task_id`" + ` AS ` + "`task_id`" + `,
  COUNT(*) AS ` + "`streak`" + `
FROM ` + "`task_run`" + ` AS ` + "`tr`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`o`" + ` ON ` + "`o`.`task_id`" + ` = ` + "`tr`.`task_id`" + `
WHERE ` + "`tr`.`finished_at`" + ` IS NOT NULL
  AND ` + "`tr`.`outcome`" + ` != 'succeeded'
  AND ` + "`tr`.`outcome`" + ` != '` + PausedOutcome + `'
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
	//
	// 'awaiting_submit' sits immediately above 'completed' and tests the
	// same completed_at: the two are one condition split by whether
	// anything is going to land the pull request without a human
	// (model.AwaitsSubmit, which this EXISTS clause and the auto_merge
	// test spell out in SQL). EXISTS rather than a join to task_link,
	// because a task carrying two fixes-links would otherwise appear
	// twice in a view whose whole point is one row per task.
	{"task_state", `
SELECT
  ` + "`t`.`id`" + ` AS ` + "`task_id`" + `,
  CASE
    WHEN ` + "`o`.`closed_at`" + `    IS NOT NULL THEN 'closed'
    WHEN ` + "`o`.`completed_at`" + ` IS NOT NULL AND ` + "`t`.`auto_merge`" + ` = FALSE AND EXISTS (
           SELECT 1 FROM ` + "`task_link`" + ` AS ` + "`fl`" + `
           WHERE ` + "`fl`.`task_id`" + ` = ` + "`t`.`id`" + ` AND ` + "`fl`.`kind`" + ` = '` + string(LinkFixes) + `'
         ) THEN 'awaiting_submit'
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
