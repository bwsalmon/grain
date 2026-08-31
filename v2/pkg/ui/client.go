package ui

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
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
	// targetReposMu guards Config.TargetRepos specifically -- the one
	// Config field a running server ever mutates after construction
	// (AddTargetRepo/RemoveTargetRepo, bwsalmon/agents#473, via
	// setTargetRepos), unlike every other Config field, which is set once
	// by NewClient's caller and read unguarded from then on.
	targetReposMu sync.RWMutex
}

// NewClient builds a Client over a store.
func NewClient(cfg Config, store *model.Store) *Client {
	return &Client{Config: cfg, Store: store}
}

// targetRepos reads Config.TargetRepos -- the accessor every read of it
// outside NewClient's own construction must use, now that
// setTargetRepos can change it while a request is in flight.
func (c *Client) targetRepos() []string {
	c.targetReposMu.RLock()
	defer c.targetReposMu.RUnlock()
	return c.Config.TargetRepos
}

// setTargetRepos updates Config.TargetRepos in place -- called once
// UpdateSettings has already written the same value to the store, so a
// GET /api/config or a CreateTask racing the update in the same process
// sees it immediately rather than only after a restart picks the stored
// value back up (cmd/grain/daemon.go's loadConfig, the only other place
// Config.TargetRepos is ever set, explicitly does not reload mid-run).
func (c *Client) setTargetRepos(repos []string) {
	c.targetReposMu.Lock()
	defer c.targetReposMu.Unlock()
	c.Config.TargetRepos = repos
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
//
// message overrides the default "no task "+ID rendering when set --
// HTTPClient's own httpError (httpclient.go) only ever has the server's
// already-formatted message text to reconstruct this from, not a bare
// ID, and formatting that message through Error() a second time doubled
// it into "no task no task <id>" for every 404 an HTTPClient caller (the
// CLI, and the browser frontend, which displays the JSON body's "error"
// field verbatim) ever saw.
type NotFoundError struct {
	ID      string
	message string
}

func (e *NotFoundError) Error() string {
	if e.message != "" {
		return e.message
	}
	return "no task " + e.ID
}

// AttachmentUpload is one file carried by a CreateTaskRequest or an
// AddComment call (bwsalmon/agents#522) -- Content is base64, matching
// how the frontend already reads a chosen File (FileReader.
// readAsDataURL, with the leading "data:...;base64," stripped) rather
// than widening this JSON API to multipart/form-data for just this one
// field.
type AttachmentUpload struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

const (
	// MaxAttachmentSize bounds one file's decoded size. Generous enough
	// for a screenshot or a small repro zip, small enough that one
	// request's worth of attachments never turns a single sqlite write
	// into one carrying tens of megabytes -- the size a real limit would
	// need to be to matter is a product decision nobody has made yet; this
	// one exists so a request has *some* bound rather than none.
	MaxAttachmentSize = 10 * 1024 * 1024
	// MaxAttachmentsPerRequest bounds how many files one task or one
	// comment can carry -- plenty for "a screenshot and a repro zip",
	// while keeping one request from turning into an unbounded number of
	// store writes.
	MaxAttachmentsPerRequest = 10
)

// decodeAttachments validates and base64-decodes uploads into the
// model.Attachment shape Store.AddAttachment writes -- TaskID and
// CommentID are left for the caller to fill in once it knows them
// (CreateTask has no task id yet when it validates; AddComment has no
// comment id), so this only ever fails on the upload itself, before
// either write happens.
func decodeAttachments(uploads []AttachmentUpload, now time.Time) ([]model.Attachment, error) {
	if len(uploads) > MaxAttachmentsPerRequest {
		return nil, validationErrorf("at most %d attachments per request, got %d", MaxAttachmentsPerRequest, len(uploads))
	}
	out := make([]model.Attachment, 0, len(uploads))
	for _, u := range uploads {
		name := sanitizeAttachmentFilename(u.Filename)
		content, err := base64.StdEncoding.DecodeString(u.Content)
		if err != nil {
			return nil, validationErrorf("attachment %q is not valid base64: %v", u.Filename, err)
		}
		if len(content) > MaxAttachmentSize {
			return nil, validationErrorf("attachment %q is %d bytes, over the %d byte limit", u.Filename, len(content), MaxAttachmentSize)
		}
		contentType := strings.TrimSpace(u.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		out = append(out, model.Attachment{
			Filename: name, ContentType: contentType,
			Size: int64(len(content)), Content: content, CreatedAt: now,
		})
	}
	return out, nil
}

// sanitizeAttachmentFilename strips any directory component a client
// sent -- orchestrator.placeAttachments joins this straight onto a
// sandbox path, and a filename is display text a human (or a browser's
// file picker) chose, never something entitled to say where it lands.
// Both separators are stripped regardless of host OS, since a name here
// came from a browser or a CLI flag, not a filesystem walk.
func sanitizeAttachmentFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}

func (c *Client) capabilityByID(id string) (Capability, bool) {
	for _, capability := range c.Config.Capabilities {
		if capability.ID == id {
			return capability, true
		}
	}
	return Capability{}, false
}

// ListTasks returns every task in this deployment's default backlog
// order: newest first, unless model.Config.NewestFirst says otherwise,
// in which case it is Store.ListTasks' own order untouched -- ascending
// OrderKey, top-to-bottom the same order Store.Ready dispatches in
// (bwsalmon/agents#476).
func (c *Client) ListTasks(ctx context.Context) ([]Task, error) {
	tasks, err := c.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	newestFirst, err := c.newestFirst(ctx)
	if err != nil {
		return nil, err
	}
	// Store.ListTasks already hands back ascending OrderKey -- Ready's
	// own order, and NewestFirst's own "read it as-is" case. The
	// traditional default (false) is that order's reverse: whichever
	// task joined the backlog most recently sorts first.
	if !newestFirst {
		for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
			tasks[i], tasks[j] = tasks[j], tasks[i]
		}
	}
	states, err := c.Store.States(ctx)
	if err != nil {
		return nil, err
	}
	// One States call already answers "is X closed?" for every task in
	// the store, so a list of many tasks costs nothing extra to also
	// carry each one's blocked signal -- unlike Task below, which has no
	// reason to fetch every task's state just to render one.
	closed := make(map[string]bool, len(states))
	for id, st := range states {
		closed[id] = st == model.StateClosed
	}
	// Same trade for the merge queue's own "gave up on this one" signal:
	// one query over every task_observation row that has it set, rather
	// than a per-task read.
	mergeQueueBlocked, err := c.Store.MergeQueueBlocked(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		var blockedAt *time.Time
		if at, ok := mergeQueueBlocked[t.ID]; ok {
			blockedAt = &at
		}
		out = append(out, taskFrom(t, states[t.ID], closed, blockedAt))
	}
	return out, nil
}

// newestFirst reads model.Config.NewestFirst fresh from the store on
// every call, deliberately unlike the deployment-wide settings Config
// (this package's own type) mirrors from it: those need a daemon restart
// to pick up a change (cmd/grain daemon's own loadConfig doc comment),
// which is the wrong trade for a setting a UI toggles and expects the
// very next task list (or task creation) to honour, rather than only the
// next full restart of the deployment. A fresh deployment with no
// grain_config row yet (nil) reads as false -- model.Config's own zero
// value, and the backlog order grain has always defaulted to.
func (c *Client) newestFirst(ctx context.Context) (bool, error) {
	cfg, err := c.Store.GetConfig(ctx)
	if err != nil {
		return false, err
	}
	return cfg != nil && cfg.NewestFirst, nil
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
	closed, err := c.closedTargets(ctx, t.Links)
	if err != nil {
		return Task{}, err
	}
	obs, err := c.Store.GetObservation(ctx, id)
	if err != nil {
		return Task{}, err
	}
	var blockedAt *time.Time
	if obs != nil {
		blockedAt = obs.MergeQueueBlockedAt
	}
	return taskFrom(*t, state, closed, blockedAt), nil
}

// closedTargets resolves whether each of a task's own blocking-link
// targets reads closed -- a handful of extra queries for one task rather
// than States' whole-store read, matching ListTasks' own "a few queries
// per task" trade-off (store.go's hydrate).
func (c *Client) closedTargets(ctx context.Context, links []model.Link) (map[string]bool, error) {
	closed := map[string]bool{}
	for _, l := range links {
		if !l.Kind.Blocks() {
			continue
		}
		if _, ok := closed[l.Target]; ok {
			continue
		}
		state, err := c.Store.State(ctx, l.Target)
		if err != nil {
			return nil, err
		}
		closed[l.Target] = state == model.StateClosed
	}
	return closed, nil
}

// GetTask returns one task plus its conversation.
func (c *Client) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	t, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	if t == nil {
		return TaskDetail{}, &NotFoundError{ID: id}
	}
	state, err := c.Store.State(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	closed, err := c.closedTargets(ctx, t.Links)
	if err != nil {
		return TaskDetail{}, err
	}

	comments, err := c.Store.Comments(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	attachments, err := c.Store.AttachmentMetas(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	// byComment groups attachments under the comment they were posted
	// alongside; the rest (CommentID nil) belong to the task's own body
	// and land on TaskDetail.Attachments directly below.
	byComment := map[int64][]Attachment{}
	var taskAttachments []Attachment
	for _, a := range attachments {
		if a.CommentID == nil {
			taskAttachments = append(taskAttachments, attachmentFrom(a))
			continue
		}
		byComment[*a.CommentID] = append(byComment[*a.CommentID], attachmentFrom(a))
	}
	obs, err := c.Store.GetObservation(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	var blockedAt *time.Time
	if obs != nil {
		blockedAt = obs.MergeQueueBlockedAt
	}
	detail := TaskDetail{
		Task:        taskFrom(*t, state, closed, blockedAt),
		Comments:    make([]Comment, 0, len(comments)),
		Attachments: taskAttachments,
	}
	for _, cm := range comments {
		detail.Comments = append(detail.Comments, commentFrom(cm, byComment[cm.ID]))
	}
	streak, err := c.Store.FailureStreak(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	// Guarded the same way model.StateOf and model.Transitions both guard
	// a failure streak against a task that has since completed or closed:
	// orchestrator.salvagePushedBranch turns a pushed branch into a pull
	// request (and the task into StateCompleted) even for a run whose own
	// outcome stays "failed" forever -- see model.Transitions' own doc
	// comment on why (bwsalmon/agents#502) -- which leaves task_streak
	// sitting at 1 or more permanently, since only a "succeeded" outcome
	// ever resets it. Left unguarded, GetTask kept reporting "1
	// consecutive failed attempt" on a task that plainly succeeded
	// (bwsalmon/agents#514), the same bogus signal #502 already fixed for
	// the task's state and its timeline, just surfacing here instead.
	completedOrClosed := obs != nil && (obs.CompletedAt != nil || obs.ClosedAt != nil)
	if streak != nil && streak.Count > 0 && !completedOrClosed {
		detail.FailedAttempts = streak.Count
		lastFailureAt := streak.LastFinishedAt
		detail.LastFailureAt = &lastFailureAt
		detail.LastFailureReason = streak.LastDetail
	}
	runs, err := c.Store.Runs(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	detail.Attempts = make([]Attempt, 0, len(runs))
	for _, r := range runs {
		detail.Attempts = append(detail.Attempts, attemptFrom(r))
	}

	transitions := model.Transitions(*t, obs, runs, streak, askedAt(obs, comments))
	detail.Transitions = make([]Transition, 0, len(transitions))
	for _, tr := range transitions {
		detail.Transitions = append(detail.Transitions, transitionFrom(tr))
	}
	detail.PullRequestEvents = pullRequestEventsFrom(obs)
	return detail, nil
}

// pullRequestEventsFrom projects Observation's own PrOpenedAt/PrMergedAt/
// PrClosedAt into TaskDetail's wire shape, oldest first. obs is nil for a
// task orchestrator.SyncPullRequests has never observed anything about,
// same as every other obs field this package reads.
func pullRequestEventsFrom(obs *model.Observation) []PullRequestEvent {
	if obs == nil {
		return nil
	}
	var out []PullRequestEvent
	if obs.PrOpenedAt != nil {
		out = append(out, PullRequestEvent{Kind: "opened", At: *obs.PrOpenedAt})
	}
	if obs.PrMergedAt != nil {
		out = append(out, PullRequestEvent{Kind: "merged", At: *obs.PrMergedAt})
	}
	if obs.PrClosedAt != nil {
		out = append(out, PullRequestEvent{Kind: "closed", At: *obs.PrClosedAt})
	}
	return out
}

// AttemptTranscript returns one attempt's own agent transcript -- the
// full narrative record GetTask's own Attempts list never carries
// (attemptFrom's own doc comment: TaskDetail stays cheap to fetch), for a
// caller that wants to read one attempt's whole story rather than just
// its outcome (bwsalmon/agents#446). For an attempt still running,
// Config.LiveTranscripts (when set) is tried first -- a still-running
// attempt's own transcript-in-progress, straight from whatever file its
// framework has been mirroring it into (bwsalmon/agents#467) -- falling
// back to Store.RunTranscript, which "" (with no error) until the
// attempt finishes, or forever for a framework that never populates one
// (agent.Result.Transcript's own doc comment) or a deployment with no
// LiveTranscripts configured at all.
func (c *Client) AttemptTranscript(ctx context.Context, taskID string, number int) (string, error) {
	runs, err := c.Store.Runs(ctx, taskID)
	if err != nil {
		return "", err
	}
	var run *model.Run
	for i := range runs {
		if runs[i].Attempt == number {
			run = &runs[i]
			break
		}
	}
	if run == nil {
		return "", &NotFoundError{message: fmt.Sprintf("no attempt %d for task %s", number, taskID)}
	}

	if run.FinishedAt == nil && c.Config.LiveTranscripts != nil {
		if text, ok, err := c.Config.LiveTranscripts.Tail(run.ID); err == nil && ok {
			return text, nil
		}
	}

	transcript, ok, err := c.Store.RunTranscript(ctx, taskID, number)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &NotFoundError{message: fmt.Sprintf("no attempt %d for task %s", number, taskID)}
	}
	return transcript, nil
}

// Attachment returns one attachment's full content, scoped to taskID so
// one task's attachment id can never be used to read another's file --
// the read behind GET /api/tasks/{id}/attachments/{attachmentId}
// (bwsalmon/agents#522).
func (c *Client) Attachment(ctx context.Context, taskID string, id int64) (model.Attachment, error) {
	a, err := c.Store.GetAttachment(ctx, taskID, id)
	if err != nil {
		return model.Attachment{}, err
	}
	if a == nil {
		return model.Attachment{}, &NotFoundError{message: fmt.Sprintf("no attachment %d on task %s", id, taskID)}
	}
	return *a, nil
}

// askedAt is the currently pending question's own CreatedAt, or nil once
// there is none -- model.Transitions' own askedAt parameter, resolved
// against the same comment list GetTask already fetched rather than a
// second query.
func askedAt(obs *model.Observation, comments []model.Comment) *time.Time {
	if obs == nil || obs.PendingQuestionCommentID == nil {
		return nil
	}
	for _, c := range comments {
		if c.ID == *obs.PendingQuestionCommentID {
			at := c.CreatedAt
			return &at
		}
	}
	return nil
}

// CreateTaskRequest is a new task's fields. Repo, Base and AutoMerge were
// directive lines in an issue body before this package spoke to the
// store; they are columns on the task now, which is what
// docs/data-model.md's "a form knows all of that before the task exists"
// asked for in the first place.
type CreateTaskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Repo        string `json:"repo"`
	Base        string `json:"base"`
	AutoMerge   bool   `json:"autoMerge"`
	// Interactive files this task as a live chat rather than a change
	// handed off to run unattended (bwsalmon/agents#539) -- model.Task's
	// own field of the same name. It also, regardless of Approved, files
	// the task already approved and dispatches it ahead of the ordinary
	// backlog (see CreateTask): a chat nobody has opened yet, sitting as
	// an unapproved proposal or waiting behind everything already
	// queued, has nothing to show for itself.
	Interactive bool `json:"interactive"`
	// SandboxCPUs and SandboxMemoryMB (bwsalmon/agents#534) set model.Task's
	// own fields of the same name -- a per-task override of the
	// deployment's default sandbox shape. 0 (the default for both) means
	// no override: the task dispatches at whatever shape the deployment
	// otherwise configures.
	SandboxCPUs     int      `json:"sandboxCpus"`
	SandboxMemoryMB int      `json:"sandboxMemoryMb"`
	Capabilities    []string `json:"capabilities"`
	// DependsOn is a set of task IDs this task cannot dispatch ahead of --
	// model.LinkDependsOn links, filed at creation the same way
	// Capabilities is. SetDependency is the picker's attach/detach
	// counterpart for a task that already exists.
	DependsOn []string `json:"dependsOn"`
	// Reads is a set of owner/name repos this task's run may read but
	// never push to -- model.Task.Reads, docs/data-model.md's "one write
	// target, many read targets". Unlike Repo, a Reads entry grants
	// nothing: it never touches Grants, so naming a capability-offering
	// repo here inherits none of its offers.
	Reads []string `json:"reads"`
	// Approved files the task already approved, so it is dispatchable at
	// once. False files it proposed, waiting for Approve.
	Approved bool `json:"approved"`
	// Attachments is files to carry alongside the task's own body --
	// bwsalmon/agents#522: a screenshot, a repro zip, anything the agent
	// needs that isn't already code in a repo it can clone. Stored under
	// the new task's own id with no CommentID, and materialized into
	// every dispatched run's sandbox the same way a comment's own
	// attachments are (orchestrator.placeAttachments).
	Attachments []AttachmentUpload `json:"attachments"`
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

	if err := validateSandboxShape(req.SandboxCPUs, req.SandboxMemoryMB); err != nil {
		return Task{}, err
	}
	grants, err := c.grantsFor(req.Capabilities)
	if err != nil {
		return Task{}, err
	}
	links, err := c.dependsOnLinks(ctx, req.DependsOn, "")
	if err != nil {
		return Task{}, err
	}
	reads, err := parseReads(req.Reads)
	if err != nil {
		return Task{}, err
	}
	now := c.now()
	// Decoded before NewTaskID allocates anything: a bad attachment should
	// fail this call with nothing written at all, not leave a task behind
	// with none of the files it was supposed to carry.
	attachments, err := decodeAttachments(req.Attachments, now)
	if err != nil {
		return Task{}, err
	}

	id, err := c.Store.NewTaskID(ctx)
	if err != nil {
		return Task{}, err
	}
	newestFirst, err := c.newestFirst(ctx)
	if err != nil {
		return Task{}, err
	}
	// atFront: NewestFirst asks a new task to dispatch ahead of
	// everything already queued (Store.OrderKeyForNewTask's own doc
	// comment), not merely to display first -- the default (false) files
	// it behind everything queued instead, the FIFO backlog grain has
	// always defaulted to. An interactive task asks for the same
	// treatment unconditionally, on top of whatever NewestFirst already
	// says, since somebody is waiting on it right now rather than
	// checking back on it later (CreateTaskRequest.Interactive's own doc
	// comment).
	orderKey, err := c.Store.OrderKeyForNewTask(ctx, newestFirst || req.Interactive)
	if err != nil {
		return Task{}, err
	}
	task := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  req.Title,
		Body:   req.Description,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: c.Config.Actor},
			Reason:      model.ReasonDirect,
		},
		Target:          target,
		Binding:         model.BindingDirective,
		Base:            req.Base,
		AutoMerge:       req.AutoMerge,
		Interactive:     req.Interactive,
		SandboxCPUs:     req.SandboxCPUs,
		SandboxMemoryMB: req.SandboxMemoryMB,
		Grants:          grants,
		Links:           links,
		Reads:           reads,
		CreatedAt:       &now,
		OrderKey:        orderKey,
	}
	if req.Approved || req.Interactive {
		task.Approval = &model.Attribution{Actor: c.Config.Actor}
		task.ApprovedAt = &now
	}
	if err := c.Store.PutTask(ctx, task); err != nil {
		return Task{}, err
	}
	for i := range attachments {
		attachments[i].TaskID = id
		if _, err := c.Store.AddAttachment(ctx, attachments[i]); err != nil {
			return Task{}, err
		}
	}
	if !targetAllowed(c.targetRepos(), *target) {
		if err := c.parkOffAllowlist(ctx, id, *target, now); err != nil {
			return Task{}, err
		}
	}
	return c.Task(ctx, id)
}

// validateSandboxShape checks a task's own SandboxCPUs/SandboxMemoryMB
// override, mirroring bwsalmon/kontur's own staticpod.VMSpec.Validate
// bounds ("cpus must be at least 1", "memory-mb must be at least 128")
// the same way UpdateSettings' identical check does for the
// deployment-wide default -- 0 is the one value each rejects that
// Validate would not, since 0 means "no override" here rather than a
// literal request for a zero-vCPU or zero-memory VM.
func validateSandboxShape(cpus, memoryMB int) error {
	if cpus != 0 && cpus < 1 {
		return validationErrorf("sandboxCpus must be 0 (no override) or at least 1")
	}
	if memoryMB != 0 && memoryMB < 128 {
		return validationErrorf("sandboxMemoryMb must be 0 (no override) or at least 128")
	}
	return nil
}

// targetAllowed reports whether target may be dispatched into: always,
// when allowed is empty (Config.TargetRepos' own "unrestricted" zero
// value, v1's target_repos "leave empty for a single-repo deployment"),
// otherwise only when target is one of allowed.
func targetAllowed(allowed []string, target model.RepoRef) bool {
	if len(allowed) == 0 {
		return true
	}
	repo := target.String()
	for _, a := range allowed {
		if a == repo {
			return true
		}
	}
	return false
}

// parkOffAllowlist is CreateTask's counterpart to a run's own
// ask_question parking (finish.go's relayComment/PendingQuestionCommentID):
// a task naming a repo outside Config.TargetRepos is filed exactly as
// asked -- Target still names it, so fixing this needs no re-entering
// anything -- but StateOf reads PendingQuestionCommentID before Approval,
// so it never reaches task_ready, and so is never dispatched, until an
// operator widens TargetRepos and replies -- AddComment's own doc
// comment on clearing PendingQuestionCommentID is what requeues it then,
// the same "reply reopens" un-park every other awaiting_reply task
// already uses.
//
// v1 enforced the same restriction twice, matching API-side and
// transport-side: core.py's own Orchestrator._resolve_target against
// the same repo-allowlist.json the git proxy checked
// (grain/proxy/allowlist.py). v2's gitproxy package deliberately checks
// a live task's own Target instead of a second allowlist
// (pkg/gitproxy/authorize.go's own doc comment), so this is the one
// place this boundary is decided now.
func (c *Client) parkOffAllowlist(ctx context.Context, taskID string, target model.RepoRef, now time.Time) error {
	reason := fmt.Sprintf(
		"`%s` is not on this deployment's target repo list, so nothing here "+
			"can clone, push to, or open a pull request against it. An operator "+
			"widens targetRepos under Settings, then replies here to let this "+
			"task run.", target)
	commentID, err := c.Store.AddComment(ctx, model.Comment{
		TaskID:    taskID,
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
		Body:      reason,
		CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("ui: parking %s off the target repo list: %w", taskID, err)
	}
	return c.Store.ObserveField(ctx, taskID, now, func(o *model.Observation) {
		o.PendingQuestionCommentID = &commentID
	})
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

// dependsOnLinks turns a set of task IDs into model.LinkDependsOn links,
// rejecting one that names no task and one that names selfID -- caught
// here, before the store is touched, rather than left to surface as a
// task silently blocked on nothing (an unknown id) or forever (itself).
// Blank entries and repeats are dropped rather than rejected: a picker
// built from free text will produce both.
func (c *Client) dependsOnLinks(ctx context.Context, ids []string, selfID string) ([]model.Link, error) {
	links := make([]model.Link, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if id == selfID {
			return nil, validationErrorf("a task cannot depend on itself")
		}
		target, err := c.Store.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, validationErrorf("depends on unknown task %s", id)
		}
		links = append(links, model.Link{Kind: model.LinkDependsOn, Target: id})
	}
	return links, nil
}

// parseReads turns a set of owner/name strings into model.RepoRefs,
// rejecting one that does not parse before the store is touched -- the
// same "caught here, not left to surface later" reasoning target's own
// parse in CreateTask follows. Blank entries are dropped rather than
// rejected, matching dependsOnLinks: a picker built from free text will
// produce them.
func parseReads(repos []string) ([]model.RepoRef, error) {
	reads := make([]model.RepoRef, 0, len(repos))
	for _, raw := range repos {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		repo, err := model.ParseRepo(text)
		if err != nil {
			return nil, &ValidationError{err: err}
		}
		reads = append(reads, repo)
	}
	return reads, nil
}

// UpdateTaskRequest is a task's editable fields -- nil means "leave this
// one alone". Repo pointing at an empty string is rejected rather than
// clearing the target: a task with no target cannot be dispatched, and
// the store has a real column for it now rather than an optional
// directive line that could simply be absent.
type UpdateTaskRequest struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Repo        *string `json:"repo,omitempty"`
	Base        *string `json:"base,omitempty"`
	AutoMerge   *bool   `json:"autoMerge,omitempty"`
	// SandboxCPUs and SandboxMemoryMB (bwsalmon/agents#534) edit the same
	// per-task override CreateTaskRequest's own fields set. Unlike most
	// pointer fields here, *req.SandboxCPUs == 0 is a meaningful, valid
	// edit (clearing a previously-set override back to "use the
	// deployment default"), not rejected the way Repo's own empty string
	// is -- only the request field itself being nil means "leave alone".
	SandboxCPUs     *int `json:"sandboxCpus,omitempty"`
	SandboxMemoryMB *int `json:"sandboxMemoryMb,omitempty"`
	// Reads, given, replaces the whole set of read-only repos rather than
	// adding to it -- there is no per-entry attach/detach endpoint for
	// Reads the way SetCapability and SetDependency give Grants and
	// Links, since a read-only repo grants nothing and so has no toggle
	// worth exposing on its own. A non-nil empty slice clears the set.
	Reads *[]string `json:"reads,omitempty"`
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
	if req.SandboxCPUs != nil || req.SandboxMemoryMB != nil {
		var cpus, memoryMB int
		if req.SandboxCPUs != nil {
			cpus = *req.SandboxCPUs
		}
		if req.SandboxMemoryMB != nil {
			memoryMB = *req.SandboxMemoryMB
		}
		if err := validateSandboxShape(cpus, memoryMB); err != nil {
			return Task{}, err
		}
	}
	var reads []model.RepoRef
	if req.Reads != nil {
		var err error
		reads, err = parseReads(*req.Reads)
		if err != nil {
			return Task{}, err
		}
	}

	// titleChanged/descriptionChanged are recomputed on every call apply
	// makes, including a retry against a fresh read after another writer
	// won the race -- so whichever attempt actually lands is the one
	// whose diff the addendum below describes, never a stale one from an
	// attempt that got overwritten before it ever took effect.
	var titleChanged, descriptionChanged bool
	if err := c.mutate(ctx, id, func(task *model.Task) error {
		if req.Title != nil {
			titleChanged = *req.Title != task.Title
			task.Title = *req.Title
		}
		if req.Description != nil {
			descriptionChanged = *req.Description != task.Body
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
		if req.SandboxCPUs != nil {
			task.SandboxCPUs = *req.SandboxCPUs
		}
		if req.SandboxMemoryMB != nil {
			task.SandboxMemoryMB = *req.SandboxMemoryMB
		}
		if req.Reads != nil {
			task.Reads = reads
		}
		return nil
	}); err != nil {
		return Task{}, err
	}
	if titleChanged || descriptionChanged {
		if err := c.noteEdit(ctx, id, titleChanged, req.Title, descriptionChanged, req.Description); err != nil {
			return Task{}, err
		}
	}
	return c.Task(ctx, id)
}

// noteEdit records an editorial change to title or description as an
// ordinary Comment, attributed the same way AddComment attributes a
// human's own reply -- there is no separate "edit log" here, because a
// second channel for the same fact would also need its own way to reach
// a run in flight, and Comment already has one: orchestrator.
// addendaPoller and commentThreadSection both read every task's
// conversation without caring whether an entry started life as a reply
// or as this. An edit to any other field (repo, base, auto-merge, reads)
// gets no comment: those are dispatch mechanics a running agent has no
// use for mid-run, unlike the title and body BuildPrompt actually hands
// it.
func (c *Client) noteEdit(ctx context.Context, id string, titleChanged bool, title *string, descriptionChanged bool, description *string) error {
	var parts []string
	if titleChanged {
		parts = append(parts, fmt.Sprintf("the title changed to %q", *title))
	}
	if descriptionChanged {
		parts = append(parts, fmt.Sprintf("the description changed to:\n\n%s", *description))
	}
	body := fmt.Sprintf("This task was just edited -- %s.", strings.Join(parts, "; and "))
	_, err := c.Store.AddComment(ctx, model.Comment{
		TaskID:    id,
		Author:    model.Attribution{Actor: c.Config.Actor},
		Body:      body,
		CreatedAt: c.now(),
	})
	return err
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

// SetDependency attaches or detaches one depends-on link -- the picker's
// attach/detach counterpart to SetCapability, so a dependency named after
// creation goes through the same mutate-and-retry path every other edit
// here does. Detaching one not attached is a no-op, matching
// SetCapability; attaching one already attached is too, since the link
// table's own primary key (task_id, kind, target) already forbids a
// duplicate.
func (c *Client) SetDependency(ctx context.Context, id, dependsOnID string, attach bool) error {
	if strings.TrimSpace(dependsOnID) == "" {
		return validationErrorf("dependsOn id is required")
	}
	if !attach {
		return c.mutate(ctx, id, func(task *model.Task) error {
			kept := make([]model.Link, 0, len(task.Links))
			for _, l := range task.Links {
				if !(l.Kind == model.LinkDependsOn && l.Target == dependsOnID) {
					kept = append(kept, l)
				}
			}
			task.Links = kept
			return nil
		})
	}
	links, err := c.dependsOnLinks(ctx, []string{dependsOnID}, id)
	if err != nil {
		return err
	}
	return c.mutate(ctx, id, func(task *model.Task) error {
		for _, l := range task.Links {
			if l.Kind == model.LinkDependsOn && l.Target == dependsOnID {
				return nil
			}
		}
		task.Links = append(task.Links, links...)
		return nil
	})
}

// ReorderRequest is a drag-and-drop move (bwsalmon/agents#476): ids, in
// the order they should keep relative to each other, dropped between
// whatever AfterID and BeforeID currently name. Either may be empty --
// AfterID empty means ids become the new minimum, dropped at the head of
// a list ("just before the following job"), BeforeID empty means they
// become the new maximum. Both resolve against the full backlog, not
// whatever view the drag happened in: TaskList.jsx computes them from the
// nearest neighbours still visible under its current filter, which is
// what lets a filtered drag land the dragged task(s) correctly in the
// unfiltered order even though it never saw most of that order.
type ReorderRequest struct {
	IDs      []string `json:"ids"`
	AfterID  string   `json:"afterId"`
	BeforeID string   `json:"beforeId"`
}

// Reorder applies a ReorderRequest. A blank AfterID/BeforeID means "no
// bound on this side" (Store.Reorder's own nil), not "the task with the
// empty string id" -- task ids are never empty (model.BranchName's own
// doc comment: NewTaskID allocates them, decimal and always non-empty).
func (c *Client) Reorder(ctx context.Context, req ReorderRequest) error {
	if len(req.IDs) == 0 {
		return validationErrorf("ids is required")
	}
	var afterID, beforeID *string
	if req.AfterID != "" {
		afterID = &req.AfterID
	}
	if req.BeforeID != "" {
		beforeID = &req.BeforeID
	}
	return c.Store.Reorder(ctx, req.IDs, afterID, beforeID)
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
	return c.Store.Approve(ctx, id, model.Attribution{Actor: c.Config.Actor}, c.now())
}

// Submit is the UI's own "submit" button: once a task has a pull request
// open, this is what puts it on its target repo's merge queue for
// automatic conflict resolution and merging -- see
// orchestrator.isQueueMember's own doc comment, which already names this
// step and reuses AutoMerge as the field that means it rather than adding
// a second one. Submitting an already-submitted task is a no-op.
func (c *Client) Submit(ctx context.Context, id string) error {
	return c.mutate(ctx, id, func(task *model.Task) error {
		task.AutoMerge = true
		return nil
	})
}

// AddComment appends to a task's conversation, optionally carrying files
// alongside it (bwsalmon/agents#522's "attachable to follow-on comments
// in addition to the main task content") -- a comment needs a body, at
// least one attachment, or both; one with neither says nothing at all.
//
// This is also how a human answers a question a run parked on: an
// awaiting_reply task has Observation.PendingQuestionCommentID set, and
// replying clears it so the task queues again. That used to require
// re-applying the trigger label to the issue so the next poll would
// notice -- the reply and the re-trigger were two separate acts, and
// forgetting the second left the task parked forever.
func (c *Client) AddComment(ctx context.Context, id, body string, uploads []AttachmentUpload) error {
	if strings.TrimSpace(body) == "" && len(uploads) == 0 {
		return validationErrorf("a comment needs a body, an attachment, or both")
	}
	now := c.now()
	// Decoded before the comment itself is written, for the same "fail
	// with nothing written" reason CreateTask decodes its own attachments
	// before NewTaskID allocates anything.
	attachments, err := decodeAttachments(uploads, now)
	if err != nil {
		return err
	}
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}

	commentID, err := c.Store.AddComment(ctx, model.Comment{
		TaskID:    id,
		Author:    model.Attribution{Actor: c.Config.Actor},
		Body:      body,
		CreatedAt: now,
	})
	if err != nil {
		return err
	}
	for i := range attachments {
		attachments[i].TaskID = id
		attachments[i].CommentID = &commentID
		if _, err := c.Store.AddAttachment(ctx, attachments[i]); err != nil {
			return err
		}
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

// Retry clears a task's own failure streak (model.Store.FailureStreak),
// the human "try again" signal bwsalmon/agents#403 asks for: without it,
// a task that reached model.MaxConsecutiveFailures would stay in
// StateFailed forever, since nothing else ever resets that count. Calling
// it on a task that is not currently failed is a harmless no-op --
// RetryRequestedAt only ever narrows how far back task_streak looks, and
// a task with no failing streak to narrow just keeps whatever state it
// already had.
func (c *Client) Retry(ctx context.Context, id string) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	now := c.now()
	return c.Store.ObserveField(ctx, id, now, func(o *model.Observation) {
		o.RetryRequestedAt = &now
	})
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
