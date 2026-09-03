package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// Task is a task's JSON shape -- everything the frontend needs to list
// and render one.
//
// ID is a string, not a number. It used to be the GitHub issue number a
// task was named after; it is now whatever Store.NewTaskID allocated,
// which happens to be decimal today and is opaque to everything here.
//
// Three fields the issue-backed shape carried are gone: htmlUrl and
// githubState, because there is no issue to link to or mirror the open/
// closed flag of, and labels, because state and capabilities are no
// longer derived from any. PullRequest replaces htmlUrl as the one thing
// worth linking to on GitHub, read off the task's own LinkFixes link.
// GeneratedFrom is the same read off LinkProposedBy: the ID of the task
// whose propose_task call filed this one, provenance only, empty for a
// task nobody proposed.
type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Author      string `json:"author"`
	// AuthorKind is that author's model.PrincipalKind -- "human",
	// "agent" or "automation", Comment.AuthorKind's own vocabulary for
	// the same question about a comment. Author alone cannot answer it:
	// an ID is a GitHub login for a person, a run ID for an agent and a
	// deployment name for automation, and nothing in the string says
	// which. The frontend needs the distinction to keep a task nobody
	// filed by hand from steering a human's own defaults -- see
	// state.js's lastBaseForRepo.
	AuthorKind string      `json:"authorKind,omitempty"`
	State      model.State `json:"state"`
	Repo       string      `json:"repo,omitempty"`
	// Reads is every repo this task's run may read but never push to --
	// model.Task.Reads, rendered as owner/name strings the same way Repo
	// renders its single Target.
	Reads     []string `json:"reads,omitempty"`
	Base      string   `json:"base,omitempty"`
	AutoMerge bool     `json:"autoMerge"`
	// Interactive mirrors model.Task's own field of the same name
	// (bwsalmon/agents#539) -- true for a task filed for a live chat
	// rather than handed off to run unattended, which is what tells the
	// frontend to open its chat view straight after creating it and to
	// label its Timeline as a conversation rather than a task history.
	Interactive bool `json:"interactive"`
	// Configuration mirrors model.Task's own field of the same name
	// (bwsalmon/agents#621) -- true for the configuration agent, so the
	// frontend can label its chat distinctly from an ordinary interactive
	// task's.
	Configuration bool `json:"configuration,omitempty"`
	// SandboxCPUs and SandboxMemoryMB (bwsalmon/agents#534) override the
	// deployment's default sandbox shape for this task's own dispatch
	// alone -- model.Task's own fields of the same name. Zero (the
	// default for both) means "use the deployment default", omitted from
	// the JSON response the same way Base's own empty string is, since
	// "no override" is the common case and worth not cluttering every
	// task response with.
	SandboxCPUs     int `json:"sandboxCpus,omitempty"`
	SandboxMemoryMB int `json:"sandboxMemoryMb,omitempty"`
	// AgentFramework is this task's own override of the deployment's
	// agent framework -- model.Task's own field of the same name.
	// Omitted, like the two above, for the common "no override" case, so
	// a frontend reading it back gets the same empty-means-default it
	// sent.
	AgentFramework string   `json:"agentFramework,omitempty"`
	Capabilities   []string `json:"capabilities"`
	PullRequest    string   `json:"pullRequest,omitempty"`
	GeneratedFrom  string   `json:"generatedFrom,omitempty"`
	// Stacked is true for a task the merge queue filed automatically to
	// repair another task's own pull request (model.ReasonFix) -- built
	// on that task's own branch and merged straight back into it once
	// green. Paired with GeneratedFrom, it is what tells the frontend
	// to nest a task under the one that generated it (bwsalmon/agents#378)
	// rather than list it as a separate task: unlike an ordinary
	// propose_task child, a stacked task is not new work, just a
	// continuation of the same change.
	Stacked bool `json:"stacked,omitempty"`
	// Scheduled is true for a task a schedule filed automatically
	// (model.ReasonSchedule) -- a UI badge, the same treatment Stacked
	// already gives model.ReasonFix, so a task that appeared with nobody
	// having filed it by hand reads as expected rather than as a mystery.
	Scheduled bool `json:"scheduled,omitempty"`
	// SuiteRun is true for a task a task suite run filed automatically
	// (model.ReasonSuite, bwsalmon/agents#642) -- Scheduled's own badge
	// treatment, so a pass of tasks that appeared with nobody filing them
	// by hand reads as expected rather than as a mystery.
	SuiteRun  bool       `json:"suiteRun,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	// DependsOn is every task this one has declared a depends-on link to,
	// resolved or not -- the definition. Blocked and BlockedBy are the
	// signal: whether any of it (or a child-of link) is still open right
	// now, re-derived on every read rather than pinned at creation, per
	// docs/data-model.md's "blocked is not a state, it is derived from
	// links" -- see model.IsBlocked.
	DependsOn []string `json:"dependsOn,omitempty"`
	Blocked   bool     `json:"blocked"`
	BlockedBy []string `json:"blockedBy,omitempty"`
	// MergeQueueBlockedAt mirrors model.Observation's own field: non-nil
	// once the merge queue has stopped driving this task's pull request
	// -- an automatic fix that did not take, or checks that never
	// finished -- so it needs a human rather than another automatic
	// attempt. Alongside PullRequest and AutoMerge, this is
	// what lets the frontend tell a completed task that is merely
	// waiting on a human's Submit click apart from one already on the
	// merge queue, or one the queue has given up on -- the distinction
	// bwsalmon/agents#494 asked for, since State itself stops at
	// "completed" for a task's entire post-run life.
	MergeQueueBlockedAt *time.Time `json:"mergeQueueBlockedAt,omitempty"`
}

// Comment is one entry in a task's conversation.
//
// AuthorAssociation is gone with the issue thread that had one. What
// replaces it says more: OnBehalfOf is set when grain relayed somebody
// else's words, so a question from a dispatched run renders as grain
// speaking for an agent rather than as grain's own.
type Comment struct {
	ID         int64     `json:"id"`
	Author     string    `json:"author"`
	AuthorKind string    `json:"authorKind"`
	OnBehalfOf string    `json:"onBehalfOf,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	// Attachments is every file posted alongside this comment
	// (bwsalmon/agents#522) -- metadata only, GET
	// /api/tasks/{id}/attachments/{attachmentId} is what serves one's
	// actual content.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file's metadata on the wire -- never its content,
// which GET /api/tasks/{id}/attachments/{attachmentId} serves separately
// so listing a task or its conversation never pays to carry every byte of
// every file attached to it (bwsalmon/agents#522).
type Attachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

func attachmentFrom(a model.AttachmentMeta) Attachment {
	return Attachment{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size}
}

// TaskDetail is one task plus its conversation -- what GET
// /api/tasks/{id} returns, wider than the list shape since a list of many
// tasks has no reason to pay for every conversation.
//
// FailedAttempts, LastFailureAt and LastFailureReason are model.Store.
// FailureStreak's own fields, surfaced here rather than on Task: a list
// of many tasks already shows enough to notice a 'failed' one (its own
// state badge), and the reason it got there is a one-task question, the
// same reasoning that keeps Comments off the list shape (bwsalmon/
// agents#403 -- "a way for grain get/the UI to show N consecutive failed
// attempts, last failure: reason").
type TaskDetail struct {
	Task
	Comments []Comment `json:"comments"`
	// FailedAttempts is 0 once the task's most recent run succeeded, or
	// none has finished yet -- otherwise how many of its most recent runs,
	// in a row, ended without succeeding.
	FailedAttempts int        `json:"failedAttempts,omitempty"`
	LastFailureAt  *time.Time `json:"lastFailureAt,omitempty"`
	// LastFailureReason is that run's own task_run.detail -- a tool
	// error, the agent framework's own error text, or ProcessResult's own
	// "finished without pushing, asking, or commenting".
	LastFailureReason string `json:"lastFailureReason,omitempty"`
	// Attempts is every run this task has had, oldest first -- the full
	// history FailedAttempts only counts and summarises (bwsalmon/
	// agents#445: "show attempts and their status, in order, in the task
	// view").
	Attempts []Attempt `json:"attempts,omitempty"`
	// Transitions is model.Transitions' own projection: every point the
	// record lets this task's state be pinned to a time, oldest first
	// (bwsalmon/agents#452 -- "show the state transitions for the task").
	Transitions []Transition `json:"transitions,omitempty"`
	// PullRequestEvents is this task's own tracked pull request's history
	// -- opened, then merged or closed without merging -- off
	// model.Observation's own PrOpenedAt/PrMergedAt/PrClosedAt, oldest
	// first (bwsalmon/agents#493 -- "show PR events in the task
	// timeline"). Empty for a task with no pull request yet, or with one
	// orchestrator.SyncPullRequests has not synced even once.
	PullRequestEvents []PullRequestEvent `json:"pullRequestEvents,omitempty"`
	// Attachments is every file carried by the task's own body (never one
	// posted alongside a comment -- see Comment.Attachments for those),
	// bwsalmon/agents#522.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// PullRequestEvent is one moment in a task's own tracked pull request's
// life. Kind is "opened", "merged" or "closed" ("closed" meaning closed
// *without* merging -- a merged PR is always "merged", never "closed",
// the same distinction model.Observation.PrMergedAt/PrClosedAt draw).
type PullRequestEvent struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
}

// Transition is one entry in a task's timeline, projected from
// model.Transition.
type Transition struct {
	State model.State `json:"state"`
	At    time.Time   `json:"at"`
}

func transitionFrom(t model.Transition) Transition {
	return Transition{State: t.State, At: t.At}
}

// Attempt is one of a task's runs, projected from model.Run.
//
// Number is model.Run.Attempt, 1-based and dense per task (dispatch.
// Cycle's own doc comment on how it assigns one). Outcome is empty for a
// run still in flight (no FinishedAt yet); otherwise it is model.Run's
// own outcome vocabulary -- "succeeded", "failed", "cancelled" -- the
// same strings orchestrator.outcomeOf and orchestrator.run already
// record, passed through rather than re-encoded so the UI's label for
// one matches `grain get`'s.
type Attempt struct {
	Number     int        `json:"number"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	Detail     string     `json:"detail,omitempty"`
}

// taskFrom projects a model.Task to its JSON shape. closed reports, for
// every task ID a blocking link might target, whether that task reads
// closed -- the caller resolves it (Client.ListTasks over the whole
// store in one query, Client.Task over just this task's own targets) so
// this function stays free of the store itself.
func taskFrom(t model.Task, state model.State, closed map[string]bool, mergeQueueBlockedAt *time.Time) Task {
	out := Task{
		ID:                  t.ID,
		Title:               t.Title,
		Description:         t.Body,
		Author:              t.Origin.Attribution.Actor.ID,
		AuthorKind:          string(t.Origin.Attribution.Actor.Kind),
		State:               state,
		Base:                t.Base,
		AutoMerge:           t.AutoMerge,
		Interactive:         t.Interactive,
		Configuration:       t.Configuration,
		SandboxCPUs:         t.SandboxCPUs,
		SandboxMemoryMB:     t.SandboxMemoryMB,
		AgentFramework:      t.AgentFramework,
		Capabilities:        []string{},
		Stacked:             t.Origin.Reason == model.ReasonFix,
		Scheduled:           t.Origin.Reason == model.ReasonSchedule,
		SuiteRun:            t.Origin.Reason == model.ReasonSuite,
		CreatedAt:           t.CreatedAt,
		MergeQueueBlockedAt: mergeQueueBlockedAt,
	}
	if t.Target != nil {
		out.Repo = t.Target.String()
	}
	for _, r := range t.Reads {
		out.Reads = append(out.Reads, r.String())
	}
	for _, g := range t.Grants {
		out.Capabilities = append(out.Capabilities, g.Capability)
	}
	for _, l := range t.Links {
		if l.Kind == model.LinkFixes {
			out.PullRequest = l.Target
		}
		if l.Kind == model.LinkProposedBy {
			out.GeneratedFrom = l.Target
		}
		if l.Kind == model.LinkDependsOn {
			out.DependsOn = append(out.DependsOn, l.Target)
		}
		if l.Kind.Blocks() && !closed[l.Target] {
			out.BlockedBy = append(out.BlockedBy, l.Target)
		}
	}
	out.Blocked = len(out.BlockedBy) > 0
	return out
}

// commentFrom projects a model.Comment to its JSON shape. attachments is
// this one comment's own files, already narrowed by the caller (GetTask
// groups a task's AttachmentMetas by CommentID before calling this) --
// commentFrom stays free of the store the same way taskFrom does.
func commentFrom(c model.Comment, attachments []Attachment) Comment {
	out := Comment{
		ID:          c.ID,
		Author:      c.Author.Actor.ID,
		AuthorKind:  string(c.Author.Actor.Kind),
		Body:        c.Body,
		CreatedAt:   c.CreatedAt,
		Attachments: attachments,
	}
	if b := c.Author.OnBehalfOf; b != nil {
		out.OnBehalfOf = b.ID
	}
	return out
}

func attemptFrom(r model.Run) Attempt {
	return Attempt{
		Number:     r.Attempt,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		Outcome:    r.Outcome,
		Detail:     r.Detail,
	}
}

// --- handlers ------------------------------------------------------------
//
// Every handler below is a thin HTTP shim over a Client method: decode
// the request, call it, translate the result (or error) into a status
// code and a JSON body. The logic that actually reads or writes the store
// lives in client.go, once, so a non-HTTP caller (cmd/grain's CLI) gets
// identical behaviour by calling the same Client directly.

// configResponse is GET /api/config's JSON shape -- deliberately its own
// type rather than json.Marshal'ing model.Principal/model.RepoRef
// directly, since neither carries wire tags (they are not otherwise a
// wire format) and would round-trip as {"Kind":...,"ID":...} instead of
// the plain strings HTTPClient and the frontend both expect.
type configResponse struct {
	Actor         string       `json:"actor"`
	ActorKind     string       `json:"actorKind"`
	DefaultTarget string       `json:"defaultTarget,omitempty"`
	Capabilities  []Capability `json:"capabilities"`
	// RebootEnabled mirrors secretsResponse's own Enabled: whether
	// Config.Reboot is set, so the frontend can hide the reboot button
	// entirely rather than show one that can only ever 404.
	RebootEnabled bool `json:"rebootEnabled"`
	// TargetRepos mirrors Client.Config.TargetRepos -- the same list
	// CreateTask enforces a task's repo against -- so the frontend can
	// offer a dropdown of known repos on the task and schedule forms
	// instead of a bare text field (bwsalmon/agents#447). Empty means
	// unrestricted, same as everywhere else this field appears.
	TargetRepos []string `json:"targetRepos,omitempty"`
	// AutoMergeDegraded mirrors Config.AutoMergeDegraded's own doc
	// comment: true means this deployment's GitHub credential cannot
	// read check runs, so a submitted task's AutoMerge is set but will
	// never actually merge. The frontend uses this to warn on Submit
	// instead of a task looking stuck for no visible reason
	// (bwsalmon/agents#483).
	AutoMergeDegraded bool `json:"autoMergeDegraded,omitempty"`
	// ShowClosedByDefault mirrors model.Config.ShowClosedByDefault
	// (bwsalmon/agents#537), read straight from the store the same way
	// Settings itself does rather than cached on Client.Config -- unlike
	// TargetRepos, nothing else in this process needs it kept in sync
	// mid-run, so there is no setTargetRepos-style write-through to keep
	// current. TaskList.jsx seeds its own "Show closed tasks" toggle from
	// this the moment a list first renders, before Settings has ever been
	// opened this session.
	ShowClosedByDefault bool `json:"showClosedByDefault"`
	// ApprovedByDefault and AutoMergeByDefault mirror model.Config's own
	// fields of the same name (bwsalmon/agents#612), the same
	// read-straight-from-the-store-not-cached way ShowClosedByDefault
	// does. NewTaskOverlay.jsx seeds its "Queue immediately" and
	// "Auto-merge once checks pass" checkboxes from these the moment it
	// first renders, before Settings has ever been opened this session.
	//
	// A deployment with no stored config yet reports model.DefaultConfig's
	// own values -- both on -- rather than the zero value beside them, the
	// same reading of "no row yet" AgentFramework below takes.
	ApprovedByDefault  bool `json:"approvedByDefault"`
	AutoMergeByDefault bool `json:"autoMergeByDefault"`
	// ReconcilerDown mirrors Config.ReconcilerDown's own doc comment:
	// true means this deployment's reconcile loop has died and nothing is
	// dispatching or reconciling tasks, even though this same API is
	// still answering. The frontend uses this to show a standing banner
	// rather than an operator having to notice tasks have stopped moving
	// and go searching logs to learn why (bwsalmon/agents#576).
	ReconcilerDown bool `json:"reconcilerDown,omitempty"`
	// AgentFramework mirrors model.Config.AgentFramework -- this
	// deployment's default -- read from the store the same way
	// ShowClosedByDefault above is. NewTaskOverlay.jsx names it in the
	// "deployment default" option of its own per-task framework picker,
	// so whoever files a task can see which framework leaving that alone
	// actually means. Never empty: an unset stored value reads back as
	// model.AgentFrameworkAntigravity, the same defaulting ui.Settings
	// does.
	AgentFramework string `json:"agentFramework"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.tasks.Store.GetConfig(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	resp := configResponse{
		Actor:         s.tasks.Config.Actor.ID,
		ActorKind:     string(s.tasks.Config.Actor.Kind),
		Capabilities:  s.tasks.Config.Capabilities,
		RebootEnabled: s.tasks.Config.Reboot != nil,
		TargetRepos:   s.tasks.targetRepos(),
	}
	resp.AgentFramework = model.AgentFrameworkAntigravity
	// Same reading of "no row yet" the AgentFramework line above takes:
	// what a form should start as on a deployment that has never stored a
	// config is the built-in default, not the zero value beside it.
	def := model.DefaultConfig()
	resp.ApprovedByDefault, resp.AutoMergeByDefault = def.ApprovedByDefault, def.AutoMergeByDefault
	if cfg != nil {
		resp.ShowClosedByDefault = cfg.ShowClosedByDefault
		resp.ApprovedByDefault = cfg.ApprovedByDefault
		resp.AutoMergeByDefault = cfg.AutoMergeByDefault
		resp.AgentFramework = model.NormalizeAgentFramework(cfg.AgentFramework)
	}
	if s.tasks.Config.AutoMergeDegraded != nil {
		resp.AutoMergeDegraded = s.tasks.Config.AutoMergeDegraded()
	}
	if s.tasks.Config.ReconcilerDown != nil {
		resp.ReconcilerDown = s.tasks.Config.ReconcilerDown()
	}
	if s.tasks.Config.DefaultTarget != nil {
		resp.DefaultTarget = s.tasks.Config.DefaultTarget.String()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	detail, err := s.tasks.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// attemptTranscriptResponse is GET
// /api/tasks/{id}/attempts/{number}/transcript's whole body -- its own
// shape, not folded into Attempt, so a caller listing many attempts (the
// task detail fetch) never pays to carry every one's full transcript,
// the same reasoning logLinesResponse's own {lines} shape follows for
// GET /api/logs/{source} (bwsalmon/agents#446).
type attemptTranscriptResponse struct {
	Transcript string `json:"transcript"`
}

func (s *Server) handleGetAttemptTranscript(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("attempt number must be an integer"))
		return
	}
	transcript, err := s.tasks.AttemptTranscript(r.Context(), r.PathValue("id"), number)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attemptTranscriptResponse{Transcript: transcript})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	task, err := s.tasks.CreateTask(r.Context(), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// handleUpdateTask is the CLI's `grain update` (cmdUpdate) reached over
// HTTP now that the CLI is a Client (this package's own HTTPClient) over
// REST rather than a direct model.Store caller -- the one Client method
// with no route before this, since the frontend has never needed a
// general field editor (it drives capabilities, dependencies, approval
// and comments through their own narrower endpoints instead).
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	var req UpdateTaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	task, err := s.tasks.UpdateTask(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleReorder is TaskList.jsx's drag-and-drop drop handler
// (bwsalmon/agents#476): move every task named in the request to sit
// between whatever neighbours it names, in one call so a multi-select
// drag lands as a single atomic move rather than one race-prone request
// per task.
func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req ReorderRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.tasks.Reorder(r.Context(), req); err != nil {
		writeClientError(w, err)
		return
	}
	tasks, err := s.tasks.ListTasks(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

type setCapabilityRequest struct {
	ID     string `json:"id"`
	Attach bool   `json:"attach"`
}

func (s *Server) handleSetCapability(w http.ResponseWriter, r *http.Request) {
	var req setCapabilityRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.tasks.SetCapability(r.Context(), id, req.ID, req.Attach); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

type setDependencyRequest struct {
	ID     string `json:"id"`
	Attach bool   `json:"attach"`
}

// handleSetDependency attaches or detaches one depends-on link, the same
// attach/detach shape handleSetCapability already gives a checkbox --
// this is the picker's "add"/"remove" button, not a general link editor.
func (s *Server) handleSetDependency(w http.ResponseWriter, r *http.Request) {
	var req setDependencyRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.tasks.SetDependency(r.Context(), id, req.ID, req.Attach); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleApprove is the UI's own "approve button" docs/data-model.md's UI
// direction asks for.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Approve(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleSubmit is the UI's own "submit" button docs/data-model.md's UI
// direction asks for, alongside the approve button handleApprove already
// is: once a task's run has produced a pull request, this is what queues
// it for the merge queue's automatic conflict resolution and merging.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Submit(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

type addCommentRequest struct {
	Body        string             `json:"body"`
	Attachments []AttachmentUpload `json:"attachments"`
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var req addCommentRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.tasks.AddComment(r.Context(), id, req.Body, req.Attachments); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleGetAttachment serves one attachment's raw content -- the only
// place an attachment's bytes leave the store, since every other
// attachment-carrying response (TaskDetail, its comments) is metadata
// only (Attachment's own doc comment). Content-Disposition is "inline"
// rather than "attachment": an image is worth previewing directly, and
// nothing here can tell an image apart from a zip to treat them
// differently -- a browser that cannot render the content type falls back
// to downloading it either way.
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	attachmentID, err := strconv.ParseInt(r.PathValue("attachmentId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("attachment id must be an integer"))
		return
	}
	a, err := s.tasks.Attachment(r.Context(), id, attachmentID)
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.Header().Set("Content-Type", a.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", a.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Content)
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Close(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Reopen(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

// handleRetry is the UI's own "Retry" button (bwsalmon/agents#403): the
// one way a human clears a task's own failure streak once it has reached
// model.MaxConsecutiveFailures and task_state has stopped calling it
// 'queued' at all, since nothing else ever resets that count.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.Retry(r.Context(), id); err != nil {
		writeClientError(w, err)
		return
	}
	s.respondWithTask(w, r, id)
}

func (s *Server) respondWithTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.tasks.Task(r.Context(), id)
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// --- request/response plumbing -----------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeClientError maps a Client error to a status: 400 for a
// ValidationError (a mistake caught before the store was touched), 404
// for a task id with nothing behind it, and 500 otherwise.
//
// The fallback is 500 rather than the 502 this used to return. 502 was
// right when every failure came from an upstream GitHub call; the store
// is not upstream of this process in the same sense, and reporting a
// database error as "the gateway had trouble" would point an operator at
// the wrong thing.
func writeClientError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var snf *scheduleNotFoundError
	if errors.As(err, &snf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var tnf *templateNotFoundError
	if errors.As(err, &tnf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var qnf *qualificationRunNotFoundError
	if errors.As(err, &qnf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var rnf *releaseNotFoundError
	if errors.As(err, &rnf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var snf2 *suiteNotFoundError
	if errors.As(err, &snf2) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var srnf *suiteRunNotFoundError
	if errors.As(err, &srnf) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	// A conflict that survived the store's own retries: the change did not
	// land, and saying so plainly beats retrying forever or calling it a
	// server fault. It should be vanishingly rare -- it needs another
	// writer to win several times in a row.
	if errors.Is(err, model.ErrConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
