package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// NewTaskSuiteID allocates a task suite identity from its own sequence,
// distinct from every other id sequence here -- NewScheduledTaskID's own
// doc comment on why.
func (s *Store) NewTaskSuiteID(ctx context.Context) (id string, err error) {
	err = s.write(ctx, "allocate a task suite id", func(tx *sql.Tx) error {
		id, err = newTaskSuiteID(ctx, tx)
		return err
	})
	return id, err
}

func newTaskSuiteID(ctx context.Context, tx *sql.Tx) (string, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `task_suite_sequence` (`issued_at`) VALUES (?)", time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("allocating a task suite id: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reading the allocated task suite id: %w", err)
	}
	return "suite-" + strconv.FormatInt(n, 10), nil
}

const taskSuiteColumns = "`id`,`name`,`mode`,`count`,`max_passes`,`require_approval`,`auto_merge`,`created_at`"

func scanTaskSuite(scan func(...any) error) (TaskSuite, error) {
	var s TaskSuite
	var mode string
	if err := scan(&s.ID, &s.Name, &mode, &s.Count, &s.MaxPasses, &s.RequireApproval, &s.AutoMerge, &s.CreatedAt); err != nil {
		return TaskSuite{}, err
	}
	s.Mode = TaskSuiteMode(mode)
	return s, nil
}

func taskSuiteItemsOf(ctx context.Context, q querier, suiteID string) ([]TaskSuiteItem, error) {
	var out []TaskSuiteItem
	err := each(ctx, q,
		"SELECT `template_id` FROM `task_suite_item` WHERE `suite_id` = ? ORDER BY `order_key`",
		suiteID, func(rows *sql.Rows) error {
			var it TaskSuiteItem
			if err := rows.Scan(&it.TemplateID); err != nil {
				return err
			}
			out = append(out, it)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("reading items of task suite %s: %w", suiteID, err)
	}
	return out, nil
}

func getTaskSuite(ctx context.Context, q querier, id string) (*TaskSuite, error) {
	suite, err := scanTaskSuite(q.QueryRowContext(ctx,
		"SELECT "+taskSuiteColumns+" FROM `task_suite` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	items, err := taskSuiteItemsOf(ctx, q, id)
	if err != nil {
		return nil, err
	}
	suite.Items = items
	return &suite, nil
}

// GetTaskSuite returns a task suite, or nil if there is none with that ID.
func (s *Store) GetTaskSuite(ctx context.Context, id string) (*TaskSuite, error) {
	return getTaskSuite(ctx, s.db, id)
}

// ListTaskSuites returns every task suite, newest first.
func (s *Store) ListTaskSuites(ctx context.Context) ([]TaskSuite, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+taskSuiteColumns+" FROM `task_suite` ORDER BY `created_at` DESC, `id` DESC")
	if err != nil {
		return nil, fmt.Errorf("listing task suites: %w", err)
	}
	var out []TaskSuite
	for rows.Next() {
		row, err := scanTaskSuite(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		items, err := taskSuiteItemsOf(ctx, s.db, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

// PutTaskSuite inserts or replaces a task suite wholesale --
// putScheduledTask's own "child rows are deleted and re-inserted rather
// than diffed" for a suite's own items.
func (s *Store) PutTaskSuite(ctx context.Context, suite TaskSuite) error {
	return s.write(ctx, "put task suite "+suite.ID,
		func(tx *sql.Tx) error { return putTaskSuite(ctx, tx, suite) })
}

func putTaskSuite(ctx context.Context, tx *sql.Tx, suite TaskSuite) error {
	_, err := tx.ExecContext(ctx,
		"REPLACE INTO `task_suite` ("+taskSuiteColumns+") VALUES (?,?,?,?,?,?,?,?)",
		suite.ID, suite.Name, string(suite.Mode), suite.Count, suite.MaxPasses,
		suite.RequireApproval, suite.AutoMerge, suite.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("writing task suite %s: %w", suite.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM `task_suite_item` WHERE `suite_id` = ?", suite.ID); err != nil {
		return err
	}
	for i, it := range suite.Items {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_suite_item` (`suite_id`,`template_id`,`order_key`) VALUES (?,?,?)",
			suite.ID, it.TemplateID, float64(i)); err != nil {
			return fmt.Errorf("writing task suite item %q: %w", it.TemplateID, err)
		}
	}
	return nil
}

// UpdateTaskSuite reads a suite, applies mutate, and writes it back --
// UpdateScheduledTask's own read-modify-write shape.
func (s *Store) UpdateTaskSuite(ctx context.Context, id string, mutate func(*TaskSuite) error) error {
	var missing bool
	err := s.write(ctx, "update task suite "+id, func(tx *sql.Tx) error {
		missing = false
		suite, err := getTaskSuite(ctx, tx, id)
		if err != nil {
			return err
		}
		if suite == nil {
			missing = true
			return nil
		}
		if err := mutate(suite); err != nil {
			return err
		}
		return putTaskSuite(ctx, tx, *suite)
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("updating task suite %s: no such task suite", id)
	}
	return nil
}

// DeleteTaskSuite removes a suite outright -- DeleteScheduledTask's own
// doc comment gives the reasoning: a suite is only ever a standing
// declaration, so there is no history on the row itself worth keeping.
// A run already started from this suite is untouched -- it carries its
// own snapshot of everything it needs (model.TaskSuiteRun's own doc
// comment) and does not join back to task_suite for anything.
func (s *Store) DeleteTaskSuite(ctx context.Context, id string) error {
	return s.write(ctx, "delete task suite "+id, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM `task_suite` WHERE `id` = ?", id)
		return err
	})
}

// TaskSuitesUsingTemplate returns every task suite with an item
// referencing templateID -- what ui.Client.DeleteTemplate checks before
// deleting one out from under a suite that still runs it,
// SchedulesUsingTemplate's own reasoning applied to this third caller of
// TaskTemplate.
func (s *Store) TaskSuitesUsingTemplate(ctx context.Context, templateID string) ([]TaskSuite, error) {
	var ids []string
	err := each(ctx, s.db,
		"SELECT DISTINCT `suite_id` FROM `task_suite_item` WHERE `template_id` = ? ORDER BY `suite_id`",
		templateID, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("finding task suites using template %s: %w", templateID, err)
	}
	out := make([]TaskSuite, 0, len(ids))
	for _, id := range ids {
		suite, err := s.GetTaskSuite(ctx, id)
		if err != nil {
			return nil, err
		}
		if suite != nil {
			out = append(out, *suite)
		}
	}
	return out, nil
}

// --- task suite runs -----------------------------------------------

const taskSuiteRunColumns = "`id`,`suite_id`,`suite_name`,`owner`,`repo`,`base`,`mode`,`count`,`max_passes`," +
	"`require_approval`,`auto_merge`,`status`,`last_error`,`created_at`,`completed_at`"

func scanTaskSuiteRun(scan func(...any) error) (TaskSuiteRun, error) {
	var r TaskSuiteRun
	var mode, status string
	var lastError sql.NullString
	var completedAt sql.NullTime
	if err := scan(&r.ID, &r.SuiteID, &r.SuiteName, &r.Target.Owner, &r.Target.Name, &r.Base,
		&mode, &r.Count, &r.MaxPasses, &r.RequireApproval, &r.AutoMerge,
		&status, &lastError, &r.CreatedAt, &completedAt); err != nil {
		return TaskSuiteRun{}, err
	}
	r.Mode = TaskSuiteMode(mode)
	r.Status = TaskSuiteRunStatus(status)
	r.LastError = lastError.String
	r.CompletedAt = timePtr(completedAt)
	return r, nil
}

func taskSuiteRunItemsOf(ctx context.Context, q querier, runID int64) ([]TaskSuiteItem, error) {
	var out []TaskSuiteItem
	err := each(ctx, q,
		"SELECT `template_id` FROM `task_suite_run_item` WHERE `run_id` = ? ORDER BY `order_key`",
		runID, func(rows *sql.Rows) error {
			var it TaskSuiteItem
			if err := rows.Scan(&it.TemplateID); err != nil {
				return err
			}
			out = append(out, it)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("reading items of task suite run %d: %w", runID, err)
	}
	return out, nil
}

// taskSuiteRunTaskStatusesOf reads every task instance a run's passes
// have instantiated so far, oldest first --
// qualificationTaskStatusesOf's own join, with OpenedPullRequest and
// Proposed read straight off task_link rather than off
// task_observation: a task's own LinkFixes link is written the same
// cycle finishWithPullRequest opens its pull request, one cycle ahead of
// when SyncPullRequests would first set Observation.PrOpenedAt, and
// OutcomeOfPass reading it here is one less cycle of lag before
// SyncTaskSuites can act on a pass that just produced a change.
func taskSuiteRunTaskStatusesOf(ctx context.Context, q querier, runID int64) ([]TaskSuiteTaskStatus, error) {
	var out []TaskSuiteTaskStatus
	err := each(ctx, q,
		"SELECT `rt`.`task_id`,`rt`.`template_id`,`rt`.`template_name`,`rt`.`pass_number`,"+
			"(`t`.`approval_actor_kind` IS NOT NULL),`ts`.`state`,"+
			"EXISTS(SELECT 1 FROM `task_link` WHERE `task_id` = `rt`.`task_id` AND `kind` = ?),"+
			"EXISTS(SELECT 1 FROM `task_link` WHERE `kind` = ? AND `target` = `rt`.`task_id`) "+
			"FROM `task_suite_run_task` AS `rt` "+
			"JOIN `task` AS `t` ON `t`.`id` = `rt`.`task_id` "+
			"JOIN `task_state` AS `ts` ON `ts`.`task_id` = `rt`.`task_id` "+
			"WHERE `rt`.`run_id` = ? ORDER BY `rt`.`id`",
		[]any{string(LinkFixes), string(LinkProposedBy), runID},
		func(rows *sql.Rows) error {
			var st TaskSuiteTaskStatus
			var state string
			if err := rows.Scan(&st.TaskID, &st.TemplateID, &st.TemplateName, &st.PassNumber,
				&st.Approved, &state, &st.OpenedPullRequest, &st.Proposed); err != nil {
				return err
			}
			st.State = State(state)
			out = append(out, st)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("reading tasks of task suite run %d: %w", runID, err)
	}
	return out, nil
}

func getTaskSuiteRun(ctx context.Context, q querier, id int64) (*TaskSuiteRun, error) {
	run, err := scanTaskSuiteRun(q.QueryRowContext(ctx,
		"SELECT "+taskSuiteRunColumns+" FROM `task_suite_run` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	items, err := taskSuiteRunItemsOf(ctx, q, id)
	if err != nil {
		return nil, err
	}
	run.Items = items
	tasks, err := taskSuiteRunTaskStatusesOf(ctx, q, id)
	if err != nil {
		return nil, err
	}
	run.Tasks = tasks
	return &run, nil
}

// GetTaskSuiteRun returns one run, fully hydrated, or nil if there is
// none with that ID.
func (s *Store) GetTaskSuiteRun(ctx context.Context, id int64) (*TaskSuiteRun, error) {
	return getTaskSuiteRun(ctx, s.db, id)
}

func listTaskSuiteRunIDs(ctx context.Context, q querier, query string, args any) ([]int64, error) {
	var ids []int64
	err := each(ctx, q, query, args, func(rows *sql.Rows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	return ids, err
}

// ListTaskSuiteRuns returns every run, newest first -- what a "see the
// status of outstanding task suite runs" view needs (bwsalmon/agents#642).
func (s *Store) ListTaskSuiteRuns(ctx context.Context) ([]TaskSuiteRun, error) {
	ids, err := listTaskSuiteRunIDs(ctx, s.db,
		"SELECT `id` FROM `task_suite_run` ORDER BY `created_at` DESC, `id` DESC", nil)
	if err != nil {
		return nil, fmt.Errorf("listing task suite runs: %w", err)
	}
	out := make([]TaskSuiteRun, 0, len(ids))
	for _, id := range ids {
		run, err := getTaskSuiteRun(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if run != nil {
			out = append(out, *run)
		}
	}
	return out, nil
}

// ActiveTaskSuiteRuns returns every run whose Status is still
// TaskSuiteRunActive -- what SyncTaskSuites polls each cycle,
// QualifiableActiveCandidates' own "only what this cycle might still
// need to do something for" shape.
func (s *Store) ActiveTaskSuiteRuns(ctx context.Context) ([]TaskSuiteRun, error) {
	ids, err := listTaskSuiteRunIDs(ctx, s.db,
		"SELECT `id` FROM `task_suite_run` WHERE `status` = ? ORDER BY `id`", string(TaskSuiteRunActive))
	if err != nil {
		return nil, fmt.Errorf("listing active task suite runs: %w", err)
	}
	out := make([]TaskSuiteRun, 0, len(ids))
	for _, id := range ids {
		run, err := getTaskSuiteRun(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if run != nil {
			out = append(out, *run)
		}
	}
	return out, nil
}

// resolveSuiteTemplates resolves every item's own template fresh from
// the store -- CreateQualificationRun's own "not a stale copy"
// discipline, read before the write transaction that uses them opens,
// the same ordering that call already uses for the same reason: a
// template lookup inside the transaction would be a second connection
// reading while the first still holds sqlite's single writer lock.
func resolveSuiteTemplates(ctx context.Context, s *Store, items []TaskSuiteItem) (map[string]TaskTemplate, error) {
	templates := make(map[string]TaskTemplate, len(items))
	for _, it := range items {
		if _, ok := templates[it.TemplateID]; ok {
			continue
		}
		tmpl, err := s.GetTaskTemplate(ctx, it.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("resolving template %s: %w", it.TemplateID, err)
		}
		if tmpl == nil {
			return nil, fmt.Errorf("template %s no longer exists", it.TemplateID)
		}
		templates[it.TemplateID] = *tmpl
	}
	return templates, nil
}

// fireSuitePass instantiates one pass of a run: every item, in order, as
// a fresh task targeting target/base, approved unless requireApproval
// asks otherwise -- CreateQualificationRun's own per-instance task
// shape, minus the per-item Repeat/DependsOn this feature has no DAG for
// (model.TaskSuite's own doc comment on why: a suite repeats whole
// passes, not individual items, so nothing here needs one item to wait
// on another within the same pass).
//
// Every task's Base is the run's own Base, unconditionally -- the
// issue's own "stack against the source branch" -- and AutoMerge is the
// run's own AutoMerge, not the template's: a suite's own switch is the
// run's policy for every task it files, the same way
// CreateQualificationRun instead trusts the template's AutoMerge because
// a qualification task has no run-level policy of its own to override it
// with.
func fireSuitePass(ctx context.Context, tx *sql.Tx, runID int64, items []TaskSuiteItem,
	templates map[string]TaskTemplate, target RepoRef, base string,
	requireApproval, autoMerge bool, passNumber int, orderKey float64, now time.Time) error {

	var approval *Attribution
	var approvedAt *time.Time
	if !requireApproval {
		approval = &Attribution{Actor: SuitePrincipal}
		approvedAt = &now
	}

	for _, it := range items {
		tmpl := templates[it.TemplateID]
		id, err := newTaskID(ctx, tx)
		if err != nil {
			return err
		}
		task := Task{
			ID:     id,
			Intent: IntentImplement,
			Title:  tmpl.Title,
			Body:   tmpl.Body,
			Origin: Origin{
				Attribution: Attribution{Actor: SuitePrincipal},
				Reason:      ReasonSuite,
			},
			Approval:   approval,
			ApprovedAt: approvedAt,
			Target:     &target,
			Binding:    BindingDirective,
			Base:       base,
			Reads:      tmpl.Reads,
			Grants:     tmpl.Grants,
			AutoMerge:  autoMerge,
			CreatedAt:  &now,
			OrderKey:   orderKey,
		}
		orderKey += orderKeySpacing
		if err := putTask(ctx, tx, task); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `task_suite_run_task` (`run_id`,`task_id`,`template_id`,`template_name`,`pass_number`) VALUES (?,?,?,?,?)",
			runID, id, it.TemplateID, tmpl.Name, passNumber); err != nil {
			return err
		}
	}
	return nil
}

// CreateTaskSuiteRun starts a new run of suite against target and base,
// filing its first pass immediately -- bwsalmon/agents#642's own "run
// the template against a repo and branch." suite's own Items/Mode/
// Count/MaxPasses/RequireApproval/AutoMerge are copied onto the run
// (model.TaskSuiteRun's own doc comment on why), so editing suite after
// this call changes nothing about the run it just started.
func (s *Store) CreateTaskSuiteRun(ctx context.Context, suite TaskSuite, target RepoRef, base string, now time.Time) (TaskSuiteRun, error) {
	templates, err := resolveSuiteTemplates(ctx, s, suite.Items)
	if err != nil {
		return TaskSuiteRun{}, err
	}
	orderKey, err := s.OrderKeyForNewTask(ctx, false)
	if err != nil {
		return TaskSuiteRun{}, err
	}

	var runID int64
	err = s.write(ctx, fmt.Sprintf("create task suite run for %s", suite.ID), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO `task_suite_run` "+
				"(`suite_id`,`suite_name`,`owner`,`repo`,`base`,`mode`,`count`,`max_passes`,"+
				"`require_approval`,`auto_merge`,`status`,`created_at`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			suite.ID, suite.Name, target.Owner, target.Name, base,
			string(suite.Mode), suite.Count, suite.MaxPasses,
			suite.RequireApproval, suite.AutoMerge, string(TaskSuiteRunActive), now.UTC())
		if err != nil {
			return fmt.Errorf("recording task suite run: %w", err)
		}
		runID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		for i, it := range suite.Items {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `task_suite_run_item` (`run_id`,`template_id`,`order_key`) VALUES (?,?,?)",
				runID, it.TemplateID, float64(i)); err != nil {
				return err
			}
		}
		return fireSuitePass(ctx, tx, runID, suite.Items, templates, target, base,
			suite.RequireApproval, suite.AutoMerge, 1, orderKey, now)
	})
	if err != nil {
		return TaskSuiteRun{}, err
	}
	run, err := getTaskSuiteRun(ctx, s.db, runID)
	if err != nil {
		return TaskSuiteRun{}, err
	}
	return *run, nil
}

// FireNextPass instantiates run's own next pass, using the same item
// snapshot and settings CreateTaskSuiteRun copied onto it -- what
// SyncTaskSuites calls once OutcomeOfPass says the current pass finished
// short of stopping: TaskSuiteCount short of Count passes, or
// TaskSuiteUntilClean short of MaxPasses with no clean pass yet.
func (s *Store) FireNextPass(ctx context.Context, run TaskSuiteRun, now time.Time) (TaskSuiteRun, error) {
	templates, err := resolveSuiteTemplates(ctx, s, run.Items)
	if err != nil {
		return TaskSuiteRun{}, err
	}
	orderKey, err := s.OrderKeyForNewTask(ctx, false)
	if err != nil {
		return TaskSuiteRun{}, err
	}
	next := run.CurrentPass() + 1
	err = s.write(ctx, fmt.Sprintf("fire pass %d of task suite run %d", next, run.ID), func(tx *sql.Tx) error {
		return fireSuitePass(ctx, tx, run.ID, run.Items, templates, run.Target, run.Base,
			run.RequireApproval, run.AutoMerge, next, orderKey, now)
	})
	if err != nil {
		return TaskSuiteRun{}, err
	}
	updated, err := s.GetTaskSuiteRun(ctx, run.ID)
	if err != nil {
		return TaskSuiteRun{}, err
	}
	return *updated, nil
}

// CompleteTaskSuiteRun marks run finished -- succeeded, with lastError
// empty, or failed, with lastError explaining why -- and stamps
// CompletedAt. SyncTaskSuites is the only caller: once a run leaves
// TaskSuiteRunActive it never becomes active again, so ActiveTaskSuiteRuns
// never sees it a second time.
func (s *Store) CompleteTaskSuiteRun(ctx context.Context, id int64, status TaskSuiteRunStatus, lastError string, now time.Time) error {
	return s.write(ctx, fmt.Sprintf("complete task suite run %d", id), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE `task_suite_run` SET `status` = ?, `last_error` = ?, `completed_at` = ? WHERE `id` = ?",
			string(status), nullable(lastError), now.UTC(), id)
		return err
	})
}
