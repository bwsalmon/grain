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
// Nothing in this file imports a database. Dolt is one representation of
// these types and GitHub labels are another; neither belongs in the types.
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
	ReasonDirect   OriginReason = "direct"   // somebody just filed it
	ReasonSchedule OriginReason = "schedule" // a scheduled job fired
	ReasonFix      OriginReason = "fix"      // filed for a broken PR
	ReasonReview   OriginReason = "review"   // review threads asked for it
	ReasonProposal OriginReason = "proposal" // an agent or parent proposed it
)

func (r OriginReason) Valid() bool {
	switch r {
	case ReasonDirect, ReasonSchedule, ReasonFix, ReasonReview, ReasonProposal:
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

func ParseRepo(text string) (RepoRef, error) {
	owner, name, ok := strings.Cut(text, "/")
	if !ok || owner == "" || name == "" {
		return RepoRef{}, fmt.Errorf("repo must be owner/name, got %q", text)
	}
	return RepoRef{Owner: owner, Name: name}, nil
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

// gitCredentialGrantPrefix marks a Grant as bwsalmon/agents#52's per-task
// git credential override: the named credential (gitproxy's
// CredentialSet.Get) a sandbox's git proxy requests should use in place
// of the owner/repo ladder, for a scope the ladder's own credentials
// deliberately withhold (docs/design.md, "Scopes to withhold" --
// `workflow`, most notably).
const gitCredentialGrantPrefix = "github-credential:"

// GitCredentialGrant is the Grant a `grain-github-<name>` label produces.
// Via is GrantByLabel: applying the label already requires the same
// "can apply a label" trust tier the trigger label itself relies on, so
// this opens no new gate -- the same reasoning bwsalmon/agents#52 gave
// for the label in the first place. Unlike grain/proxy's
// SandboxCredentialOverrides, this needs no storage or dispatch/release
// lifecycle of its own: it lives and dies with the task the same as
// every other Grant.
func GitCredentialGrant(name string) Grant {
	return Grant{Capability: gitCredentialGrantPrefix + name, Via: GrantByLabel}
}

// gitCredentialOverride returns the credential name a GitCredentialGrant
// among grants asks for, if any.
func gitCredentialOverride(grants []Grant) (name string, ok bool) {
	for _, g := range grants {
		if n, isOverride := strings.CutPrefix(g.Capability, gitCredentialGrantPrefix); isOverride {
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
	StateCompleted     State = "completed"
	StateClosed        State = "closed"
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

	Target  *RepoRef // the one write target
	Binding RepoBinding
	Base    string
	Folder  *FolderRef
	Reads   []RepoRef // read-only. Grant nothing.

	Grants []Grant
	Links  []Link
	Tags   []string

	AutoMerge bool
	CreatedAt *time.Time
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
	// MergeQueueBlockedAt is set once the merge queue has tried and failed
	// to fix this task's own PR automatically (a LinkFixTask task ran and
	// closed, but the PR is still conflicted or failing) -- see
	// orchestrator.SyncPullRequests. A non-nil value means the merge queue
	// has stopped driving this task: it no longer counts as any repo's
	// queue head (so a stuck PR cannot block the ones behind it) and gets
	// no second automatic fix, but it is still merged the moment it reads
	// clean, the same as a fix task itself, in case a human pushes the
	// fix by hand.
	MergeQueueBlockedAt *time.Time
	ObservedAt          *time.Time
}

// Run is one attempt. A live run is a Run with no FinishedAt.
type Run struct {
	ID     string
	TaskID string
	// The concurrency unit and the VM instance. Equal while sandboxes are
	// long-lived; distinct once one is created per task.
	Slot       string
	Sandbox    string
	Unit       string
	Attempt    int
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    string
	Leases     []Lease
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

// BranchName is the branch a task's work goes on — derived, never stored
// and never self-reported, so any two callers compute the same name.
func BranchName(taskID string) string { return "grain/task-" + taskID }
