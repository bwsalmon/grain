package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Store is the model's read and write surface over any database/sql
// database. It imports no driver: opening an embedded SQLite database
// lives in the sqlite subpackage, so this file — and everything above
// it — stays free of that dependency and testable against anything that
// speaks SQL.
//
// Every statement here is parameterised. That is the whole of what the
// language change bought at this layer: a Python controller cannot embed
// SQLite the way Go can, so an earlier version of this store shelled out
// to a CLI with no bind parameters — which meant hand-rendering every
// untrusted issue title and comment body into a statement and getting
// escaping rules right with no server to check against. That module does
// not exist here. Writes are also real transactions rather than a
// best-effort batch, so a task and its child rows land together or not
// at all.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

// querier is the subset of *sql.DB that *sql.Tx also provides.
//
// It exists so a read and the write that depends on it happen inside one
// transaction. That is not a nicety: Store.write retries an operation
// from the top when it loses a race, and a retry is only correct if the
// re-read sees the winner's state.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrSchemaTooNew is returned when the database was written by a build
// that knows a later schema. Refusing up front beats failing later with a
// confusing missing column.
var ErrSchemaTooNew = errors.New("database schema is newer than this build")

// ErrSchemaTooOld is returned when the database was written by a build
// that knows an earlier schema, and so carries columns and indexes this
// build cannot reconcile it into.
//
// It is the same argument ErrSchemaTooNew makes, in the direction that
// used to be assumed harmless. Init's own CREATE TABLE IF NOT EXISTS
// never alters a table that already exists, and the ensure*Column
// migrations above only ever *add* a column -- neither can drop one, or
// replace an index. A schema change that removes something therefore
// leaves an older database intact and wrong: SchemaVersion 16 dropped
// task_run.slot, so a database written at 15 keeps that column, still
// declared NOT NULL with no default, and every startRun fails to satisfy
// it.
//
// Left unchecked, that surfaces as a dispatch failure on every tick,
// forever, from a daemon that started up perfectly happily -- so the
// operator sees a deployment that runs and never runs anything, rather
// than one that says why. ../scripts/setup.sh moves such a store aside at
// deploy (cmd/grain/schemaversion.go's own doc comment on how it knows
// to), and this is what a build started any other way does instead.
var ErrSchemaTooOld = errors.New("database schema is older than this build")

// Init creates the schema if absent and stamps the version.
func (s *Store) Init(ctx context.Context) error {
	// Before the DDL, not after: renameScheduleTables' own doc comment on
	// why a rename of a table Statements() also creates has to get there
	// first.
	if err := s.renameScheduleTables(ctx); err != nil {
		return fmt.Errorf("renaming scheduled_task to schedule: %w", err)
	}
	for _, stmt := range Statements() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
	}
	if err := s.ensureConfigTargetReposColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigMaxConcurrentColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	// After ensureConfigMaxConcurrentColumn, not before: the two are one
	// chain -- slots became max_concurrent, and max_concurrent became
	// max_workers -- and a database old enough to still hold slots has to
	// walk both steps in order.
	if err := s.ensureConfigWorkerMergerColumns(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureTaskApprovedAtColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureTaskRunTranscriptColumn(ctx); err != nil {
		return fmt.Errorf("migrating task_run: %w", err)
	}
	if err := s.ensureTaskRunAgentStartedAtColumn(ctx); err != nil {
		return fmt.Errorf("migrating task_run: %w", err)
	}
	if err := s.ensureScheduleRecurrenceColumns(ctx); err != nil {
		return fmt.Errorf("migrating schedule: %w", err)
	}
	if err := s.ensureTaskOrderKeyColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureConfigNewestFirstColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigShowClosedByDefaultColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigAgentFrameworkColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigTaskDefaultsColumns(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigTaskDefaultsOn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureScheduleTemplateColumn(ctx); err != nil {
		return fmt.Errorf("migrating schedule: %w", err)
	}
	if err := s.ensureConfigSandboxShapeColumns(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureTaskSandboxShapeColumns(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureConfigSandboxDiskColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureTaskSandboxDiskColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureTaskInteractiveColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureTaskConfigurationColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureTaskAgentFrameworkColumn(ctx); err != nil {
		return fmt.Errorf("migrating task: %w", err)
	}
	if err := s.ensureConfigClaudeModelColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureTaskTemplateNoTargetColumns(ctx); err != nil {
		return fmt.Errorf("migrating task_template: %w", err)
	}
	if err := s.ensureScheduleSuiteColumn(ctx); err != nil {
		return fmt.Errorf("migrating schedule: %w", err)
	}
	if err := s.ensureTaskSuiteRunScheduleColumn(ctx); err != nil {
		return fmt.Errorf("migrating task_suite_run: %w", err)
	}
	if err := s.ensureConfigDefaultCapabilitiesColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureConfigEnvironmentNameColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
	}
	if err := s.ensureTaskObservationRefreshedColumn(ctx); err != nil {
		return fmt.Errorf("migrating task_observation: %w", err)
	}
	var version int
	err := s.db.QueryRowContext(ctx,
		"SELECT `version` FROM `grain_schema` WHERE `id` = 1").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx,
			"INSERT INTO `grain_schema` (`id`, `version`) VALUES (1, ?)", SchemaVersion)
		return err
	}
	if err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: found %d, this build knows %d",
			ErrSchemaTooNew, version, SchemaVersion)
	}
	if version < SchemaVersion {
		return fmt.Errorf("%w: found %d, this build knows %d -- ../scripts/setup.sh moves such a store aside at deploy; "+
			"to do it by hand, move the store file out of the way and let this build create a fresh one",
			ErrSchemaTooOld, version, SchemaVersion)
	}
	return nil
}

// ensureConfigTargetReposColumn adds grain_config.target_repos (schema.go's
// own doc comment on the table has the history) to a database created
// before bwsalmon/agents#427, when this column did not exist anywhere --
// neither in the DDL Statements() above applies (CREATE TABLE IF NOT
// EXISTS never alters a table that is already there) nor in configColumns/
// scanConfig/PutConfig below, which is why a Settings change widening
// Config.TargetRepos was never actually durable. Probing with a
// zero-row SELECT rather than a dialect-specific PRAGMA/information_schema
// query keeps this portable across whatever database/sql driver Store is
// opened against (this file's own doc comment: "any database/sql
// database"), the same reasoning schema.go gives backtick quoting; ALTER
// TABLE ADD COLUMN is standard SQL either way.
func (s *Store) ensureConfigTargetReposColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `target_repos` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `target_repos` TEXT NOT NULL DEFAULT ''")
	return err
}

// ensureConfigMaxConcurrentColumn replaces grain_config's old slots column
// (a comma-separated list of operator-chosen concurrency-slot names,
// bwsalmon/agents#320) with max_concurrent (a plain count, bwsalmon/
// agents#461) on a database created before that switch -- the same
// probe-then-ALTER approach ensureConfigTargetReposColumn already uses,
// since CREATE TABLE IF NOT EXISTS never alters a table that is already
// there. max_concurrent is backfilled from however many comma-separated
// names the old slots column held (its LENGTH-vs-REPLACE arithmetic is
// just "count the commas, plus one", the same thing splitCSV would do in
// Go against the same string), so a deployment's dispatch pool stays the
// same size across the upgrade even though the individual slots it names
// no longer exist. The old column is then dropped: leaving it in place,
// still NOT NULL with no default, would break every later PutConfig,
// which stops supplying it.
func (s *Store) ensureConfigMaxConcurrentColumn(ctx context.Context) error {
	// max_workers means this database is already past the step *after*
	// this one (ensureConfigWorkerMergerColumns, which renamed
	// max_concurrent to it) -- including every database created fresh
	// from schema.go's own DDL, which has never had a max_concurrent
	// column at all. Re-adding one here would leave a column nothing
	// reads and every later migration has to step around.
	if rows, err := s.db.QueryContext(ctx, "SELECT `max_workers` FROM `grain_config` WHERE 1 = 0"); err == nil {
		return rows.Close()
	}
	if rows, err := s.db.QueryContext(ctx, "SELECT `max_concurrent` FROM `grain_config` WHERE 1 = 0"); err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `max_concurrent` INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT `slots` FROM `grain_config` WHERE 1 = 0")
	if err != nil {
		// No old slots column either -- a database that never had one --
		// so the DEFAULT above already leaves max_concurrent at 1.
		return nil
	}
	rows.Close()
	if _, err := s.db.ExecContext(ctx,
		"UPDATE `grain_config` SET `max_concurrent` = "+
			"LENGTH(`slots`) - LENGTH(REPLACE(`slots`, ',', '')) + 1 "+
			"WHERE `slots` IS NOT NULL AND `slots` != ''"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `grain_config` DROP COLUMN `slots`")
	return err
}

// ensureConfigWorkerMergerColumns splits grain_config's single
// max_concurrent count into the pair model.Limits is now made of
// (grain/task-63): max_workers, the ordinary-run ceiling max_concurrent
// already was, and max_mergers, the extra capacity only a merge-queue fix
// task may reach. Same probe-then-ALTER shape as every migration above,
// and for the same reason: CREATE TABLE IF NOT EXISTS never alters a
// table that is already there.
//
// max_workers is backfilled from max_concurrent, so a deployment keeps
// exactly the ordinary concurrency it was already running -- a rename in
// everything but SQL's inability to do one portably. The old column is
// then dropped for the reason its own predecessor's was: leaving a NOT
// NULL column with no default behind would break every later PutConfig,
// which stops supplying it.
//
// max_mergers is not backfilled from anything, because there is nothing
// it could be backfilled from: no deployment has ever expressed a merge
// capacity. It takes DefaultMaxMergers, the same value a fresh database
// gets from the DDL and a fresh Config from DefaultConfig -- so an
// upgraded deployment gains that one merger slot rather than silently
// keeping the "mergers contend for worker capacity" behaviour its stored
// row predates.
func (s *Store) ensureConfigWorkerMergerColumns(ctx context.Context) error {
	mergers, err := s.db.QueryContext(ctx, "SELECT `max_mergers` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		if err := mergers.Close(); err != nil {
			return err
		}
	} else if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE `grain_config` ADD COLUMN `max_mergers` INTEGER NOT NULL DEFAULT %d",
			DefaultMaxMergers)); err != nil {
		return err
	}
	if rows, err := s.db.QueryContext(ctx, "SELECT `max_workers` FROM `grain_config` WHERE 1 = 0"); err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `max_workers` INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT `max_concurrent` FROM `grain_config` WHERE 1 = 0")
	if err != nil {
		// No max_concurrent to carry over -- a database whose
		// grain_config was created after the split -- so the DEFAULT
		// above already leaves max_workers where it belongs.
		return nil
	}
	rows.Close()
	if _, err := s.db.ExecContext(ctx,
		"UPDATE `grain_config` SET `max_workers` = `max_concurrent`"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `grain_config` DROP COLUMN `max_concurrent`")
	return err
}

// ensureTaskApprovedAtColumn adds task.approved_at (schema.go's own DDL
// comment on the table has the reasoning) to a database created before
// this column existed, the same probe-then-ALTER approach
// ensureConfigTargetReposColumn already uses for the same reason: CREATE
// TABLE IF NOT EXISTS never alters a table that is already there.
func (s *Store) ensureTaskApprovedAtColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `approved_at` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `task` ADD COLUMN `approved_at` DATETIME NULL")
	return err
}

// ensureTaskRunTranscriptColumn adds task_run.transcript (schema.go's own
// DDL comment on the table has the reasoning) to a database created
// before bwsalmon/agents#446, the same probe-then-ALTER approach
// ensureConfigTargetReposColumn and ensureTaskApprovedAtColumn already
// use for the same reason: CREATE TABLE IF NOT EXISTS never alters a
// table that is already there.
func (s *Store) ensureTaskRunTranscriptColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `transcript` FROM `task_run` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `task_run` ADD COLUMN `transcript` TEXT NULL")
	return err
}

// ensureTaskRunAgentStartedAtColumn adds task_run.agent_started_at
// (schema.go's own DDL comment on the table has the reasoning) to a
// database created before this build, the same probe-then-ALTER approach
// ensureTaskRunTranscriptColumn already uses for the same reason: CREATE
// TABLE IF NOT EXISTS never alters a table that is already there.
//
// No SchemaVersion bump goes with it: the column is nullable and added
// here, so an existing database migrates into the new shape rather than
// being one this build "cannot simply be re-created into" (SchemaVersion's
// own doc comment). Every run recorded before it existed reads back with
// no agent_started_at, which pkg/metrics already has to handle for a run
// that never reached its agent at all -- such a run contributes to no
// setup or agent latency rather than to a wrong one.
func (s *Store) ensureTaskRunAgentStartedAtColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `agent_started_at` FROM `task_run` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `task_run` ADD COLUMN `agent_started_at` DATETIME NULL")
	return err
}

// ensureTaskObservationRefreshedColumn adds
// task_observation.merge_queue_refreshed_at (Observation's own field has
// the reasoning) to a database created before it existed, the same
// probe-then-ALTER approach ensureTaskRunTranscriptColumn already uses
// for the same reason: CREATE TABLE IF NOT EXISTS never alters a table
// that is already there.
//
// No SchemaVersion bump goes with it: the column is nullable and added
// here, so an existing database migrates into the new shape rather than
// being one this build "cannot simply be re-created into" (SchemaVersion's
// own doc comment). A pull request the merge queue looked at before this
// column existed reads back as never refreshed, which is the direction
// that errs toward one more merge attempt rather than toward never making
// one.
func (s *Store) ensureTaskObservationRefreshedColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `merge_queue_refreshed_at` FROM `task_observation` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task_observation` ADD COLUMN `merge_queue_refreshed_at` DATETIME NULL")
	return err
}

// renameScheduleTables carries a database written before this build's
// rename of the feature -- "scheduled tasks" everywhere, now just
// schedules (docs/schedules.md) -- onto the table names that rename
// leaves: scheduled_task and its three companions become schedule,
// schedule_sequence, schedule_read and schedule_grant, and the child
// tables' scheduled_task_id column becomes schedule_id. Nothing about a
// row changes; only what the tables holding them are called.
//
// Unlike every ensure* migration below, this one runs *before* Init
// applies the DDL, and has to: Statements() would otherwise create an
// empty schedule table beside the populated scheduled_task one, leaving
// the rename with nowhere to go and every schedule an operator had
// silently gone. Running first also means the column migrations below see
// one table under one name, whichever name the database arrived with,
// rather than each having to know about both.
//
// Each step is guarded on the old name being there and the new one not,
// so this is idempotent and safe to interrupt: a database already renamed
// skips every step, and one interrupted part-way through finishes the
// steps it has left. No SchemaVersion bump goes with it, for the reason
// ensureConfigWorkerMergerColumns' own rename did not need one: an
// existing database migrates into the new shape here rather than being
// one this build "cannot simply be re-created into" (SchemaVersion's own
// doc comment).
func (s *Store) renameScheduleTables(ctx context.Context) error {
	for _, rename := range []struct{ from, to string }{
		{"scheduled_task", "schedule"},
		{"scheduled_task_sequence", "schedule_sequence"},
		{"scheduled_task_read", "schedule_read"},
		{"scheduled_task_grant", "schedule_grant"},
	} {
		if !s.has(ctx, "SELECT 1 FROM `"+rename.from+"` WHERE 1 = 0") ||
			s.has(ctx, "SELECT 1 FROM `"+rename.to+"` WHERE 1 = 0") {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			"ALTER TABLE `"+rename.from+"` RENAME TO `"+rename.to+"`"); err != nil {
			return err
		}
	}
	for _, table := range []string{"schedule_read", "schedule_grant"} {
		if !s.has(ctx, "SELECT `scheduled_task_id` FROM `"+table+"` WHERE 1 = 0") {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			"ALTER TABLE `"+table+"` RENAME COLUMN `scheduled_task_id` TO `schedule_id`"); err != nil {
			return err
		}
	}
	return nil
}

// has reports whether probe -- a zero-row SELECT of a table or a column,
// the probe every migration in this file already open-codes -- runs at
// all, which is what "that table/column is there" looks like without a
// dialect-specific PRAGMA or information_schema query
// (ensureConfigTargetReposColumn's own doc comment gives the reasoning
// for probing this way). The driver's error is deliberately dropped:
// nothing portably distinguishes "no such table" from "no such column",
// and either answer means the same thing to every caller here.
func (s *Store) has(ctx context.Context, probe string) bool {
	rows, err := s.db.QueryContext(ctx, probe)
	if err != nil {
		return false
	}
	rows.Close()
	return true
}

// ensureScheduleRecurrenceColumns replaces schedule's original
// interval_ms (every N hours since it last fired, no wall-clock
// alignment) with model.Recurrence's own columns (bwsalmon/agents#464),
// the same probe-then-ALTER approach ensureConfigMaxConcurrentColumn
// already uses for the same reason: CREATE TABLE IF NOT EXISTS never
// alters a table that is already there. Every existing row is backfilled
// as RecurrenceEveryNHours, rounded down from its old interval_ms to the
// nearest whole hour (minimum 1) -- the same cadence it already had,
// expressed as hours rather than milliseconds, since bwsalmon/agents#464
// only ever asks for hour granularity on this cadence.
func (s *Store) ensureScheduleRecurrenceColumns(ctx context.Context) error {
	if rows, err := s.db.QueryContext(ctx,
		"SELECT `recurrence_kind` FROM `schedule` WHERE 1 = 0"); err == nil {
		return rows.Close()
	}
	for _, stmt := range []string{
		"ALTER TABLE `schedule` ADD COLUMN `recurrence_kind` TEXT NOT NULL DEFAULT 'everyNHours'",
		"ALTER TABLE `schedule` ADD COLUMN `every_n_hours` INTEGER NULL",
		"ALTER TABLE `schedule` ADD COLUMN `time_of_day_minutes` INTEGER NULL",
		"ALTER TABLE `schedule` ADD COLUMN `weekday` INTEGER NULL",
		"ALTER TABLE `schedule` ADD COLUMN `day_of_month` INTEGER NULL",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if rows, err := s.db.QueryContext(ctx, "SELECT `interval_ms` FROM `schedule` WHERE 1 = 0"); err == nil {
		rows.Close()
		if _, err := s.db.ExecContext(ctx,
			"UPDATE `schedule` SET `every_n_hours` = MAX(1, `interval_ms` / 3600000)"); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE `schedule` DROP COLUMN `interval_ms`"); err != nil {
			return err
		}
	}
	return nil
}

// ensureScheduleTemplateColumn adds schedule.template_id
// (bwsalmon/agents#516, schema.go's own DDL comment on the table has the
// reasoning) to a database created before task_template existed, the same
// probe-then-ALTER approach every ensure* migration in this file already
// uses. NULL for every existing row, matching every schedule already on
// it: none of them can have pointed at a template that did not yet exist
// to point at.
func (s *Store) ensureScheduleTemplateColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `template_id` FROM `schedule` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `schedule` ADD COLUMN `template_id` TEXT NULL")
	return err
}

// ensureScheduleSuiteColumn adds schedule.suite_id
// (model.Schedule.SuiteID's own doc comment has the reasoning) to a
// database created before a schedule could fire a task suite, the same
// probe-then-ALTER approach ensureScheduleTemplateColumn already uses for
// the column beside it. NULL for every existing row: a schedule already
// on such a database files a single task, which is exactly what NULL
// means here.
func (s *Store) ensureScheduleSuiteColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `suite_id` FROM `schedule` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `schedule` ADD COLUMN `suite_id` TEXT NULL")
	return err
}

// ensureTaskSuiteRunScheduleColumn adds task_suite_run.schedule_id
// (schema.go's own DDL comment on the table has the reasoning) to a
// database created before a schedule could fire a task suite. NULL for
// every existing run, matching what every one of them was: started by a
// human, not by a schedule.
func (s *Store) ensureTaskSuiteRunScheduleColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `schedule_id` FROM `task_suite_run` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE `task_suite_run` ADD COLUMN `schedule_id` TEXT NULL")
	return err
}

// ensureTaskOrderKeyColumn adds task.order_key (schema.go's own DDL
// comment on the table has the reasoning) to a database created before
// bwsalmon/agents#476, the same probe-then-ALTER approach every ensure*
// migration above already uses. Existing rows are backfilled in ascending
// id order and spaced by orderKeySpacing, the exact tiebreak Store.Ready's
// own ORDER BY used for task id before OrderKey existed -- so a database
// upgraded through this sees no change in dispatch order, only a new
// column recording what that order already was.
func (s *Store) ensureTaskOrderKeyColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `order_key` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `order_key` REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	var ids []string
	if err := each(ctx, s.db, "SELECT `id` FROM `task` ORDER BY `id`", nil,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		}); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			"UPDATE `task` SET `order_key` = ? WHERE `id` = ?",
			orderKeySpacing*float64(i+1), id); err != nil {
			return err
		}
	}
	return nil
}

// ensureConfigNewestFirstColumn adds grain_config.newest_first
// (model.Config.NewestFirst's own doc comment has the reasoning) to a
// database created before bwsalmon/agents#476, the same probe-then-ALTER
// approach ensureConfigTargetReposColumn already uses. Defaulting to 0
// (false) is what keeps an upgraded deployment's backlog order exactly as
// it was -- NewestFirst false is grain's original "new task dispatches
// last" shape.
func (s *Store) ensureConfigNewestFirstColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `newest_first` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `newest_first` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureConfigSandboxShapeColumns adds grain_config.sandbox_cpus/
// sandbox_memory_mb (schema.go's own DDL comment on the table has the
// reasoning -- bwsalmon/agents#534) to a database created before these
// columns existed, the same probe-then-ALTER approach every other
// ensure*Column migration here uses. Both default to 0, Config.
// SandboxCPUs/SandboxMemoryMB's own "unset, use grain's own default
// shape" zero value, so a database migrated from before this setting
// existed reads back exactly as if nobody had ever configured either.
func (s *Store) ensureConfigSandboxShapeColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `sandbox_cpus`, `sandbox_memory_mb` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `sandbox_cpus` INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `sandbox_memory_mb` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureTaskSandboxShapeColumns adds task.sandbox_cpus/sandbox_memory_mb
// (schema.go's own DDL comment on the table has the reasoning --
// bwsalmon/agents#534) to a database created before these columns
// existed, the same probe-then-ALTER approach ensureTaskApprovedAtColumn
// already uses. Both default to 0, Task.SandboxCPUs/SandboxMemoryMB's own
// "unset, use the deployment default" zero value.
func (s *Store) ensureTaskSandboxShapeColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `sandbox_cpus`, `sandbox_memory_mb` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `sandbox_cpus` INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `sandbox_memory_mb` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureConfigSandboxDiskColumn adds grain_config.sandbox_disk_gb, the
// third dimension of the same VM shape (model.Config.SandboxDiskGB,
// grain/task-41), to a database created before it existed.
//
// Its own migration rather than a third column in
// ensureConfigSandboxShapeColumns above: that one's probe finds
// sandbox_cpus/sandbox_memory_mb already present on every database
// migrated by an earlier build and returns without adding anything, so a
// column appended to it would only ever reach a database that had none
// of the three. Defaults to 0, SandboxDiskGB's own "unset, take grain's
// own default disk size" zero value.
func (s *Store) ensureConfigSandboxDiskColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `sandbox_disk_gb` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `sandbox_disk_gb` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureTaskSandboxDiskColumn is ensureConfigSandboxDiskColumn's
// per-task counterpart -- task.sandbox_disk_gb, Task.SandboxDiskGB's own
// column -- added separately from ensureTaskSandboxShapeColumns for the
// reason that function's doc comment gives. Defaults to 0, the "use the
// deployment default" zero value, so every task already stored reads
// back deferring to the deployment exactly as it did before a task could
// ask for a disk size at all.
func (s *Store) ensureTaskSandboxDiskColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `sandbox_disk_gb` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `sandbox_disk_gb` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureTaskAgentFrameworkColumn adds task.agent_framework (schema.go's
// own DDL comment on the table has the reasoning) to a database created
// before this column existed, the same probe-then-ALTER approach every
// other ensure*Column migration here uses. It defaults to ”,
// Task.AgentFramework's own "unset, use the deployment default" zero
// value -- so every task already in such a database reads back as
// deferring to Config.AgentFramework, which is exactly what it did
// before a task could carry a framework of its own.
func (s *Store) ensureTaskAgentFrameworkColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `agent_framework` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `agent_framework` TEXT NOT NULL DEFAULT ''")
	return err
}

// ensureTaskInteractiveColumn adds task.interactive (schema.go's own DDL
// comment on the table has the reasoning -- bwsalmon/agents#539) to a
// database created before this column existed, the same probe-then-ALTER
// approach every other ensure*Column migration here uses. It defaults to
// 0, Task.Interactive's own zero value, so a database migrated from
// before this field existed reads back as though every task in it had
// always been an ordinary, non-interactive one.
func (s *Store) ensureTaskInteractiveColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `interactive` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `interactive` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureTaskConfigurationColumn adds task.configuration (schema.go's own
// DDL comment on the table, and Task.Configuration's own doc comment,
// have the reasoning -- bwsalmon/agents#621) to a database created
// before this column existed, the same probe-then-ALTER approach every
// other ensure*Column migration here uses. It defaults to 0,
// Task.Configuration's own zero value, so a database migrated from
// before this field existed reads back as though no task in it had ever
// been the configuration agent.
func (s *Store) ensureTaskConfigurationColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `configuration` FROM `task` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `task` ADD COLUMN `configuration` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureConfigShowClosedByDefaultColumn adds
// grain_config.show_closed_by_default (model.Config.ShowClosedByDefault's
// own doc comment has the reasoning) to a database created before
// bwsalmon/agents#537, the same probe-then-ALTER approach
// ensureConfigNewestFirstColumn already uses. Defaulting to 0 (false)
// matches model.Config's own zero value, so an upgraded deployment gets
// exactly the new "hide closed tasks by default" behaviour the issue
// asked for, the same as a fresh one, until an operator opts back into
// showing them through Settings.
func (s *Store) ensureConfigShowClosedByDefaultColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `show_closed_by_default` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `show_closed_by_default` INTEGER NOT NULL DEFAULT 0")
	return err
}

// ensureConfigAgentFrameworkColumn adds grain_config.agent_framework
// (model.Config.AgentFramework's own doc comment has the reasoning) to a
// database created before bwsalmon/agents#609, the same probe-then-ALTER
// approach ensureConfigShowClosedByDefaultColumn already uses. It
// defaults to model.AgentFrameworkAntigravity, the framework a
// deployment that has never chosen one runs.
//
// A database that already has this column may well hold the legacy
// 'gemini' spelling instead, from before agent/antigravity replaced the
// home-grown Gemini runtime that word named. Nothing rewrites those rows
// -- ReadConfig runs every value through model.NormalizeAgentFramework
// on the way out, which is both cheaper than a data migration and the
// same answer for a config file or a -agent-framework flag that also
// still says "gemini".
func (s *Store) ensureConfigAgentFrameworkColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `agent_framework` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `agent_framework` TEXT NOT NULL DEFAULT 'antigravity'")
	return err
}

// ensureConfigTaskDefaultsColumns adds grain_config.approved_by_default/
// auto_merge_by_default (model.Config.ApprovedByDefault/AutoMergeByDefault's
// own doc comments have the reasoning -- bwsalmon/agents#612) to a
// database created before these columns existed, the same probe-then-ALTER
// approach ensureConfigSandboxShapeColumns already uses for a pair of
// columns at once. Both default to 1, matching model.DefaultConfig, so an
// upgraded deployment lands on the same "Queue immediately"/"Auto-merge
// once checks pass" starting state a fresh one does rather than on the
// zero value of a column it has never seen.
func (s *Store) ensureConfigTaskDefaultsColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `approved_by_default`, `auto_merge_by_default` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `approved_by_default` INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `auto_merge_by_default` INTEGER NOT NULL DEFAULT 1")
	return err
}

// ensureConfigTaskDefaultsOn turns both task defaults on for the one row
// of a database that already carries these columns from when their
// default was off, and records that it has done so.
//
// Changing model.DefaultConfig alone would not reach such a database:
// grain_config's single row is written wholesale by PutConfig with every
// column bound, so the 0 it stores was seeded by a build whose default
// was off, and it reads back afterwards indistinguishably from an
// operator having chosen off. Between those two readings, "seeded" is the
// only one any deployment can actually have: these two settings are days
// old (bwsalmon/agents#612), off was never anything a deployment opted
// into so much as what it got, and a deployment that had deliberately
// turned them off is not one asking for them to default on. Left alone,
// the new default would apply to fresh databases and to nobody currently
// running.
//
// It must not run twice, though -- an operator who turns either setting
// off after this lands has made exactly the deliberate choice the
// paragraph above says nobody had made yet, and a backfill re-running at
// the next restart would quietly overwrite it. The presence of
// task_defaults_on_backfilled is what records that, added in the same
// step as the UPDATE: probe-then-ALTER as usual, except that what the
// probe answers is "has this migration run" rather than "does this
// setting have a column". Its value is never read (schema.go's own note
// on the column: PutConfig doesn't bind it, so REPLACE re-defaults it on
// every settings save).
func (s *Store) ensureConfigTaskDefaultsOn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `task_defaults_on_backfilled` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	if _, err := s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `task_defaults_on_backfilled` INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE `grain_config` SET `approved_by_default` = 1, `auto_merge_by_default` = 1")
	return err
}

// ensureConfigClaudeModelColumn adds grain_config.claude_model
// (model.Config.ClaudeModel's own doc comment has the reasoning) to a
// database created before this column existed, the same probe-then-ALTER
// approach ensureConfigAgentFrameworkColumn already uses. It defaults to
// the empty string -- a database upgraded across this migration reads
// back an empty ClaudeModel until an operator sets one through Settings,
// the same gap ui.UpdateSettings' own "required the first time settings
// are saved" check exists to prevent for a deployment configured from
// scratch.
func (s *Store) ensureConfigClaudeModelColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `claude_model` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `claude_model` TEXT NOT NULL DEFAULT ''")
	return err
}

// ensureConfigDefaultCapabilitiesColumn adds grain_config.
// default_capabilities (model.Config.DefaultCapabilities' own doc
// comment has the reasoning) to a database created before this column
// existed, the same probe-then-ALTER approach
// ensureConfigClaudeModelColumn above uses. It defaults to the empty
// string, which splitCSV reads back as no defaults at all: a deployment
// upgraded across this migration keeps filing tasks with exactly the
// capabilities whoever filed them asked for, until an operator chooses a
// default set through Settings.
func (s *Store) ensureConfigDefaultCapabilitiesColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `default_capabilities` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `default_capabilities` TEXT NOT NULL DEFAULT ''")
	return err
}

// ensureConfigEnvironmentNameColumn adds grain_config.environment_name
// (model.Config.EnvironmentName's own doc comment has the reasoning) to
// a database created before this column existed, the same
// probe-then-ALTER approach ensureConfigDefaultCapabilitiesColumn above
// uses. It defaults to the empty string, which reads back as an unnamed
// deployment: an upgraded deployment's UI looks exactly as it did until
// an operator names it through Settings.
func (s *Store) ensureConfigEnvironmentNameColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `environment_name` FROM `grain_config` WHERE 1 = 0")
	if err == nil {
		return rows.Close()
	}
	_, err = s.db.ExecContext(ctx,
		"ALTER TABLE `grain_config` ADD COLUMN `environment_name` TEXT NOT NULL DEFAULT ''")
	return err
}

// ensureTaskTemplateNoTargetColumns drops task_template's old
// target_owner/target_name/base columns (schema.go's own DDL comment on
// the table has the reasoning: which repo and branch a firing targets is
// a property of the caller using a template, not of the template itself)
// from a database created before that split. Unlike every ensure*Column
// migration above, which probes for a column's absence and adds it, this
// probes for the old columns' presence and removes them -- the same
// direction ensureConfigMaxConcurrentColumn's own slots removal and
// ensureScheduleRecurrenceColumns' own interval_ms removal already go,
// since target_owner and target_name are NOT NULL and Init's own CREATE
// TABLE IF NOT EXISTS never alters a table that already exists: left in
// place, they would fail every PutTaskTemplate, which stops supplying
// them.
func (s *Store) ensureTaskTemplateNoTargetColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `target_owner` FROM `task_template` WHERE 1 = 0")
	if err != nil {
		// Already gone -- either a fresh database (Statements() above
		// never created it) or one already migrated past this point.
		return nil
	}
	rows.Close()
	for _, col := range []string{"target_owner", "target_name", "base"} {
		if _, err := s.db.ExecContext(ctx,
			"ALTER TABLE `task_template` DROP COLUMN `"+col+"`"); err != nil {
			return err
		}
	}
	return nil
}

// ErrConflict reports that an operation could not get a write in even
// after retrying -- the store stayed busy with some other writer for
// longer than it was willing to wait. A caller seeing it should tell
// whoever asked that their change did not land.
var ErrConflict = errors.New("conflict: the store kept changing under this operation")

// maxWriteAttempts bounds write's retry loop. A handful is plenty: the
// sqlite package's own busy_timeout already gives a blocked writer
// several seconds to get in on its own; a caller still losing this many
// attempts after that is contending with something that is not going to
// stop.
const maxWriteAttempts = 5

// write runs a mutation in one transaction, retrying it if the attempt
// could not get the store's write lock in time.
//
// Locking, not merging, is the whole mechanism now. sqlite.Open's
// _txlock=immediate takes SQLite's write lock at BEGIN rather than at
// the transaction's first write statement, so two overlapping mutations
// are serialised at the same point every time -- one proceeds, and the
// other either waits out busy_timeout or fails outright, in both cases
// before either has touched a row. That is a stronger guarantee than the
// store had against Dolt, which merged concurrent writers cell by cell
// and only reported a conflict when two of them touched the same cell
// with different values (dolt/store_test.go's now-deleted
// TestACounterStampWouldNotConflict pinned exactly that hazard); SQLite
// admits only one writer at a time, full stop, so there is nothing left
// for an artificial per-write marker to catch that the lock itself does
// not already catch.
//
// fn may run more than once, on a fresh transaction each time. It must
// therefore read what it needs through the querier it is handed rather
// than relying on anything read before write was called -- that is what
// makes a retry see the previous attempt's own result rather than
// rewriting over it.
func (s *Store) write(ctx context.Context, what string, fn func(*sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		err := s.writeOnce(ctx, fn)
		if err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("%w after %d attempts (%s): %v", ErrConflict, maxWriteAttempts, what, lastErr)
}

func (s *Store) writeOnce(ctx context.Context, fn func(*sql.Tx) error) error {
	// BeginTx is where a lost race shows up: sqlite.Open's DSN puts every
	// transaction in immediate mode, so this is where SQLite's write lock
	// is actually acquired (or waited for, or refused) rather than at the
	// first write statement inside fn.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// isSerializationFailure reports whether the database is telling us this
// attempt could not get the write lock and should be tried again.
//
// modernc.org/sqlite reports SQLite's own SQLITE_BUSY as "database is
// locked (5) (SQLITE_BUSY)" -- measured directly, in sqlite/store_test.go's
// TestSQLiteReportsABusyDatabase, which is what this matches against
// rather than a hard-coded error code from the driver's own package (that
// package imports no driver, on purpose -- pkg/model/sqlite's own doc
// comment).
//
// A miss here costs a pointless surfaced error rather than a wrong
// answer, and the pinning test is there to catch a reword.
func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"database is locked",
		"SQLITE_BUSY",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// PutTask inserts or replaces a task and everything hanging off it, in
// one transaction.
//
// Child rows are deleted and re-inserted rather than diffed: the sets are
// tiny, and "the row set equals the object" is a property worth having
// outright rather than maintaining.
func (s *Store) PutTask(ctx context.Context, t Task) error {
	return s.write(ctx, "put task "+t.ID, func(tx *sql.Tx) error { return putTask(ctx, tx, t) })
}

// putTask is PutTask's body, against whatever transaction it is running
// in -- so UpdateTask can read and write in the same one.
func putTask(ctx context.Context, tx *sql.Tx, t Task) error {
	oActor, oBehalf := t.Origin.Attribution.Actor, t.Origin.Attribution.OnBehalfOf
	var aActorKind, aActorID, aBehalfKind, aBehalfID any
	if t.Approval != nil {
		aActorKind, aActorID = string(t.Approval.Actor.Kind), t.Approval.Actor.ID
		if b := t.Approval.OnBehalfOf; b != nil {
			aBehalfKind, aBehalfID = string(b.Kind), b.ID
		}
	}
	var targetOwner, targetName any
	if t.Target != nil {
		targetOwner, targetName = t.Target.Owner, t.Target.Name
	}

	if _, err := tx.ExecContext(ctx, `REPLACE INTO `+"`task`"+` (
  `+"`id`, `intent`, `title`, `body`"+`,
  `+"`origin_actor_kind`, `origin_actor_id`, `origin_behalf_kind`, `origin_behalf_id`, `origin_reason`"+`,
  `+"`approval_actor_kind`, `approval_actor_id`, `approval_behalf_kind`, `approval_behalf_id`, `approved_at`"+`,
  `+"`target_owner`, `target_name`, `binding`, `base`, `folder`"+`,
  `+"`auto_merge`, `created_at`, `order_key`, `sandbox_cpus`, `sandbox_memory_mb`, `sandbox_disk_gb`, `interactive`, `configuration`, `agent_framework`"+`
) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?,?)`,
		t.ID, string(t.Intent), t.Title, t.Body,
		string(oActor.Kind), oActor.ID, kindOf(oBehalf), idOf(oBehalf), string(t.Origin.Reason),
		aActorKind, aActorID, aBehalfKind, aBehalfID, timeOf(t.ApprovedAt),
		targetOwner, targetName, string(t.Binding), nullable(t.Base), folderOf(t.Folder),
		t.AutoMerge, timeOf(t.CreatedAt), t.OrderKey, t.SandboxCPUs, t.SandboxMemoryMB, t.SandboxDiskGB, t.Interactive, t.Configuration,
		t.AgentFramework,
	); err != nil {
		return fmt.Errorf("writing task %s: %w", t.ID, err)
	}

	for _, table := range []string{"task_read", "task_grant", "task_link", "task_tag"} {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM `"+table+"` WHERE `task_id` = ?", t.ID); err != nil {
			return err
		}
	}
	for _, r := range t.Reads {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_read` (`task_id`, `owner`, `name`) VALUES (?,?,?)",
			t.ID, r.Owner, r.Name); err != nil {
			return err
		}
	}
	for _, g := range t.Grants {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_grant` (`task_id`, `capability`, `via`, `folder`) VALUES (?,?,?,?)",
			t.ID, g.Capability, string(g.Via), folderOf(g.Folder)); err != nil {
			return err
		}
	}
	for _, l := range t.Links {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_link` (`task_id`, `kind`, `target`, `blocks`) VALUES (?,?,?,?)",
			t.ID, string(l.Kind), l.Target, l.Kind.Blocks()); err != nil {
			return err
		}
	}
	for _, tag := range t.Tags {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_tag` (`task_id`, `tag`) VALUES (?,?)",
			t.ID, tag); err != nil {
			return err
		}
	}
	return nil
}

// NewTaskID allocates a task identity from the store.
//
// A task used to be named after the GitHub issue it was filed from, so
// nothing could file one without creating an issue first. This is that
// coupling's replacement: ids come from task_sequence, and a caller with
// a store and nothing else can create a task.
//
// The result is the decimal sequence number ("42"), which is what makes
// `grain get 42` and the branch `grain/task-42` read the way they do --
// where a GitHub-derived id put a repo path inside the branch name.
// Task.ID stays an opaque string, so nothing but this function is
// entitled to assume the shape.
func (s *Store) NewTaskID(ctx context.Context) (id string, err error) {
	err = s.write(ctx, "allocate a task id", func(tx *sql.Tx) error {
		id, err = newTaskID(ctx, tx)
		return err
	})
	return id, err
}

func newTaskID(ctx context.Context, tx *sql.Tx) (string, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `task_sequence` (`issued_at`) VALUES (?)", time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("allocating a task id: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reading the allocated task id: %w", err)
	}
	return strconv.FormatInt(n, 10), nil
}

// AddComment appends one entry to a task's conversation and returns the
// id the store assigned it -- which a caller recording an outstanding
// question needs, since Observation.PendingQuestionCommentID names one of
// these.
func (s *Store) AddComment(ctx context.Context, c Comment) (id int64, err error) {
	err = s.write(ctx, "comment on task "+c.TaskID, func(tx *sql.Tx) error {
		id, err = addComment(ctx, tx, c)
		return err
	})
	return id, err
}

func addComment(ctx context.Context, tx *sql.Tx, c Comment) (int64, error) {
	behalf := c.Author.OnBehalfOf
	res, err := tx.ExecContext(ctx, "INSERT INTO `task_comment` ("+
		"`task_id`, `author_kind`, `author_id`, `author_behalf_kind`, `author_behalf_id`, "+
		"`body`, `created_at`) VALUES (?,?,?,?,?,?,?)",
		c.TaskID, string(c.Author.Actor.Kind), c.Author.Actor.ID,
		kindOf(behalf), idOf(behalf), c.Body, c.CreatedAt.UTC())
	if err != nil {
		return 0, fmt.Errorf("commenting on task %s: %w", c.TaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading the new comment's id on task %s: %w", c.TaskID, err)
	}
	return id, nil
}

// Comments returns a task's conversation, oldest first. Ordering is by
// the assigned id rather than created_at: two comments written in the
// same instant still have an order, and it is the one they were written
// in.
func (s *Store) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+
		"`id`, `author_kind`, `author_id`, `author_behalf_kind`, `author_behalf_id`, "+
		"`body`, `created_at` FROM `task_comment` WHERE `task_id` = ? ORDER BY `id`", taskID)
	if err != nil {
		return nil, fmt.Errorf("reading comments on task %s: %w", taskID, err)
	}
	defer rows.Close()

	var out []Comment
	for rows.Next() {
		c := Comment{TaskID: taskID}
		var actorKind, actorID string
		var behalfKind, behalfID sql.NullString
		if err := rows.Scan(&c.ID, &actorKind, &actorID, &behalfKind, &behalfID,
			&c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Author.Actor = Principal{Kind: PrincipalKind(actorKind), ID: actorID}
		if behalfKind.Valid {
			c.Author.OnBehalfOf = &Principal{
				Kind: PrincipalKind(behalfKind.String), ID: behalfID.String,
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AttachmentMeta is one attachment's wire-worthy metadata, without its
// content -- what a task or a comment listing needs (bwsalmon/agents#522),
// kept separate from Attachment so reading every attachment on a busy
// task never pulls every one's full bytes along with it just to report a
// filename and a size.
type AttachmentMeta struct {
	ID          int64
	TaskID      string
	CommentID   *int64
	Filename    string
	ContentType string
	Size        int64
	CreatedAt   time.Time
}

// AddAttachment stores one file against a.TaskID -- a.CommentID nil for a
// file carried by the task's own body, or naming the Comment.ID it was
// posted alongside otherwise -- and returns the id the store assigned it.
func (s *Store) AddAttachment(ctx context.Context, a Attachment) (id int64, err error) {
	err = s.write(ctx, "add attachment to task "+a.TaskID, func(tx *sql.Tx) error {
		id, err = addAttachment(ctx, tx, a)
		return err
	})
	return id, err
}

func addAttachment(ctx context.Context, tx *sql.Tx, a Attachment) (int64, error) {
	res, err := tx.ExecContext(ctx, "INSERT INTO `task_attachment` ("+
		"`task_id`, `comment_id`, `filename`, `content_type`, `size`, `content`, `created_at`"+
		") VALUES (?,?,?,?,?,?,?)",
		a.TaskID, int64Of(a.CommentID), a.Filename, a.ContentType, a.Size, a.Content, a.CreatedAt.UTC())
	if err != nil {
		return 0, fmt.Errorf("adding attachment %q to task %s: %w", a.Filename, a.TaskID, err)
	}
	return res.LastInsertId()
}

// AttachmentMetas returns every attachment on taskID, oldest first, with
// no content -- what GetTask's own projection needs to list a task's own
// and every comment's attachments. Attachments and GetAttachment are the
// two calls that read content back.
func (s *Store) AttachmentMetas(ctx context.Context, taskID string) ([]AttachmentMeta, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+
		"`id`, `comment_id`, `filename`, `content_type`, `size`, `created_at` "+
		"FROM `task_attachment` WHERE `task_id` = ? ORDER BY `id`", taskID)
	if err != nil {
		return nil, fmt.Errorf("reading attachments on task %s: %w", taskID, err)
	}
	defer rows.Close()

	var out []AttachmentMeta
	for rows.Next() {
		m := AttachmentMeta{TaskID: taskID}
		var commentID sql.NullInt64
		if err := rows.Scan(&m.ID, &commentID, &m.Filename, &m.ContentType, &m.Size, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.CommentID = int64Ptr(commentID)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Attachments returns every attachment on taskID, oldest first, content
// included -- what a dispatched run needs to materialize them into its
// sandbox (orchestrator.placeAttachments).
func (s *Store) Attachments(ctx context.Context, taskID string) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+
		"`id`, `comment_id`, `filename`, `content_type`, `size`, `content`, `created_at` "+
		"FROM `task_attachment` WHERE `task_id` = ? ORDER BY `id`", taskID)
	if err != nil {
		return nil, fmt.Errorf("reading attachments on task %s: %w", taskID, err)
	}
	defer rows.Close()

	var out []Attachment
	for rows.Next() {
		a := Attachment{TaskID: taskID}
		var commentID sql.NullInt64
		if err := rows.Scan(&a.ID, &commentID, &a.Filename, &a.ContentType, &a.Size, &a.Content, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.CommentID = int64Ptr(commentID)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAttachment returns one attachment on taskID, content included, or
// nil if there is none with that id (including one that exists but
// belongs to a different task) -- the read behind a download endpoint,
// scoped to taskID the same way Comments already scopes by task so one
// task's attachment id can never be used to read another's file.
func (s *Store) GetAttachment(ctx context.Context, taskID string, id int64) (*Attachment, error) {
	a := Attachment{TaskID: taskID}
	var commentID sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT "+
		"`id`, `comment_id`, `filename`, `content_type`, `size`, `content`, `created_at` "+
		"FROM `task_attachment` WHERE `task_id` = ? AND `id` = ?", taskID, id).
		Scan(&a.ID, &commentID, &a.Filename, &a.ContentType, &a.Size, &a.Content, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading attachment %d on task %s: %w", id, taskID, err)
	}
	a.CommentID = int64Ptr(commentID)
	return &a, nil
}

// GetTask returns a task, or nil if there is none with that ID.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return getTask(ctx, s.db, id)
}

func getTask(ctx context.Context, q querier, id string) (*Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM `task` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := hydrate(ctx, q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// taskColumns is the task row, in the order scanTask reads it. Shared so
// GetTask and ListTasks cannot drift into scanning different column
// orders -- the kind of mismatch that fails as a type error at a
// distance, if it fails at all.
const taskColumns = "`id`,`intent`,`title`,`body`," +
	"`origin_actor_kind`,`origin_actor_id`,`origin_behalf_kind`,`origin_behalf_id`,`origin_reason`," +
	"`approval_actor_kind`,`approval_actor_id`,`approval_behalf_kind`,`approval_behalf_id`,`approved_at`," +
	"`target_owner`,`target_name`,`binding`,`base`,`folder`," +
	"`auto_merge`,`created_at`,`order_key`,`sandbox_cpus`,`sandbox_memory_mb`,`sandbox_disk_gb`,`interactive`,`configuration`," +
	"`agent_framework`"

// scanTask reads one task row. It takes the Scan method rather than a
// *sql.Row or *sql.Rows so one function serves both the single-row and
// the many-row query.
func scanTask(scan func(...any) error) (Task, error) {
	var t Task
	var intent, binding string
	var oaKind, oaID, oReason string
	var obKind, obID, aaKind, aaID, abKind, abID sql.NullString
	var tOwner, tName, base, folder sql.NullString
	var createdAt, approvedAt sql.NullTime
	if err := scan(&t.ID, &intent, &t.Title, &t.Body,
		&oaKind, &oaID, &obKind, &obID, &oReason,
		&aaKind, &aaID, &abKind, &abID, &approvedAt,
		&tOwner, &tName, &binding, &base, &folder,
		&t.AutoMerge, &createdAt, &t.OrderKey, &t.SandboxCPUs, &t.SandboxMemoryMB, &t.SandboxDiskGB, &t.Interactive, &t.Configuration,
		&t.AgentFramework); err != nil {
		return Task{}, err
	}

	t.Intent, t.Binding = Intent(intent), RepoBinding(binding)
	t.Origin = Origin{
		Attribution: Attribution{
			Actor:      Principal{Kind: PrincipalKind(oaKind), ID: oaID},
			OnBehalfOf: principalFrom(obKind, obID),
		},
		Reason: OriginReason(oReason),
	}
	if aaKind.Valid {
		t.Approval = &Attribution{
			Actor:      Principal{Kind: PrincipalKind(aaKind.String), ID: aaID.String},
			OnBehalfOf: principalFrom(abKind, abID),
		}
	}
	t.ApprovedAt = timePtr(approvedAt)
	if tOwner.Valid {
		t.Target = &RepoRef{Owner: tOwner.String, Name: tName.String}
	}
	t.Base = base.String
	t.Folder = ParseFolder(folder.String)
	if createdAt.Valid {
		t.CreatedAt = &createdAt.Time
	}
	return t, nil
}

// ListTasks returns every task in backlog order -- ascending OrderKey,
// the same order Store.Ready dispatches in -- fully hydrated.
//
// This is what a UI or a CLI lists, and it is deliberately the whole
// table: grain's task count is bounded by what a small team files by
// hand, and a store that answers "everything" in one call costs less
// complexity than a pagination scheme nothing has yet asked for. When
// that stops being true, this is where a filter goes.
//
// Hydration is a handful of queries per task rather than one join over
// all of them. The join would return a task's row once per read target
// per grant per link and need de-duplicating in Go, which is more code
// to get wrong than the extra round trips are worth at this size.
//
// A caller wanting the traditional newest-first list (ui.Client.ListTasks,
// unless model.Config.NewestFirst says otherwise -- bwsalmon/agents#476)
// reverses this slice rather than asking for a second order here: OrderKey
// ascending is the one order Ready also needs, so it is the one this
// store computes. Ties break by id, ascending: OrderKey is unique in
// practice (Store.OrderKeyForNewTask and Store.Reorder both space new
// values away from their neighbours) but nothing enforces it, so a tie
// still needs a stable, deterministic break.
func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM `task` ORDER BY `order_key`, `id`")
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := hydrate(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// States reads every task's state from the view in one query, for a
// caller rendering a list -- State per task would be one round trip each,
// and the view already computes the whole column.
func (s *Store) States(ctx context.Context) (map[string]State, error) {
	out := map[string]State{}
	err := each(ctx, s.db, "SELECT `task_id`,`state` FROM `task_state`", nil,
		func(rows *sql.Rows) error {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				return err
			}
			out[id] = State(state)
			return nil
		})
	return out, err
}

// MergeQueueBlocked reads every task_observation row whose
// merge_queue_blocked_at is set, in one query, for a caller rendering a
// list -- the same "one round trip for everyone" trade States already
// makes, applied to the one Observation field a task list needs
// (bwsalmon/agents#494's "make post complete states clear": a human
// needs to see, without opening each task, which ones the merge queue
// has given up fixing on its own).
func (s *Store) MergeQueueBlocked(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := each(ctx, s.db,
		"SELECT `task_id`,`merge_queue_blocked_at` FROM `task_observation` WHERE `merge_queue_blocked_at` IS NOT NULL", nil,
		func(rows *sql.Rows) error {
			var id string
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				return err
			}
			out[id] = at
			return nil
		})
	return out, err
}

func hydrate(ctx context.Context, q querier, t *Task) error {
	if err := each(ctx, q,
		"SELECT `owner`,`name` FROM `task_read` WHERE `task_id` = ? ORDER BY `owner`,`name`",
		t.ID, func(rows *sql.Rows) error {
			var r RepoRef
			if err := rows.Scan(&r.Owner, &r.Name); err != nil {
				return err
			}
			t.Reads = append(t.Reads, r)
			return nil
		}); err != nil {
		return err
	}
	grants, err := grantsOf(ctx, q, t.ID)
	if err != nil {
		return err
	}
	t.Grants = grants
	if err := each(ctx, q,
		"SELECT `kind`,`target` FROM `task_link` WHERE `task_id` = ? ORDER BY `kind`,`target`",
		t.ID, func(rows *sql.Rows) error {
			var l Link
			var kind string
			if err := rows.Scan(&kind, &l.Target); err != nil {
				return err
			}
			l.Kind = LinkKind(kind)
			t.Links = append(t.Links, l)
			return nil
		}); err != nil {
		return err
	}
	return each(ctx, q,
		"SELECT `tag` FROM `task_tag` WHERE `task_id` = ? ORDER BY `tag`",
		t.ID, func(rows *sql.Rows) error {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				return err
			}
			t.Tags = append(t.Tags, tag)
			return nil
		})
}

// Approve records who approved a task — the whole difference between
// proposed and queued, and what withdrawing would cancel.
// UpdateTask reads a task, applies mutate, and writes it back -- all in
// one stamped transaction, retried from the top if another writer wins.
//
// This is the way to change a task. mutate may run more than once, on a
// task freshly read inside each attempt, so it must be a function of the
// task it is handed rather than of anything captured earlier -- that is
// what makes the retry build on the winner's state rather than rewrite
// over it.
func (s *Store) UpdateTask(ctx context.Context, id string, mutate func(*Task) error) error {
	var missing bool
	err := s.write(ctx, "update task "+id, func(tx *sql.Tx) error {
		missing = false
		task, err := getTask(ctx, tx, id)
		if err != nil {
			return err
		}
		if task == nil {
			missing = true
			return nil
		}
		if err := mutate(task); err != nil {
			return err
		}
		return putTask(ctx, tx, *task)
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("updating task %s: no such task", id)
	}
	return nil
}

// ObserveField reads a task's observation (or starts a fresh one),
// applies set, and writes it back with ObservedAt stamped.
//
// Observe REPLACEs the whole row rather than patching one column, so a
// caller changing one field has to read the row first or erase the
// others. That read-modify-write lives here rather than in every caller,
// inside the same transaction as the write, for the reason UpdateTask
// gives.
func (s *Store) ObserveField(ctx context.Context, taskID string, now time.Time,
	set func(*Observation)) error {

	return s.write(ctx, "observe task "+taskID, func(tx *sql.Tx) error {
		obs, err := getObservation(ctx, tx, taskID)
		if err != nil {
			return fmt.Errorf("reading observation for %s: %w", taskID, err)
		}
		if obs == nil {
			obs = &Observation{TaskID: taskID}
		}
		set(obs)
		obs.ObservedAt = &now
		return observe(ctx, tx, *obs)
	})
}

// Approve takes approvedAt from the caller rather than reading the clock
// itself, the same discipline ObserveField and FinishRun already hold to
// -- ui.Client.now() is the one clock this store's writes ever run
// against, so a test gets a deterministic timestamp and a real deployment
// gets one instant shared with everything else that call recorded.
func (s *Store) Approve(ctx context.Context, taskID string, a Attribution, approvedAt time.Time) error {
	return s.write(ctx, "approve task "+taskID, func(tx *sql.Tx) error { return approve(ctx, tx, taskID, a, approvedAt) })
}

func approve(ctx context.Context, tx *sql.Tx, taskID string, a Attribution, approvedAt time.Time) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE `task` SET `approval_actor_kind` = ?, `approval_actor_id` = ?, "+
			"`approval_behalf_kind` = ?, `approval_behalf_id` = ?, `approved_at` = ? WHERE `id` = ?",
		string(a.Actor.Kind), a.Actor.ID, kindOf(a.OnBehalfOf), idOf(a.OnBehalfOf), approvedAt.UTC(), taskID)
	return err
}

// WithdrawApproval is approve's exact inverse, down to the columns it
// writes: it clears a task's approval, and a task with no approval is a
// proposal again -- task_state reads it as 'proposed' and task_ready
// never selects one, so nothing dispatches it until somebody approves it
// a second time. Withdrawing from a task that carries no approval writes
// the NULLs that are already there.
//
// approved_at goes with it rather than being kept as history, because
// keeping it would make the row ambiguous: Task.ApprovedAt's own doc
// comment reads a nil approval alongside a set approved_at as "approved
// before this column existed", and Transitions would go on reporting the
// task as having queued at an instant its approval no longer claims.
func (s *Store) WithdrawApproval(ctx context.Context, taskID string) error {
	return s.write(ctx, "withdraw approval from task "+taskID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task` SET `approval_actor_kind` = NULL, `approval_actor_id` = NULL, "+
				"`approval_behalf_kind` = NULL, `approval_behalf_id` = NULL, `approved_at` = NULL "+
				"WHERE `id` = ?", taskID)
		return err
	})
}

// Observe records what grain has seen about a task.
func (s *Store) Observe(ctx context.Context, o Observation) error {
	return s.write(ctx, "observe task "+o.TaskID, func(tx *sql.Tx) error { return observe(ctx, tx, o) })
}

// observe is Observe's body, against whatever transaction it is running
// in -- so ObserveField can read and write in the same one.
func observe(ctx context.Context, tx *sql.Tx, o Observation) error {
	_, err := tx.ExecContext(ctx,
		"REPLACE INTO `task_observation` (`task_id`,`closed_at`,`completed_at`,"+
			"`pending_question_comment_id`,`baseline_comment_id`,`merge_queue_blocked_at`,"+
			"`merge_queue_refreshed_at`,`observed_at`,"+
			"`retry_requested_at`,`pr_opened_at`,`pr_merged_at`,`pr_closed_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		o.TaskID, timeOf(o.ClosedAt), timeOf(o.CompletedAt),
		int64Of(o.PendingQuestionCommentID), int64Of(o.BaselineCommentID),
		timeOf(o.MergeQueueBlockedAt), timeOf(o.MergeQueueRefreshedAt),
		timeOf(o.ObservedAt), timeOf(o.RetryRequestedAt),
		timeOf(o.PrOpenedAt), timeOf(o.PrMergedAt), timeOf(o.PrClosedAt))
	return err
}

func (s *Store) GetObservation(ctx context.Context, taskID string) (*Observation, error) {
	return getObservation(ctx, s.db, taskID)
}

func getObservation(ctx context.Context, q querier, taskID string) (*Observation, error) {
	row := q.QueryRowContext(ctx,
		"SELECT `closed_at`,`completed_at`,`pending_question_comment_id`,"+
			"`baseline_comment_id`,`merge_queue_blocked_at`,`merge_queue_refreshed_at`,"+
			"`observed_at`,`retry_requested_at`,"+
			"`pr_opened_at`,`pr_merged_at`,`pr_closed_at` "+
			"FROM `task_observation` WHERE `task_id` = ?", taskID)
	o := Observation{TaskID: taskID}
	var closed, completed, blocked, refreshed, observed, retried, prOpened, prMerged, prClosed sql.NullTime
	var pending, baseline sql.NullInt64
	if err := row.Scan(&closed, &completed, &pending, &baseline, &blocked, &refreshed, &observed, &retried,
		&prOpened, &prMerged, &prClosed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o.ClosedAt, o.CompletedAt, o.ObservedAt = timePtr(closed), timePtr(completed), timePtr(observed)
	o.PendingQuestionCommentID, o.BaselineCommentID = int64Ptr(pending), int64Ptr(baseline)
	o.MergeQueueBlockedAt, o.MergeQueueRefreshedAt = timePtr(blocked), timePtr(refreshed)
	o.RetryRequestedAt = timePtr(retried)
	o.PrOpenedAt, o.PrMergedAt, o.PrClosedAt = timePtr(prOpened), timePtr(prMerged), timePtr(prClosed)
	return &o, nil
}

// ErrAtCapacity is what StartRun returns instead of recording a run when
// the limits it was given already admit no run of this one's kind. It is
// an ordinary outcome, not a fault: dispatch.Cycle counts live runs
// before it decides what to start, so seeing this means another caller
// started one in between -- the race this exists to lose safely. A caller
// treats it as "no free capacity this tick" and tries again on the next
// one.
var ErrAtCapacity = errors.New("model: the concurrency limit is already reached")

// StartRun records a run and its leases together, so long as limits still
// admit one more run of this one's kind -- worker or merger, decided from
// the task's own origin reason (OriginReason.Merger). Limits' own doc
// comment is the rule the two numbers make; Limits.Admits is the one
// implementation of it, shared with the caller's own look-before-you-leap
// check so the two cannot drift.
//
// The capacity check happens here, inside the same transaction as the
// insert, rather than in the caller that decided to start this run --
// which is the whole reason it takes limits at all. dispatch.Cycle reads
// the live-run counts and task_ready outside any single transaction and
// then issues a StartRun per unit of headroom it found, so nothing in Go
// stops two overlapping Cycle calls from both seeing the same headroom
// and both spending it. Under slots, a unique index on the slot each run
// claimed caught that after the fact (bwsalmon/agents#434); with nothing
// left to claim, the counts and the insert simply happen together
// instead, which rules the race out rather than detecting it. Limits that
// bound nothing (Limits.Unlimited, which the zero value is) disable the
// check -- for a caller with no limit of its own to enforce, such as a
// test starting a run directly, or dispatch's own configuration agent.
func (s *Store) StartRun(ctx context.Context, r Run, limits Limits) error {
	return s.write(ctx, "start run "+r.ID+" for task "+r.TaskID, func(tx *sql.Tx) error {
		if !limits.Unlimited() {
			live, err := liveRunCounts(ctx, tx)
			if err != nil {
				return err
			}
			merger, err := taskIsMerger(ctx, tx, r.TaskID)
			if err != nil {
				return err
			}
			if !limits.Admits(live, merger) {
				return ErrAtCapacity
			}
		}
		return startRun(ctx, tx, r)
	})
}

// taskIsMerger answers OriginReason.Merger for one task id, from the
// column rather than from a whole Task: this runs inside StartRun's own
// transaction, where the only thing worth reading is the one field the
// capacity check turns on.
//
// A task id with no row is not a merger. StartRun has no business being
// called for one, and a foreign key on task_run.task_id is what actually
// rejects it a statement later -- guessing "merger" for a task nobody can
// see would be the one reading that spends the reserved capacity.
func taskIsMerger(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var reason string
	err := tx.QueryRowContext(ctx,
		"SELECT `origin_reason` FROM `task` WHERE `id` = ?", taskID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading task %s's origin reason: %w", taskID, err)
	}
	return OriginReason(reason).Merger(), nil
}

// startRun uses INSERT rather than REPLACE, unlike most writes in this
// file, precisely so it does not behave like them here: REPLACE INTO
// resolves a conflict -- on task_run's own id, or on task_run_open_task's
// one-live-run-per-task index (schema.go's own doc comment on it) -- by
// silently deleting the conflicting row and inserting this one, which is
// exactly the silent-overwrite failure mode that index exists to rule
// out. A caller never legitimately starts the same run id twice (RunID's
// own doc comment: an id already names its task and attempt), so INSERT's
// ordinary conflict error is both correct and, for the task index
// specifically, the loud failure a second live run on one task should
// produce instead of one run's row quietly clobbering the other's.
func startRun(ctx context.Context, tx *sql.Tx, r Run) error {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `task_run` (`id`,`task_id`,`sandbox`,`unit`,`attempt`,"+
			"`started_at`,`finished_at`,`outcome`) VALUES (?,?,?,?,?,?,?,?)",
		r.ID, r.TaskID, r.Sandbox, nullable(r.Unit), r.Attempt,
		r.StartedAt.UTC(), timeOf(r.FinishedAt), nullable(r.Outcome)); err != nil {
		return err
	}
	for _, l := range r.Leases {
		if _, err := tx.ExecContext(ctx,
			"REPLACE INTO `lease` (`run_id`,`capability`,`resource`,`minted_by`,"+
				"`issued_at`,`expires_at`) VALUES (?,?,?,?,?,?)",
			r.ID, l.Capability, l.Resource, l.MintedBy.Name,
			l.IssuedAt.UTC(), timeOf(l.ExpiresAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) FinishRun(ctx context.Context, runID string, at time.Time, outcome, detail string) error {
	return s.write(ctx, "finish run "+runID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_run` SET `finished_at` = ?, `outcome` = ?, `detail` = ? WHERE `id` = ?",
			at.UTC(), outcome, nullable(detail), runID)
		return err
	})
}

// SetRunOutcome overrides a run's outcome and detail after FinishRun has
// already recorded one -- the one case FinishRun's own caller cannot yet
// know: RunDispatch judges outcome purely from whether the agent made a
// tool call at all (outcomeOf), before ProcessResult has checked
// whether that tool call amounted to anything -- a push, a question, a
// closing comment. A run that made calls but produced none
// of those would otherwise read "succeeded" forever, which both
// misreports what happened and would let it dodge FailureStreak's own
// cap indefinitely (bwsalmon/agents#403).
func (s *Store) SetRunOutcome(ctx context.Context, runID, outcome, detail string) error {
	return s.write(ctx, "set run "+runID+" outcome", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_run` SET `outcome` = ?, `detail` = ? WHERE `id` = ?",
			outcome, nullable(detail), runID)
		return err
	})
}

// SetRunSandbox records which sandbox a run was actually given, once one
// has been acquired for it. It is a write of its own, after StartRun
// rather than part of it, because the two happen at genuinely different
// moments: dispatch decides a run may start and records that decision
// durably (dispatch.Cycle's own doc comment) before any sandbox exists,
// and the orchestrator then builds one -- a kontur VM that has to boot,
// or a directory that has to be made -- and names it here.
//
// The gap between the two is deliberately visible rather than papered
// over. A live run whose sandbox is still "" is one whose sandbox is
// still being built, and every reader that resolves a sandbox to its task
// (Store.GitScope, Store.GitCredentialOverride) answers "no live run" for
// the empty name, which is the fail-closed default gitproxy already
// applies to a sandbox it does not recognise: a run that has not got a
// sandbox yet cannot have anything calling the proxy on its behalf.
func (s *Store) SetRunSandbox(ctx context.Context, runID, sandbox string) error {
	return s.write(ctx, "set run "+runID+" sandbox", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_run` SET `sandbox` = ? WHERE `id` = ?", sandbox, runID)
		return err
	})
}

// SetRunAgentStarted records the moment a run's agent actually got its
// first turn -- everything before it (a sandbox built, a repo cloned, a
// capability minted) is this run's setup, and everything after it is the
// agent's own work. It is its own write, after StartRun, for the same
// reason SetRunSandbox is: the two moments are genuinely different, and
// how far apart they are is the measurement (schema.go's own DDL comment
// on agent_started_at, and pkg/metrics).
//
// Recording it must never cost a run: the caller (orchestrator.
// RunDispatch) logs a failure here and dispatches anyway, since a
// measurement that cannot be taken is not a reason to refuse the work
// being measured.
func (s *Store) SetRunAgentStarted(ctx context.Context, runID string, at time.Time) error {
	return s.write(ctx, "set run "+runID+" agent start", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_run` SET `agent_started_at` = ? WHERE `id` = ?", at.UTC(), runID)
		return err
	})
}

// SetRunTranscript records a run's own agent transcript -- the full
// narrative record (agent.Result.Transcript) that FinishRun's own
// outcome/detail only summarise. It is its own write, called after
// FinishRun rather than folded into it, for the same reason SetRunOutcome
// is: RunDispatch only has a transcript to record once framework.Run has
// already returned, by which point FinishRun's own outcome/detail have
// already been decided from it (bwsalmon/agents#446 -- "Show attempt
// agent logs").
func (s *Store) SetRunTranscript(ctx context.Context, runID, transcript string) error {
	return s.write(ctx, "set run "+runID+" transcript", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_run` SET `transcript` = ? WHERE `id` = ?",
			nullable(transcript), runID)
		return err
	})
}

// RunTranscript returns the transcript recorded for taskID's numbered
// attempt, and whether such an attempt exists at all -- taskID+attempt
// rather than a bare run ID, since that pair (model.Run.Attempt) is the
// only handle on a run the wire's own Attempt shape ever gives a caller
// (ui.Attempt carries no run ID). The transcript itself may be "" either
// because the attempt has not finished yet or because its framework
// never populated one; both look the same here, and ui.Client tells them
// apart against the attempt's own FinishedAt.
func (s *Store) RunTranscript(ctx context.Context, taskID string, attempt int) (transcript string, found bool, err error) {
	var t sql.NullString
	err = s.db.QueryRowContext(ctx,
		"SELECT `transcript` FROM `task_run` WHERE `task_id` = ? AND `attempt` = ?",
		taskID, attempt).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return t.String, true, nil
}

// DropLease forgets a lease once its resource is actually revoked.
// Idempotent by construction — a DELETE matching nothing is not an error,
// which is what lets release and the expiry reaper both reach the same
// lease without coordinating.
func (s *Store) DropLease(ctx context.Context, runID, capability, resource string) error {
	return s.write(ctx, "drop lease "+capability+" on "+resource, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM `lease` WHERE `run_id` = ? AND `capability` = ? AND `resource` = ?",
			runID, capability, resource)
		return err
	})
}

// Attempts is how many times a task has been run — answerable because
// runs are rows, where the records previously existed as files nothing
// aggregated.
func (s *Store) Attempts(ctx context.Context, taskID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `task_run` WHERE `task_id` = ?", taskID).Scan(&n)
	return n, err
}

// Runs is every attempt at taskID, oldest first -- the full history
// Attempts only counts and FailureStreak only summarises, for a caller
// (Client.GetTask) that wants to show each one: when it ran, whether it
// finished, and how (bwsalmon/agents#445).
func (s *Store) Runs(ctx context.Context, taskID string) ([]Run, error) {
	var out []Run
	err := each(ctx, s.db,
		"SELECT `id`,`sandbox`,`unit`,`attempt`,`started_at`,`finished_at`,`outcome`,`detail` "+
			"FROM `task_run` WHERE `task_id` = ? ORDER BY `attempt` ASC", taskID,
		func(rows *sql.Rows) error {
			r := Run{TaskID: taskID}
			var unit, outcome, detail sql.NullString
			var finishedAt sql.NullTime
			if err := rows.Scan(&r.ID, &r.Sandbox, &unit, &r.Attempt,
				&r.StartedAt, &finishedAt, &outcome, &detail); err != nil {
				return err
			}
			r.Unit, r.Outcome, r.Detail = unit.String, outcome.String, detail.String
			r.FinishedAt = timePtr(finishedAt)
			out = append(out, r)
			return nil
		})
	return out, err
}

// FailureStreak is taskID's own task_streak.streak (Count), plus the most
// recent finished run's own outcome/detail -- the two things task_streak
// itself cannot carry (schema.go's own doc comment on that view: "the
// view intentionally carries no more than the count task_state's cutoff
// needs"), and the two things a real timestamp comparison against a
// caller-supplied now needs that a view, re-evaluated against whatever
// the wall clock says at query time, cannot give a deterministic test.
//
// nil, with no error, means taskID has never finished a run at all --
// dispatch.Cycle's own retry backoff and Client.GetTask's own display
// both treat that the same as "not currently failing".
//
// A PausedOutcome run is passed over rather than counted, exactly as
// task_streak's own WHERE clause passes over it: it is neither a failure
// of this task nor a success that clears the failures before it. Its
// LastFinishedAt/LastOutcome still describe it where it is the most
// recent run, since those two fields answer "what happened last", not
// "how badly is this task going".
func (s *Store) FailureStreak(ctx context.Context, taskID string) (*FailureStreak, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `outcome`,`started_at`,`finished_at`,`detail` FROM `task_run` "+
			"WHERE `task_id` = ? AND `finished_at` IS NOT NULL ORDER BY `started_at` DESC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var since time.Time
	if obs, err := getObservation(ctx, s.db, taskID); err != nil {
		return nil, err
	} else if obs != nil && obs.RetryRequestedAt != nil {
		since = *obs.RetryRequestedAt
	}

	var streak *FailureStreak
	for rows.Next() {
		var outcome string
		var startedAt, finishedAt time.Time
		var detail sql.NullString
		if err := rows.Scan(&outcome, &startedAt, &finishedAt, &detail); err != nil {
			return nil, err
		}
		if streak == nil {
			streak = &FailureStreak{LastFinishedAt: finishedAt, LastOutcome: outcome, LastDetail: detail.String}
		}
		if outcome == "succeeded" || !since.IsZero() && !startedAt.After(since) {
			break
		}
		if outcome == PausedOutcome {
			// Skipped, not counted and not a boundary: a run stopped
			// because the deployment had no agent budget left is
			// evidence about the deployment and none at all about this
			// task (PausedOutcome's own doc comment), so it neither adds
			// to the streak behind it nor clears it. task_streak
			// excludes the same word with the same WHERE clause.
			continue
		}
		streak.Count++
	}
	return streak, rows.Err()
}

// FailureStreak is one task's own retry history -- see Store.FailureStreak.
type FailureStreak struct {
	// Count is how many of the task's most recent runs, in a row, ended
	// without succeeding -- 0 once the most recent run itself succeeded,
	// or was requested more recently than the task's last retry request.
	Count          int
	LastFinishedAt time.Time
	LastOutcome    string
	LastDetail     string
}

// LiveLease is one outstanding lease, joined to the run holding it.
type LiveLease struct {
	RunID      string
	TaskID     string
	Capability string
	Resource   string
	MintedBy   string
	IssuedAt   time.Time
	ExpiresAt  *time.Time
}

// LiveLeases returns outstanding leases, optionally only those minted by
// one credential — which is what makes "what would rotating this break?"
// a query rather than an unanswerable question.
func (s *Store) LiveLeases(ctx context.Context, mintedBy string) ([]LiveLease, error) {
	q := "SELECT `run_id`,`task_id`,`capability`,`resource`,`minted_by`,`issued_at`,`expires_at` " +
		"FROM `lease_live`"
	args := []any{}
	if mintedBy != "" {
		q += " WHERE `minted_by` = ?"
		args = append(args, mintedBy)
	}
	q += " ORDER BY `issued_at`"
	var out []LiveLease
	err := each(ctx, s.db, q, args, func(rows *sql.Rows) error {
		var l LiveLease
		var expires sql.NullTime
		if err := rows.Scan(&l.RunID, &l.TaskID, &l.Capability, &l.Resource,
			&l.MintedBy, &l.IssuedAt, &expires); err != nil {
			return err
		}
		l.ExpiresAt = timePtr(expires)
		out = append(out, l)
		return nil
	})
	return out, err
}

// State reads a task's state from the view. There is no column to read it
// from, which is the point.
func (s *Store) State(ctx context.Context, taskID string) (State, error) {
	var st string
	err := s.db.QueryRowContext(ctx,
		"SELECT `state` FROM `task_state` WHERE `task_id` = ?", taskID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return State(st), err
}

// Ready is every task dispatchable right now: approved, not running, with
// no open blocker -- in dispatch order, which is the backlog's own order
// (bwsalmon/agents#476): ascending OrderKey, the same order ListTasks
// hands a UI or CLI before any newest-first display flip, ties broken on
// task ID.
//
// That is the whole rule, with no carve-out for any kind of task. A fix
// task the merge queue filed (Origin.Reason == ReasonFix) used to sort
// ahead of everything else here regardless of where the backlog put it --
// bwsalmon/agents#389's "a queue head's repair must not wait behind
// unrelated new work", since the longer it waits the more likely
// something else lands on the branch it targets and the fix has to be
// refiled rather than simply merged. That priority is a position now
// rather than a sort: orchestrator.fileFixTask files the task at the very
// head of the backlog (OrderKeyForNewTask, atFront), where a human can
// see it sitting first, see why the queue behind it is waiting, and drag
// it elsewhere if they disagree. A rule that lived only in this ORDER BY
// could do none of that -- the list said one thing and the dispatcher did
// another -- and two orderings that have to agree but are computed in
// different places are the kind that quietly stop agreeing.
func (s *Store) Ready(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db,
		"SELECT `r`.`task_id` FROM `task_ready` AS `r` "+
			"JOIN `task` AS `t` ON `t`.`id` = `r`.`task_id` "+
			"ORDER BY `t`.`order_key`, `r`.`task_id`",
		nil,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		})
	return out, err
}

// ReadyMergers is Ready narrowed to the merge queue's own fix tasks
// (Origin.Reason == ReasonFix, OriginReason.Merger) -- the ready tasks
// whose runs are mergers, and so bounded by Limits.Mergers on top of
// Limits.Workers rather than by the worker ceiling alone.
//
// It is a second, narrower query rather than a kind returned alongside
// each entry of Ready because that is all its caller needs: dispatch.
// Cycle walks Ready in Ready's order and asks of each candidate only
// "which of the two limits does this one spend", which membership of this
// set answers. Ready keeps its own shape, and the order the two agree on
// stays the one order task_ready and the backlog already define -- there
// is no second ordering here to disagree with it.
func (s *Store) ReadyMergers(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db,
		"SELECT `r`.`task_id` FROM `task_ready` AS `r` "+
			"JOIN `task` AS `t` ON `t`.`id` = `r`.`task_id` "+
			"WHERE `t`.`origin_reason` = ? "+
			"ORDER BY `t`.`order_key`, `r`.`task_id`",
		[]any{string(ReasonFix)},
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		})
	return out, err
}

// ReadyConfiguration is Ready narrowed to the configuration agent
// (Task.Configuration, bwsalmon/agents#621): every such task
// dispatchable right now, in the same backlog order Ready itself uses
// (ascending OrderKey, task ID the tiebreak), and with no carve-out of
// its own for the same reason Ready has none: position in the backlog is
// the whole of the order, here as there.
//
// dispatch.Cycle calls this before it ever looks at any limit, and
// starts every task it returns unconditionally: the configuration agent
// exists precisely for a person to reach for when something -- possibly
// the deployment's own concurrency limit having no headroom left -- is
// already wrong, so it cannot itself wait on that headroom.
func (s *Store) ReadyConfiguration(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db,
		"SELECT `r`.`task_id` FROM `task_ready` AS `r` "+
			"JOIN `task` AS `t` ON `t`.`id` = `r`.`task_id` "+
			"WHERE `t`.`configuration` = 1 "+
			"ORDER BY `t`.`order_key`, `r`.`task_id`",
		nil,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		})
	return out, err
}

// orderKeySpacing is the gap Store.OrderKeyForNewTask and a rebalance
// (rebalanceOrderKeys) leave between adjacent tasks' OrderKey. It only
// has to be wide enough that ordinary use doesn't need a rebalance often
// -- Store.Reorder's own splitOrderKeys still narrows the gap between two
// neighbours every time something is dropped between them, and
// rebalanceOrderKeys is what restores it once minOrderKeyGap says that
// gap is getting too fine to split again.
const orderKeySpacing = 1 << 20

// minOrderKeyGap is the smallest gap Store.Reorder will still split
// rather than rebalance first. float64 has roughly 15-17 significant
// decimal digits; orderKeySpacing's 2^20 leaves this many orders of
// magnitude of headroom below it before two distinct float64 values
// would stop being representably distinct, which is the actual failure
// this bounds against -- not a UX judgement about how fine a manual
// reorder is allowed to get.
const minOrderKeyGap = 1e-6

// OrderKeyForNewTask returns the OrderKey a newly filed task should take:
// one orderKeySpacing step past whichever extreme of the backlog
// model.Config.NewestFirst currently asks new work to join. atFront asks
// for the low end -- Ready dispatches ascending OrderKey, so a task
// placed there runs before everything already queued, which is what
// NewestFirst true means. atFront false (NewestFirst's own default) is
// the opposite end: last in line, behind everything already queued, the
// FIFO backlog grain has always defaulted to. An empty task table (no
// extreme to step past) returns 0, same as OrderKey's own zero value.
func (s *Store) OrderKeyForNewTask(ctx context.Context, atFront bool) (float64, error) {
	q := "SELECT MAX(`order_key`) FROM `task`"
	if atFront {
		q = "SELECT MIN(`order_key`) FROM `task`"
	}
	var extreme sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, q).Scan(&extreme); err != nil {
		return 0, fmt.Errorf("computing a new task's order key: %w", err)
	}
	if !extreme.Valid {
		return 0, nil
	}
	if atFront {
		return extreme.Float64 - orderKeySpacing, nil
	}
	return extreme.Float64 + orderKeySpacing, nil
}

// Reorder moves ids to sit between whatever afterID and beforeID
// currently name -- a drag-and-drop move (bwsalmon/agents#476), against
// the same OrderKey column ListTasks and Ready both already read, so a
// move here is immediately visible to both. Either bound may be nil: no
// afterID means ids become the new minimum -- dropped at the head of a
// list, "just before the following job" -- and no beforeID means they
// become the new maximum. Both nil is only reachable by dropping into an
// empty list, which cannot happen through the UI (there would be nothing
// to drop onto) but is handled the same as any other unbounded case
// rather than rejected. ids keep their relative order among themselves,
// so dragging a multi-selection reorders it as a block rather than by id.
//
// A neighbour named by afterID or beforeID that does not exist is an
// error: both are read fresh from the store inside this call's own
// transaction, so this can only mean the caller's own view was already
// stale (the task was closed or reordered out from under it) between
// when it computed the request and when this ran. So is an id among ids
// that does not exist, for the same reason.
//
// ids is re-sorted by each task's own current OrderKey before anything is
// written, rather than trusted to already be in that order -- a
// multi-select drag's relative order is a property of the backlog these
// ids already had, not an incidental fact about Set iteration order or
// click order a caller would otherwise have to get right itself.
func (s *Store) Reorder(ctx context.Context, ids []string, afterID, beforeID *string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, "reorder tasks", func(tx *sql.Tx) error {
		ordered, _, err := sortByOrderKey(ctx, tx, ids)
		if err != nil {
			return err
		}
		lower, upper, err := orderKeyBounds(ctx, tx, afterID, beforeID)
		if err != nil {
			return err
		}
		if !orderKeysFitBetween(lower, upper, len(ordered)) {
			if err := rebalanceOrderKeys(ctx, tx); err != nil {
				return err
			}
			if lower, upper, err = orderKeyBounds(ctx, tx, afterID, beforeID); err != nil {
				return err
			}
		}
		for i, key := range splitOrderKeys(lower, upper, len(ordered)) {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `task` SET `order_key` = ? WHERE `id` = ?", key, ordered[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// MoveToFrontOfBacklog places ids at the front of the backlog: ahead of
// every other task, and behind nothing but the merge tasks
// (Origin.Reason == ReasonFix) already scheduled at the very head of it.
// They keep their existing relative order among themselves, the same
// guarantee a multi-select drag gets from Reorder and against the same
// OrderKey column, so this is a move a human could have made by hand
// rather than a second ordering rule layered on top of theirs.
//
// It is the merge queue making its own order visible
// (orchestrator.SyncPullRequests): a pull request waiting to land is the
// work closest to done, and which of them lands next used to be a
// comparison the queue made inside one cycle and never wrote down.
// Putting them at the front, in the order they will land, lets a list
// answer "what is grain about to finish" without anyone opening a task --
// and, because the queue reads its own head back off this same order
// (orchestrator.queueHeads), dragging one of those tasks above another
// really does change which merges first. An order a human can see and an
// order a human can set are the same fact here, deliberately.
//
// Nothing is written when ids already sit there in that order, which is
// what lets a reconciler call this every cycle. A task dragged out of the
// block altogether does come back to it next cycle -- while it is in the
// queue its position belongs to the queue -- but it comes back where the
// drag left it relative to the others, so "merge this one last" still
// lands.
//
// An id naming no task is an error, the same as Reorder's own and for the
// same reason: every key here is read inside this call's transaction, so
// missing one means the caller's view was already stale.
func (s *Store) MoveToFrontOfBacklog(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, "move tasks to the front of the backlog", func(tx *sql.Tx) error {
		ordered, keys, err := sortByOrderKey(ctx, tx, ids)
		if err != nil {
			return err
		}
		lower, upper, err := frontOfBacklogBounds(ctx, tx, ordered)
		if err != nil {
			return err
		}
		if alreadyAtFrontOfBacklog(ordered, keys, lower, upper) {
			return nil
		}
		if !orderKeysFitBetween(lower, upper, len(ordered)) {
			if err := rebalanceOrderKeys(ctx, tx); err != nil {
				return err
			}
			if lower, upper, err = frontOfBacklogBounds(ctx, tx, ordered); err != nil {
				return err
			}
		}
		for i, key := range splitOrderKeys(lower, upper, len(ordered)) {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `task` SET `order_key` = ? WHERE `id` = ?", key, ordered[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// frontOfBacklogBounds is the interval MoveToFrontOfBacklog moves ids
// into: below the first task of the ordinary backlog, above whatever
// merge tasks are already scheduled at the very head of it. Either side
// is nil when there is nothing on it -- a deployment whose whole backlog
// is the merge queue, or one with no fix task filed -- which
// splitOrderKeys reads as "step away from the other side" the same way
// Reorder's own unbounded drops do.
//
// The head is decided by position, not by state: only a fix task that
// actually sits ahead of the whole ordinary backlog bounds the block from
// below. One dragged down into the middle of the backlog since, or filed
// before fix tasks had a place of their own and left at OrderKey's zero
// value, is just another task there -- pinning the merge queue behind it
// would put the block somewhere nobody is looking, which is the opposite
// of what moving it to the front is for.
func frontOfBacklogBounds(ctx context.Context, tx *sql.Tx, ids []string) (lower, upper *float64, err error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+2)
	args = append(args, string(ReasonFix))
	for _, id := range ids {
		args = append(args, id)
	}

	var ordinary sql.NullFloat64
	if err := tx.QueryRowContext(ctx,
		"SELECT MIN(`order_key`) FROM `task` "+
			"WHERE `origin_reason` <> ? AND `id` NOT IN ("+placeholders+")",
		args...).Scan(&ordinary); err != nil {
		return nil, nil, fmt.Errorf("reading the first task of the backlog: %w", err)
	}

	q := "SELECT MAX(`order_key`) FROM `task` " +
		"WHERE `origin_reason` = ? AND `id` NOT IN (" + placeholders + ")"
	if ordinary.Valid {
		q += " AND `order_key` < ?"
		args = append(args, ordinary.Float64)
	}
	var head sql.NullFloat64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&head); err != nil {
		return nil, nil, fmt.Errorf("reading the merge tasks at the head of the backlog: %w", err)
	}

	if head.Valid {
		lower = &head.Float64
	}
	if ordinary.Valid {
		upper = &ordinary.Float64
	}
	return lower, upper, nil
}

// alreadyAtFrontOfBacklog reports whether ordered already holds distinct,
// ascending OrderKey values strictly inside (lower, upper) -- exactly what
// MoveToFrontOfBacklog would otherwise write, so writing it would move
// nothing. It is asked before every one of those writes because the
// reconciler calling it has nothing to do on almost every cycle, and a
// write that changes no order still narrows the gaps the next one has to
// split.
func alreadyAtFrontOfBacklog(ordered []string, keys map[string]float64, lower, upper *float64) bool {
	first, last := keys[ordered[0]], keys[ordered[len(ordered)-1]]
	if lower != nil && first <= *lower {
		return false
	}
	if upper != nil && last >= *upper {
		return false
	}
	for i := 1; i < len(ordered); i++ {
		if keys[ordered[i]] <= keys[ordered[i-1]] {
			return false
		}
	}
	return true
}

// sortByOrderKey returns ids sorted ascending by each task's current
// OrderKey, alongside the keys it read to do it -- Reorder's own "the
// block keeps its existing relative order" guarantee, and the same order
// MoveToFrontOfBacklog carries to the front of the backlog.
func sortByOrderKey(ctx context.Context, tx *sql.Tx, ids []string) ([]string, map[string]float64, error) {
	keys := make(map[string]float64, len(ids))
	for _, id := range ids {
		k, err := orderKeyOf(ctx, tx, id)
		if err != nil {
			return nil, nil, err
		}
		keys[id] = k
	}
	ordered := append([]string(nil), ids...)
	sort.SliceStable(ordered, func(i, j int) bool { return keys[ordered[i]] < keys[ordered[j]] })
	return ordered, keys, nil
}

// orderKeyBounds resolves Reorder's afterID/beforeID to the OrderKey
// values already in the store -- read inside Reorder's own transaction,
// never trusted from an earlier read, the same "re-read, never pin"
// discipline IsBlocked's own doc comment argues for.
func orderKeyBounds(ctx context.Context, tx *sql.Tx, afterID, beforeID *string) (lower, upper *float64, err error) {
	if afterID != nil {
		k, err := orderKeyOf(ctx, tx, *afterID)
		if err != nil {
			return nil, nil, err
		}
		lower = &k
	}
	if beforeID != nil {
		k, err := orderKeyOf(ctx, tx, *beforeID)
		if err != nil {
			return nil, nil, err
		}
		upper = &k
	}
	return lower, upper, nil
}

func orderKeyOf(ctx context.Context, tx *sql.Tx, taskID string) (float64, error) {
	var k float64
	err := tx.QueryRowContext(ctx, "SELECT `order_key` FROM `task` WHERE `id` = ?", taskID).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reordering: no such task %s", taskID)
	}
	return k, err
}

// orderKeysFitBetween reports whether n new, distinct OrderKey values can
// still be split out of (lower, upper) without any pair closer than
// minOrderKeyGap -- Reorder's own signal that a rebalance has to run
// before it can compute keys, not after. An unbounded side (either nil)
// always fits: Reorder only ever narrows a gap by splitting between two
// existing neighbours, so the only way to run out of room is repeated
// splits of the same bounded interval.
func orderKeysFitBetween(lower, upper *float64, n int) bool {
	if lower == nil || upper == nil {
		return true
	}
	return (*upper-*lower)/float64(n+1) >= minOrderKeyGap
}

// splitOrderKeys computes n OrderKey values strictly between lower and
// upper (whichever are non-nil), evenly spaced and in ascending order --
// Reorder's own ids keep this order, which is what keeps a multi-select
// drag's relative order intact.
func splitOrderKeys(lower, upper *float64, n int) []float64 {
	keys := make([]float64, n)
	switch {
	case lower != nil && upper != nil:
		step := (*upper - *lower) / float64(n+1)
		for i := range keys {
			keys[i] = *lower + step*float64(i+1)
		}
	case lower != nil:
		for i := range keys {
			keys[i] = *lower + orderKeySpacing*float64(i+1)
		}
	case upper != nil:
		for i := range keys {
			keys[i] = *upper - orderKeySpacing*float64(n-i)
		}
	default:
		for i := range keys {
			keys[i] = orderKeySpacing * float64(i+1)
		}
	}
	return keys
}

// rebalanceOrderKeys renumbers every task's OrderKey, ascending and
// spaced by orderKeySpacing, without changing their relative order --
// Reorder's own backstop against minOrderKeyGap, restoring room to split
// between two neighbours that repeated drops have crowded together. It
// runs inside Reorder's own transaction, so the renumbering and the move
// that triggered it land together or not at all.
func rebalanceOrderKeys(ctx context.Context, tx *sql.Tx) error {
	var ids []string
	if err := each(ctx, tx, "SELECT `id` FROM `task` ORDER BY `order_key`, `id`", nil,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		}); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE `task` SET `order_key` = ? WHERE `id` = ?",
			orderKeySpacing*float64(i+1), id); err != nil {
			return err
		}
	}
	return nil
}

// LiveRunCount is how many runs are currently in flight -- rows with no
// `finished_at`. A dispatch loop reads this, rather than remembering what
// it handed out last cycle, for the same reason IsBlocked re-reads closed
// dependencies: a run can finish between cycles with nothing about the
// loop's own state changing, and the headroom that frees must show up the
// moment it is asked about.
//
// This is the count on its own because a count is all a caller deciding
// what to dispatch needs now. It replaced OccupiedSlots, which returned
// the identifier of every slot holding a live run, back when what a cycle
// needed was the difference between a fixed pool and the part of it in
// use. There is no pool to difference against any more: a sandbox is
// created for a run and destroyed with it, so the only question left is
// how many are in flight against model.Limits.
//
// It is deliberately not what StartRun enforces the limit with -- see
// that method's own doc comment on why the check has to happen inside the
// insert's transaction rather than against a count read beforehand.
func (s *Store) LiveRunCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `task_run` WHERE `finished_at` IS NULL").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting live runs: %w", err)
	}
	return n, nil
}

// LiveRunCounts is LiveRunCount split the way Limits is (grain/task-63):
// the same live rows, counted as mergers (a run of a merge-queue fix
// task) and workers (everything else) separately, since the two are
// bounded differently and a caller deciding what to dispatch has to know
// which of the two it has room for.
//
// Everything LiveRunCount's own doc comment says about re-reading rather
// than remembering applies here unchanged; this is that count with the
// one distinction the limits draw, not a second notion of what is live.
func (s *Store) LiveRunCounts(ctx context.Context) (RunCounts, error) {
	return liveRunCounts(ctx, s.db)
}

// liveRunCounts is LiveRunCounts against either the store's own handle or
// one transaction -- StartRun needs exactly this count inside the same
// transaction as its insert, which is the only way the limit is enforced
// at all rather than merely checked.
//
// LEFT JOIN, not JOIN: a live run whose task row has somehow gone still
// occupies a sandbox and still has to be counted against the total. It
// counts as a worker, which is the same "don't spend the merge queue's
// reserved capacity on a task nobody can see" reading taskIsMerger takes.
func liveRunCounts(ctx context.Context, q querier) (RunCounts, error) {
	var c RunCounts
	var mergers, total int
	if err := q.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(CASE WHEN `t`.`origin_reason` = ? THEN 1 ELSE 0 END), 0), COUNT(*) "+
			"FROM `task_run` AS `r` LEFT JOIN `task` AS `t` ON `t`.`id` = `r`.`task_id` "+
			"WHERE `r`.`finished_at` IS NULL",
		string(ReasonFix)).Scan(&mergers, &total); err != nil {
		return RunCounts{}, fmt.Errorf("counting live runs: %w", err)
	}
	c.Mergers, c.Workers = mergers, total-mergers
	return c, nil
}

// LiveRuns is every task_run row with no `finished_at` -- the same rows
// LiveRunCount counts, in full, for a caller (orchestrator.
// RecoverOrphanedRuns) that needs to know which task and run each one
// belongs to rather than just how many there are. A daemon calls
// this exactly once, at startup, before it has driven any run of its
// own: at that point every row here can only be left over from a
// process that is no longer around to finish it -- see that func's own
// doc comment for why a run still legitimately in flight is never
// mistaken for one of these.
func (s *Store) LiveRuns(ctx context.Context) ([]Run, error) {
	var out []Run
	err := each(ctx, s.db,
		"SELECT `id`,`task_id`,`sandbox`,`attempt`,`started_at` "+
			"FROM `task_run` WHERE `finished_at` IS NULL ORDER BY `id`", nil,
		func(rows *sql.Rows) error {
			var r Run
			if err := rows.Scan(&r.ID, &r.TaskID, &r.Sandbox, &r.Attempt, &r.StartedAt); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	return out, err
}

// GitScope is what a sandbox's currently live run may touch through the
// git proxy: the write target of the task it is running, and its
// read-only repos. Nil target and an empty slice mean the sandbox has no
// live run right now, which the proxy must treat as "touch nothing" --
// the same fail-closed default a static allowlist gave it, except this
// one can never drift from what a task actually declares, because it
// isn't a second copy of that declaration.
func (s *Store) GitScope(ctx context.Context, sandbox string) (target *RepoRef, reads []RepoRef, err error) {
	taskID, live, err := s.liveTaskID(ctx, sandbox)
	if err != nil || !live {
		return nil, nil, err
	}

	row := s.db.QueryRowContext(ctx,
		"SELECT `target_owner`, `target_name` FROM `task` WHERE `id` = ?", taskID)
	var tOwner, tName sql.NullString
	if err = row.Scan(&tOwner, &tName); err != nil {
		return nil, nil, fmt.Errorf("reading target of task %s: %w", taskID, err)
	}
	if tOwner.Valid {
		target = &RepoRef{Owner: tOwner.String, Name: tName.String}
	}

	err = each(ctx, s.db,
		"SELECT `owner`,`name` FROM `task_read` WHERE `task_id` = ? ORDER BY `owner`,`name`",
		taskID, func(rows *sql.Rows) error {
			var r RepoRef
			if err := rows.Scan(&r.Owner, &r.Name); err != nil {
				return err
			}
			reads = append(reads, r)
			return nil
		})
	if err != nil {
		return nil, nil, fmt.Errorf("reading reads of task %s: %w", taskID, err)
	}
	return target, reads, nil
}

// GitCredentialOverride is the named credential a sandbox's currently
// live task asks the git proxy to use in place of the owner/repo ladder,
// via a GitCredentialGrant among its Grants -- bwsalmon/agents#52's
// `grain-github-<name>` label, ported onto Task.Grants rather than
// grain/proxy/tokens.py's second, sandbox-keyed SandboxCredentialOverrides
// file. false means no override: either the sandbox has no live run, or
// its task carries none, and the proxy falls back to the ordinary
// per-repo credential ladder.
func (s *Store) GitCredentialOverride(ctx context.Context, sandbox string) (name string, ok bool, err error) {
	taskID, live, err := s.liveTaskID(ctx, sandbox)
	if err != nil || !live {
		return "", false, err
	}
	grants, err := grantsOf(ctx, s.db, taskID)
	if err != nil {
		return "", false, fmt.Errorf("reading grants of task %s: %w", taskID, err)
	}
	name, ok = gitCredentialOverride(grants)
	return name, ok, nil
}

// liveTaskID is the task ID of the sandbox's currently live run, if any --
// shared by GitScope and GitCredentialOverride, which answer two
// different questions about the same live task. live is false, with a
// nil error, for a sandbox with nothing running on it right now.
func (s *Store) liveTaskID(ctx context.Context, sandbox string) (taskID string, live bool, err error) {
	// A run records its sandbox only once one has been acquired for it
	// (SetRunSandbox), so "" is not a name -- it is the absence of one,
	// and matching it against the rows that have not been filled in yet
	// would hand a caller some arbitrary still-provisioning run's task.
	// Refusing it here keeps that out of every reader below at once.
	if sandbox == "" {
		return "", false, nil
	}
	err = s.db.QueryRowContext(ctx,
		"SELECT `task_id` FROM `task_run` WHERE `sandbox` = ? AND `finished_at` IS NULL "+
			"ORDER BY `started_at` DESC LIMIT 1", sandbox).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("finding the live run on %s: %w", sandbox, err)
	}
	return taskID, true, nil
}

// grantsOf is a task's Grants, read straight off task_grant -- shared by
// hydrate (a full Task) and GitCredentialOverride (which needs only
// these, and without ever paying for the rest of the task).
func grantsOf(ctx context.Context, q querier, taskID string) ([]Grant, error) {
	var grants []Grant
	err := each(ctx, q,
		"SELECT `capability`,`via`,`folder` FROM `task_grant` WHERE `task_id` = ? ORDER BY `capability`",
		taskID, func(rows *sql.Rows) error {
			var g Grant
			var via string
			var folder sql.NullString
			if err := rows.Scan(&g.Capability, &via, &folder); err != nil {
				return err
			}
			g.Via, g.Folder = GrantSource(via), ParseFolder(folder.String)
			grants = append(grants, g)
			return nil
		})
	return grants, err
}

// TaskPullRequestLink is one task_link row of kind LinkFixes, belonging to
// a task whose state is 'completed' — a run pushed and a PR was opened or
// found for it, and grain has not yet observed that PR finish.
type TaskPullRequestLink struct {
	TaskID string
	// PullRequest is the link's own target, a model.PullRequestRef's
	// String() — parse it back with model.ParsePullRequestRef.
	PullRequest string
}

// OpenPullRequestLinks returns every fixes-link on a completed task —
// what a GitHub-sync component polls each cycle to find a PR whose health
// it should refresh, without needing a table of its own: task_link and
// task_state already carry everything this needs, and task_state already
// stops returning 'completed' the moment task_observation's closed_at is
// set, so a closed-out task drops out of this list with no extra
// bookkeeping.
func (s *Store) OpenPullRequestLinks(ctx context.Context) ([]TaskPullRequestLink, error) {
	var out []TaskPullRequestLink
	err := each(ctx, s.db,
		"SELECT `l`.`task_id`, `l`.`target` FROM `task_link` AS `l` "+
			"JOIN `task_state` AS `st` ON `st`.`task_id` = `l`.`task_id` "+
			"WHERE `l`.`kind` = ? AND `st`.`state` = 'completed' ORDER BY `l`.`task_id`",
		string(LinkFixes),
		func(rows *sql.Rows) error {
			var l TaskPullRequestLink
			if err := rows.Scan(&l.TaskID, &l.PullRequest); err != nil {
				return err
			}
			out = append(out, l)
			return nil
		})
	return out, err
}

// OpenBlockers is how many unclosed tasks stand in front of this one.
func (s *Store) OpenBlockers(ctx context.Context, taskID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT `open_blockers` FROM `task_blocked` WHERE `task_id` = ?", taskID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// GetConfig reads the deployment's stored configuration, or nil (with no
// error) if nothing has written one yet -- a fresh database, or one
// whose daemon has never started. See Config's own doc comment on why
// nil, not a zero-value Config, marks that case.
func (s *Store) GetConfig(ctx context.Context) (*Config, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+configColumns+" FROM `grain_config` WHERE `id` = 1")
	c, err := scanConfig(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

const configColumns = "`poll_interval_ms`,`max_workers`,`max_mergers`,`gemini_model`,`max_agent_turns`," +
	"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`," +
	"`newest_first`,`sandbox_cpus`,`sandbox_memory_mb`,`sandbox_disk_gb`,`show_closed_by_default`,`agent_framework`," +
	"`approved_by_default`,`auto_merge_by_default`,`claude_model`,`default_capabilities`,`environment_name`"

func scanConfig(scan func(...any) error) (Config, error) {
	var c Config
	var pollMS int64
	var targetRepos string
	var defaultCapabilities string
	if err := scan(&pollMS, &c.MaxWorkers, &c.MaxMergers, &c.GeminiModel, &c.MaxAgentTurns,
		&c.GitHubHost, &c.GitHubInsecureHTTP, &c.GCPProject, &c.GCPServiceAccountEmail,
		&targetRepos, &c.NewestFirst, &c.SandboxCPUs, &c.SandboxMemoryMB, &c.SandboxDiskGB, &c.ShowClosedByDefault,
		&c.AgentFramework, &c.ApprovedByDefault, &c.AutoMergeByDefault, &c.ClaudeModel,
		&defaultCapabilities, &c.EnvironmentName); err != nil {
		return Config{}, err
	}
	c.PollInterval = time.Duration(pollMS) * time.Millisecond
	c.TargetRepos = splitCSV(targetRepos)
	c.DefaultCapabilities = splitCSV(defaultCapabilities)
	// A row written before agent/antigravity replaced the home-grown
	// Gemini runtime still says "gemini"; folding that in here rather
	// than migrating the row is what ensureConfigAgentFrameworkColumn's
	// own doc comment describes.
	c.AgentFramework = NormalizeAgentFramework(c.AgentFramework)
	return c, nil
}

// PutConfig replaces the deployment's stored configuration wholesale --
// there is one row, so there is nothing to merge a partial update
// against; a caller changing one field reads Config first the same way
// UpdateTask's mutate does for a task.
func (s *Store) PutConfig(ctx context.Context, c Config) error {
	return s.write(ctx, "update config", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"REPLACE INTO `grain_config` (`id`, "+configColumns+") VALUES (1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			c.PollInterval.Milliseconds(), c.MaxWorkers, c.MaxMergers, c.GeminiModel, c.MaxAgentTurns,
			c.GitHubHost, c.GitHubInsecureHTTP, c.GCPProject, c.GCPServiceAccountEmail,
			joinCSV(c.TargetRepos), c.NewestFirst, c.SandboxCPUs, c.SandboxMemoryMB, c.SandboxDiskGB, c.ShowClosedByDefault,
			c.AgentFramework, c.ApprovedByDefault, c.AutoMergeByDefault, c.ClaudeModel,
			joinCSV(c.DefaultCapabilities), c.EnvironmentName)
		return err
	})
}

// joinCSV/splitCSV round-trip Config.TargetRepos (an owner/name repo can
// never contain a comma) through the same comma-separated shape the
// daemon's own -target-repos flag already parses, so a value written by
// one reads back identically through the other.
//
// Config.DefaultCapabilities is stored the same way, for the same reason:
// a capability id is a bare word (ui.OfferedCapabilities' own rows), with
// no more room for a comma in it than a repo name has.
func joinCSV(items []string) string { return strings.Join(items, ",") }

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// --- schedules -------------------------------------------------------

// NewScheduleID allocates a schedule identity from its own sequence,
// distinct from task_sequence -- so a schedule's id (e.g. "sched-3") is
// never mistaken for one of the tasks it files, the same "not the GitHub
// issue it was filed from" reasoning NewTaskID's own doc comment gives.
func (s *Store) NewScheduleID(ctx context.Context) (id string, err error) {
	err = s.write(ctx, "allocate a schedule id", func(tx *sql.Tx) error {
		id, err = newScheduleID(ctx, tx)
		return err
	})
	return id, err
}

func newScheduleID(ctx context.Context, tx *sql.Tx) (string, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `schedule_sequence` (`issued_at`) VALUES (?)", time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("allocating a schedule id: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reading the allocated schedule id: %w", err)
	}
	return "sched-" + strconv.FormatInt(n, 10), nil
}

const scheduleColumns = "`id`,`title`,`body`,`target_owner`,`target_name`,`base`," +
	"`auto_merge`,`template_id`,`suite_id`,`recurrence_kind`,`every_n_hours`,`time_of_day_minutes`,`weekday`,`day_of_month`," +
	"`enabled`,`next_run_at`,`last_run_at`,`created_at`"

func scanSchedule(scan func(...any) error) (Schedule, error) {
	var t Schedule
	var base, templateID, suiteID sql.NullString
	var kind string
	var everyNHours, timeOfDay, weekday, dayOfMonth sql.NullInt64
	var lastRun sql.NullTime
	if err := scan(&t.ID, &t.Title, &t.Body, &t.Target.Owner, &t.Target.Name, &base,
		&t.AutoMerge, &templateID, &suiteID, &kind, &everyNHours, &timeOfDay, &weekday, &dayOfMonth,
		&t.Enabled, &t.NextRunAt, &lastRun, &t.CreatedAt); err != nil {
		return Schedule{}, err
	}
	t.Base = base.String
	if templateID.Valid {
		t.TemplateID = &templateID.String
	}
	if suiteID.Valid {
		t.SuiteID = &suiteID.String
	}
	t.Recurrence = Recurrence{
		Kind:        RecurrenceKind(kind),
		EveryNHours: int(everyNHours.Int64),
		TimeOfDay:   int(timeOfDay.Int64),
		Weekday:     time.Weekday(weekday.Int64),
		DayOfMonth:  int(dayOfMonth.Int64),
	}
	t.LastRunAt = timePtr(lastRun)
	return t, nil
}

// PutSchedule inserts or replaces a schedule wholesale -- putTask's own
// multi-table dance, now that Reads and Grants give a schedule child rows
// of its own (bwsalmon/agents#464).
func (s *Store) PutSchedule(ctx context.Context, t Schedule) error {
	return s.write(ctx, "put schedule "+t.ID,
		func(tx *sql.Tx) error { return putSchedule(ctx, tx, t) })
}

func putSchedule(ctx context.Context, tx *sql.Tx, t Schedule) error {
	r := t.Recurrence
	var everyNHours, timeOfDay, weekday, dayOfMonth any
	switch r.Kind {
	case RecurrenceEveryNHours:
		everyNHours = r.EveryNHours
	case RecurrenceDaily:
		timeOfDay = r.TimeOfDay
	case RecurrenceWeekly:
		timeOfDay, weekday = r.TimeOfDay, int(r.Weekday)
	case RecurrenceMonthly:
		timeOfDay, dayOfMonth = r.TimeOfDay, r.DayOfMonth
	}
	_, err := tx.ExecContext(ctx, `REPLACE INTO `+"`schedule`"+` (
  `+"`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
		"`auto_merge`,`template_id`,`suite_id`,`recurrence_kind`,`every_n_hours`,`time_of_day_minutes`,`weekday`,`day_of_month`,"+
		"`enabled`,`next_run_at`,`last_run_at`,`created_at`"+`
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Body, t.Target.Owner, t.Target.Name, nullable(t.Base),
		t.AutoMerge, stringOf(t.TemplateID), stringOf(t.SuiteID), string(r.Kind), everyNHours, timeOfDay, weekday, dayOfMonth,
		t.Enabled, t.NextRunAt.UTC(), timeOf(t.LastRunAt), t.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("writing schedule %s: %w", t.ID, err)
	}

	for _, table := range []string{"schedule_read", "schedule_grant"} {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM `"+table+"` WHERE `schedule_id` = ?", t.ID); err != nil {
			return err
		}
	}
	for _, r := range t.Reads {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `schedule_read` (`schedule_id`, `owner`, `name`) VALUES (?,?,?)",
			t.ID, r.Owner, r.Name); err != nil {
			return err
		}
	}
	for _, g := range t.Grants {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `schedule_grant` (`schedule_id`, `capability`, `via`, `folder`) VALUES (?,?,?,?)",
			t.ID, g.Capability, string(g.Via), folderOf(g.Folder)); err != nil {
			return err
		}
	}
	return nil
}

// scheduleReadsOf and scheduleGrantsOf are a schedule's Reads and Grants,
// read straight off their own tables -- task.go's grantsOf and GetTask's
// own reads query, ported onto schedule_id.
func scheduleReadsOf(ctx context.Context, q querier, id string) ([]RepoRef, error) {
	var reads []RepoRef
	err := each(ctx, q,
		"SELECT `owner`,`name` FROM `schedule_read` WHERE `schedule_id` = ? ORDER BY `owner`,`name`",
		id, func(rows *sql.Rows) error {
			var r RepoRef
			if err := rows.Scan(&r.Owner, &r.Name); err != nil {
				return err
			}
			reads = append(reads, r)
			return nil
		})
	return reads, err
}

func scheduleGrantsOf(ctx context.Context, q querier, id string) ([]Grant, error) {
	var grants []Grant
	err := each(ctx, q,
		"SELECT `capability`,`via`,`folder` FROM `schedule_grant` WHERE `schedule_id` = ? ORDER BY `capability`",
		id, func(rows *sql.Rows) error {
			var g Grant
			var via string
			var folder sql.NullString
			if err := rows.Scan(&g.Capability, &via, &folder); err != nil {
				return err
			}
			g.Via, g.Folder = GrantSource(via), ParseFolder(folder.String)
			grants = append(grants, g)
			return nil
		})
	return grants, err
}

// hydrateSchedule fills in t's Reads and Grants, read off their own
// tables -- scanSchedule itself only ever reads schedule's own columns,
// the same split scanning a Task's own row has from grantsOf/its reads
// query.
func hydrateSchedule(ctx context.Context, q querier, t *Schedule) error {
	reads, err := scheduleReadsOf(ctx, q, t.ID)
	if err != nil {
		return fmt.Errorf("reading reads of schedule %s: %w", t.ID, err)
	}
	grants, err := scheduleGrantsOf(ctx, q, t.ID)
	if err != nil {
		return fmt.Errorf("reading grants of schedule %s: %w", t.ID, err)
	}
	t.Reads, t.Grants = reads, grants
	return nil
}

func getSchedule(ctx context.Context, q querier, id string) (*Schedule, error) {
	t, err := scanSchedule(q.QueryRowContext(ctx,
		"SELECT "+scheduleColumns+" FROM `schedule` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := hydrateSchedule(ctx, q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetSchedule returns a schedule, or nil if there is none with that ID.
func (s *Store) GetSchedule(ctx context.Context, id string) (*Schedule, error) {
	return getSchedule(ctx, s.db, id)
}

// ListSchedules returns every schedule, newest first -- ListTasks' own
// "the whole table" reasoning applies again at this size.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduleColumns+" FROM `schedule` ORDER BY `created_at` DESC, `id` DESC")
	if err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	var out []Schedule
	for rows.Next() {
		t, err := scanSchedule(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := hydrateSchedule(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DueSchedules is every enabled schedule whose next run has come -- what
// the orchestrator's schedule reconciler fires each cycle. Ordered by id
// for a deterministic firing order, the same reasoning v1's own
// ScheduledJobsConfig.load gives for sorting by name.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduleColumns+" FROM `schedule` "+
			"WHERE `enabled` = 1 AND `next_run_at` <= ? ORDER BY `id`", now.UTC())
	if err != nil {
		return nil, fmt.Errorf("listing due schedules: %w", err)
	}
	var out []Schedule
	for rows.Next() {
		t, err := scanSchedule(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := hydrateSchedule(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateSchedule reads a schedule, applies mutate, and writes it back --
// UpdateTask's own read-modify-write-and-retry shape, for the same
// reason: mutate may run more than once, on a schedule freshly read
// inside each attempt.
func (s *Store) UpdateSchedule(ctx context.Context, id string, mutate func(*Schedule) error) error {
	var missing bool
	err := s.write(ctx, "update schedule "+id, func(tx *sql.Tx) error {
		missing = false
		t, err := getSchedule(ctx, tx, id)
		if err != nil {
			return err
		}
		if t == nil {
			missing = true
			return nil
		}
		if err := mutate(t); err != nil {
			return err
		}
		return putSchedule(ctx, tx, *t)
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("updating schedule %s: no such schedule", id)
	}
	return nil
}

// DeleteSchedule removes a schedule -- unlike a task (Close's own doc
// comment: "a task that ran is a record of a dispatch that happened"), a
// schedule is only ever a standing declaration with no history of its own
// worth keeping once a human no longer wants it, so deleting it outright
// (rather than adding a closed-like flag) loses nothing: every task it
// already filed remains exactly where it always was, untouched by this.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	return s.write(ctx, "delete schedule "+id, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM `schedule` WHERE `id` = ?", id)
		return err
	})
}

// HasOpenTaskWithTag reports whether any task carrying tag has not yet
// reached model.StateClosed -- the idempotency check a schedule's own
// firing needs before filing another one. v1's scheduled_jobs.py gave
// every job a marker_label and had _scheduled_jobs list issues by it to
// find a previous firing that has not finished; docs/data-model.md kept
// that idea as a plain tag rather than a capability or a state of its
// own ("neither a state nor a capability: it is an idempotency tag"),
// and this is the query that reads it back.
func (s *Store) HasOpenTaskWithTag(ctx context.Context, tag string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `task_tag` AS `tg` "+
			"JOIN `task_state` AS `st` ON `st`.`task_id` = `tg`.`task_id` "+
			"WHERE `tg`.`tag` = ? AND `st`.`state` != ?", tag, string(StateClosed)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking for an open task tagged %s: %w", tag, err)
	}
	return n > 0, nil
}

// --- task templates ----------------------------------------------------

// NewTaskTemplateID allocates a template identity from its own sequence,
// distinct from task_sequence and schedule_sequence -- the same "not
// mistaken for one of the things that use it" reasoning NewScheduleID's
// own doc comment gives.
func (s *Store) NewTaskTemplateID(ctx context.Context) (id string, err error) {
	err = s.write(ctx, "allocate a task template id", func(tx *sql.Tx) error {
		id, err = newTaskTemplateID(ctx, tx)
		return err
	})
	return id, err
}

func newTaskTemplateID(ctx context.Context, tx *sql.Tx) (string, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `task_template_sequence` (`issued_at`) VALUES (?)", time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("allocating a task template id: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reading the allocated task template id: %w", err)
	}
	return "template-" + strconv.FormatInt(n, 10), nil
}

const taskTemplateColumns = "`id`,`name`,`title`,`body`,`auto_merge`,`created_at`"

func scanTaskTemplate(scan func(...any) error) (TaskTemplate, error) {
	var t TaskTemplate
	if err := scan(&t.ID, &t.Name, &t.Title, &t.Body, &t.AutoMerge, &t.CreatedAt); err != nil {
		return TaskTemplate{}, err
	}
	return t, nil
}

// PutTaskTemplate inserts or replaces a template wholesale --
// putSchedule's own multi-table dance, ported onto task_template's own
// child tables.
func (s *Store) PutTaskTemplate(ctx context.Context, t TaskTemplate) error {
	return s.write(ctx, "put task template "+t.ID,
		func(tx *sql.Tx) error { return putTaskTemplate(ctx, tx, t) })
}

func putTaskTemplate(ctx context.Context, tx *sql.Tx, t TaskTemplate) error {
	_, err := tx.ExecContext(ctx, `REPLACE INTO `+"`task_template`"+` (
  `+"`id`,`name`,`title`,`body`,`auto_merge`,`created_at`"+`
) VALUES (?,?,?,?,?,?)`,
		t.ID, t.Name, t.Title, t.Body, t.AutoMerge, t.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("writing task template %s: %w", t.ID, err)
	}

	for _, table := range []string{"task_template_read", "task_template_grant"} {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM `"+table+"` WHERE `task_template_id` = ?", t.ID); err != nil {
			return err
		}
	}
	for _, r := range t.Reads {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_template_read` (`task_template_id`, `owner`, `name`) VALUES (?,?,?)",
			t.ID, r.Owner, r.Name); err != nil {
			return err
		}
	}
	for _, g := range t.Grants {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_template_grant` (`task_template_id`, `capability`, `via`, `folder`) VALUES (?,?,?,?)",
			t.ID, g.Capability, string(g.Via), folderOf(g.Folder)); err != nil {
			return err
		}
	}
	return nil
}

// hydrateTaskTemplate fills in t's Reads and Grants, read off their own
// tables -- hydrateSchedule's own split-scan reasoning applies again
// here.
func hydrateTaskTemplate(ctx context.Context, q querier, t *TaskTemplate) error {
	var reads []RepoRef
	if err := each(ctx, q,
		"SELECT `owner`,`name` FROM `task_template_read` WHERE `task_template_id` = ? ORDER BY `owner`,`name`",
		t.ID, func(rows *sql.Rows) error {
			var r RepoRef
			if err := rows.Scan(&r.Owner, &r.Name); err != nil {
				return err
			}
			reads = append(reads, r)
			return nil
		}); err != nil {
		return fmt.Errorf("reading reads of task template %s: %w", t.ID, err)
	}
	var grants []Grant
	if err := each(ctx, q,
		"SELECT `capability`,`via`,`folder` FROM `task_template_grant` WHERE `task_template_id` = ? ORDER BY `capability`",
		t.ID, func(rows *sql.Rows) error {
			var g Grant
			var via string
			var folder sql.NullString
			if err := rows.Scan(&g.Capability, &via, &folder); err != nil {
				return err
			}
			g.Via, g.Folder = GrantSource(via), ParseFolder(folder.String)
			grants = append(grants, g)
			return nil
		}); err != nil {
		return fmt.Errorf("reading grants of task template %s: %w", t.ID, err)
	}
	t.Reads, t.Grants = reads, grants
	return nil
}

func getTaskTemplate(ctx context.Context, q querier, id string) (*TaskTemplate, error) {
	t, err := scanTaskTemplate(q.QueryRowContext(ctx,
		"SELECT "+taskTemplateColumns+" FROM `task_template` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := hydrateTaskTemplate(ctx, q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTaskTemplate returns a template, or nil if there is none with that ID.
func (s *Store) GetTaskTemplate(ctx context.Context, id string) (*TaskTemplate, error) {
	return getTaskTemplate(ctx, s.db, id)
}

// ListTaskTemplates returns every template, newest first --
// ListSchedules' own "the whole table" reasoning applies again at this
// size.
func (s *Store) ListTaskTemplates(ctx context.Context) ([]TaskTemplate, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+taskTemplateColumns+" FROM `task_template` ORDER BY `created_at` DESC, `id` DESC")
	if err != nil {
		return nil, fmt.Errorf("listing task templates: %w", err)
	}
	var out []TaskTemplate
	for rows.Next() {
		t, err := scanTaskTemplate(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := hydrateTaskTemplate(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateTaskTemplate reads a template, applies mutate, and writes it back
// -- UpdateSchedule's own read-modify-write-and-retry shape, for the same
// reason: mutate may run more than once, on a template freshly read
// inside each attempt.
func (s *Store) UpdateTaskTemplate(ctx context.Context, id string, mutate func(*TaskTemplate) error) error {
	var missing bool
	err := s.write(ctx, "update task template "+id, func(tx *sql.Tx) error {
		missing = false
		t, err := getTaskTemplate(ctx, tx, id)
		if err != nil {
			return err
		}
		if t == nil {
			missing = true
			return nil
		}
		if err := mutate(t); err != nil {
			return err
		}
		return putTaskTemplate(ctx, tx, *t)
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("updating task template %s: no such task template", id)
	}
	return nil
}

// DeleteTaskTemplate removes a template outright -- DeleteSchedule's own
// doc comment gives the reasoning: a template is only ever a standing
// declaration, so there is no history on the row itself worth keeping
// once nobody wants it. Callers that must not orphan a schedule still
// pointing at this template (ui.Client.DeleteTemplate) check
// SchedulesUsingTemplate first; the store itself enforces nothing here,
// the same way it enforces nothing about a task naming a repo nobody
// configured.
func (s *Store) DeleteTaskTemplate(ctx context.Context, id string) error {
	return s.write(ctx, "delete task template "+id, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM `task_template` WHERE `id` = ?", id)
		return err
	})
}

// SchedulesUsingTemplate returns every schedule whose TemplateID is id --
// what ui.Client.DeleteTemplate checks before deleting one out from under
// a schedule that still fires from it, and what a template's own "used by
// N schedules" display reads.
func (s *Store) SchedulesUsingTemplate(ctx context.Context, id string) ([]Schedule, error) {
	return s.schedulesUsing(ctx, "`template_id`", "template", id)
}

// SchedulesUsingSuite returns every schedule whose SuiteID is id -- what
// ui.Client.DeleteSuite checks before deleting a suite out from under a
// schedule that still runs it, SchedulesUsingTemplate's own reasoning
// applied to the other thing a schedule can point at.
func (s *Store) SchedulesUsingSuite(ctx context.Context, id string) ([]Schedule, error) {
	return s.schedulesUsing(ctx, "`suite_id`", "task suite", id)
}

func (s *Store) schedulesUsing(ctx context.Context, column, what, id string) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduleColumns+" FROM `schedule` WHERE "+column+" = ? ORDER BY `id`", id)
	if err != nil {
		return nil, fmt.Errorf("listing schedules using %s %s: %w", what, id, err)
	}
	var out []Schedule
	for rows.Next() {
		t, err := scanSchedule(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := hydrateSchedule(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- helpers ---------------------------------------------------------

func each(ctx context.Context, q querier, query string, args any,
	scan func(*sql.Rows) error) error {
	var list []any
	switch a := args.(type) {
	case nil:
	case []any:
		list = a
	default:
		list = []any{a}
	}
	rows, err := q.QueryContext(ctx, query, list...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func principalFrom(kind, id sql.NullString) *Principal {
	if !kind.Valid || kind.String == "" {
		return nil
	}
	return &Principal{Kind: PrincipalKind(kind.String), ID: id.String}
}

func kindOf(p *Principal) any {
	if p == nil {
		return nil
	}
	return string(p.Kind)
}

func idOf(p *Principal) any {
	if p == nil {
		return nil
	}
	return p.ID
}

func folderOf(f *FolderRef) any {
	if f == nil {
		return nil
	}
	return f.String()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stringOf(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func timeOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func int64Of(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func timePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

func int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
