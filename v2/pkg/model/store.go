package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// Init creates the schema if absent and stamps the version.
func (s *Store) Init(ctx context.Context) error {
	for _, stmt := range Statements() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
	}
	if err := s.ensureConfigTargetReposColumn(ctx); err != nil {
		return fmt.Errorf("migrating grain_config: %w", err)
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
  `+"`approval_actor_kind`, `approval_actor_id`, `approval_behalf_kind`, `approval_behalf_id`"+`,
  `+"`target_owner`, `target_name`, `binding`, `base`, `folder`"+`,
  `+"`auto_merge`, `created_at`"+`
) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?, ?,?,?,?,?, ?,?)`,
		t.ID, string(t.Intent), t.Title, t.Body,
		string(oActor.Kind), oActor.ID, kindOf(oBehalf), idOf(oBehalf), string(t.Origin.Reason),
		aActorKind, aActorID, aBehalfKind, aBehalfID,
		targetOwner, targetName, string(t.Binding), nullable(t.Base), folderOf(t.Folder),
		t.AutoMerge, timeOf(t.CreatedAt),
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
	"`approval_actor_kind`,`approval_actor_id`,`approval_behalf_kind`,`approval_behalf_id`," +
	"`target_owner`,`target_name`,`binding`,`base`,`folder`," +
	"`auto_merge`,`created_at`"

// scanTask reads one task row. It takes the Scan method rather than a
// *sql.Row or *sql.Rows so one function serves both the single-row and
// the many-row query.
func scanTask(scan func(...any) error) (Task, error) {
	var t Task
	var intent, binding string
	var oaKind, oaID, oReason string
	var obKind, obID, aaKind, aaID, abKind, abID sql.NullString
	var tOwner, tName, base, folder sql.NullString
	var createdAt sql.NullTime
	if err := scan(&t.ID, &intent, &t.Title, &t.Body,
		&oaKind, &oaID, &obKind, &obID, &oReason,
		&aaKind, &aaID, &abKind, &abID,
		&tOwner, &tName, &binding, &base, &folder,
		&t.AutoMerge, &createdAt); err != nil {
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

// ListTasks returns every task, newest first, fully hydrated.
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
// Ties on created_at break by id, lexically -- ids are decimal strings,
// so that is not numeric order. Nothing depends on the tie-break beyond
// it being stable.
func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM `task` ORDER BY `created_at` DESC, `id` DESC")
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

func (s *Store) Approve(ctx context.Context, taskID string, a Attribution) error {
	return s.write(ctx, "approve task "+taskID, func(tx *sql.Tx) error { return approve(ctx, tx, taskID, a) })
}

func approve(ctx context.Context, tx *sql.Tx, taskID string, a Attribution) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE `task` SET `approval_actor_kind` = ?, `approval_actor_id` = ?, "+
			"`approval_behalf_kind` = ?, `approval_behalf_id` = ? WHERE `id` = ?",
		string(a.Actor.Kind), a.Actor.ID, kindOf(a.OnBehalfOf), idOf(a.OnBehalfOf), taskID)
	return err
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
			"`pending_question_comment_id`,`baseline_comment_id`,`merge_queue_blocked_at`,`observed_at`,"+
			"`retry_requested_at`) VALUES (?,?,?,?,?,?,?,?)",
		o.TaskID, timeOf(o.ClosedAt), timeOf(o.CompletedAt),
		int64Of(o.PendingQuestionCommentID), int64Of(o.BaselineCommentID),
		timeOf(o.MergeQueueBlockedAt), timeOf(o.ObservedAt), timeOf(o.RetryRequestedAt))
	return err
}

func (s *Store) GetObservation(ctx context.Context, taskID string) (*Observation, error) {
	return getObservation(ctx, s.db, taskID)
}

func getObservation(ctx context.Context, q querier, taskID string) (*Observation, error) {
	row := q.QueryRowContext(ctx,
		"SELECT `closed_at`,`completed_at`,`pending_question_comment_id`,"+
			"`baseline_comment_id`,`merge_queue_blocked_at`,`observed_at`,`retry_requested_at` "+
			"FROM `task_observation` WHERE `task_id` = ?", taskID)
	o := Observation{TaskID: taskID}
	var closed, completed, blocked, observed, retried sql.NullTime
	var pending, baseline sql.NullInt64
	if err := row.Scan(&closed, &completed, &pending, &baseline, &blocked, &observed, &retried); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o.ClosedAt, o.CompletedAt, o.ObservedAt = timePtr(closed), timePtr(completed), timePtr(observed)
	o.PendingQuestionCommentID, o.BaselineCommentID = int64Ptr(pending), int64Ptr(baseline)
	o.MergeQueueBlockedAt = timePtr(blocked)
	o.RetryRequestedAt = timePtr(retried)
	return &o, nil
}

// StartRun records a run and its leases together.
func (s *Store) StartRun(ctx context.Context, r Run) error {
	return s.write(ctx, "start run "+r.ID+" for task "+r.TaskID, func(tx *sql.Tx) error { return startRun(ctx, tx, r) })
}

func startRun(ctx context.Context, tx *sql.Tx, r Run) error {
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO `task_run` (`id`,`task_id`,`slot`,`sandbox`,`unit`,`attempt`,"+
			"`started_at`,`finished_at`,`outcome`) VALUES (?,?,?,?,?,?,?,?,?)",
		r.ID, r.TaskID, r.Slot, r.Sandbox, nullable(r.Unit), r.Attempt,
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
// harmless tool call at all (outcomeOf), before ProcessResult has checked
// whether that tool call amounted to anything -- a push, a question, a
// closing comment. A run that made only harmless calls but produced none
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
// no open blocker -- in dispatch order. A fix task (Origin.Reason ==
// ReasonFix) sorts before everything else: it exists only because
// orchestrator.queueHeads found its repo's merge queue head broken, and
// queueHeads already guarantees at most one such task per repo at a
// time, so there is never more than a handful competing for this and
// nothing else to weigh them against. Leaving one to wait behind
// unrelated new work is what bwsalmon/agents#389 asks to avoid: the
// longer a queue head's repair sits queued, the more likely something
// else lands on the branch it targets first and the fix has to be
// refiled rather than simply merged. Ties within each group still break
// on task ID, the same stable tiebreak as before.
func (s *Store) Ready(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db,
		"SELECT `r`.`task_id` FROM `task_ready` AS `r` "+
			"JOIN `task` AS `t` ON `t`.`id` = `r`.`task_id` "+
			"ORDER BY (`t`.`origin_reason` = ?) DESC, `r`.`task_id`",
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

// OccupiedSlots is every slot currently holding a live run — a run with
// no `finished_at`. A dispatch loop reads this, rather than remembering
// what it handed out last cycle, for the same reason IsBlocked re-reads
// closed dependencies: a run can finish between cycles with nothing
// about the loop's own state changing, and a slot freed that way must
// show up as free the moment it is asked about.
func (s *Store) OccupiedSlots(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db,
		"SELECT `slot` FROM `task_run` WHERE `finished_at` IS NULL ORDER BY `slot`", nil,
		func(rows *sql.Rows) error {
			var slot string
			if err := rows.Scan(&slot); err != nil {
				return err
			}
			out = append(out, slot)
			return nil
		})
	return out, err
}

// LiveRuns is every task_run row with no `finished_at` -- the same rows
// OccupiedSlots counts, in full, for a caller (orchestrator.
// RecoverOrphanedRuns) that needs to know which task and run each one
// belongs to rather than just which slots they occupy. A daemon calls
// this exactly once, at startup, before it has driven any run of its
// own: at that point every row here can only be left over from a
// process that is no longer around to finish it -- see that func's own
// doc comment for why a run still legitimately in flight is never
// mistaken for one of these.
func (s *Store) LiveRuns(ctx context.Context) ([]Run, error) {
	var out []Run
	err := each(ctx, s.db,
		"SELECT `id`,`task_id`,`slot`,`sandbox`,`attempt`,`started_at` "+
			"FROM `task_run` WHERE `finished_at` IS NULL ORDER BY `id`", nil,
		func(rows *sql.Rows) error {
			var r Run
			if err := rows.Scan(&r.ID, &r.TaskID, &r.Slot, &r.Sandbox, &r.Attempt, &r.StartedAt); err != nil {
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

const configColumns = "`poll_interval_ms`,`slots`,`gemini_model`,`max_agent_turns`," +
	"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`"

func scanConfig(scan func(...any) error) (Config, error) {
	var c Config
	var pollMS int64
	var slots, targetRepos string
	if err := scan(&pollMS, &slots, &c.GeminiModel, &c.MaxAgentTurns,
		&c.GitHubHost, &c.GitHubInsecureHTTP, &c.GCPProject, &c.GCPServiceAccountEmail,
		&targetRepos); err != nil {
		return Config{}, err
	}
	c.PollInterval = time.Duration(pollMS) * time.Millisecond
	c.Slots = splitSlots(slots)
	c.TargetRepos = splitSlots(targetRepos)
	return c, nil
}

// PutConfig replaces the deployment's stored configuration wholesale --
// there is one row, so there is nothing to merge a partial update
// against; a caller changing one field reads Config first the same way
// UpdateTask's mutate does for a task.
func (s *Store) PutConfig(ctx context.Context, c Config) error {
	return s.write(ctx, "update config", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"REPLACE INTO `grain_config` (`id`, "+configColumns+") VALUES (1,?,?,?,?,?,?,?,?,?)",
			c.PollInterval.Milliseconds(), joinSlots(c.Slots), c.GeminiModel, c.MaxAgentTurns,
			c.GitHubHost, c.GitHubInsecureHTTP, c.GCPProject, c.GCPServiceAccountEmail,
			joinSlots(c.TargetRepos))
		return err
	})
}

// joinSlots/splitSlots round-trip Config.Slots, and equally Config.
// TargetRepos (an owner/name repo can never contain a comma), through
// the same comma-separated shape the daemon's own -slots/-target-repos
// flags already parse, so a value written by one reads back identically
// through the other.
func joinSlots(slots []string) string { return strings.Join(slots, ",") }

func splitSlots(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// --- scheduled tasks ---------------------------------------------------

// NewScheduledTaskID allocates a schedule identity from its own sequence,
// distinct from task_sequence -- so a schedule's id (e.g. "sched-3") is
// never mistaken for one of the tasks it files, the same "not the GitHub
// issue it was filed from" reasoning NewTaskID's own doc comment gives.
func (s *Store) NewScheduledTaskID(ctx context.Context) (id string, err error) {
	err = s.write(ctx, "allocate a scheduled task id", func(tx *sql.Tx) error {
		id, err = newScheduledTaskID(ctx, tx)
		return err
	})
	return id, err
}

func newScheduledTaskID(ctx context.Context, tx *sql.Tx) (string, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO `scheduled_task_sequence` (`issued_at`) VALUES (?)", time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("allocating a scheduled task id: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("reading the allocated scheduled task id: %w", err)
	}
	return "sched-" + strconv.FormatInt(n, 10), nil
}

const scheduledTaskColumns = "`id`,`title`,`body`,`target_owner`,`target_name`,`base`," +
	"`auto_merge`,`interval_ms`,`enabled`,`next_run_at`,`last_run_at`,`created_at`"

func scanScheduledTask(scan func(...any) error) (ScheduledTask, error) {
	var t ScheduledTask
	var base sql.NullString
	var intervalMS int64
	var lastRun sql.NullTime
	if err := scan(&t.ID, &t.Title, &t.Body, &t.Target.Owner, &t.Target.Name, &base,
		&t.AutoMerge, &intervalMS, &t.Enabled, &t.NextRunAt, &lastRun, &t.CreatedAt); err != nil {
		return ScheduledTask{}, err
	}
	t.Base = base.String
	t.Interval = time.Duration(intervalMS) * time.Millisecond
	t.LastRunAt = timePtr(lastRun)
	return t, nil
}

// PutScheduledTask inserts or replaces a schedule wholesale -- there are
// no child rows the way a task has, so this is a single REPLACE rather
// than putTask's own multi-table dance.
func (s *Store) PutScheduledTask(ctx context.Context, t ScheduledTask) error {
	return s.write(ctx, "put scheduled task "+t.ID,
		func(tx *sql.Tx) error { return putScheduledTask(ctx, tx, t) })
}

func putScheduledTask(ctx context.Context, tx *sql.Tx, t ScheduledTask) error {
	_, err := tx.ExecContext(ctx, `REPLACE INTO `+"`scheduled_task`"+` (
  `+"`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
		"`auto_merge`,`interval_ms`,`enabled`,`next_run_at`,`last_run_at`,`created_at`"+`
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Body, t.Target.Owner, t.Target.Name, nullable(t.Base),
		t.AutoMerge, t.Interval.Milliseconds(), t.Enabled,
		t.NextRunAt.UTC(), timeOf(t.LastRunAt), t.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("writing scheduled task %s: %w", t.ID, err)
	}
	return nil
}

func getScheduledTask(ctx context.Context, q querier, id string) (*ScheduledTask, error) {
	t, err := scanScheduledTask(q.QueryRowContext(ctx,
		"SELECT "+scheduledTaskColumns+" FROM `scheduled_task` WHERE `id` = ?", id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// GetScheduledTask returns a schedule, or nil if there is none with that ID.
func (s *Store) GetScheduledTask(ctx context.Context, id string) (*ScheduledTask, error) {
	return getScheduledTask(ctx, s.db, id)
}

// ListScheduledTasks returns every schedule, newest first -- ListTasks'
// own "the whole table" reasoning applies again at this size.
func (s *Store) ListScheduledTasks(ctx context.Context) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduledTaskColumns+" FROM `scheduled_task` ORDER BY `created_at` DESC, `id` DESC")
	if err != nil {
		return nil, fmt.Errorf("listing scheduled tasks: %w", err)
	}
	defer rows.Close()
	var out []ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueScheduledTasks is every enabled schedule whose next run has come --
// what the orchestrator's schedule reconciler fires each cycle. Ordered
// by id for a deterministic firing order, the same reasoning v1's own
// ScheduledJobsConfig.load gives for sorting by name.
func (s *Store) DueScheduledTasks(ctx context.Context, now time.Time) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+scheduledTaskColumns+" FROM `scheduled_task` "+
			"WHERE `enabled` = 1 AND `next_run_at` <= ? ORDER BY `id`", now.UTC())
	if err != nil {
		return nil, fmt.Errorf("listing due scheduled tasks: %w", err)
	}
	defer rows.Close()
	var out []ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateScheduledTask reads a schedule, applies mutate, and writes it
// back -- UpdateTask's own read-modify-write-and-retry shape, for the
// same reason: mutate may run more than once, on a schedule freshly read
// inside each attempt.
func (s *Store) UpdateScheduledTask(ctx context.Context, id string, mutate func(*ScheduledTask) error) error {
	var missing bool
	err := s.write(ctx, "update scheduled task "+id, func(tx *sql.Tx) error {
		missing = false
		t, err := getScheduledTask(ctx, tx, id)
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
		return putScheduledTask(ctx, tx, *t)
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("updating scheduled task %s: no such scheduled task", id)
	}
	return nil
}

// DeleteScheduledTask removes a schedule -- unlike a task (Close's own
// doc comment: "a task that ran is a record of a dispatch that
// happened"), a schedule is only ever a standing declaration with no
// history of its own worth keeping once a human no longer wants it, so
// deleting it outright (rather than adding a closed-like flag) loses
// nothing: every task it already filed remains exactly where it always
// was, untouched by this.
func (s *Store) DeleteScheduledTask(ctx context.Context, id string) error {
	return s.write(ctx, "delete scheduled task "+id, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM `scheduled_task` WHERE `id` = ?", id)
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
