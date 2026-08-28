package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Client is the model code the server wraps: reading and writing tasks in
// a model.Store (list, create, modify, approve, attach/detach a
// capability, comment, close/reopen). Server is a thin JSON/HTTP shim
// over this and nothing more, so a caller that has no reason to speak
// HTTP -- cmd/grain's CLI, most notably -- gets the exact same behaviour
// with no server to run.
//
// Every method takes a context because every one of them is a database
// call now. That is the visible half of the change: this used to be a
// GitHub client, and a task's identity used to be an issue number.
type Client struct {
	Config Config
	Store  *model.Store
	// Now is the clock, injectable so tests get deterministic timestamps.
	// nil means time.Now().UTC().
	Now func() time.Time
}

// NewClient builds a Client over a store.
func NewClient(cfg Config, store *model.Store) *Client {
	return &Client{Config: cfg, Store: store}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// ValidationError marks a request Client rejected before touching the
// store -- a blank title, an unparseable repo, an unknown capability ID.
// Server maps this to a 400; a CLI caller can print Error() on its own
// without a stack of database detail behind it.
type ValidationError struct{ err error }

func (e *ValidationError) Error() string { return e.err.Error() }
func (e *ValidationError) Unwrap() error { return e.err }

func validationErrorf(format string, a ...any) error {
	return &ValidationError{err: fmt.Errorf(format, a...)}
}

// NotFoundError marks a task ID with no task behind it. Server maps this
// to a 404 -- the one case where the caller's mistake and the store's own
// trouble need telling apart, which github.Error's status used to answer.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return "no task " + e.ID }

func (c *Client) capabilityByID(id string) (Capability, bool) {
	for _, capability := range c.Config.Capabilities {
		if capability.ID == id {
			return capability, true
		}
	}
	return Capability{}, false
}

// ListTasks returns every task, newest first.
func (c *Client) ListTasks(ctx context.Context) ([]Task, error) {
	tasks, err := c.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	states, err := c.Store.States(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskFrom(t, states[t.ID]))
	}
	return out, nil
}

// Task returns one task's list-shaped view.
func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	t, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if t == nil {
		return Task{}, &NotFoundError{ID: id}
	}
	state, err := c.Store.State(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return taskFrom(*t, state), nil
}

// GetTask returns one task plus its conversation.
func (c *Client) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	task, err := c.Task(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	comments, err := c.Store.Comments(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	detail := TaskDetail{Task: task, Comments: make([]Comment, 0, len(comments))}
	for _, cm := range comments {
		detail.Comments = append(detail.Comments, commentFrom(cm))
	}
	return detail, nil
}

// CreateTaskRequest is a new task's fields. Repo, Base and AutoMerge were
// directive lines in an issue body before this package spoke to the
// store; they are columns on the task now, which is what
// docs/data-model.md's "a form knows all of that before the task exists"
// asked for in the first place.
type CreateTaskRequest struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Repo         string   `json:"repo"`
	Base         string   `json:"base"`
	AutoMerge    bool     `json:"autoMerge"`
	Capabilities []string `json:"capabilities"`
	// Approved files the task already approved, so it is dispatchable at
	// once. False files it proposed, waiting for Approve.
	Approved bool `json:"approved"`
}

// CreateTask files a task straight into the store.
//
// No GitHub issue is created, and none is needed: Store.NewTaskID
// allocates identity, so a task exists the moment this returns. That is
// the whole inversion in one method -- this used to open an issue and let
// a poll notice it, which meant a task could not be created without
// GitHub reachable, and could not be dispatched until the next tick.
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Task{}, validationErrorf("title is required")
	}

	target := c.Config.DefaultTarget
	if strings.TrimSpace(req.Repo) != "" {
		parsed, err := model.ParseRepo(req.Repo)
		if err != nil {
			return Task{}, &ValidationError{err: err}
		}
		target = &parsed
	}
	if target == nil {
		return Task{}, validationErrorf(
			"no repo given, and this deployment has no default target repo configured")
	}

	grants, err := c.grantsFor(req.Capabilities)
	if err != nil {
		return Task{}, err
	}

	id, err := c.Store.NewTaskID(ctx)
	if err != nil {
		return Task{}, err
	}
	now := c.now()
	task := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  req.Title,
		Body:   req.Description,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: c.Config.Actor},
			Reason:      model.ReasonDirect,
		},
		Target:    target,
		Binding:   model.BindingDirective,
		Base:      req.Base,
		AutoMerge: req.AutoMerge,
		Grants:    grants,
		CreatedAt: &now,
	}
	if req.Approved {
		task.Approval = &model.Attribution{Actor: c.Config.Actor}
	}
	if err := c.Store.PutTask(ctx, task); err != nil {
		return Task{}, err
	}
	return c.Task(ctx, id)
}

// grantsFor turns capability ids into model.Grants, rejecting any this
// deployment does not offer.
func (c *Client) grantsFor(ids []string) ([]model.Grant, error) {
	grants := make([]model.Grant, 0, len(ids))
	for _, id := range ids {
		if _, ok := c.capabilityByID(id); !ok {
			return nil, validationErrorf("unknown capability %s", id)
		}
		grants = append(grants, model.Grant{Capability: id, Via: model.GrantByLabel})
	}
	return grants, nil
}

// UpdateTaskRequest is a task's editable fields -- nil means "leave this
// one alone". Repo pointing at an empty string is rejected rather than
// clearing the target: a task with no target cannot be dispatched, and
// the store has a real column for it now rather than an optional
// directive line that could simply be absent.
type UpdateTaskRequest struct {
	Title       *string
	Description *string
	Repo        *string
	Base        *string
	AutoMerge   *bool
}

// UpdateTask edits a task's fields. Unlike the issue-backed version, this
// needs no read-modify-write of a body with directives embedded in it:
// each field is a column, so leaving one alone means not setting it.
func (c *Client) UpdateTask(ctx context.Context, id string, req UpdateTaskRequest) (Task, error) {
	// Validated once, up front. Only the applying half runs inside the
	// store's closure, which may run more than once if another writer
	// wins: validating in there would repeat work whose answer cannot
	// change, and would make the error a caller sees depend on how many
	// times the retry happened to run.
	var target *model.RepoRef
	if req.Repo != nil {
		if strings.TrimSpace(*req.Repo) == "" {
			return Task{}, validationErrorf("repo cannot be empty: a task with no target cannot be dispatched")
		}
		parsed, err := model.ParseRepo(*req.Repo)
		if err != nil {
			return Task{}, &ValidationError{err: err}
		}
		target = &parsed
	}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		return Task{}, validationErrorf("title cannot be empty")
	}

	if err := c.mutate(ctx, id, func(task *model.Task) error {
		if req.Title != nil {
			task.Title = *req.Title
		}
		if req.Description != nil {
			task.Body = *req.Description
		}
		if target != nil {
			task.Target = target
		}
		if req.Base != nil {
			task.Base = *req.Base
		}
		if req.AutoMerge != nil {
			task.AutoMerge = *req.AutoMerge
		}
		return nil
	}); err != nil {
		return Task{}, err
	}
	return c.Task(ctx, id)
}

// mutate wraps Store.UpdateTask so a missing task reports as this
// package's own NotFoundError -- which the server turns into a 404 --
// rather than as the store's own "no such task" string.
func (c *Client) mutate(ctx context.Context, id string, apply func(*model.Task) error) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	return c.Store.UpdateTask(ctx, id, apply)
}

// SetCapability attaches or detaches one capability grant. Detaching one
// that is not attached is a no-op rather than an error, matching what
// removing an absent label used to do.
func (c *Client) SetCapability(ctx context.Context, id, capabilityID string, attach bool) error {
	if _, ok := c.capabilityByID(capabilityID); !ok {
		return validationErrorf("unknown capability %s", capabilityID)
	}
	// Rebuilding the grant set inside the closure is what lets two people
	// attach two different capabilities without one losing: a retry runs
	// again against the set the winner already wrote.
	return c.mutate(ctx, id, func(task *model.Task) error {
		kept := make([]model.Grant, 0, len(task.Grants)+1)
		for _, g := range task.Grants {
			if g.Capability != capabilityID {
				kept = append(kept, g)
			}
		}
		if attach {
			kept = append(kept, model.Grant{Capability: capabilityID, Via: model.GrantByLabel})
		}
		task.Grants = kept
		return nil
	})
}

// Approve records approval on a proposed task, which is what makes it
// dispatchable -- model.StateOf reads a task with no Approval as
// 'proposed' and one with it as 'queued'. Approving an already-approved
// task is a no-op.
func (c *Client) Approve(ctx context.Context, id string) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	if task.Approval != nil {
		return nil
	}
	return c.Store.Approve(ctx, id, model.Attribution{Actor: c.Config.Actor})
}

// AddComment appends to a task's conversation.
//
// This is also how a human answers a question a run parked on: an
// awaiting_reply task has Observation.PendingQuestionCommentID set, and
// replying clears it so the task queues again. That used to require
// re-applying the trigger label to the issue so the next poll would
// notice -- the reply and the re-trigger were two separate acts, and
// forgetting the second left the task parked forever.
func (c *Client) AddComment(ctx context.Context, id, body string) error {
	if strings.TrimSpace(body) == "" {
		return validationErrorf("body is required")
	}
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}

	now := c.now()
	if _, err := c.Store.AddComment(ctx, model.Comment{
		TaskID:    id,
		Author:    model.Attribution{Actor: c.Config.Actor},
		Body:      body,
		CreatedAt: now,
	}); err != nil {
		return err
	}

	obs, err := c.Store.GetObservation(ctx, id)
	if err != nil {
		return err
	}
	if obs == nil || obs.PendingQuestionCommentID == nil {
		return nil
	}
	return c.Store.ObserveField(ctx, id, now, func(o *model.Observation) {
		o.PendingQuestionCommentID = nil
	})
}

// Close marks a task closed. Closed is the terminal state
// model.StateClosed already names, and it is what "delete a task" means
// here -- the store has no delete either, deliberately: a task that ran
// is a record of a dispatch that happened.
func (c *Client) Close(ctx context.Context, id string) error {
	return c.setClosed(ctx, id, true)
}

// Reopen clears a task's closure, returning it to whatever state its
// observations and approval imply.
func (c *Client) Reopen(ctx context.Context, id string) error {
	return c.setClosed(ctx, id, false)
}

func (c *Client) setClosed(ctx context.Context, id string, closed bool) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	now := c.now()
	return c.Store.ObserveField(ctx, id, now, func(o *model.Observation) {
		if closed {
			o.ClosedAt = &now
			return
		}
		o.ClosedAt = nil
	})
}
