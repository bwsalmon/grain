package ui

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
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
// sees it immediately rather than a request later. cmd/grain/daemon.go
// sets Config.TargetRepos once, from what loadConfig resolved at
// startup; every change after that arrives here, since UpdateSettings is
// the only way one is ever made.
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

// ListTasks returns every task in backlog order -- Store.ListTasks' own
// order untouched, ascending OrderKey, which is top-to-bottom the order
// Store.Ready dispatches in. Whatever runs next is at the top, the pull
// requests the merge queue is about to land at the very top of that
// (Store.MoveToFrontOfBacklog), and the work furthest out is at the
// bottom.
//
// There is no display flip left to apply (grain/task-201). This used to
// hand back the reverse of the store's order -- newest first -- unless
// model.Config.NewestFirst was set, so a list read in the opposite
// direction to the one grain works through it: the task at the top was
// the last one that would run, and "what is grain about to do" meant
// scrolling to the end. Reading the list downwards is reading the future
// forwards, which is what makes the merge queue's own order at the head
// of it (README's "what is grain about to finish, and in what order")
// legible at all.
//
// NewestFirst survives as what it always really was underneath: where
// new work joins the backlog, not how the backlog is drawn. Off, the
// default, files a new task at the end of the list, behind everything
// already queued; on files it at the front so it runs next
// (Store.OrderKeyForNewTask, Client.CreateTask). Either way the list is
// this one order.
func (c *Client) ListTasks(ctx context.Context) ([]Task, error) {
	tasks, err := c.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
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
// every call: a setting a UI toggles is expected to be honoured by the
// very next task creation, not by the next restart, and this package is
// the one that consumes it, so this is where re-reading it belongs. A
// fresh deployment with no grain_config row yet (nil) reads as false --
// model.Config's own zero value, and the end of the backlog grain has
// always filed new work at.
//
// Every other setting earns the same "no restart" treatment wherever its
// own consumer is: orchestrator.RunCycle re-reads MaxWorkers/MaxMergers and
// MaxAgentTurns every cycle, cmd/grain's dispatchConfig re-reads the
// agent framework and its model per dispatch, and its liveConfig
// re-applies the rest once per reconcile tick (that type's own doc
// comment, including the two settings that genuinely cannot be applied
// live and are reported to this pane as needing a restart).
//
// The Settings pane is no longer the only thing that writes it: a task
// filed with CreateTaskRequest.AtFront set remembers that choice here
// (Store.SetNewestFirst), so what this reads is usually where the last
// task added went rather than a toggle anybody had to go looking for.
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
	detail.PendingSecret = c.pendingSecretFor(obs)
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

// TaskPrompt is a task's own prompt, as one attempt was actually given
// it: the whole text orchestrator.RunDispatch assembled and handed the
// agent, plus which attempt it belongs to so a reader knows how old the
// answer is (a redispatched task's prompt grows with its conversation,
// so attempt 3's is not attempt 1's).
//
// Attempt is 0 and Prompt "" for a task nothing has recorded a prompt
// for -- one never dispatched, one whose every attempt died in setup
// before its agent got a turn, and one that ran before the column
// existed all land here. They are one case on the wire on purpose: none
// of them has a prompt to show, and the frontend says exactly that
// rather than inventing one by re-deriving what a dispatch *would*
// build, which would differ from the real thing in the details (the
// checkout, the capability sections) that make it worth reading.
type TaskPrompt struct {
	Prompt  string `json:"prompt"`
	Attempt int    `json:"attempt,omitempty"`
}

// TaskPrompt returns the prompt taskID's most recent attempt was given
// -- the read behind GET /api/tasks/{id}/prompt, and the answer to "what
// was the agent actually told?" that a task's own title and description
// only partly are.
//
// Most recent rather than every attempt: the question is what this task
// looks like to an agent now, and the newest prompt is the one that
// includes everything said on the task since. An attempt that never
// reached its agent recorded no prompt, so this walks back from the
// newest until it finds one rather than reporting "no prompt" for a task
// whose latest attempt merely failed to clone.
func (c *Client) TaskPrompt(ctx context.Context, taskID string) (TaskPrompt, error) {
	t, err := c.Store.GetTask(ctx, taskID)
	if err != nil {
		return TaskPrompt{}, err
	}
	if t == nil {
		return TaskPrompt{}, &NotFoundError{ID: taskID}
	}
	runs, err := c.Store.Runs(ctx, taskID)
	if err != nil {
		return TaskPrompt{}, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		prompt, found, err := c.Store.RunPrompt(ctx, taskID, runs[i].Attempt)
		if err != nil {
			return TaskPrompt{}, err
		}
		if found && prompt != "" {
			return TaskPrompt{Prompt: prompt, Attempt: runs[i].Attempt}, nil
		}
	}
	return TaskPrompt{}, nil
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
	// SandboxCPUs, SandboxMemoryMB and SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) set model.Task's own fields of
	// the same name -- a per-task override of the deployment's default
	// sandbox shape. 0 (the default for all three) means no override: the
	// task dispatches at whatever shape the deployment otherwise
	// configures.
	SandboxCPUs     int `json:"sandboxCpus"`
	SandboxMemoryMB int `json:"sandboxMemoryMb"`
	SandboxDiskGB   int `json:"sandboxDiskGb"`
	// AgentFramework sets model.Task's own field of the same name -- a
	// per-task override of the deployment's default agent framework
	// (Settings' own "Agent framework"). "" (the default) means no
	// override: the task is driven by whichever framework the deployment
	// is set to when it dispatches. Anything but "" must be one of
	// model.AgentFrameworks() (the legacy "gemini" spelling is accepted
	// and normalized to "antigravity"), validated the same way
	// UpdateSettings validates the deployment-wide setting.
	AgentFramework string `json:"agentFramework"`
	// PromptExtension sets model.Task's own field of the same name
	// (grain/task-114) -- this task's own standing instructions, used
	// *instead of* the deployment's and its repo's for this task's runs.
	// "" (the default) means no override, so the task gets whatever those
	// two say when it dispatches. Trimmed on the way in, the same as the
	// deployment-wide and per-repo layers, so a box someone typed a
	// newline into files as no override rather than as an override that
	// silences both.
	PromptExtension string `json:"promptExtension"`
	// Capabilities is the exact set of capability ids this task is filed
	// holding -- but only when the caller names one. nil (the field left
	// out, or JSON null) means the caller expressed no opinion, and the
	// task is filed with this deployment's own default set instead
	// (model.Config.DefaultCapabilities, recorded as model.GrantByDefault
	// grants). An empty-but-present list is an opinion: exactly no
	// capabilities, defaults included.
	//
	// A pointer for the same reason UpdateSettingsRequest's fields are
	// one: "leave this to whatever the deployment says" and "I want none
	// of them" are different requests, and a bare []string cannot tell an
	// omitted field from an empty one for a caller writing Go rather than
	// JSON. NewTaskOverlay.jsx always sends a list, seeded from GET
	// /api/config's own defaultCapabilities, so what a human sees ticked
	// on the form is what the task is filed with, unticking included.
	Capabilities *[]string `json:"capabilities"`
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
	// AtFront is which end of the backlog this task joins: true files it
	// at the front, ahead of everything already queued, so it runs next;
	// false files it at the end, behind everything already queued, the
	// FIFO backlog grain has always defaulted to
	// (Store.OrderKeyForNewTask).
	//
	// A pointer, for the reason Capabilities above is one: nil -- the
	// field left out, or JSON null -- is a caller with no opinion, and
	// the task joins whichever end this deployment currently remembers
	// (model.Config.NewestFirst). A caller that does state one is
	// remembered: CreateTask writes that choice back as the deployment's
	// own default (Store.SetNewestFirst), so the next task filed with no
	// opinion, and the next new-task form that opens, start from what the
	// last task added chose rather than from a setting somebody has to go
	// and find in Settings.
	//
	// An interactive task ignores all of this and files at the front
	// regardless (see CreateTask): somebody is waiting on the chat right
	// now. Stating AtFront on one still remembers it, since that is the
	// filing choice being made either way.
	AtFront *bool `json:"atFront"`
	// Attachments is files to carry alongside the task's own body --
	// bwsalmon/agents#522: a screenshot, a repro zip, anything the agent
	// needs that isn't already code in a repo it can clone. Stored under
	// the new task's own id with no CommentID, and materialized into
	// every dispatched run's sandbox the same way a comment's own
	// attachments are (orchestrator.placeAttachments).
	Attachments []AttachmentUpload `json:"attachments"`
	// NoRepo files a task with no repo at all, deliberately -- a
	// standalone task nothing will clone, check out, or push a branch
	// to. Distinct from simply leaving Repo blank, which instead falls
	// back to Config.DefaultTarget (or fails validation if there is
	// none): a blank Repo says nothing either way, while NoRepo says
	// outright that no target exists. orchestrator.BuildPrompt tells the
	// dispatched agent as much, rather than leaving it to guess from an
	// empty checkout directory.
	NoRepo bool `json:"noRepo"`
}

// defaultCapabilities is the capability ids a task filed on this
// deployment, against target, starts out holding: two layers, unioned,
// deployment-wide first.
//
//   - model.Config.DefaultCapabilities, the deployment's own set
//     (task-14), which every task gets wherever it points.
//   - model.RepoConfig.DefaultCapabilities for target, the repo's own
//     additions (task-24). A nil target -- a task filed with NoRepo --
//     has no repo to key on, so it gets the deployment layer alone.
//
// Both are read from the store on every CreateTask rather than cached on
// Client.Config, so an operator who changes either set changes what the
// next task is filed with rather than what the next process is.
//
// Union is the whole composition rule: a repo adds, and never subtracts.
// model.RepoConfig.DefaultCapabilities has why "everything except gcp-key
// here" is deferred rather than spelled some other way.
//
// An id this build no longer offers is dropped rather than failing the
// creation: UpdateSettings and SetRepoDefaultCapabilities each validate
// their own set against OfferedCapabilities when it is saved, so a stored
// id with no row can only be a capability grain has retired since, and a
// settings row left behind by an upgrade must not become a deployment
// where no task can be filed at all. That is the one place this differs
// from a caller naming its own ids, which are still rejected as unknown
// (grantsFor) -- a human who asks for a capability by name should hear
// that it does not exist.
//
// Only tasks filed through CreateTask are seeded. A schedule, a template
// and a suite each carry a grant set that was authored once, in a form
// of its own, and the tasks they file are filed with it -- defaulting
// them here as well would quietly widen a set somebody already wrote
// down.
func (c *Client) defaultCapabilities(ctx context.Context, target *model.RepoRef) ([]string, error) {
	cfg, err := c.Store.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	var stored []string
	if cfg != nil {
		stored = cfg.DefaultCapabilities
	}
	if target != nil {
		repoCfg, err := c.Store.GetRepoConfig(ctx, *target)
		if err != nil {
			return nil, err
		}
		if repoCfg != nil {
			stored = append(append([]string{}, stored...), repoCfg.DefaultCapabilities...)
		}
	}
	ids := make([]string, 0, len(stored))
	seen := make(map[string]bool, len(stored))
	for _, id := range stored {
		if seen[id] {
			continue
		}
		if _, ok := c.capabilityByID(id); !ok {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// CreateTask files a task straight into the store.
//
// No GitHub issue is created, and none is needed: Store.NewTaskID
// allocates identity, so a task exists the moment this returns. That is
// the whole inversion in one method -- this used to open an issue and let
// a poll notice it, which meant a task could not be created without
// GitHub reachable, and could not be dispatched until the next tick.
//
// Capabilities are resolved once, for every caller: a
// request that names its own set is filed with exactly that set, and one
// that names none at all is filed with the default set resolved for the
// repo it targets -- this deployment's, plus that repo's own
// (CreateTaskRequest.Capabilities, model.Config.DefaultCapabilities,
// model.RepoConfig.DefaultCapabilities). That resolution happens after
// the target repo is decided just above, so a task filed with no repo at
// all gets Config.DefaultTarget's own repo defaults rather than none.
// Either way what comes out is a plain model.Grant on the task itself --
// there is no second, deployment-level grant set read again at dispatch,
// so every capability a run holds is one that can be seen and detached
// on the task that holds it.
//
// req.AtFront (grain/task-202) is resolved the same "the request has the
// last word, and is remembered" way: it says which end of the backlog
// this task joins, and stating it also stores that end as the
// deployment's own default (model.Config.NewestFirst) for the next task
// filed without one. That is what makes the choice sticky from the form
// rather than from Settings -- whoever files a task at the front is
// usually about to file the next one there too, and the new-task form
// seeds itself from the same stored value (GET /api/config's
// newestFirst).
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Task{}, validationErrorf("title is required")
	}

	var target *model.RepoRef
	switch {
	case req.NoRepo:
		if strings.TrimSpace(req.Repo) != "" {
			return Task{}, validationErrorf("repo and noRepo are mutually exclusive")
		}
	case strings.TrimSpace(req.Repo) != "":
		parsed, err := model.ParseRepo(req.Repo)
		if err != nil {
			return Task{}, &ValidationError{err: err}
		}
		target = &parsed
	default:
		target = c.Config.DefaultTarget
		if target == nil {
			return Task{}, validationErrorf(
				"no repo given, and this deployment has no default target repo configured")
		}
	}

	if err := validateSandboxShape(req.SandboxCPUs, req.SandboxMemoryMB, req.SandboxDiskGB); err != nil {
		return Task{}, err
	}
	if err := validateAgentFramework(req.AgentFramework); err != nil {
		return Task{}, err
	}
	grants, err := c.creationGrants(ctx, req, target)
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
	// atFront: which end of the backlog this task joins. A request that
	// states it (CreateTaskRequest.AtFront) is the last word; one that
	// does not gets the end this deployment remembers, which is the end
	// the last task filed with an opinion chose
	// (model.Config.NewestFirst). Either way, true files it ahead of
	// everything already queued -- the top of the list, dispatched next
	// (Store.OrderKeyForNewTask's own doc comment) -- and false at the
	// end instead, behind everything queued, the FIFO backlog grain has
	// always defaulted to. An interactive task asks for the front
	// unconditionally, on top of whatever either of those says, since
	// somebody is waiting on it right now rather than checking back on it
	// later (CreateTaskRequest.Interactive's own doc comment).
	atFront := false
	if req.AtFront != nil {
		atFront = *req.AtFront
	} else if atFront, err = c.newestFirst(ctx); err != nil {
		return Task{}, err
	}
	orderKey, err := c.Store.OrderKeyForNewTask(ctx, atFront || req.Interactive)
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
		SandboxDiskGB:   req.SandboxDiskGB,
		AgentFramework:  req.AgentFramework,
		PromptExtension: strings.TrimSpace(req.PromptExtension),
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
	// Remembered only once the task it came with is actually filed, and
	// only when the request stated a choice at all: "where the last task
	// added went" is a fact about tasks that exist, and a caller that
	// said nothing has expressed nothing to remember
	// (CreateTaskRequest.AtFront).
	if req.AtFront != nil {
		if err := c.Store.SetNewestFirst(ctx, *req.AtFront); err != nil {
			return Task{}, err
		}
	}
	for i := range attachments {
		attachments[i].TaskID = id
		if _, err := c.Store.AddAttachment(ctx, attachments[i]); err != nil {
			return Task{}, err
		}
	}
	if target != nil && !targetAllowed(c.targetRepos(), *target) {
		if err := c.parkOffAllowlist(ctx, id, *target, now); err != nil {
			return Task{}, err
		}
	}
	return c.Task(ctx, id)
}

// validateAgentFramework checks a task's own AgentFramework override.
// "" is the common case and always valid -- it is what "use the
// deployment's own framework" is spelled as (model.Task.AgentFramework's
// own doc comment) -- so unlike UpdateSettings' check of the
// deployment-wide setting, which has no such reading, empty is accepted
// here rather than rejected as unset.
func validateAgentFramework(framework string) error {
	// "" is checked here rather than left to ValidAgentFramework, which
	// deliberately rejects it: for a task, empty means "no override"
	// rather than naming a framework (model.Task.AgentFramework's own
	// doc comment), and only this caller reads it that way.
	if framework == "" || model.ValidAgentFramework(framework) {
		return nil
	}
	return validationErrorf("agentFramework must be %s, or empty for this deployment's default",
		model.AgentFrameworkNames())
}

// validateSandboxShape checks a task's own SandboxCPUs/SandboxMemoryMB/
// SandboxDiskGB override, mirroring bwsalmon/kontur's own
// staticpod.VMSpec.Validate bounds ("cpus must be at least 1",
// "memory-mb must be at least 128") the same way UpdateSettings'
// identical check does for the deployment-wide default -- 0 is the one
// value each rejects that Validate would not, since 0 means "no
// override" here rather than a literal request for a zero-vCPU or
// zero-memory VM.
//
// Disk has no Validate bound to mirror (konturctl checks a disk size
// against the guest image it reads through to rather than against a
// floor of its own), so the only value rejected there is a negative one.
func validateSandboxShape(cpus, memoryMB, diskGB int) error {
	if cpus != 0 && cpus < 1 {
		return validationErrorf("sandboxCpus must be 0 (no override) or at least 1")
	}
	if memoryMB != 0 && memoryMB < 128 {
		return validationErrorf("sandboxMemoryMb must be 0 (no override) or at least 128")
	}
	if diskGB < 0 {
		return validationErrorf("sandboxDiskGb must be 0 (no override) or at least 1")
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
// deployment does not offer. via is what each one records as its source
// -- model.GrantByLabel for ids a caller named, model.GrantByDefault for
// ids this deployment attaches to every new task by itself.
func (c *Client) grantsFor(ids []string, via model.GrantSource) ([]model.Grant, error) {
	grants := make([]model.Grant, 0, len(ids))
	for _, id := range ids {
		if _, ok := c.capabilityByID(id); !ok {
			return nil, validationErrorf("unknown capability %s", id)
		}
		grants = append(grants, model.Grant{Capability: id, Via: via})
	}
	return grants, nil
}

// creationGrants is the grant set a new task is filed with: whatever the
// request named, or -- when it named nothing at all -- the default
// capabilities resolved for target (this deployment's, plus target's own
// repo row).
//
// target is the repo CreateTask has already resolved, defaults and all,
// rather than req.Repo as written: a request that names no repo is filed
// against Config.DefaultTarget, and that repo's own defaults are the ones
// such a task should get. nil is a NoRepo task, which has no repo layer
// to resolve at all.
//
// The two are told apart by CreateTaskRequest.Capabilities being nil
// rather than empty, and they record different sources: a caller's ids
// are model.GrantByLabel ("a human applied it"), a deployment's are
// model.GrantByDefault. Nothing reads Via to decide what a grant does --
// it is provenance, so that a task carrying gcp-key nobody remembers
// asking for can be traced back to Settings or to the repo page rather
// than to whoever filed it. One source for both layers, deliberately:
// which of the two attached it is a question the two panes that own them
// answer, and a task holding a grant it can drop either way has nothing
// to do differently with the answer.
func (c *Client) creationGrants(ctx context.Context, req CreateTaskRequest, target *model.RepoRef) ([]model.Grant, error) {
	var grants []model.Grant
	if req.Capabilities != nil {
		named, err := c.grantsFor(*req.Capabilities, model.GrantByLabel)
		if err != nil {
			return nil, err
		}
		grants = named
	} else {
		ids, err := c.defaultCapabilities(ctx, target)
		if err != nil {
			return nil, err
		}
		defaults, err := c.grantsFor(ids, model.GrantByDefault)
		if err != nil {
			return nil, err
		}
		grants = defaults
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
	// SandboxCPUs, SandboxMemoryMB and SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) edit the same per-task
	// override CreateTaskRequest's own fields set. Unlike most
	// pointer fields here, *req.SandboxCPUs == 0 is a meaningful, valid
	// edit (clearing a previously-set override back to "use the
	// deployment default"), not rejected the way Repo's own empty string
	// is -- only the request field itself being nil means "leave alone".
	SandboxCPUs     *int `json:"sandboxCpus,omitempty"`
	SandboxMemoryMB *int `json:"sandboxMemoryMb,omitempty"`
	SandboxDiskGB   *int `json:"sandboxDiskGb,omitempty"`
	// AgentFramework edits the same per-task override
	// CreateTaskRequest.AgentFramework sets, and an empty string is a
	// meaningful edit here for the same reason a 0 is above: it clears
	// the override back to "use the deployment's framework". A task
	// already dispatched keeps whichever framework its live run started
	// with -- Deps.Framework is asked once, when the run begins.
	AgentFramework *string `json:"agentFramework,omitempty"`
	// PromptExtension edits the same per-task override
	// CreateTaskRequest.PromptExtension sets, with an empty string
	// meaningful for the same reason it is on AgentFramework above: it
	// clears the override, so the task's next run is told whatever its
	// deployment and its repo say. Read at dispatch rather than at
	// creation (model.Task.PromptExtension), so an edit here reaches any
	// run this task has not started yet -- but not one already live,
	// whose prompt was built when it began.
	PromptExtension *string `json:"promptExtension,omitempty"`
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
	if req.SandboxCPUs != nil || req.SandboxMemoryMB != nil || req.SandboxDiskGB != nil {
		var cpus, memoryMB, diskGB int
		if req.SandboxCPUs != nil {
			cpus = *req.SandboxCPUs
		}
		if req.SandboxMemoryMB != nil {
			memoryMB = *req.SandboxMemoryMB
		}
		if req.SandboxDiskGB != nil {
			diskGB = *req.SandboxDiskGB
		}
		if err := validateSandboxShape(cpus, memoryMB, diskGB); err != nil {
			return Task{}, err
		}
	}
	if req.AgentFramework != nil {
		if err := validateAgentFramework(*req.AgentFramework); err != nil {
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
		if req.SandboxDiskGB != nil {
			task.SandboxDiskGB = *req.SandboxDiskGB
		}
		if req.AgentFramework != nil {
			task.AgentFramework = *req.AgentFramework
		}
		if req.PromptExtension != nil {
			task.PromptExtension = strings.TrimSpace(*req.PromptExtension)
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
//
// Only attaching checks the id against this deployment's listing.
// Detaching an id with no row is allowed, because a task can be holding
// a grant nothing offers any more -- a renamed capability
// (OfferedCapabilities' own "scratch-repo", now github-sandbox), or one
// a deployment stopped listing -- and such a grant is exactly the one an
// operator most needs to remove: it fails the task's every dispatch at
// model.ResolveGrants. Refusing to detach it would leave the only route
// out through the store. Detaching can only ever shrink the grant set,
// so nothing the validation protects is reachable this way.
//
// DetailOverlay.jsx's CapabilityToggles is the other half of that route:
// it gives such a grant a picker row of its own purely so it can be
// unticked, since only rows can be toggled off.
func (c *Client) SetCapability(ctx context.Context, id, capabilityID string, attach bool) error {
	if _, ok := c.capabilityByID(capabilityID); attach && !ok {
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
	// A drop naming a task nothing exists behind is a stale list in
	// somebody's browser, not a broken server -- so it reports as this
	// package's own NotFoundError, which the server answers 404, rather
	// than falling out of the store as "reordering: no such task 999"
	// and being reported as a 500 (found by hand, task 244). The same
	// translation Client.mutate does for every single-task edit.
	for _, id := range append(append([]string{}, req.IDs...), req.AfterID, req.BeforeID) {
		if id == "" {
			continue
		}
		task, err := c.Store.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &NotFoundError{ID: id}
		}
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

// WithdrawApproval takes an approved task back out of the queue: it
// clears the approval Approve recorded, which is all it takes for
// model.StateOf to read the task as 'proposed' again and for task_ready
// to stop offering it to dispatch. It is the way to stop a task that has
// been queued -- reconsidered, filed too early, or simply not wanted
// this week -- without closing it, and re-approving it later is the
// ordinary Approve call, nothing special.
//
// "Withdraw approval" rather than "unapprove" because that is what
// docs/data-model.md already calls it, in the same paragraph that gave
// this operation its reason to exist: approval is a declaration, so
// "cancellation gets a mechanism it did not have ... withdrawing
// approval is [available], and it is [reviewable] like any other
// declaration change". Nothing here writes a state -- the state moves
// because the declaration behind it went away.
//
// Two boundaries, both about not rewriting history:
//
//   - A task with no approval is untouched, mirroring Approve's own
//     no-op on an already-approved task.
//   - A task that is running, or past its run (completed, awaiting
//     submit, or closed) is refused. Its
//     approval has already been spent on work that happened, so clearing
//     it would erase the record (Task.ApprovedAt, which metrics reads as
//     "queued since") of a queue wait that was real, and it would stop
//     nothing: the way to stop a run in flight is Close, which cancels
//     it. The check races a dispatch that starts a run just after it --
//     harmlessly, since the withdrawal only means that run is the last
//     one, which is what withdrawing asks for anyway.
func (c *Client) WithdrawApproval(ctx context.Context, id string) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	if task.Approval == nil {
		return nil
	}
	state, err := c.Store.State(ctx, id)
	if err != nil {
		return err
	}
	switch state {
	case model.StateRunning:
		return validationErrorf("task %s is running: close it to cancel the run", id)
	case model.StateCompleted, model.StateAwaitingSubmit, model.StateClosed:
		return validationErrorf("task %s is %s: its approval is a record of work that already happened", id, state)
	}
	return c.Store.WithdrawApproval(ctx, id)
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
	// PendingSecret goes with it. A run's request_secret call parks the
	// task through the very field above (orchestrator.relayParkingCalls),
	// so a reply un-parks a task that was waiting for a credential just
	// as surely as one that was waiting for an answer -- and a task back
	// in the queue must not still be offering a box that writes a value
	// nothing is waiting for. Answering "not this one, use the staging
	// key" in words is a legitimate answer to a request for a secret.
	return c.Store.ObserveField(ctx, id, now, func(o *model.Observation) {
		o.PendingQuestionCommentID = nil
		o.PendingSecret = ""
	})
}

// CloseOptions is what a human chose at the moment of closing, beyond
// closing the task itself.
//
// An argument rather than a setting anywhere, on purpose: its one field
// destroys work on GitHub, and the only defensible basis for that is
// somebody saying so about this task, now. Nothing stored could mean
// that, and a default that meant it would eventually mean it about a
// task nobody was looking at. The zero value is therefore the whole of
// grain's own behaviour -- every caller that is not a human close passes
// it, and the checkbox beside the Close button is the only thing in the
// tree that ever sets the field.
type CloseOptions struct {
	// ClosePullRequest asks grain to close the pull request this close
	// would otherwise orphan, on GitHub, without merging it. The commits
	// and the branch survive it untouched (reopening the pull request
	// restores it whole), which is the only reason it is offered at all.
	//
	// Best-effort, like the note itself: a GitHub that refuses leaves the
	// task closed and the pull request open, and says so in the note. See
	// noteOrphanedPullRequests.
	ClosePullRequest bool
}

// Close marks a task closed. Closed is the terminal state
// model.StateClosed already names, and it is what "delete a task" means
// here -- the store has no delete either, deliberately: a task that ran
// is a record of a dispatch that happened.
//
// A task closed with a pull request still open leaves that pull request
// behind: grain will not merge it and will not look at it again (only an
// unclosed task's fixes-link reaches Store.OpenPullRequestLinks). That
// is the intended outcome, and this is where the person who caused it --
// and whoever finds the pull request later -- is told about it.
//
// opts.ClosePullRequest is the other ending, and the only way grain ever
// closes a pull request: whoever is closing the task says, in the same
// breath, that its pull request should be closed with it. Both endings
// are said in both places. See noteOrphanedPullRequests.
func (c *Client) Close(ctx context.Context, id string, opts CloseOptions) error {
	return c.setClosed(ctx, id, true, opts)
}

// Reopen clears a task's closure, returning it to whatever state its
// observations and approval imply.
//
// It does not reopen a pull request a close closed. Reopening one is a
// click on GitHub and grain has no way to tell a pull request it closed
// from one a human closed themselves, so guessing here would be grain
// reopening work somebody deliberately shut -- the note left by the
// close says as much, and points at the one place that can.
func (c *Client) Reopen(ctx context.Context, id string) error {
	return c.setClosed(ctx, id, false, CloseOptions{})
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

func (c *Client) setClosed(ctx context.Context, id string, closed bool, opts CloseOptions) error {
	task, err := c.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task == nil {
		return &NotFoundError{ID: id}
	}
	now := c.now()
	if err := c.Store.ObserveField(ctx, id, now, func(o *model.Observation) {
		if closed {
			o.ClosedAt = &now
			return
		}
		o.ClosedAt = nil
	}); err != nil {
		return err
	}
	if !closed {
		return nil
	}
	// After the close is written, not before: the note says the task *is*
	// closed, and a note left in front of a close that then failed would
	// say something untrue. Reading the links off the task fetched above
	// is enough -- a link written between that read and this call belongs
	// to a run still finishing, and the finish path re-reads the closure
	// and leaves the same note itself (orchestrator's own
	// salvagePushedBranch).
	return c.noteOrphanedPullRequests(ctx, *task, opts, now)
}
