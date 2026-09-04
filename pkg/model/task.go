// Package model is the grain task model: the entities, their derivations,
// and a store for them.
//
// docs/data-model.md is the reasoning; this is that document's decided
// shape in code. Four of its decisions are load-bearing here, and each is
// why something is absent rather than present:
//
//   - TaskState is not a field. It is derived from approval plus grain's
//     own observations, so Task has no State and no StateSince. StateOf
//     computes it, and schema.go computes the same thing as a SQL view —
//     deliberately twice, with state_test.go holding the two to agreeing.
//   - Approval is an Attribution, not a bool, so "who approved this, and
//     did they do it directly?" is answerable from the task rather than
//     only from an audit log.
//   - Landing state is decided by the creating actor, never the reason —
//     see LandsQueued. A new OriginReason therefore cannot queue itself.
//   - Blocked is not a state. It is derived from links, and a blocked
//     task is still queued.
//
// Nothing in this file imports a database. SQLite is one representation
// of these types and GitHub labels are another; neither belongs in the
// types.
package model

import (
	"fmt"
	"strings"
	"time"
)

// --- principals ------------------------------------------------------

// PrincipalKind is who acted. GitHub knows one entity, the user; grain
// has three, and collapsing them is what forces the trust gate to
// authenticate by looking for a signature substring in a comment body.
type PrincipalKind string

const (
	PrincipalAutomation PrincipalKind = "automation" // the controller loop
	PrincipalAgent      PrincipalKind = "agent"      // one dispatched run
	PrincipalHuman      PrincipalKind = "human"      // a person
)

func (k PrincipalKind) Valid() bool {
	switch k {
	case PrincipalAutomation, PrincipalAgent, PrincipalHuman:
		return true
	}
	return false
}

// Principal names an actor. ID is a GitHub login for a human, a Run.ID
// for an agent, the deployment name for automation — opaque here on
// purpose, since this model records which principal acted and never
// resolves one to an account.
type Principal struct {
	Kind PrincipalKind
	ID   string
}

// Attribution is who performed an action and whose output they relayed.
//
// OnBehalfOf is what a signature marker gestures at with one bit. Grain
// relaying an agent's question is (automation, on behalf of agent); grain
// restarting a task because a human commented is (automation, on behalf
// of human); a human acting directly is (human, nil).
type Attribution struct {
	Actor      Principal
	OnBehalfOf *Principal
}

// --- origin ----------------------------------------------------------

type OriginReason string

const (
	ReasonDirect        OriginReason = "direct"        // somebody just filed it
	ReasonSchedule      OriginReason = "schedule"      // a scheduled job fired
	ReasonFix           OriginReason = "fix"           // filed for a broken PR
	ReasonReview        OriginReason = "review"        // review threads asked for it
	ReasonProposal      OriginReason = "proposal"      // an agent or parent proposed it
	ReasonQualification OriginReason = "qualification" // a release candidate's qualification plan fired it
	ReasonSuite         OriginReason = "suite"         // a suite run fired it (bwsalmon/agents#642)
)

func (r OriginReason) Valid() bool {
	switch r {
	case ReasonDirect, ReasonSchedule, ReasonFix, ReasonReview, ReasonProposal, ReasonQualification, ReasonSuite:
		return true
	}
	return false
}

// Origin is two orthogonal facts kept together: who created a task, and
// why. An earlier design fused them into one enum, which hid that two of
// its cases were identical in who and differed only in why.
type Origin struct {
	Attribution Attribution
	Reason      OriginReason
}

// LandsQueued reports whether a task with this origin starts approved.
//
// The actor decides; the reason does not. A sixth OriginReason added
// later cannot queue itself, because reasons are never consulted — the
// trust gate as code rather than as a convention each filing path
// remembers.
func LandsQueued(o Origin) bool {
	return o.Attribution.Actor.Kind == PrincipalHuman
}

// --- repos and folders -----------------------------------------------

type RepoRef struct {
	Owner string
	Name  string
}

func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

// ParseRepo reads the one written form of a repository: owner/name.
//
// It also accepts the two shapes an operator most often has in the
// clipboard instead -- a browser URL (https://github.com/owner/name,
// with or without a trailing .git or /) and an SSH remote
// (git@github.com:owner/name.git) -- and normalises both to owner/name,
// because the alternative was worse than either accepting or rejecting
// them: a bare strings.Cut on the first "/" took
// "https://github.com/owner/name" as owner "https:", name
// "/github.com/owner/name", stored it, and left the task to fail much
// later at clone time with an error naming neither the paste nor the
// field it went into.
//
// Everything else is refused here rather than downstream. What comes out
// of this function is used to build clone URLs, GitHub API paths and
// branch refs, so a segment carrying a slash, a space, a colon or a
// control character is not a repository grain can act on however it was
// typed.
func ParseRepo(text string) (RepoRef, error) {
	trimmed := strings.TrimSpace(text)
	spec := trimmed
	// scp-style SSH remotes: everything up to the colon is the host.
	if at := strings.Index(spec, "@"); at >= 0 && !strings.Contains(spec, "://") {
		if colon := strings.Index(spec[at:], ":"); colon >= 0 {
			spec = spec[at+colon+1:]
		}
	}
	if scheme := strings.Index(spec, "://"); scheme >= 0 {
		spec = spec[scheme+len("://"):]
		// Drop the host (and any userinfo already folded into it), which
		// is everything up to the first slash.
		if slash := strings.Index(spec, "/"); slash >= 0 {
			spec = spec[slash+1:]
		} else {
			spec = ""
		}
	}
	spec = strings.TrimSuffix(strings.TrimSuffix(strings.Trim(spec, "/"), ".git"), "/")

	owner, name, ok := strings.Cut(spec, "/")
	if !ok || !validRepoSegment(owner) || !validRepoSegment(name) {
		return RepoRef{}, fmt.Errorf("repo must be owner/name, got %q", trimmed)
	}
	return RepoRef{Owner: owner, Name: name}, nil
}

// validRepoSegment reports whether one half of an owner/name pair is a
// segment grain can safely put in a URL or a ref. GitHub is stricter
// than this (its own owner names are alphanumerics and hyphens); the
// point here is not to re-implement its rules but to refuse anything
// that would change the *shape* of a URL built from it, plus the two
// path segments that would escape it.
func validRepoSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '?', '#', '@', '[', ']', '%', '~', '^':
			return false
		}
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

type RepoBinding string

const (
	BindingDirective RepoBinding = "directive" // named explicitly; pinned
	BindingDefault   RepoBinding = "default"   // the deployment default
	BindingScratch   RepoBinding = "scratch"   // resolved at assignment
	BindingInherited RepoBinding = "inherited" // a sub-task's parent target
)

// FolderRef is a node in the containment tree, as a path from the root.
// Around (a group of repos) and inside (a path within one) are the same
// kind of node, which is why one type covers both.
type FolderRef struct {
	Path []string
}

func (f FolderRef) String() string { return strings.Join(f.Path, "/") }

func ParseFolder(text string) *FolderRef {
	if text == "" {
		return nil
	}
	return &FolderRef{Path: strings.Split(text, "/")}
}

// --- capabilities ----------------------------------------------------

type GrantSource string

const (
	GrantByLabel     GrantSource = "label"     // a human applied it
	GrantByDirective GrantSource = "directive" // a trusted author wrote it
	GrantByFolder    GrantSource = "folder"    // a folder's offers granted it
	GrantByGrain     GrantSource = "grain"     // grain applied it to itself
	// GrantByDefault records a grant a task was filed with because it was
	// listed as a default -- by the deployment
	// (Config.DefaultCapabilities) or by the repo the task targets
	// (RepoConfig.DefaultCapabilities), unioned at creation. Nobody
	// ticked it for this task in particular; it is what filing a task
	// here, against that repo, starts out with (ui.CreateTask).
	//
	// One source for both layers, deliberately: which of the two attached
	// it is a question the two panes that own them answer, and nothing
	// about a task changes with the answer.
	//
	// It is provenance only, exactly like the four above: nothing reads
	// Via to decide what a grant lets a task do, and a default-sourced
	// grant is in every other way an ordinary one -- it sits on the task,
	// shows in its capability list, is detached through the same picker,
	// and fails the same way at ResolveGrants/MaterializeGrants if the
	// capability behind it is misconfigured. That is deliberate: a
	// deployment-wide grant applied at dispatch instead would have no
	// task to be visible on and no way to be taken off one.
	GrantByDefault GrantSource = "default"
)

type Provision string

const (
	ProvisionMint   Provision = "mint"   // mints a lease from a credential
	ProvisionSelect Provision = "select" // names which credential to use
	ProvisionGrant  Provision = "grant"  // needs no credential at all
)

// CredentialRef is a credential name. Never material: resolving this to a
// token happens on the controller at the moment of use, and the result
// reaches neither this model, the store, a declaration, nor a UI.
type CredentialRef struct{ Name string }

// Grant records how this task came by a capability, which a capability's
// own definition cannot say: the same capability can be label-requested
// on one task and folder-inherited on another.
type Grant struct {
	Capability string
	Via        GrantSource
	Folder     *FolderRef
}

// GrantsSubsetOf reports whether every capability in grants also appears
// in allowed, comparing by Capability name alone -- Via and Folder record
// how a grant was come by, not what it lets a task do, so two grants for
// the same capability compare equal here regardless of either.
//
// This is the guard docs/data-model.md's "sub-tasks are tasks" section
// asks for before relaxing any trust gate for a proposed child: a task
// that asks for nothing beyond what its parent already held cannot turn
// one human grant into a wider one just by being proposed.
func GrantsSubsetOf(grants, allowed []Grant) bool {
	have := make(map[string]bool, len(allowed))
	for _, g := range allowed {
		have[g.Capability] = true
	}
	for _, g := range grants {
		if !have[g.Capability] {
			return false
		}
	}
	return true
}

// GitCredentialCapabilityPrefix marks a capability id as a per-task git
// credential override: the named credential (gitproxy's
// CredentialSet.Get) a sandbox's git proxy requests should use in place
// of the owner/repo ladder, for a scope the ladder's own credentials
// deliberately withhold (docs/design.md, "Scopes to withhold" --
// `workflow`, most notably).
//
// One id per named credential a deployment has configured beyond its
// default one, which is what makes each of those tokens a capability of
// its own -- offered by ui.GitHubTokenCapabilities, provided by
// pkg/capability/githubtoken, and attached to a task like any other.
const GitCredentialCapabilityPrefix = "github-credential:"

// GitCredentialCapability is the capability id standing for "use the
// GitHub token named name instead of this deployment's default one".
func GitCredentialCapability(name string) string {
	return GitCredentialCapabilityPrefix + name
}

// GitCredentialName is the credential a GitCredentialCapability id
// names, and false for any other capability id.
func GitCredentialName(capability string) (name string, ok bool) {
	name, ok = strings.CutPrefix(capability, GitCredentialCapabilityPrefix)
	return name, ok && name != ""
}

// GitCredentialGrant is the Grant attaching one of those capabilities to
// a task produces. Via is GrantByLabel: it is something a human turns on
// per task, the same trust tier applying the trigger label itself relies
// on, so this opens no new gate -- the same reasoning bwsalmon/agents#52
// gave for the `grain-github-<name>` label this started as. Unlike
// grain/proxy's SandboxCredentialOverrides, it needs no storage or
// dispatch/release lifecycle of its own: it lives and dies with the task
// the same as every other Grant.
func GitCredentialGrant(name string) Grant {
	return Grant{Capability: GitCredentialCapability(name), Via: GrantByLabel}
}

// gitCredentialOverride returns the credential name a GitCredentialGrant
// among grants asks for, if any.
//
// A task holding more than one of these gets the first in grants' own
// order -- which, coming from Store.GitCredentialOverride, is the
// capability id sorted ascending, so the same task resolves to the same
// token on every request rather than to whichever row was read first.
// Picking a winner rather than refusing is deliberate: two named tokens
// on one task is an odd thing to ask for, not a dangerous one (both are
// credentials this deployment already trusts a task with), and failing
// the run over it would be a worse answer than quietly using one of
// them.
func gitCredentialOverride(grants []Grant) (name string, ok bool) {
	for _, g := range grants {
		if n, isOverride := GitCredentialName(g.Capability); isOverride {
			return n, true
		}
	}
	return "", false
}

// Lease is something minted for a task that must be given back.
//
// MintedBy turns revocation and rotation from control flow into data:
// releasing asks the lease what to call, and "which live leases came from
// the credential I am about to rotate?" becomes a query.
type Lease struct {
	Capability string
	Resource   string
	MintedBy   CredentialRef
	IssuedAt   time.Time
	ExpiresAt  *time.Time
}

// Expired reports whether a lease is past its own expiry or past an
// unconditional backstop. The backstop exists because materialisation has
// a window that cannot be closed: a failure between minting and recording
// leaks a credential nothing knows to revoke.
func (l Lease) Expired(now time.Time, maxAge time.Duration) bool {
	if l.ExpiresAt != nil && !now.Before(*l.ExpiresAt) {
		return true
	}
	if maxAge > 0 && now.Sub(l.IssuedAt) >= maxAge {
		return true
	}
	return false
}

// --- links -----------------------------------------------------------

type LinkKind string

const (
	LinkDependsOn  LinkKind = "depends-on"  // blocks dispatch
	LinkChildOf    LinkKind = "child-of"    // blocks the parent
	LinkFixes      LinkKind = "fixes"       // -> a pull request
	LinkMergeWith  LinkKind = "merge-with"  // blocks the merge, not the run
	LinkAddresses  LinkKind = "addresses"   // -> a review thread
	LinkProposedBy LinkKind = "proposed-by" // provenance only
	// LinkFixTask records the task the merge queue automatically filed to
	// repair this task's own PR (-> the fix task's ID), so a later cycle
	// knows one is already in flight rather than filing a second, and can
	// tell whether it has finished. It does not block dispatch -- the
	// fix task runs on its own the moment dispatch.Cycle sees it ready,
	// independent of this task's own state.
	LinkFixTask LinkKind = "fix-task"
)

// Blocks reports whether a link holds a task back from dispatch.
//
// LinkMergeWith is deliberately absent: it gates merging, not running,
// which is what lets the members of a coordinated cross-repo change be
// worked in parallel.
func (k LinkKind) Blocks() bool {
	return k == LinkDependsOn || k == LinkChildOf
}

type Link struct {
	Kind LinkKind
	// A task ID for depends-on/child-of/proposed-by; a pull request or
	// review thread reference for fixes/addresses.
	Target string
}

// --- tasks -----------------------------------------------------------

type Intent string

const (
	IntentImplement Intent = "implement" // fresh branch -> a new PR
	IntentContinue  Intent = "continue"  // more commits on a branch
	IntentReview    Intent = "review"    // post a review, push nothing
	IntentAnalyze   Intent = "analyze"   // answer in a comment
)

type State string

const (
	StateProposed      State = "proposed"
	StateQueued        State = "queued"
	StateRunning       State = "running"
	StateAwaitingReply State = "awaiting_reply"
	// StateFailed means MaxConsecutiveFailures runs in a row ended
	// without succeeding, and none of them has succeeded since -- see
	// StateOf and task_streak (schema.go). Unlike the other states, a
	// task does not leave this one on its own: task_ready only ever
	// selects 'queued', so nothing here retries automatically, and a
	// human has to set Observation.RetryRequestedAt (Store.Retry) before
	// it is eligible for another attempt.
	StateFailed State = "failed"
	// StateAwaitingSubmit is a task whose run is over and whose pull
	// request nobody has submitted: it has a LinkFixes pull request and
	// AutoMerge is not set, so the merge queue is not driving it and
	// nothing will ever land it on its own. Like StateAwaitingReply, and
	// unlike every other post-run state, the task does not leave this one
	// by itself -- somebody has to click Submit (ui.Client.Submit, which
	// is what sets AutoMerge) or close it.
	//
	// It was a chip beside the state badge before it was a state
	// (ui/src/state.js's completionPhase, bwsalmon/agents#494), which
	// meant the badge itself said "Queued for merge" about a task that
	// was on no queue at all -- the one reading that is never true of it.
	// A state of its own is what makes the wait countable: it gets its
	// own sidebar entry and its own filter, so "what is sitting here
	// waiting on me?" is a question the task list can answer.
	StateAwaitingSubmit State = "awaiting_submit"
	// StateCompleted is a task whose run is over and which needs nobody:
	// its pull request is on the merge queue (AutoMerge set), or it
	// produced no pull request at all. StateOf holds it here until that
	// pull request merges or the task is closed.
	StateCompleted State = "completed"
	StateClosed    State = "closed"
)

// Task is the declared half: what a human, or grain proposing, asked for.
// No State field and no StateSince — see StateOf.
type Task struct {
	ID     string
	Intent Intent
	Origin Origin
	Title  string
	Body   string

	// nil means not approved, which is what makes a task proposed.
	Approval *Attribution
	// ApprovedAt is when Approval was set, display-only -- nothing in
	// StateOf or task_state reads it, the same way CreatedAt decides
	// nothing about a task's state. It exists for Transitions, which needs
	// a "queued since" timestamp Approval's own Attribution has no room
	// for. nil alongside a non-nil Approval means a task approved before
	// this field existed, not that it was never approved.
	ApprovedAt *time.Time

	Target  *RepoRef // the one write target
	Binding RepoBinding
	Base    string
	Folder  *FolderRef
	Reads   []RepoRef // read-only. Grant nothing.

	Grants []Grant
	Links  []Link
	Tags   []string

	AutoMerge bool
	// Interactive marks a task filed for a live back-and-forth with
	// whoever created it, rather than a change handed off to run
	// unattended (bwsalmon/agents#539). It changes nothing about how a
	// dispatched run is driven -- a comment posted while one is live
	// already reaches the agent through the same channel every task's
	// conversation does (agent.RunConfig.Addenda, orchestrator.
	// addendaPoller) -- only how the task is prioritised and presented:
	// ui.Client.CreateTask files it ahead of the ordinary backlog, the
	// same way Config.NewestFirst already can, and the frontend opens
	// its chat view immediately after filing it instead of returning to
	// the task list.
	Interactive bool
	// SandboxCPUs, SandboxMemoryMB and SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) override
	// Config.SandboxCPUs/SandboxMemoryMB/SandboxDiskGB for this task's
	// own dispatch only -- the per-job escape hatch alongside the
	// deployment-wide setting, for a task that is known ahead of time to
	// need more (or less) than the default shape, e.g. a build-heavy repo
	// whose checkout and toolchain do not fit the guest image's own disk,
	// or a task deliberately run on a constrained VM to reproduce a
	// memory-pressure bug. Zero, the default for all three, means "use
	// the deployment default" -- the same "zero means unset" contract
	// Config.SandboxCPUs/SandboxMemoryMB/SandboxDiskGB itself uses,
	// chosen so a task created before these fields existed reads back as
	// unset rather than as an explicit "shrink this VM to nothing."
	//
	// Applied by orchestrator.runOne, once per dispatch, immediately
	// before the sandbox is handed to the run: a slot's sandbox is
	// otherwise sized from orchestrator.KonturConfig's own
	// deployment-wide default, so a task asking for a different shape
	// overrides that default for its own sandbox -- per dimension, at the
	// moment the sandbox is created, which is the only moment its size is
	// decided now that one is built per run (orchestrator.Shape, passed to
	// Sandboxes.Acquire). Only orchestrator.KonturSandboxes can honour it
	// -- a task with any of the three set,
	// dispatched onto the default orchestrator.HostSandboxes backend
	// (no VM to resize), fails that dispatch outright rather than
	// silently running at whatever shape the host itself happens to be,
	// the same "refuse rather than silently do something else" choice
	// runOne already makes for a task that requests a capability with no
	// local directory to place it in.
	SandboxCPUs     int
	SandboxMemoryMB int
	SandboxDiskGB   int
	// AgentFramework overrides Config.AgentFramework for this task's own
	// dispatch only -- the per-task escape hatch alongside the
	// deployment-wide default, for a task better suited to one framework
	// than the others (a change the `claude` CLI's own tooling handles
	// well, say, filed on a deployment that runs agent/antigravity by
	// default). Empty, the default, means "use the deployment default",
	// the same "zero means unset" contract SandboxCPUs/SandboxMemoryMB
	// above already use -- so a task created before this field existed
	// reads back as deferring to the deployment rather than as an
	// explicit choice of a framework that did not exist for it.
	//
	// Its vocabulary is Config.AgentFramework's own
	// (model.AgentFrameworks(), with the legacy
	// "gemini" spelling folded in by NormalizeAgentFrameworkName rather
	// than NormalizeAgentFramework, so that "" keeps meaning "no
	// override" here); ui.CreateTask
	// validates it the same way ui.UpdateSettings validates the
	// deployment-wide one, rather than the schema (schema.go's own doc
	// comment on why an enum is TEXT here). Applied by
	// orchestrator.runOne, once per dispatch, when it asks Deps.Framework
	// for the agent.Framework this run is driven by.
	AgentFramework string
	// PromptExtension overrides the deployment's and the target repo's
	// standing instructions for this task's own dispatch alone
	// (grain/task-114) -- prompt_extension.go's own doc comment has what
	// those are and why this layer replaces them rather than adding to
	// them. Empty, the default, means "no override", the same "zero means
	// unset" contract AgentFramework and SandboxCPUs above already use,
	// so a task created before this field existed is told exactly what
	// the deployment and its repo say.
	//
	// Read at dispatch (orchestrator.RunDispatch, through
	// model.PromptExtensionFor), not at creation, which is what makes an
	// edit to a queued task's own override reach the run it eventually
	// gets.
	PromptExtension string
	CreatedAt       *time.Time

	// OrderKey is this task's position in the backlog -- Store.Ready
	// dispatches ascending, so the task with the smallest OrderKey among
	// everything queued runs next (bwsalmon/agents#476). It is never a
	// timestamp: Store.OrderKeyForNewTask assigns a fresh one a fixed
	// step past whichever extreme is currently in play, and Store.Reorder
	// (a drag-and-drop move) rewrites it directly, so two tasks created
	// or moved in the same instant still compare distinctly.
	//
	// It is also where grain writes down its own ordering, rather than
	// keeping a second one to itself: Store.MoveToFrontOfBacklog puts the
	// tasks waiting on a repo's merge queue at the front of the backlog in
	// the order they will land, and orchestrator.fileFixTask files a
	// repair at the very head of it. Everything that decides what happens
	// next -- Ready, ListTasks, orchestrator.queueOrder -- then reads that
	// one column, which is the one a human can see and drag.
	OrderKey float64
}

// Observation is the half grain writes: what it has seen happen.
//
// Separate from Task because they answer to different records, and
// separate in the schema for the same reason — which is what would let a
// declaration change be branched and reviewed while observations keep
// landing on the trunk.
type Observation struct {
	TaskID      string
	ClosedAt    *time.Time
	CompletedAt *time.Time
	// Baselines, not beliefs: the highest comment id seen, against which
	// a fresh read is compared. Losing one degrades rather than corrupts.
	PendingQuestionCommentID *int64
	BaselineCommentID        *int64
	// PendingSecret is the credential a parked run asked a human to set
	// -- mcp's request_secret escape hatch, relayed by
	// orchestrator.ProcessResult exactly the way a question is. It holds
	// a *name* in the form secrets.Store.Resolve takes ("stripe-api-key",
	// or "github-app/app-id"), never material: docs/data-model.md's "no
	// secret store in the model" is about values, and this is the same
	// kind of pointer a CredentialRef already is.
	//
	// It is set alongside PendingQuestionCommentID rather than instead of
	// it, because the parking is the same parking: the task sits in
	// awaiting_reply, out of task_ready, until a human acts. What this
	// field adds is *what* to offer them -- a UI holding it renders a
	// write-only box addressed at that name (ui.TaskDetail.PendingSecret)
	// instead of only a reply box, and the value it takes goes straight
	// into the secret store, never through the conversation and never
	// back to the run that asked.
	//
	// Cleared when the secret is set (ui.Client.SetPendingSecret) and
	// when a human replies in words instead (ui.Client.AddComment): both
	// un-park the task, and an input left offered on a task nobody is
	// waiting to hear from would be an offer to write a value nothing
	// asked for.
	PendingSecret string
	// MergeQueueBlockedAt is set once the merge queue has stopped driving
	// this task, for any of the three reasons it ever does: the automatic
	// fix it filed ran and closed and the PR is still conflicted or
	// failing, or that fix never finished at all within the time a fix is
	// given (orchestrator.defaultFixTaskDeadline), or the PR's checks
	// stayed unfinished for longer than the queue is willing to wait on
	// CI that may never report
	// (orchestrator.defaultCheckStallDeadline). The task's own thread
	// says which -- see orchestrator.SyncPullRequests. Either way
	// it no longer counts as any repo's queue head (so a stuck PR cannot
	// block the ones behind it) and gets no automatic fix from here on,
	// but it is still merged the moment it reads clean, the same as a fix
	// task itself, in case a human pushes the fix by hand or the checks
	// it was waiting on turn up green after all.
	MergeQueueBlockedAt *time.Time
	// MergeQueueRefreshedAt is set the cycle the merge queue tried to
	// bring this task's pull request branch up to date with its base --
	// whether or not that merge landed, and whether or not it helped. It
	// is what stops a head whose refresh went red from being refreshed
	// again on the next cycle, the same one-attempt-then-a-person policy
	// MergeQueueBlockedAt above already records for the fix task the
	// queue files (orchestrator.refreshStaleHead).
	//
	// Persisted rather than kept in memory, unlike the two CI clocks
	// orchestrator.sync.go runs: losing one of those costs another window
	// of waiting, where losing this one costs a repeated *write to
	// GitHub*, so a process restarting in a loop would merge in a loop.
	MergeQueueRefreshedAt *time.Time
	ObservedAt            *time.Time
	// RetryRequestedAt is a human's "clear the failure streak and let it
	// try again" signal (Store.Retry) -- the only way a task stuck in
	// StateFailed becomes dispatchable again, since nothing else ever
	// resets task_streak's own count once it reaches MaxConsecutiveFailures.
	// It also bounds task_streak going forward: a failed run only counts
	// toward the streak if it started after the later of this and the
	// task's last succeeded run, so asking for a retry and then failing
	// again does not instantly re-cap the task on the very next attempt.
	RetryRequestedAt *time.Time
	// PrOpenedAt, PrMergedAt and PrClosedAt are the task's own tracked
	// pull request's history (LinkFixes), off github.PullRequestDetail's
	// own CreatedAt/MergedAt -- what lets a task's timeline show "PR
	// opened"/"PR merged"/"PR closed unmerged" (bwsalmon/agents#493), none
	// of which TrackedPullRequest itself can answer since that type is
	// deliberately never stored (its own doc comment). Unlike Health,
	// which is re-read every cycle because it can still change, these are
	// each a fact about one moment in the past that orchestrator.
	// SyncPullRequests writes once, the first cycle it observes each one,
	// and never revisits.
	//
	// PrMergedAt and PrClosedAt are mutually exclusive -- a pull request
	// either merged or it did not -- and distinct from ClosedAt above:
	// ClosedAt is this task's own closure, set on a task with no pull
	// request at all just as often as on one whose PR closed, where
	// PrClosedAt means specifically "the pull request closed without
	// merging."
	PrOpenedAt *time.Time
	PrMergedAt *time.Time
	PrClosedAt *time.Time
}

// Run is one attempt. A live run is a Run with no FinishedAt.
type Run struct {
	ID     string
	TaskID string
	// Sandbox is the sandbox this run was given -- the kontur VM name, or
	// the host directory's own name. It used to sit alongside a Slot, the
	// concurrency unit a long-lived sandbox was reused under, the two
	// being equal in practice and distinct only in principle. A sandbox
	// created and destroyed with the run has no such unit to belong to,
	// so only this half is left, and it names something that exists for
	// exactly as long as the run does.
	//
	// It is written after the row is: dispatch records a run before any
	// sandbox exists for it, and orchestrator fills this in via
	// SetRunSandbox once one has actually been acquired. A live run with
	// an empty Sandbox is one whose sandbox is still being built -- which
	// gitproxy reads, correctly, as a sandbox identity that authorizes
	// nothing (Store.GitScope).
	Sandbox    string
	Unit       string
	Attempt    int
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    string
	// Detail is a short, human-readable explanation of how Outcome was
	// reached -- the agent framework's own error text ("exceeded max
	// turns (20) without a final answer"), a tool error, or
	// ProcessResult's own "finished without pushing, asking, or
	// commenting" -- recorded so an operator (or the person who filed the
	// task) can see why a run failed from `grain get` without reading
	// graind's own stdout, which per README's security design is not
	// necessarily somewhere they can reach at all.
	//
	// A succeeded run fills it in too, with the tools it called and how
	// often (orchestrator.outcomeOf). "How Outcome was reached" is the
	// honest reading of the field, not "why it went wrong", and success is
	// the ending where nothing else survives: agent.Result is discarded,
	// so without this there was no stored answer to "did this run ever
	// call the tool we built for it" for the runs that worked.
	//
	// Its own transcript -- the agent framework's full narrative record of
	// the run, agent.Result.Transcript -- is not a field here: unlike
	// Detail, nothing needs it alongside a whole Run, only on its own
	// (bwsalmon/agents#446's "show attempt agent logs" pane), so
	// Store.SetRunTranscript/RunTranscript read and write it directly by
	// task ID and attempt number instead.
	Detail string
	Leases []Lease
}

// --- conversation ----------------------------------------------------

// Comment is one entry in a task's conversation: what a human asked for,
// what grain relayed on an agent's behalf, why the merge queue gave up.
//
// This is grain's own record, not a copy of one. v2 used to hold the
// conversation in a GitHub issue's comment thread and read it back as
// input -- which made the issue a second place a task's state lived, and
// made "has a human replied yet?" a question only a poll could answer.
// A task is a row here; so is everything said about it.
//
// The author is an Attribution rather than a bare Principal because the
// distinction is load-bearing exactly here: grain relaying an agent's
// question is (automation, on behalf of agent), and a human answering it
// directly is (human, nil). That is the difference a signature substring
// in a comment body used to gesture at with one bit.
type Comment struct {
	// ID is assigned by the store on write, and ordering by it is the
	// conversation's order. Observation.PendingQuestionCommentID names one
	// of these.
	ID        int64
	TaskID    string
	Author    Attribution
	Body      string
	CreatedAt time.Time
}

// Attachment is one file carried alongside a task -- bwsalmon/agents#522's
// "attach files (images, zip files, etc) to a task" and "attachable to
// follow-on comments in addition to the main task content." Content lives
// here, in the store, for the same reason Comment's own doc comment gives
// for the conversation itself: a task is a row, and so is everything
// carried with it, rather than a pointer to somewhere else that has to
// stay reachable to read it back.
//
// CommentID is nil for a file carried by the task's own body, and names a
// Comment.ID for one posted alongside a later comment -- one table for
// both rather than two, since a dispatched run materializing them into
// its sandbox (orchestrator's AttachmentsDir) treats every attachment
// exactly alike regardless of which one it came from.
type Attachment struct {
	ID          int64
	TaskID      string
	CommentID   *int64
	Filename    string
	ContentType string
	Size        int64
	Content     []byte
	CreatedAt   time.Time
}

// BranchName is the branch a task's work goes on — derived, never stored
// and never self-reported, so any two callers compute the same name.
func BranchName(taskID string) string { return "grain/task-" + taskID }
