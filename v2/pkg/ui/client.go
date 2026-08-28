package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Client is the model code the server wraps: reading and writing GitHub
// task issues (list, create, modify, approve, attach/detach a
// capability, comment, close/reopen) against Config's label taxonomy.
// Server is a thin JSON/HTTP shim over this and nothing more, so a
// caller that has no reason to speak HTTP -- cmd/grain's CLI, most
// notably -- gets the exact same behaviour with no server to run.
type Client struct {
	Config Config
	GitHub github.Client
}

// NewClient builds a Client. gh is deliberately the github.Client
// interface, not *github.RESTClient -- see NewServer's own doc comment
// for why (DryRunClient, githubsim.Sim, a test double).
func NewClient(cfg Config, gh github.Client) *Client {
	return &Client{Config: cfg, GitHub: gh}
}

// ValidationError marks a request Client rejected before making any
// GitHub call -- a blank title, an unparseable repo, an unknown
// capability ID. Server maps this to a 400; a CLI caller can print
// Error() on its own without a stack of GitHub wire detail behind it.
type ValidationError struct{ err error }

func (e *ValidationError) Error() string { return e.err.Error() }
func (e *ValidationError) Unwrap() error { return e.err }

func validationErrorf(format string, a ...any) error {
	return &ValidationError{err: fmt.Errorf(format, a...)}
}

func (c *Client) capabilityByID(id string) (Capability, bool) {
	for _, capability := range c.Config.Capabilities {
		if capability.ID == id {
			return capability, true
		}
	}
	return Capability{}, false
}

// annotate fills in State and Capabilities against Config's label
// taxonomy -- split out of taskFrom since that has no Config to read
// taxonomy off of, and every call site here has one on hand instead.
func (c *Client) annotate(t Task) Task {
	set := make(map[string]struct{}, len(t.Labels))
	for _, l := range t.Labels {
		set[l] = struct{}{}
	}
	t.State = deriveState(set, c.Config.Labels)
	for _, capability := range c.Config.Capabilities {
		if _, ok := set[capability.Label]; ok {
			t.Capabilities = append(t.Capabilities, capability.ID)
		}
	}
	return t
}

// ListTasks merges the open issues carrying each state label into one
// deduplicated, newest-first list. One call per state label rather than
// one unfiltered call: GitHub's issues-list endpoint has no "any of
// these labels" filter (a comma-separated list is AND, not OR), and
// every real task issue carries exactly one state label by construction,
// so the union of five label-scoped lists is exactly the task list --
// with nothing skipped that a single, filterless call (which would also
// return every non-task issue on the repo) would have included instead.
func (c *Client) ListTasks() ([]Task, error) {
	l := c.Config.Labels
	seen := map[int]Task{}
	for _, label := range []string{l.Trigger, l.InProgress, l.AwaitingReply, l.NeedsApproval, l.Completed} {
		issues, err := c.GitHub.ListIssues(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, label)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			seen[issue.Number] = c.annotate(taskFrom(issue))
		}
	}
	tasks := make([]Task, 0, len(seen))
	for _, t := range seen {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Number > tasks[j].Number })
	return tasks, nil
}

// Task returns one task, annotated, with no comment thread -- what a
// mutation below hands back once it succeeds. GetTask returns the wider
// TaskDetail, comments included.
func (c *Client) Task(number int) (Task, error) {
	issue, err := c.GitHub.GetIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
	if err != nil {
		return Task{}, err
	}
	return c.annotate(taskFrom(issue)), nil
}

// GetTask returns one task plus its conversation thread.
func (c *Client) GetTask(number int) (TaskDetail, error) {
	issue, err := c.GitHub.GetIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
	if err != nil {
		return TaskDetail{}, err
	}
	comments, err := c.GitHub.ListComments(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
	if err != nil {
		return TaskDetail{}, err
	}
	detail := TaskDetail{Task: c.annotate(taskFrom(issue))}
	for _, cm := range comments {
		detail.Comments = append(detail.Comments, Comment{
			ID: cm.ID, User: cm.User, Body: cm.Body, AuthorAssociation: cm.AuthorAssociation,
		})
	}
	return detail, nil
}

// CreateTaskRequest is a new task's declared fields -- a form's fields,
// not an issue body: the repo picker, base field, auto-merge checkbox
// and capability checkboxes docs/data-model.md's "a form knows all of
// that before the task exists" describes, rendered into directive lines
// and labels here so neither the frontend nor a CLI caller constructs
// GitHub-flavoured text itself.
type CreateTaskRequest struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Repo         string   `json:"repo"`
	Base         string   `json:"base"`
	AutoMerge    *bool    `json:"autoMerge"`
	Capabilities []string `json:"capabilities"`
	// Approved is false for "file this as a proposal" (needsApproval
	// label, which a maintainer must swap for the trigger label before
	// anything runs -- see Approve) and true for "queue it now" (trigger
	// label directly) -- model.LandsQueued's own human/agent distinction,
	// decided by a flag or checkbox instead of which principal filed it,
	// since every task Client files was filed by whoever is holding the
	// credential.
	Approved bool `json:"approved"`
}

func (c *Client) CreateTask(req CreateTaskRequest) (Task, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Task{}, validationErrorf("title is required")
	}
	var repo *model.RepoRef
	if strings.TrimSpace(req.Repo) != "" {
		parsed, err := model.ParseRepo(req.Repo)
		if err != nil {
			return Task{}, &ValidationError{err: err}
		}
		repo = &parsed
	}

	labels := make([]string, 0, len(req.Capabilities)+1)
	for _, id := range req.Capabilities {
		capability, ok := c.capabilityByID(id)
		if !ok {
			return Task{}, validationErrorf("unknown capability %s", id)
		}
		labels = append(labels, capability.Label)
	}
	if req.Approved {
		labels = append(labels, c.Config.Labels.Trigger)
	} else {
		labels = append(labels, c.Config.Labels.NeedsApproval)
	}

	body := bodyOf(req.Description, repo, req.Base, req.AutoMerge)
	issue, err := c.GitHub.CreateIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, req.Title, body, labels)
	if err != nil {
		return Task{}, err
	}
	return c.annotate(taskFrom(issue)), nil
}

// UpdateTaskRequest is a task's editable fields for UpdateTask -- nil
// means "leave this one alone," the same convention CreateTaskRequest's
// AutoMerge already uses. Repo pointing at an empty string clears the
// /repo directive entirely (the documented "not yet targeted" case),
// rather than being read as "no change."
type UpdateTaskRequest struct {
	Title       *string
	Description *string
	Repo        *string
	Base        *string
	AutoMerge   *bool
}

// UpdateTask edits a task issue's title and/or its declared fields --
// the "modify" half of create/modify/delete, alongside SetCapability
// (which edits capability labels) and AddComment (which edits nothing,
// only appends). It reads the issue's current body first because the
// declared fields and the free-text description share that one body:
// changing only Base, say, must not clobber Description or a /repo line
// UpdateTaskRequest left alone.
func (c *Client) UpdateTask(number int, req UpdateTaskRequest) (Task, error) {
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	issue, err := c.GitHub.GetIssue(owner, name, number)
	if err != nil {
		return Task{}, err
	}
	current, err := parseDirectives(issue.Body)
	if err != nil {
		return Task{}, validationErrorf("existing task body has unparseable directives: %w", err)
	}

	description := stripDirectives(issue.Body)
	if req.Description != nil {
		description = *req.Description
	}

	repo := current.Repo
	if req.Repo != nil {
		if strings.TrimSpace(*req.Repo) == "" {
			repo = nil
		} else {
			parsed, err := model.ParseRepo(*req.Repo)
			if err != nil {
				return Task{}, &ValidationError{err: err}
			}
			repo = &parsed
		}
	}

	base := current.Base
	if req.Base != nil {
		base = *req.Base
	}

	var autoMerge *bool
	if current.HasAutoMerge {
		v := current.AutoMerge
		autoMerge = &v
	}
	if req.AutoMerge != nil {
		autoMerge = req.AutoMerge
	}

	var title *string
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			return Task{}, validationErrorf("title cannot be empty")
		}
		title = req.Title
	}

	body := bodyOf(description, repo, base, autoMerge)
	if err := c.GitHub.UpdateIssue(owner, name, number, title, &body); err != nil {
		return Task{}, err
	}
	return c.Task(number)
}

// SetCapability attaches or detaches one capability label. Removing a
// label that isn't there 404s at the github.Client level, which
// RemoveLabel already treats as success, so detaching an already-absent
// capability is a no-op rather than an error.
func (c *Client) SetCapability(number int, id string, attach bool) error {
	capability, ok := c.capabilityByID(id)
	if !ok {
		return validationErrorf("unknown capability %s", id)
	}
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if attach {
		return c.GitHub.AddLabel(owner, name, number, capability.Label)
	}
	return c.GitHub.RemoveLabel(owner, name, number, capability.Label)
}

// Approve swaps needsApproval for trigger -- the one thing a plain
// GitHub issue has no button for, and grain's own "accept this task"
// action. See SetCapability's own doc comment for why removing an
// absent label is not an error: approving an already-queued task is a
// no-op.
func (c *Client) Approve(number int) error {
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if err := c.GitHub.RemoveLabel(owner, name, number, c.Config.Labels.NeedsApproval); err != nil {
		return err
	}
	return c.GitHub.AddLabel(owner, name, number, c.Config.Labels.Trigger)
}

func (c *Client) AddComment(number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return validationErrorf("body is required")
	}
	_, err := c.GitHub.CreateComment(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number, body)
	return err
}

// Close closes the task's issue. A GitHub issue carries no delete
// endpoint reachable through an ordinary token -- closed is the
// terminal state model.StateClosed already names, so this is what
// "delete a task" means here, the same way it does everywhere else this
// model is read.
func (c *Client) Close(number int) error {
	return c.GitHub.CloseIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
}

func (c *Client) Reopen(number int) error {
	return c.GitHub.ReopenIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
}
