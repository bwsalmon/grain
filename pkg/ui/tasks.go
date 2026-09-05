package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/version"
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
	// SandboxCPUs, SandboxMemoryMB and SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) override the
	// deployment's default sandbox shape for this task's own dispatch
	// alone -- model.Task's own fields of the same name. Zero (the
	// default for all three) means "use the deployment default", omitted
	// from the JSON response the same way Base's own empty string is,
	// since "no override" is the common case and worth not cluttering
	// every task response with.
	SandboxCPUs     int `json:"sandboxCpus,omitempty"`
	SandboxMemoryMB int `json:"sandboxMemoryMb,omitempty"`
	SandboxDiskGB   int `json:"sandboxDiskGb,omitempty"`
	// AgentFramework is this task's own override of the deployment's
	// agent framework -- model.Task's own field of the same name.
	// Omitted, like the three above, for the common "no override" case, so
	// a frontend reading it back gets the same empty-means-default it
	// sent.
	AgentFramework string `json:"agentFramework,omitempty"`
	// PromptExtension is this task's own override of the standing
	// instructions its deployment and its repo would otherwise give its
	// run -- model.Task's own field of the same name (grain/task-114).
	// Omitted, like the fields above, for the common "no override" case,
	// so a frontend reading it back gets the same empty-means-default it
	// sent.
	PromptExtension string `json:"promptExtension,omitempty"`
	// ReviewTemplateID is the template a review of this task's own work
	// is filed from once it is done -- model.Task.ReviewTemplateID.
	// Omitted, like the overrides above, for the common case of a task
	// with no review attached.
	//
	// ReviewTemplateName is that template's own Name, resolved by the
	// caller (Client.ListTasks reads every template once for a whole
	// list; Client.Task reads the one), so a task pane can say which
	// review is attached without the frontend having to hold the whole
	// template list to look an id up in. Empty when the id names a
	// template that is no longer there, which is the one thing worth
	// telling a reader apart from "no review".
	ReviewTemplateID   string `json:"reviewTemplateId,omitempty"`
	ReviewTemplateName string `json:"reviewTemplateName,omitempty"`
	// ReviewTask is the ID of the task actually filed to review this one
	// (model.LinkReviewTask), empty until orchestrator.SyncReviews has
	// filed it. Paired with ReviewTemplateID it is what tells a reader
	// whether the review is still owed or already in flight -- and it is
	// what a task pane links to.
	ReviewTask    string   `json:"reviewTask,omitempty"`
	Capabilities  []string `json:"capabilities"`
	PullRequest   string   `json:"pullRequest,omitempty"`
	GeneratedFrom string   `json:"generatedFrom,omitempty"`
	// Stacked is true for a task filed automatically onto another task's
	// own branch and merged straight back into it once green -- a repair
	// of a broken pull request (model.ReasonFix) or a review of one
	// (model.ReasonReview). Paired with GeneratedFrom, it is what tells
	// the frontend
	// to nest a task under the one that generated it (bwsalmon/agents#378)
	// rather than list it as a separate task: unlike an ordinary
	// propose_task child, a stacked task is not new work, just a
	// continuation of the same change.
	Stacked bool `json:"stacked,omitempty"`
	// Review is true for the half of Stacked that is a review of another
	// task's work rather than a repair of its pull request
	// (model.ReasonReview, grain/task-284). Both nest under the task
	// they came from, which is what Stacked says; only this says which
	// of the two a nested task is, and a reader wants to know -- "a
	// second agent read this change" and "the merge queue patched a red
	// build" are not the same event.
	Review bool `json:"review,omitempty"`
	// Scheduled is true for a task a schedule filed automatically
	// (model.ReasonSchedule) -- a UI badge, the same treatment Stacked
	// already gives model.ReasonFix, so a task that appeared with nobody
	// having filed it by hand reads as expected rather than as a mystery.
	Scheduled bool `json:"scheduled,omitempty"`
	// SuiteRun is true for a task a suite run filed automatically
	// (model.ReasonSuite, bwsalmon/agents#642) -- Scheduled's own badge
	// treatment, so a pass of tasks that appeared with nobody filing
	// them by hand reads as expected rather than as a mystery.
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
	// attempt. It is what lets the frontend put a "Merge blocked" chip
	// beside the state badge of a task whose pull request is on the queue
	// in name only (ui/src/state.js's completionPhase).
	//
	// The other half of the distinction bwsalmon/agents#494 asked for is
	// no longer a chip: a task waiting on a human's Submit click has its
	// own State now (model.StateAwaitingSubmit), so the badge says it
	// rather than contradicting it.
	MergeQueueBlockedAt *time.Time `json:"mergeQueueBlockedAt,omitempty"`
	// Repairing is true for a task the merge queue has sent back to an
	// agent to repair its own pull request branch, and whose repair has
	// not finished (model.Observation.MergeQueueRepairAt,
	// orchestrator.repairInFlight). Such a task reads 'running' or
	// 'queued' like any other attempt, and from the state alone a person
	// watching cannot tell "still being written" from "written, merged
	// nowhere, and now being unstuck" -- so the frontend colours the
	// running mark differently for it (ui/src/components/StateDot.jsx).
	Repairing bool `json:"repairing,omitempty"`
	// Activity is what this task's live run is doing right now -- one
	// short phrase (model.Run.Activity), with ActivityAt the moment it
	// was written.
	//
	// Both are empty for every task that is not running, and for a running
	// task whose agent has not said anything yet: a run is under no
	// obligation to narrate itself, so their absence is the ordinary case
	// and never a sign of trouble. TaskList.jsx shows the phrase on the
	// row and leans on ActivityAt to say how long it has stood, since
	// "waiting for CI" ten seconds old and the same words an hour old mean
	// opposite things.
	//
	// It rides on the list shape rather than only on TaskDetail --
	// unlike Comments or Attempts -- because a list is exactly where it
	// earns its place: the question it answers is "what are my running
	// tasks doing?", asked of all of them at once.
	Activity   string     `json:"activity,omitempty"`
	ActivityAt *time.Time `json:"activityAt,omitempty"`
	// ActivityBySetup says the phrase is grain's own rather than the
	// run's: what the dispatch path is doing to get this run started --
	// building its sandbox, cloning its repo, minting its credentials --
	// over the stretch before its agent's first turn, which is the one
	// part of a run nothing could narrate before (orchestrator.setupNotes,
	// model.RunActivity.BySetup).
	//
	// It is carried rather than blurred because everything else that has
	// ever appeared in this field is something an agent wrote, and a
	// reader who has learnt to read the phrase as the agent's voice should
	// not have to guess which of the two is speaking. The frontend marks
	// such a phrase as grain's (TaskList.jsx, DetailOverlay.jsx); it is
	// never true for a phrase written through update_status, because
	// grain's own are cleared the moment the agent starts.
	ActivityBySetup bool `json:"activityBySetup,omitempty"`
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
	// PendingSecret is the credential a parked run asked a human to set
	// (mcp's request_secret, model.Observation.PendingSecret), resolved
	// into the secret and key a write addresses -- what tells the task
	// pane to offer a write-only box for it beside the reply box, and
	// nil for the overwhelming majority of tasks, which are parked on
	// nothing of the kind.
	//
	// A name and a location, never a value: nothing in this package can
	// read a stored secret back, and PUT /api/tasks/{id}/secret is the
	// one direction this travels (see task_secret.go).
	PendingSecret *CapabilitySecret `json:"pendingSecret,omitempty"`
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
//
// SetupNote is the last phrase grain stamped on this run while it was
// still getting it started -- "building a sandbox", "cloning
// acme/widgets", "minting the task's credentials" (orchestrator.
// setupNotes) -- and SetupNoteAt when it said it. It is set only for a
// run that never reached its agent (model.Run.AgentStartedAt is nil), and
// so is empty for the overwhelming majority of attempts: grain clears its
// last phrase in the same breath as handing the run over, and everything
// written after that is the agent's own.
//
// That narrowing is the point, not a limitation. A run that ends
// "setup-failed" gets a detail saying only that its sandbox could not be
// prepared, and the phrase it broke on is the one record of how far setup
// actually got -- which nothing showed a reader, because Store.
// TaskActivity reads live runs only, by design. What the *agent* last
// said through update_status is a different thing entirely, and is
// deliberately not carried here: "waiting for CI on the second push"
// beside a failed attempt reads as that attempt's diagnosis and is not
// one, and an attempt whose agent ran has its whole transcript one click
// away (AttemptTranscript). So only grain's own half travels, and the
// frontend can render it in grain's voice without having to ask whose
// sentence it is (DetailOverlay.jsx's attempt timeline).
type Attempt struct {
	Number      int        `json:"number"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	SetupNote   string     `json:"setupNote,omitempty"`
	SetupNoteAt *time.Time `json:"setupNoteAt,omitempty"`
}

// taskFrom projects a model.Task to its JSON shape. closed reports, for
// every task ID a blocking link might target, whether that task reads
// closed -- the caller resolves it (Client.ListTasks over the whole
// store in one query, Client.Task over just this task's own targets) so
// this function stays free of the store itself. activity is the same
// arrangement for the task's live run's own synopsis, resolved by the
// caller for the same reason and nil for a task with no live run or one
// whose run has said nothing. reviewTemplateName is the same again for
// the review attached to the task (Client.ListTasks reads every template
// once for a whole list; Client.Task reads the one it needs), empty for
// a task with no review and for one whose template has since gone.
func taskFrom(t model.Task, state model.State, closed map[string]bool, mergeQueueBlockedAt *time.Time,
	repairing bool, activity *model.RunActivity, reviewTemplateName string) Task {
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
		SandboxCPUs:         t.SandboxCPUs,
		SandboxMemoryMB:     t.SandboxMemoryMB,
		SandboxDiskGB:       t.SandboxDiskGB,
		AgentFramework:      t.AgentFramework,
		PromptExtension:     t.PromptExtension,
		ReviewTemplateID:    t.ReviewTemplateID,
		ReviewTemplateName:  reviewTemplateName,
		Capabilities:        []string{},
		Stacked:             t.Origin.Reason == model.ReasonFix || t.Origin.Reason == model.ReasonReview,
		Review:              t.Origin.Reason == model.ReasonReview,
		Scheduled:           t.Origin.Reason == model.ReasonSchedule,
		SuiteRun:            t.Origin.Reason == model.ReasonSuite,
		CreatedAt:           t.CreatedAt,
		MergeQueueBlockedAt: mergeQueueBlockedAt,
		Repairing:           repairing,
	}
	if activity != nil {
		out.Activity, out.ActivityAt = activity.Note, activity.At
		out.ActivityBySetup = activity.BySetup
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
		if l.Kind == model.LinkReviewTask {
			out.ReviewTask = l.Target
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
	out := Attempt{
		Number:     r.Attempt,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		Outcome:    r.Outcome,
		Detail:     r.Detail,
	}
	// Grain's own phrase travels; the agent's does not (Attempt's own doc
	// comment). AgentStartedAt is the same test model.RunActivity.BySetup
	// makes for a live run, asked of a finished one.
	if r.AgentStartedAt == nil {
		out.SetupNote, out.SetupNoteAt = r.Activity, r.ActivityAt
	}
	return out
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
	// EnvironmentName mirrors model.Config.EnvironmentName -- what this
	// deployment is called on screen -- read from the store the same way
	// ShowClosedByDefault above is. It rides on this response rather than
	// on GET /api/settings alone because the frontend shows it in the
	// sidebar and the tab title from the first paint, on every view, and
	// this is the one call App.jsx makes before rendering anything.
	//
	// omitempty: absent and empty both mean an unnamed deployment, and
	// unlike ui.Settings there is nothing here to merge a cleared value
	// over -- App.jsx replaces this response wholesale on every poll.
	EnvironmentName string `json:"environmentName,omitempty"`
	// TimeZone mirrors model.Config.TimeZone -- the IANA zone this
	// deployment keeps its wall clock in -- read from the store the same
	// way EnvironmentName above is, and never empty: a deployment with
	// no row yet, or a row written before the column existed, reports
	// model.DefaultTimeZone.
	//
	// It rides on this response rather than on GET /api/settings alone
	// for a stronger reason than the name above it: every timestamp the
	// frontend prints is formatted in this zone (ui/src/time.js), on
	// every view, from the first paint. Reading it from Settings would
	// mean every screen showed the browser's own zone until somebody
	// opened Settings -- which is the bug this setting exists to fix,
	// only intermittently.
	TimeZone string `json:"timeZone"`
	// Version is which build of grain is answering -- the commit this
	// binary was stamped with and when it was made (version.go, over
	// pkg/version). It rides here for the same reason EnvironmentName
	// above does: the sidebar shows it in every view, and this is the
	// call the frontend already makes before its first paint.
	//
	// Unlike every other field on this response it comes from the
	// process rather than from the store or from Config, and nil -- a
	// binary built with no VCS stamp at all -- means the sidebar simply
	// has nothing to print.
	Version *versionResponse `json:"version,omitempty"`
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
	// NewestFirst mirrors model.Config's own field of the same name --
	// which end of the backlog a new task joins -- read from the store
	// the same way the two above are. NewTaskOverlay.jsx seeds its
	// "Add to backlog" picker from it, so the form opens on the end the
	// last task added chose (ui.CreateTaskRequest.AtFront writes it back
	// on every filing that states one, grain/task-202) rather than on a
	// guess.
	//
	// Not omitempty, unlike the optional strings below: false is the
	// meaningful answer "the end of the backlog", not the absence of one,
	// and a picker that has to render one of two positions cannot tell an
	// absent field from a chosen false.
	NewestFirst bool `json:"newestFirst"`
	// ReconcilerDown mirrors Config.ReconcilerDown's own doc comment:
	// true means this deployment's reconcile loop has died and nothing is
	// dispatching or reconciling tasks, even though this same API is
	// still answering. The frontend uses this to show a standing banner
	// rather than an operator having to notice tasks have stopped moving
	// and go searching logs to learn why (bwsalmon/agents#576).
	ReconcilerDown bool `json:"reconcilerDown,omitempty"`
	// AgentPause mirrors Config.AgentPause's own doc comment: set when
	// this deployment's agent has said it has no budget left in the
	// current window, so nothing is being dispatched until that window
	// resets. It rides on this response, rather than only on
	// GET /api/pause beside it, for the reason ReconcilerDown above does
	// -- it drives a standing banner on every page, and this is the one
	// call the frontend already polls from every view (grain/task-132).
	//
	// nil for a deployment that is not paused as well as for one with no
	// gate wired: a banner has nothing to draw either way, and the
	// difference between them is what GET /api/pause exists to state.
	AgentPause *AgentPauseStatus `json:"agentPause,omitempty"`
	// AgentFramework mirrors model.Config.AgentFramework -- this
	// deployment's default -- read from the store the same way
	// ShowClosedByDefault above is. NewTaskOverlay.jsx names it in the
	// "deployment default" option of its own per-task framework picker,
	// so whoever files a task can see which framework leaving that alone
	// actually means. Never empty: an unset stored value reads back as
	// model.AgentFrameworkAntigravity, the same defaulting ui.Settings
	// does.
	AgentFramework string `json:"agentFramework"`
	// DefaultCapabilities mirrors model.Config's own field of the same
	// name, read from the store the same way ShowClosedByDefault above
	// is: the capability ids a task filed here starts out holding.
	// NewTaskOverlay.jsx seeds its capability picker from this, so the
	// boxes are already ticked when the form opens and whoever files the
	// task can untick one they do not want -- the form is where that
	// choice belongs, since the request it sends is the last word on
	// which capabilities the task gets (CreateTaskRequest.Capabilities).
	//
	// Only ever the form's starting state, like ApprovedByDefault and
	// AutoMergeByDefault above. What is filed is what the request names;
	// this is only what it names when nobody has said otherwise.
	DefaultCapabilities []string `json:"defaultCapabilities"`
	// RepoDefaultCapabilities is the per-repo layer of the same thing
	// (model.RepoConfig.DefaultCapabilities, grain/task-24): what each
	// repo adds to DefaultCapabilities above, keyed by "owner/name". Only
	// repos that add something appear -- Store.ListRepoConfigs keeps no
	// row for a repo that says nothing -- so an absent key and an empty
	// list mean the same thing here.
	//
	// Sent with the config rather than fetched per repo because the
	// new-task form needs it the moment the repo picker changes, on a
	// form that has not been submitted yet: this is one small map for a
	// deployment's whole repo list, and re-seeding the capability picker
	// from a round trip per keystroke would be a request for every
	// character typed into a repo field. The repos pane, which edits one
	// repo at a time and has somewhere to show a save failing, reads and
	// writes GET/PUT /api/repos/{owner}/{name}/capabilities instead.
	//
	// Filtered to what this build offers, the same as DefaultCapabilities
	// above and for the same reason.
	RepoDefaultCapabilities map[string][]string `json:"repoDefaultCapabilities,omitempty"`
	// PromptExtension mirrors model.Config.PromptExtension -- this
	// deployment's own standing instructions for every run -- read from
	// the store the same way AgentFramework above is.
	// NewTaskOverlay.jsx shows it beside its own per-task override box,
	// since that box *replaces* this text rather than adding to it
	// (model.Task.PromptExtension) and nobody can make that choice
	// sensibly without seeing what they would be replacing.
	//
	// Deployment-wide only, unlike RepoDefaultCapabilities above: the
	// per-repo layer is a paragraph of prose per repo rather than a short
	// list of ids, and this response is fetched on every poll by every
	// open tab. The repos pane reads a repo's own through GET
	// /api/repos/{owner}/{name}/prompt-extension, one repo at a time,
	// which is where the editing of it happens anyway --
	// ReposWithPromptExtension below is all this response says about
	// them.
	//
	// omitempty: absent and empty both mean a deployment that adds
	// nothing, and App.jsx replaces this response wholesale on every
	// poll, so there is nothing here for a cleared value to have to
	// overwrite.
	PromptExtension string `json:"promptExtension,omitempty"`
	// ReposWithPromptExtension names every repo that has standing
	// instructions of its own -- the names alone, not the text, for the
	// reason PromptExtension above gives.
	//
	// Names are enough for what the frontend needs it for, which is the
	// same job RepoDefaultCapabilities does for a repo whose only
	// presence here is its stored defaults: making the repo appear on the
	// repos page at all (state.js's repoRows). A repo can carry
	// configuration without being allow-listed and without any task
	// targeting it (SetRepoPromptExtension's own doc comment), and this
	// page is the only place that configuration can be read or edited --
	// so a repo missing from every other source has to come from
	// somewhere, or its text goes on reaching every run against it with
	// nowhere to see it.
	ReposWithPromptExtension []string `json:"reposWithPromptExtension,omitempty"`
	// ReposWithSetupCommand names every repo that has a setup command of
	// its own (model.RepoConfig.SetupCommand) -- names alone, for the
	// same two reasons as the field above: a shell command per repo is
	// not something every open tab needs on every poll, and what the
	// frontend needs the list for is making the repo appear on the repos
	// page at all (state.js's repoRows). A repo whose only presence here
	// is the command grain runs in its checkouts would otherwise be
	// reachable from nowhere.
	ReposWithSetupCommand []string `json:"reposWithSetupCommand,omitempty"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.tasks.Store.GetConfig(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	// Read before the capability listing below, which needs both to say
	// whether a row a human can tick would actually work on this
	// deployment -- the same two inputs the Settings pane's own
	// Capabilities tab reads.
	repoConfigs, err := s.tasks.Store.ListRepoConfigs(r.Context())
	if err != nil {
		writeClientError(w, err)
		return
	}
	resp := configResponse{
		Actor:         s.tasks.Config.Actor.ID,
		ActorKind:     string(s.tasks.Config.Actor.Kind),
		Capabilities:  s.tasks.capabilitiesWithReadiness(cfg, repoConfigs),
		RebootEnabled: s.tasks.Config.Reboot != nil,
		TargetRepos:   s.tasks.targetRepos(),
		Version:       versionResponseFrom(version.Get()),
	}
	resp.AgentFramework = model.AgentFrameworkAntigravity
	// Same reading of "no row yet" the AgentFramework line above takes:
	// what a form should start as on a deployment that has never stored a
	// config is the built-in default, not the zero value beside it.
	def := model.DefaultConfig()
	resp.ApprovedByDefault, resp.AutoMergeByDefault = def.ApprovedByDefault, def.AutoMergeByDefault
	resp.TimeZone = def.TimeZone
	if cfg != nil {
		resp.ShowClosedByDefault = cfg.ShowClosedByDefault
		resp.EnvironmentName = cfg.EnvironmentName
		resp.TimeZone = model.TimeZoneOrDefault(cfg.TimeZone)
		resp.ApprovedByDefault = cfg.ApprovedByDefault
		resp.AutoMergeByDefault = cfg.AutoMergeByDefault
		resp.NewestFirst = cfg.NewestFirst
		resp.AgentFramework = model.NormalizeAgentFramework(cfg.AgentFramework)
		resp.PromptExtension = cfg.PromptExtension
		// Filtered to what this build actually offers, the same way
		// (*Client).defaultCapabilities filters before granting -- the
		// form should tick what a task would really be filed with, and
		// a stored id no row answers to is neither.
		for _, id := range cfg.DefaultCapabilities {
			if _, ok := s.tasks.capabilityByID(id); ok {
				resp.DefaultCapabilities = append(resp.DefaultCapabilities, id)
			}
		}
	}
	// Outside the cfg != nil branch above: a repo can carry defaults of
	// its own on a deployment that has never saved a settings row, the
	// same case GetSettings reads this in both of its branches for.
	for _, rc := range repoConfigs {
		if rc.PromptExtension != "" {
			resp.ReposWithPromptExtension = append(resp.ReposWithPromptExtension, rc.Repo.String())
		}
		if rc.SetupCommand != "" {
			resp.ReposWithSetupCommand = append(resp.ReposWithSetupCommand, rc.Repo.String())
		}
		var ids []string
		for _, id := range rc.DefaultCapabilities {
			if _, ok := s.tasks.capabilityByID(id); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		if resp.RepoDefaultCapabilities == nil {
			resp.RepoDefaultCapabilities = make(map[string][]string, len(repoConfigs))
		}
		resp.RepoDefaultCapabilities[rc.Repo.String()] = ids
	}
	if s.tasks.Config.AutoMergeDegraded != nil {
		resp.AutoMergeDegraded = s.tasks.Config.AutoMergeDegraded()
	}
	if s.tasks.Config.ReconcilerDown != nil {
		resp.ReconcilerDown = s.tasks.Config.ReconcilerDown()
	}
	if status, ok := s.tasks.AgentPause(); ok && status.Paused {
		resp.AgentPause = &status
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

// handleGetTaskPrompt serves the prompt a task's own agent was handed --
// its own route rather than a field on TaskDetail, the same reasoning
// attemptTranscriptResponse gives for the transcript: a prompt runs to
// thousands of words, and the list every task detail fetch already pays
// for should not carry one per task for the sake of a pane most readers
// never open.
//
// A task that has no recorded prompt is a 200 with an empty one, not a
// 404: the task exists and the honest answer is "nothing has been
// dispatched for it yet" (Client.TaskPrompt), which the frontend renders
// as such. Only an unknown task id 404s.
func (s *Server) handleGetTaskPrompt(w http.ResponseWriter, r *http.Request) {
	prompt, err := s.tasks.TaskPrompt(r.Context(), r.PathValue("id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prompt)
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

// handleWithdrawApproval is handleApprove's own undo: the button that
// takes a queued task back out of the queue and leaves it a proposal
// again, without closing it.
func (s *Server) handleWithdrawApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.tasks.WithdrawApproval(r.Context(), id); err != nil {
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

// closeRequest is the body of POST /api/tasks/{id}/close: the choice
// made at the moment of closing, and nothing else. See CloseOptions,
// whose one field this is.
type closeRequest struct {
	ClosePullRequest bool `json:"close_pull_request"`
}

// handleClose closes a task, and closes its pull request too if -- and
// only if -- the request said to.
//
// The flag is read out of the request body rather than a query
// parameter, and defaults to false on a body that does not mention it,
// which is what keeps every caller that predates it (the CLI without its
// flag, `curl -X POST`, the batch Close button) closing tasks and
// nothing else. An empty body is one of those: this is a POST that has
// never needed one.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req closeRequest
	if !readOptionalJSON(w, r, &req) {
		return
	}
	if err := s.tasks.Close(r.Context(), id, CloseOptions{ClosePullRequest: req.ClosePullRequest}); err != nil {
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

// readOptionalJSON is readJSON for a route whose body is optional: no
// body at all leaves v at its zero value and succeeds, while a body that
// is there and malformed is still the caller's mistake and still a 400.
//
// It exists for POSTs that acquired a field after callers already
// existed without one -- see handleClose. io.EOF is precisely "there was
// nothing to decode", and is the only error swallowed here.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
