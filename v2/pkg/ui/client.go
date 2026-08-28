package ui

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Client is the model code the server wraps: reading and writing task
// state (list, create, modify, approve, attach/detach a capability,
// comment, close/reopen) against Config's label taxonomy, off GitHub, off
// a model.Store, or both -- see the package doc comment for which does
// which and why. Server is a thin JSON/HTTP shim over this and nothing
// more, so a caller that has no reason to speak HTTP -- cmd/grain's CLI,
// most notably -- gets the exact same behaviour with no server to run.
type Client struct {
	Config Config
	GitHub github.Client
	// Store is nil, or the same model.Store cmd/graind's own
	// pkg/orchestrator.PollIssues polls tasks into -- see NewServer's own
	// doc comment on why the two must never run against it at once.
	Store *model.Store
}

// NewClient builds a Client. gh is deliberately the github.Client
// interface, not *github.RESTClient -- see NewServer's own doc comment
// for why (DryRunClient, githubsim.Sim, a test double). store may be nil.
func NewClient(cfg Config, gh github.Client, store *model.Store) *Client {
	return &Client{Config: cfg, GitHub: gh, Store: store}
}

// ValidationError marks a request Client rejected before making any
// GitHub or store call -- a blank title, an unparseable repo, an unknown
// capability ID. Server maps this to a 400; a CLI caller can print
// Error() on its own without a stack of wire detail behind it.
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

// storeTaskID is pkg/orchestrator.TaskID's own derivation, duplicated
// rather than imported -- pkg/orchestrator pulls in the agent and mcp
// packages transitively, the same reason directives.go's directiveRe is
// duplicated instead of imported. The two must never drift: a UI reading
// a task the poller filed, and a poller re-reading one the UI is about to
// approve, both need to land on the same ID from the same owner/name/
// number with no coordination but the format itself.
func storeTaskID(repo model.RepoRef, issueNumber int) string {
	return fmt.Sprintf("%s/%s/%d", repo.Owner, repo.Name, issueNumber)
}

// issueHTMLURL builds the URL a tracked task's own issue.HTMLURL would
// have carried, with no GitHub call spent fetching it -- see Config's own
// GitHubHost doc comment for why this package cannot just ask GitHub.
func (c *Client) issueHTMLURL(number int) string {
	host := c.Config.GitHubHost
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s/%s/issues/%d", host, c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
}

// annotate fills in State and Capabilities against Config's label
// taxonomy -- split out of taskFrom since that has no Config to read
// taxonomy off of, and every call site here has one on hand instead. Used
// only for a task read straight off GitHub (see taskFrom); a tracked
// task is annotated by taskFromModel instead.
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

// stateFromModel translates a tracked task's model.State into this
// package's own State vocabulary, which the frontend renders against --
// see labels.go's own State doc comments for the one case
// (model.StateProposed) that is not yet reachable through
// pkg/orchestrator.PollIssues today, and StateClosed's own doc comment
// for the one direction this package's State could not previously
// express at all.
func stateFromModel(st model.State) State {
	switch st {
	case model.StateProposed:
		return StateNeedsApproval
	case model.StateQueued:
		return StateQueued
	case model.StateRunning:
		return StateRunning
	case model.StateAwaitingReply:
		return StateAwaitingReply
	case model.StateCompleted:
		return StateCompleted
	case model.StateClosed:
		return StateClosed
	default:
		return StateUntracked
	}
}

// githubStateFromModel is the open/closed half of a tracked task's own
// githubState field -- StateOf already folds a closed issue into
// model.StateClosed, so there is nothing else to read to answer this.
func githubStateFromModel(st model.State) string {
	if st == model.StateClosed {
		return "closed"
	}
	return "open"
}

// capabilityIDsFromGrants is annotate's own capability loop, against a
// tracked task's Grants instead of GitHub labels -- Grant.Capability is
// always a CapabilitySpec.Name (e.g. "gemini-key"), the same short ID
// Config.Capabilities and the frontend's capability picker use, never a
// GitHub label string. Iterating Config.Capabilities in order, rather
// than Grants, keeps the result in the same deterministic order annotate
// already produces.
func capabilityIDsFromGrants(grants []model.Grant, capabilities []Capability) []string {
	set := make(map[string]struct{}, len(grants))
	for _, g := range grants {
		set[g.Capability] = struct{}{}
	}
	ids := []string{}
	for _, c := range capabilities {
		if _, ok := set[c.ID]; ok {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// taskFromModel is taskFrom's own counterpart for a tracked task: every
// field comes from the store, with no GitHub call spent re-deriving any
// of it. number is passed in rather than re-parsed from t.ExternalRef at
// every call site that already knows it (trackedTask, most notably).
func (c *Client) taskFromModel(t model.Task, number int, st model.State) Task {
	out := Task{
		Number:       number,
		Title:        t.Title,
		Description:  stripDirectives(t.Body),
		HTMLURL:      c.issueHTMLURL(number),
		Author:       t.Origin.Attribution.Actor.ID,
		State:        stateFromModel(st),
		GitHubState:  githubStateFromModel(st),
		Capabilities: capabilityIDsFromGrants(t.Grants, c.Config.Capabilities),
		Labels:       append([]string{}, t.Tags...),
	}
	if t.Target != nil {
		out.Repo = t.Target.String()
	}
	out.Base = t.Base
	autoMerge := t.AutoMerge
	out.AutoMerge = &autoMerge
	return out
}

// runsFrom is a tracked task's run history, translated into the wire
// shape TaskDetail carries -- see Run's own doc comment.
func runsFrom(runs []model.Run) []Run {
	out := make([]Run, 0, len(runs))
	for _, r := range runs {
		out = append(out, Run{
			ID: r.ID, Slot: r.Slot, Sandbox: r.Sandbox, Attempt: r.Attempt,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Outcome: r.Outcome,
		})
	}
	return out
}

// trackedTask reads number off Store, if Store is configured and a
// model.Task for it exists -- ok is false for every other case (no
// store, or a task the poller has not filed yet), which every caller
// below treats as "fall back to reading GitHub directly."
func (c *Client) trackedTask(ctx context.Context, number int) (task Task, ok bool, err error) {
	if c.Store == nil {
		return Task{}, false, nil
	}
	id := storeTaskID(c.Config.TaskRepo, number)
	mt, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return Task{}, false, fmt.Errorf("ui: reading task %s from the store: %w", id, err)
	}
	if mt == nil {
		return Task{}, false, nil
	}
	st, err := c.Store.State(ctx, id)
	if err != nil {
		return Task{}, false, fmt.Errorf("ui: reading state of %s: %w", id, err)
	}
	return c.taskFromModel(*mt, number, st), true, nil
}

// trackedTasks is every task Store knows about for Config.TaskRepo, or
// nil if Store is nil -- ListTasks' own store-backed half.
func (c *Client) trackedTasks(ctx context.Context) ([]Task, error) {
	if c.Store == nil {
		return nil, nil
	}
	modelTasks, err := c.Store.TasksInRepo(ctx, c.Config.TaskRepo)
	if err != nil {
		return nil, fmt.Errorf("ui: listing tasks from the store: %w", err)
	}
	out := make([]Task, 0, len(modelTasks))
	for _, mt := range modelTasks {
		_, number, err := model.ParseExternalRef(mt.ExternalRef)
		if err != nil {
			// A store row with no parseable issue number has nothing this
			// API's /api/tasks/{number} routes could key on -- skip it
			// rather than fail the whole list over one row this package
			// has no way to link to on GitHub. Logged, not surfaced as a
			// 500, the same "one bad row must not take the list down"
			// rule taskFrom's own doc comment already states.
			log.Printf("ui: task %s has no parseable external ref: %v", mt.ID, err)
			continue
		}
		st, err := c.Store.State(ctx, mt.ID)
		if err != nil {
			return nil, fmt.Errorf("ui: reading state of %s: %w", mt.ID, err)
		}
		out = append(out, c.taskFromModel(mt, number, st))
	}
	return out, nil
}

// untrackedTasks is ListTasks' own GitHub-backed half: issues carrying a
// label that means "not yet in the store." Trigger and NeedsApproval are
// the only two that still mean that once pkg/orchestrator.PollIssues is
// live -- v2's orchestrator never applies InProgress/AwaitingReply/
// Completed to an issue itself (those are read off the store instead, see
// stateFromModel), so an issue carrying one of those with no store row
// behind it would only ever be a leftover from a deployment that has not
// migrated off label-derived state, or a label applied by hand. This is
// also, unchanged, the whole of what this method does when Store is nil.
//
// One call per label rather than one unfiltered call, same as before this
// package could read a store at all: GitHub's issues-list endpoint has no
// "any of these labels" filter (a comma-separated list is AND, not OR).
func (c *Client) untrackedTasks() ([]Task, error) {
	l := c.Config.Labels
	seen := map[int]Task{}
	for _, label := range []string{l.Trigger, l.NeedsApproval} {
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
	return tasks, nil
}

// ListTasks merges the store's own tracked tasks with whatever is still
// only a labelled GitHub issue, deduplicated by number with the store's
// copy winning -- see trackedTasks and untrackedTasks for which is which.
// A number can appear in both only in the narrow window between
// PollIssues filing a task and this package next listing it, since filing
// removes the trigger label that untrackedTasks searches for; the store
// copy is authoritative the moment it exists.
func (c *Client) ListTasks(ctx context.Context) ([]Task, error) {
	tracked, err := c.trackedTasks(ctx)
	if err != nil {
		return nil, err
	}
	untracked, err := c.untrackedTasks()
	if err != nil {
		return nil, err
	}

	seen := make(map[int]struct{}, len(tracked))
	tasks := make([]Task, 0, len(tracked)+len(untracked))
	for _, t := range tracked {
		seen[t.Number] = struct{}{}
		tasks = append(tasks, t)
	}
	for _, t := range untracked {
		if _, ok := seen[t.Number]; ok {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Number > tasks[j].Number })
	return tasks, nil
}

// Task returns one task, annotated, with no comment thread -- what a
// mutation below hands back once it succeeds. GetTask returns the wider
// TaskDetail, comments (and, for a tracked task, run history) included.
func (c *Client) Task(ctx context.Context, number int) (Task, error) {
	if t, ok, err := c.trackedTask(ctx, number); err != nil {
		return Task{}, err
	} else if ok {
		return t, nil
	}
	issue, err := c.GitHub.GetIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
	if err != nil {
		return Task{}, err
	}
	return c.annotate(taskFrom(issue)), nil
}

// GetTask returns one task plus its conversation thread, and -- for a
// tracked task -- its run history. Comments are always read from GitHub;
// see the package doc comment for why.
func (c *Client) GetTask(ctx context.Context, number int) (TaskDetail, error) {
	task, tracked, err := c.trackedTask(ctx, number)
	if err != nil {
		return TaskDetail{}, err
	}
	var runs []Run
	if tracked {
		modelRuns, err := c.Store.Runs(ctx, storeTaskID(c.Config.TaskRepo, number))
		if err != nil {
			return TaskDetail{}, fmt.Errorf("ui: reading run history for #%d: %w", number, err)
		}
		runs = runsFrom(modelRuns)
	} else {
		issue, err := c.GitHub.GetIssue(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
		if err != nil {
			return TaskDetail{}, err
		}
		task = c.annotate(taskFrom(issue))
	}

	comments, err := c.GitHub.ListComments(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number)
	if err != nil {
		return TaskDetail{}, err
	}
	detail := TaskDetail{Task: task, Runs: runs}
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

// CreateTask always files a plain GitHub issue, tracked or not: an issue
// carrying the trigger or needsApproval label is exactly the intake
// surface pkg/orchestrator.PollIssues watches, and there is no task for
// the store to hold until a poll actually runs. The task this returns is
// therefore read straight back off the fresh issue, not the store, even
// when Store is configured -- it will not have a row until the next
// tick.
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
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
// /repo directive (or, for a tracked task, Task.Target) entirely (the
// documented "not yet targeted" case), rather than being read as "no
// change."
type UpdateTaskRequest struct {
	Title       *string
	Description *string
	Repo        *string
	Base        *string
	AutoMerge   *bool
}

// UpdateTask edits a task's title and/or its declared fields -- the
// "modify" half of create/modify/delete, alongside SetCapability (which
// edits capability grants) and AddComment (which edits nothing, only
// appends). A tracked task is edited on the store, which is what
// pkg/orchestrator and a dispatched run actually read from -- editing
// only its GitHub issue, the way this package always did before it could
// read a store, would silently stop the change from ever taking effect.
// GitHub's own copy of the title and body is still updated too, best
// effort, purely so a human reading the issue on GitHub sees the same
// thing.
func (c *Client) UpdateTask(ctx context.Context, number int, req UpdateTaskRequest) (Task, error) {
	if c.Store != nil {
		id := storeTaskID(c.Config.TaskRepo, number)
		mt, err := c.Store.GetTask(ctx, id)
		if err != nil {
			return Task{}, fmt.Errorf("ui: reading task %s from the store: %w", id, err)
		}
		if mt != nil {
			return c.updateTrackedTask(ctx, *mt, number, req)
		}
	}
	return c.updateUntrackedTask(number, req)
}

func (c *Client) updateTrackedTask(ctx context.Context, mt model.Task, number int, req UpdateTaskRequest) (Task, error) {
	description := stripDirectives(mt.Body)
	if req.Description != nil {
		description = *req.Description
	}
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			return Task{}, validationErrorf("title cannot be empty")
		}
		mt.Title = *req.Title
	}
	if req.Repo != nil {
		if strings.TrimSpace(*req.Repo) == "" {
			mt.Target = nil
		} else {
			parsed, err := model.ParseRepo(*req.Repo)
			if err != nil {
				return Task{}, &ValidationError{err: err}
			}
			mt.Target = &parsed
		}
	}
	if req.Base != nil {
		mt.Base = *req.Base
	}
	if req.AutoMerge != nil {
		mt.AutoMerge = *req.AutoMerge
	}
	autoMerge := mt.AutoMerge
	mt.Body = bodyOf(description, mt.Target, mt.Base, &autoMerge)

	if err := c.Store.PutTask(ctx, mt); err != nil {
		return Task{}, fmt.Errorf("ui: updating task %s: %w", mt.ID, err)
	}
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if err := c.GitHub.UpdateIssue(owner, name, number, &mt.Title, &mt.Body); err != nil {
		return Task{}, fmt.Errorf("ui: mirroring task %s onto its issue: %w", mt.ID, err)
	}
	return c.Task(ctx, number)
}

// updateUntrackedTask is UpdateTask's own pre-intake half, unchanged from
// how this package always edited a task issue: read the issue's current
// body, since the declared fields and the free-text description share it,
// so changing only Base, say, must not clobber Description or a /repo
// line UpdateTaskRequest left alone.
func (c *Client) updateUntrackedTask(number int, req UpdateTaskRequest) (Task, error) {
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
	issue, err = c.GitHub.GetIssue(owner, name, number)
	if err != nil {
		return Task{}, err
	}
	return c.annotate(taskFrom(issue)), nil
}

// setGrant returns grants with every entry named name removed, plus a
// fresh one appended if attach -- SetCapability's own read-modify-write
// step for a tracked task, mirroring the "already there is a no-op,
// already gone is a no-op" idempotence AddLabel/RemoveLabel give the
// pre-intake path.
func setGrant(grants []model.Grant, name string, attach bool) []model.Grant {
	out := make([]model.Grant, 0, len(grants)+1)
	for _, g := range grants {
		if g.Capability != name {
			out = append(out, g)
		}
	}
	if attach {
		out = append(out, model.Grant{Capability: name, Via: model.GrantByLabel})
	}
	return out
}

// SetCapability attaches or detaches one capability. For a tracked task
// this edits Task.Grants on the store directly -- a GitHub label change
// on an already-filed issue is not read by anything again (see
// UpdateTask's own doc comment), so writing one would silently do
// nothing. For a task not yet in the store, this is still a label on the
// issue, unchanged: SetCapability's own doc comment on why removing an
// absent label/grant is a no-op still holds either way.
func (c *Client) SetCapability(ctx context.Context, number int, id string, attach bool) error {
	capability, ok := c.capabilityByID(id)
	if !ok {
		return validationErrorf("unknown capability %s", id)
	}
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name

	if c.Store != nil {
		storeID := storeTaskID(c.Config.TaskRepo, number)
		mt, err := c.Store.GetTask(ctx, storeID)
		if err != nil {
			return fmt.Errorf("ui: reading task %s from the store: %w", storeID, err)
		}
		if mt != nil {
			mt.Grants = setGrant(mt.Grants, id, attach)
			if err := c.Store.PutTask(ctx, *mt); err != nil {
				return fmt.Errorf("ui: updating grants on %s: %w", storeID, err)
			}
			return nil
		}
	}

	if attach {
		return c.GitHub.AddLabel(owner, name, number, capability.Label)
	}
	return c.GitHub.RemoveLabel(owner, name, number, capability.Label)
}

// Approve swaps needsApproval for trigger -- the one thing a plain
// GitHub issue has no button for, and grain's own "accept this task"
// action. This is always a GitHub-only operation, even with a store
// configured: applying the trigger label is exactly what makes
// pkg/orchestrator.PollIssues file the task in the first place, so there
// is never a store row to approve here (a task StateOf reports as
// model.StateProposed -- e.g. one an agent proposed rather than a human
// filing it directly -- has no path into this method yet; nothing files
// one that way today, per model.LandsQueued's own doc comment). See
// SetCapability's own doc comment for why removing an absent label is
// not an error: approving an already-queued task is a no-op.
func (c *Client) Approve(ctx context.Context, number int) error {
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if err := c.GitHub.RemoveLabel(owner, name, number, c.Config.Labels.NeedsApproval); err != nil {
		return err
	}
	return c.GitHub.AddLabel(owner, name, number, c.Config.Labels.Trigger)
}

func (c *Client) AddComment(ctx context.Context, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return validationErrorf("body is required")
	}
	_, err := c.GitHub.CreateComment(c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name, number, body)
	return err
}

// observeField is pkg/orchestrator's own unexported helper of the same
// name, duplicated for the reason storeTaskID's own doc comment gives:
// read taskID's current observation (or a zero one), apply set, and write
// it back with ObservedAt stamped.
func observeField(ctx context.Context, store *model.Store, taskID string, now time.Time,
	set func(*model.Observation)) error {

	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		return fmt.Errorf("ui: reading observation for %s: %w", taskID, err)
	}
	if obs == nil {
		obs = &model.Observation{TaskID: taskID}
	}
	set(obs)
	obs.ObservedAt = &now
	return store.Observe(ctx, *obs)
}

// Close closes the task's issue. A GitHub issue carries no delete
// endpoint reachable through an ordinary token -- closed is the terminal
// state model.StateClosed already names, so this is what "delete a task"
// means here, the same way it does everywhere else this model is read.
//
// For a tracked task this also records task_observation.closed_at, the
// same field pkg/orchestrator.SyncPullRequests sets once a task's own PR
// merges or closes -- without it, StateOf would keep reporting whatever
// state the task was in right up until some later process happened to
// notice the issue closed from underneath it.
func (c *Client) Close(ctx context.Context, number int) error {
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if err := c.GitHub.CloseIssue(owner, name, number); err != nil {
		return err
	}
	return c.observeClosed(ctx, number, true)
}

func (c *Client) Reopen(ctx context.Context, number int) error {
	owner, name := c.Config.TaskRepo.Owner, c.Config.TaskRepo.Name
	if err := c.GitHub.ReopenIssue(owner, name, number); err != nil {
		return err
	}
	return c.observeClosed(ctx, number, false)
}

// observeClosed sets or clears task_observation.closed_at for number, if
// Store is configured and number is tracked -- Close and Reopen's own
// shared tail. A task not yet in the store has nothing to observe: the
// GitHub mutation above is the whole of the effect, same as before this
// package could read a store at all.
func (c *Client) observeClosed(ctx context.Context, number int, closed bool) error {
	if c.Store == nil {
		return nil
	}
	id := storeTaskID(c.Config.TaskRepo, number)
	mt, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return fmt.Errorf("ui: reading task %s from the store: %w", id, err)
	}
	if mt == nil {
		return nil
	}
	now := time.Now().UTC()
	return observeField(ctx, c.Store, id, now, func(o *model.Observation) {
		if closed {
			o.ClosedAt = &now
		} else {
			o.ClosedAt = nil
		}
	})
}
