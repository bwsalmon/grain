package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Store is the model's read and write surface over any database/sql
// database. It imports no driver: opening an embedded Dolt database lives
// in the dolt subpackage, so this file — and everything above it — stays
// free of that dependency and testable against anything that speaks SQL.
//
// Every statement here is parameterised. That is the whole of what the
// language change bought at this layer: a Python controller cannot embed
// Dolt, so it had to shell out to the CLI, and a CLI has no bind
// parameters — which meant hand-rendering every untrusted issue title and
// comment body into a statement and getting MySQL's escaping rules right
// without a server to check against. That module does not exist here.
// Writes are also real transactions rather than a best-effort batch, so a
// task and its child rows land together or not at all.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

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

// PutTask inserts or replaces a task and everything hanging off it, in
// one transaction.
//
// Child rows are deleted and re-inserted rather than diffed: the sets are
// tiny, and "the row set equals the object" is a property worth having
// outright rather than maintaining.
func (s *Store) PutTask(ctx context.Context, t Task) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

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

	if _, err = tx.ExecContext(ctx, `REPLACE INTO `+"`task`"+` (
  `+"`id`, `intent`, `title`, `body`"+`,
  `+"`origin_actor_kind`, `origin_actor_id`, `origin_behalf_kind`, `origin_behalf_id`, `origin_reason`"+`,
  `+"`approval_actor_kind`, `approval_actor_id`, `approval_behalf_kind`, `approval_behalf_id`"+`,
  `+"`target_owner`, `target_name`, `binding`, `base`, `folder`"+`,
  `+"`auto_merge`, `external_ref`, `created_at`"+`
) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?, ?,?,?,?,?, ?,?,?)`,
		t.ID, string(t.Intent), t.Title, t.Body,
		string(oActor.Kind), oActor.ID, kindOf(oBehalf), idOf(oBehalf), string(t.Origin.Reason),
		aActorKind, aActorID, aBehalfKind, aBehalfID,
		targetOwner, targetName, string(t.Binding), nullable(t.Base), folderOf(t.Folder),
		t.AutoMerge, nullable(t.ExternalRef), timeOf(t.CreatedAt),
	); err != nil {
		return fmt.Errorf("writing task %s: %w", t.ID, err)
	}

	for _, table := range []string{"task_read", "task_grant", "task_link", "task_tag"} {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM `"+table+"` WHERE `task_id` = ?", t.ID); err != nil {
			return err
		}
	}
	for _, r := range t.Reads {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO `task_read` (`task_id`, `owner`, `name`) VALUES (?,?,?)",
			t.ID, r.Owner, r.Name); err != nil {
			return err
		}
	}
	for _, g := range t.Grants {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO `task_grant` (`task_id`, `capability`, `via`, `folder`) VALUES (?,?,?,?)",
			t.ID, g.Capability, string(g.Via), folderOf(g.Folder)); err != nil {
			return err
		}
	}
	for _, l := range t.Links {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO `task_link` (`task_id`, `kind`, `target`, `blocks`) VALUES (?,?,?,?)",
			t.ID, string(l.Kind), l.Target, l.Kind.Blocks()); err != nil {
			return err
		}
	}
	for _, tag := range t.Tags {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO `task_tag` (`task_id`, `tag`) VALUES (?,?)",
			t.ID, tag); err != nil {
			return err
		}
	}
	return tx.Commit()
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
func (s *Store) NewTaskID(ctx context.Context) (string, error) {
	res, err := s.db.ExecContext(ctx,
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
func (s *Store) AddComment(ctx context.Context, c Comment) (int64, error) {
	behalf := c.Author.OnBehalfOf
	res, err := s.db.ExecContext(ctx, "INSERT INTO `task_comment` ("+
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
	row := s.db.QueryRowContext(ctx, "SELECT "+
		"`id`,`intent`,`title`,`body`,"+
		"`origin_actor_kind`,`origin_actor_id`,`origin_behalf_kind`,`origin_behalf_id`,`origin_reason`,"+
		"`approval_actor_kind`,`approval_actor_id`,`approval_behalf_kind`,`approval_behalf_id`,"+
		"`target_owner`,`target_name`,`binding`,`base`,`folder`,"+
		"`auto_merge`,`external_ref`,`created_at` "+
		"FROM `task` WHERE `id` = ?", id)

	var t Task
	var intent, binding string
	var oaKind, oaID, oReason string
	var obKind, obID, aaKind, aaID, abKind, abID sql.NullString
	var tOwner, tName, base, folder, extRef sql.NullString
	var createdAt sql.NullTime
	if err := row.Scan(&t.ID, &intent, &t.Title, &t.Body,
		&oaKind, &oaID, &obKind, &obID, &oReason,
		&aaKind, &aaID, &abKind, &abID,
		&tOwner, &tName, &binding, &base, &folder,
		&t.AutoMerge, &extRef, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
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
	t.Base, t.ExternalRef = base.String, extRef.String
	t.Folder = ParseFolder(folder.String)
	if createdAt.Valid {
		t.CreatedAt = &createdAt.Time
	}

	if err := s.hydrate(ctx, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) hydrate(ctx context.Context, t *Task) error {
	if err := each(ctx, s.db,
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
	grants, err := s.grantsOf(ctx, t.ID)
	if err != nil {
		return err
	}
	t.Grants = grants
	if err := each(ctx, s.db,
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
	return each(ctx, s.db,
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
func (s *Store) Approve(ctx context.Context, taskID string, a Attribution) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE `task` SET `approval_actor_kind` = ?, `approval_actor_id` = ?, "+
			"`approval_behalf_kind` = ?, `approval_behalf_id` = ? WHERE `id` = ?",
		string(a.Actor.Kind), a.Actor.ID, kindOf(a.OnBehalfOf), idOf(a.OnBehalfOf), taskID)
	return err
}

// Observe records what grain has seen about a task.
func (s *Store) Observe(ctx context.Context, o Observation) error {
	_, err := s.db.ExecContext(ctx,
		"REPLACE INTO `task_observation` (`task_id`,`closed_at`,`completed_at`,"+
			"`pending_question_comment_id`,`baseline_comment_id`,`merge_queue_blocked_at`,`observed_at`) "+
			"VALUES (?,?,?,?,?,?,?)",
		o.TaskID, timeOf(o.ClosedAt), timeOf(o.CompletedAt),
		int64Of(o.PendingQuestionCommentID), int64Of(o.BaselineCommentID),
		timeOf(o.MergeQueueBlockedAt), timeOf(o.ObservedAt))
	return err
}

func (s *Store) GetObservation(ctx context.Context, taskID string) (*Observation, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT `closed_at`,`completed_at`,`pending_question_comment_id`,"+
			"`baseline_comment_id`,`merge_queue_blocked_at`,`observed_at` "+
			"FROM `task_observation` WHERE `task_id` = ?", taskID)
	o := Observation{TaskID: taskID}
	var closed, completed, blocked, observed sql.NullTime
	var pending, baseline sql.NullInt64
	if err := row.Scan(&closed, &completed, &pending, &baseline, &blocked, &observed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o.ClosedAt, o.CompletedAt, o.ObservedAt = timePtr(closed), timePtr(completed), timePtr(observed)
	o.PendingQuestionCommentID, o.BaselineCommentID = int64Ptr(pending), int64Ptr(baseline)
	o.MergeQueueBlockedAt = timePtr(blocked)
	return &o, nil
}

// StartRun records a run and its leases together.
func (s *Store) StartRun(ctx context.Context, r Run) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx,
		"REPLACE INTO `task_run` (`id`,`task_id`,`slot`,`sandbox`,`unit`,`attempt`,"+
			"`started_at`,`finished_at`,`outcome`) VALUES (?,?,?,?,?,?,?,?,?)",
		r.ID, r.TaskID, r.Slot, r.Sandbox, nullable(r.Unit), r.Attempt,
		r.StartedAt.UTC(), timeOf(r.FinishedAt), nullable(r.Outcome)); err != nil {
		return err
	}
	for _, l := range r.Leases {
		if _, err = tx.ExecContext(ctx,
			"REPLACE INTO `lease` (`run_id`,`capability`,`resource`,`minted_by`,"+
				"`issued_at`,`expires_at`) VALUES (?,?,?,?,?,?)",
			r.ID, l.Capability, l.Resource, l.MintedBy.Name,
			l.IssuedAt.UTC(), timeOf(l.ExpiresAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FinishRun(ctx context.Context, runID string, at time.Time, outcome string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE `task_run` SET `finished_at` = ?, `outcome` = ? WHERE `id` = ?",
		at.UTC(), outcome, runID)
	return err
}

// DropLease forgets a lease once its resource is actually revoked.
// Idempotent by construction — a DELETE matching nothing is not an error,
// which is what lets release and the expiry reaper both reach the same
// lease without coordinating.
func (s *Store) DropLease(ctx context.Context, runID, capability, resource string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM `lease` WHERE `run_id` = ? AND `capability` = ? AND `resource` = ?",
		runID, capability, resource)
	return err
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
// no open blocker. The whole dispatch query, as one view.
func (s *Store) Ready(ctx context.Context) ([]string, error) {
	var out []string
	err := each(ctx, s.db, "SELECT `task_id` FROM `task_ready` ORDER BY `task_id`", nil,
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
	grants, err := s.grantsOf(ctx, taskID)
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
func (s *Store) grantsOf(ctx context.Context, taskID string) ([]Grant, error) {
	var grants []Grant
	err := each(ctx, s.db,
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

// --- helpers ---------------------------------------------------------

func each(ctx context.Context, db *sql.DB, query string, args any,
	scan func(*sql.Rows) error) error {
	var list []any
	switch a := args.(type) {
	case nil:
	case []any:
		list = a
	default:
		list = []any{a}
	}
	rows, err := db.QueryContext(ctx, query, list...)
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
