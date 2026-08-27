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
// grain writes the second — and keeping them apart is what would let a
// declaration change land on a Dolt branch and be reviewed as a data diff
// while observations keep being written to the trunk.
//
// On naming: every table is prefixed with its entity and every identifier
// is backtick-quoted, because MySQL reserves GRANT, READ and READS, all
// three of which are nouns this model wants.
//
// On types: enums are VARCHAR rather than MySQL ENUM. A MySQL ENUM puts
// the vocabulary in the schema, so adding a LinkKind becomes a migration;
// a VARCHAR keeps it in task.go, where the model already says it lives.
// The cost is that the database will not reject an unknown value, which
// the Valid methods and schema_test.go cover instead.

// SchemaVersion is bumped whenever Tables or Views change in a way an
// existing database cannot simply be re-created into. Open records this
// and refuses a database written by a newer build, rather than failing
// later with a confusing missing column.
const SchemaVersion = 1

// Tables is the DDL, in dependency order.
var Tables = []string{
	`CREATE TABLE IF NOT EXISTS ` + "`task`" + ` (
  ` + "`id`" + `                    VARCHAR(64)  NOT NULL,
  ` + "`intent`" + `                VARCHAR(32)  NOT NULL,
  ` + "`title`" + `                 TEXT         NOT NULL,
  ` + "`body`" + `                  LONGTEXT     NOT NULL,

  ` + "`origin_actor_kind`" + `     VARCHAR(32)  NOT NULL,
  ` + "`origin_actor_id`" + `       VARCHAR(255) NOT NULL,
  ` + "`origin_behalf_kind`" + `    VARCHAR(32)  NULL,
  ` + "`origin_behalf_id`" + `      VARCHAR(255) NULL,
  ` + "`origin_reason`" + `         VARCHAR(32)  NOT NULL,

  ` + "`approval_actor_kind`" + `   VARCHAR(32)  NULL,
  ` + "`approval_actor_id`" + `     VARCHAR(255) NULL,
  ` + "`approval_behalf_kind`" + `  VARCHAR(32)  NULL,
  ` + "`approval_behalf_id`" + `    VARCHAR(255) NULL,

  ` + "`target_owner`" + `          VARCHAR(255) NULL,
  ` + "`target_name`" + `           VARCHAR(255) NULL,
  ` + "`binding`" + `               VARCHAR(32)  NOT NULL,
  ` + "`base`" + `                  VARCHAR(255) NULL,
  ` + "`folder`" + `                VARCHAR(512) NULL,

  ` + "`auto_merge`" + `            BOOLEAN      NOT NULL,
  ` + "`external_ref`" + `          VARCHAR(255) NULL,
  ` + "`created_at`" + `            DATETIME(6)  NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	// Read targets get a table rather than a JSON column: "which tasks
	// read this repo?" is worth being able to ask, and keeping reads
	// structurally distinct from task.target is what stops a read target
	// becoming a capability-laundering channel by accident.
	`CREATE TABLE IF NOT EXISTS ` + "`task_read`" + ` (
  ` + "`task_id`" + ` VARCHAR(64)  NOT NULL,
  ` + "`owner`" + `   VARCHAR(255) NOT NULL,
  ` + "`name`" + `    VARCHAR(255) NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`owner`" + `, ` + "`name`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_grant`" + ` (
  ` + "`task_id`" + `    VARCHAR(64)  NOT NULL,
  ` + "`capability`" + ` VARCHAR(128) NOT NULL,
  ` + "`via`" + `        VARCHAR(32)  NOT NULL,
  ` + "`folder`" + `     VARCHAR(512) NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`capability`" + `)
)`,

	// blocks is stored rather than recomputed in SQL so task_blocked can
	// be a plain join instead of hard-coding the kind vocabulary in two
	// places where it could drift.
	`CREATE TABLE IF NOT EXISTS ` + "`task_link`" + ` (
  ` + "`task_id`" + ` VARCHAR(64)  NOT NULL,
  ` + "`kind`" + `    VARCHAR(32)  NOT NULL,
  ` + "`target`" + `  VARCHAR(255) NOT NULL,
  ` + "`blocks`" + `  BOOLEAN      NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`kind`" + `, ` + "`target`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_tag`" + ` (
  ` + "`task_id`" + ` VARCHAR(64)  NOT NULL,
  ` + "`tag`" + `     VARCHAR(255) NOT NULL,
  PRIMARY KEY (` + "`task_id`" + `, ` + "`tag`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_observation`" + ` (
  ` + "`task_id`" + `                     VARCHAR(64) NOT NULL,
  ` + "`closed_at`" + `                   DATETIME(6) NULL,
  ` + "`completed_at`" + `                DATETIME(6) NULL,
  ` + "`pending_question_comment_id`" + ` BIGINT      NULL,
  ` + "`baseline_comment_id`" + `         BIGINT      NULL,
  ` + "`observed_at`" + `                 DATETIME(6) NULL,
  PRIMARY KEY (` + "`task_id`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`task_run`" + ` (
  ` + "`id`" + `          VARCHAR(64)  NOT NULL,
  ` + "`task_id`" + `     VARCHAR(64)  NOT NULL,
  ` + "`slot`" + `        VARCHAR(128) NOT NULL,
  ` + "`sandbox`" + `     VARCHAR(128) NOT NULL,
  ` + "`unit`" + `        VARCHAR(255) NULL,
  ` + "`attempt`" + `     INT          NOT NULL,
  ` + "`started_at`" + `  DATETIME(6)  NOT NULL,
  ` + "`finished_at`" + ` DATETIME(6)  NULL,
  ` + "`outcome`" + `     TEXT         NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`lease`" + ` (
  ` + "`run_id`" + `     VARCHAR(64)  NOT NULL,
  ` + "`capability`" + ` VARCHAR(128) NOT NULL,
  ` + "`resource`" + `   VARCHAR(512) NOT NULL,
  ` + "`minted_by`" + `  VARCHAR(128) NOT NULL,
  ` + "`issued_at`" + `  DATETIME(6)  NOT NULL,
  ` + "`expires_at`" + ` DATETIME(6)  NULL,
  PRIMARY KEY (` + "`run_id`" + `, ` + "`capability`" + `, ` + "`resource`" + `)
)`,

	`CREATE TABLE IF NOT EXISTS ` + "`grain_schema`" + ` (
  ` + "`id`" + `      INT NOT NULL,
  ` + "`version`" + ` INT NOT NULL,
  PRIMARY KEY (` + "`id`" + `)
)`,
}

// Views are the derivations. Order matters: task_ready reads the two
// above it.
var Views = []string{
	// The invariant, as a view. Precedence top to bottom, matching
	// StateOf exactly — see state_test.go on what holds them together.
	`CREATE OR REPLACE VIEW ` + "`task_state`" + ` AS
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
       ON ` + "`r`.`task_id`" + ` = ` + "`t`.`id`" + ` AND ` + "`r`.`finished_at`" + ` IS NULL`,

	// Blocked is an annotation on queued, never a state of its own, so it
	// gets its own view rather than another branch of the CASE above. A
	// link whose target is not a task at all — a review thread, a pull
	// request — never blocks, which falls out of the join rather than
	// needing a rule.
	`CREATE OR REPLACE VIEW ` + "`task_blocked`" + ` AS
SELECT
  ` + "`l`.`task_id`" + ` AS ` + "`task_id`" + `,
  COUNT(*)      AS ` + "`open_blockers`" + `
FROM ` + "`task_link`" + ` AS ` + "`l`" + `
JOIN ` + "`task`" + ` AS ` + "`bt`" + ` ON ` + "`bt`.`id`" + ` = ` + "`l`.`target`" + `
LEFT JOIN ` + "`task_observation`" + ` AS ` + "`bo`" + ` ON ` + "`bo`.`task_id`" + ` = ` + "`l`.`target`" + `
WHERE ` + "`l`.`blocks`" + ` = TRUE AND ` + "`bo`.`closed_at`" + ` IS NULL
GROUP BY ` + "`l`.`task_id`" + ``,

	// What is dispatchable right now: approved, not running, nothing open
	// in front of it. The whole dispatch query, as one view.
	`CREATE OR REPLACE VIEW ` + "`task_ready`" + ` AS
SELECT ` + "`s`.`task_id`" + ` AS ` + "`task_id`" + `
FROM ` + "`task_state`" + ` AS ` + "`s`" + `
LEFT JOIN ` + "`task_blocked`" + ` AS ` + "`b`" + ` ON ` + "`b`.`task_id`" + ` = ` + "`s`.`task_id`" + `
WHERE ` + "`s`.`state`" + ` = 'queued'
  AND COALESCE(` + "`b`.`open_blockers`" + `, 0) = 0`,

	// Every lease still outstanding, with the credential that minted it —
	// which makes "what would I break by rotating this?" a query.
	`CREATE OR REPLACE VIEW ` + "`lease_live`" + ` AS
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
WHERE ` + "`r`.`finished_at`" + ` IS NULL`,
}

// Statements is the order Open applies things in: tables, then views, a
// view referencing a missing table being an error.
func Statements() []string {
	out := make([]string, 0, len(Tables)+len(Views))
	out = append(out, Tables...)
	return append(out, Views...)
}
