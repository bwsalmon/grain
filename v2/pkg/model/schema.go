package model

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
const SchemaVersion = 8

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

  ` + "`target_owner`" + `          TEXT    NULL,
  ` + "`target_name`" + `           TEXT    NULL,
  ` + "`binding`" + `               TEXT    NOT NULL,
  ` + "`base`" + `                  TEXT    NULL,
  ` + "`folder`" + `                TEXT    NULL,

  ` + "`auto_merge`" + `            INTEGER  NOT NULL,
  ` + "`created_at`" + `            DATETIME NULL,
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
  PRIMARY KEY (` + "`task_id`" + `)
)`,

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
  PRIMARY KEY (` + "`id`" + `)
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
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task`" + ` (
  ` + "`id`" + `            TEXT     NOT NULL,
  ` + "`title`" + `         TEXT     NOT NULL,
  ` + "`body`" + `          TEXT     NOT NULL,
  ` + "`target_owner`" + `  TEXT     NOT NULL,
  ` + "`target_name`" + `   TEXT     NOT NULL,
  ` + "`base`" + `          TEXT     NULL,
  ` + "`auto_merge`" + `    INTEGER  NOT NULL,
  ` + "`interval_ms`" + `   INTEGER  NOT NULL,
  ` + "`enabled`" + `       INTEGER  NOT NULL,
  ` + "`next_run_at`" + `   DATETIME NOT NULL,
  ` + "`last_run_at`" + `   DATETIME NULL,
  ` + "`created_at`" + `    DATETIME NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// task_sequence's own doc comment gives the reasoning for a dedicated
	// allocator rather than a counter column: an INSERT that lets SQLite
	// assign the rowid is atomic where read-modify-write is a race.
	`CREATE TABLE IF NOT EXISTS ` + "`scheduled_task_sequence`" + ` (
  ` + "`number`" + `    INTEGER PRIMARY KEY AUTOINCREMENT,
  ` + "`issued_at`" + ` DATETIME NOT NULL
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
	`CREATE TABLE IF NOT EXISTS ` + "`grain_config`" + ` (
  ` + "`id`" + `                         INTEGER NOT NULL,
  ` + "`poll_interval_ms`" + `           INTEGER NOT NULL,
  ` + "`slots`" + `                      TEXT    NOT NULL,
  ` + "`gemini_model`" + `                TEXT    NOT NULL,
  ` + "`max_agent_turns`" + `             INTEGER NOT NULL,
  ` + "`github_host`" + `                 TEXT    NOT NULL,
  ` + "`github_insecure_http`" + `        INTEGER NOT NULL,
  ` + "`gcp_project`" + `                 TEXT    NOT NULL,
  ` + "`gcp_service_account_email`" + `   TEXT    NOT NULL,
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
}

// Views is the derivations, each a (name, DDL) pair so Init can drop and
// recreate one by name -- SQLite has no CREATE OR REPLACE VIEW, unlike
// the MySQL dialect Dolt spoke. Order matters: task_ready reads the two
// above it.
var Views = []View{
	// The invariant, as a view. Precedence top to bottom, matching
	// StateOf exactly — see state_test.go on what holds them together.
	{"task_state", `
SELECT
  ` + "`t`.`id`" + ` AS ` + "`task_id`" + `,
  CASE
    WHEN ` + "`o`.`closed_at`" + `    IS NOT NULL THEN 'closed'
    WHEN ` + "`o`.`completed_at`" + ` IS NOT NULL THEN 'completed'
    WHEN ` + "`o`.`pending_question_comment_id`" + ` IS NOT NULL THEN 'awaiting_reply'
    WHEN ` + "`r`.`id`" + ` IS NOT NULL THEN 'running'
    WHEN ` + "`t`.`approval_actor_kind`" + ` IS NULL THEN 'proposed'
    ELSE 'queued'
  END AS ` + "`state`" + `
FROM ` + "`task`" + ` AS ` + "`t`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`o`" + ` ON ` + "`o`.`task_id`" + ` = ` + "`t`.`id`" + `
LEFT JOIN ` + "`task_run`" + ` AS ` + "`r`" + `
       ON ` + "`r`.`task_id`" + ` = ` + "`t`.`id`" + ` AND ` + "`r`.`finished_at`" + ` IS NULL`},

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
