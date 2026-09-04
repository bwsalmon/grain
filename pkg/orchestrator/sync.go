package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// healthFrom computes a model.PrHealth from a fresh GitHub read, the pure
// half of SyncPullRequests split out so it needs no store or client to
// test. A closed PR reads PrMerged or PrClosed off detail.Merged --
// GitHub's own field for the one distinction detail.State alone collapses
// (github.PullRequestDetail.State's own doc comment) -- which is what
// lets recordPullRequestEvents tell a task's timeline "PR merged" from
// "PR closed unmerged" (bwsalmon/agents#493).
//
// A check GitHub has not finished running reads PrPending, and pending
// beats every CI answer below it: a PR whose tests are still going is
// neither clean (merging it would land untested code, the whole point of
// running tests at all) nor yet failing. Only "completed" runs are
// judged, so the answer settles on its own within a cycle or two of CI
// finishing.
//
// An *empty* check list is the case no read of GitHub can resolve on its
// own: a repo with no CI configured and a repo whose CI has not
// registered its first check run yet look exactly alike from the Checks
// API. checksSettled is the caller's answer to that -- true once the
// current head commit has been sitting there with nothing reported for
// long enough that CI would have shown up if there were any (see
// emptyChecksSettled). Until then an empty list reads PrPending, so a
// pull request opened seconds ago waits a cycle or two rather than
// merging in the gap between the push and GitHub creating the workflow
// run's check runs. checksSettled is only consulted for an empty list;
// once even one check exists, the rules above already decide.
//
// Pending deliberately outranks failing, so a red check alongside a
// still-running one waits rather than escalating immediately. The queue
// files exactly one automatic fix per PR (fileFixTask, escalateToUser),
// so it is worth spending a cycle to file that one against CI's whole
// verdict instead of against whichever job happened to go red first.
//
// Of the conclusions a completed run can carry, "failure", "timed_out"
// and "startup_failure" (the last only ever from the Actions fallback,
// where a workflow file too broken to run at all lands) read as
// PrFailing: each is a check that ran, or tried to, and did not pass --
// which is what a fix task exists to repair. "cancelled",
// "action_required", "neutral", "skipped" and "stale" do not: CheckRun's
// own doc comment leaves which conclusions count as broken to the
// caller, and treating every non-"success" run as failing would make a
// merge queue's own "cancelled, will retry" check block a PR this
// package has no business blocking, or file a fix task against a
// workflow waiting on a human's approval, which no agent can fix.
func healthFrom(detail github.PullRequestDetail, checks []github.CheckRun, checksKnown, checksSettled bool) model.PrHealth {
	if detail.State == "closed" {
		if detail.Merged {
			return model.PrMerged
		}
		return model.PrClosed
	}
	if detail.Mergeable != nil && !*detail.Mergeable {
		return model.PrConflicted
	}
	// Checks we could not read are not checks that passed. Both of the
	// facts above are read straight off the PR and stay authoritative
	// without them, but everything below this line is a judgement about
	// CI, and an empty `checks` is how a genuinely clean PR looks too --
	// so a deployment whose credential cannot reach the Checks API at
	// all (checkRunsFor, on a scoped PAT) would otherwise read every PR
	// as PrClean and merge it with CI red. PrUnknown is the honest
	// answer, and the one syncEntry declines to act on.
	if !checksKnown {
		return model.PrUnknown
	}
	for _, c := range checks {
		if c.Status != "completed" {
			return model.PrPending
		}
	}
	// Nothing reported at all, and not yet for long enough to believe
	// that means there is nothing to report. PrPending rather than
	// PrUnknown: this resolves by itself, on a clock, which is what
	// PENDING says and what UNKNOWN does not.
	if len(checks) == 0 && !checksSettled {
		return model.PrPending
	}
	if len(failingChecks(checks)) > 0 {
		return model.PrFailing
	}
	if detail.Mergeable == nil {
		return model.PrUnknown
	}
	return model.PrClean
}

// failingChecks returns the names of the completed runs in checks that
// did not pass, in the order GitHub reported them -- what decides
// PrFailing above, and what healthReason names in the fix task's own
// body so the agent repairing the PR is told which jobs to look at
// rather than being left to rediscover them.
func failingChecks(checks []github.CheckRun) []string {
	var names []string
	for _, c := range checks {
		if c.Status != "completed" || c.Conclusion == nil {
			continue
		}
		switch *c.Conclusion {
		case "failure", "timed_out", "startup_failure":
			name := c.Name
			if name == "" {
				name = "(unnamed check)"
			}
			names = append(names, name)
		}
	}
	return names
}

// unfinishedChecks returns the names of the runs in checks GitHub has not
// finished -- what makes healthFrom answer PrPending, and what
// escalateStalledChecks names once they have gone on doing so for longer
// than the merge queue is willing to wait. A person asked to go and look
// at a stuck pull request starts from which job is stuck.
func unfinishedChecks(checks []github.CheckRun) []string {
	var names []string
	for _, c := range checks {
		if c.Status == "completed" {
			continue
		}
		name := c.Name
		if name == "" {
			name = "(unnamed check)"
		}
		names = append(names, name)
	}
	return names
}

// defaultCheckRegistrationWindow is how long a pull request's head commit
// has to have had *no* checks reported against it before healthFrom
// believes that means the repo has no CI, rather than that CI has not got
// round to registering yet.
//
// It exists because the two are the same API response. GitHub Actions
// creates a workflow run's check runs asynchronously, after it has
// processed the push, and finishWithPullRequest opens the pull request
// the moment the branch lands -- so a SyncPullRequests tick landing in
// that gap reads an empty list on a repo whose CI is about to start, and
// reading that as clean is how a merge beats the tests. A repo with no CI
// configured never fills the list in, and there is nothing in the Checks
// API that tells the two apart; only time does.
//
// GitHub's own mergeable_state is not the exception it looks like: it
// reads "clean" for an empty check list, the same answer it gives a
// genuinely green one, because it is computed from that same empty list.
// A live pull request on this repository was caught reading "clean" with
// zero check runs four seconds after a push, two seconds before its
// first check registered -- this gap, measured, in the field that was
// supposed to close it. See github.PullRequestDetail.MergeableState for
// the whole run. Nothing there removes this wait.
//
// Two minutes is chosen against what it costs on each side. On a repo
// that does have CI it costs nothing at all: the checks appear within
// seconds and the very next tick sees them. On a repo with genuinely no
// CI it is the whole cost -- every merge waits this out, once, per head
// commit -- so it wants to be small enough not to feel like a stall and
// large enough to cover GitHub being slow to dispatch a workflow, which
// is measured in seconds but not reliably in single digits.
const defaultCheckRegistrationWindow = 2 * time.Minute

// defaultCheckStallDeadline is the other end of the same wait: how long a
// pull request's checks may sit unfinished before the merge queue stops
// waiting on them, comments, and moves on (checksStalled,
// escalateStalledChecks).
//
// PrPending is acted on by neither arm of syncEntry's switch, which is
// right for CI that finishes and unbounded for CI that does not. A
// workflow waiting on an approval nobody gives, a self-hosted runner that
// never picks the job up, a third-party check whose provider posted
// "queued" and then went away: each leaves its task at the head of its
// repo's queue forever, with nothing merged, nothing filed and nothing
// said to anyone -- and, since only the head is ever acted on
// (queueHeads), everything queued behind it in the same repo waits too.
//
// Two hours is chosen against what each direction costs. Too long is the
// stall above, paid by every task behind it. Too short costs less than it
// looks: giving up is a comment and MergeQueueBlockedAt, which stops this
// task being anyone's queue head but leaves it merging the moment it does
// read clean (syncEntry's `blocked` arm), so a genuinely slow suite that
// finishes green still lands -- just out of queue order, and having said
// something alarming on the way. So this wants to sit past any honest CI
// run rather than exactly on the longest one, and two hours is well past
// the CI a deployment running an agent set on a poll interval measured in
// seconds is plausibly waiting on.
//
// It is a constant rather than a grain_config field on purpose. Every
// setting in that row is one an operator has a reason to reach for during
// ordinary use (which repos, how many at once, which model); this is one
// they would only ever reach for if their CI is slower than any CI here
// is expected to be, and adding it costs a schema field, a settings
// control, and a line in three docs. SetCheckStallDeadline is the escape
// hatch until a deployment actually needs the setting -- at which point
// this const becomes that field's default and nothing else here changes.
const defaultCheckStallDeadline = 2 * time.Hour

// sightings is the bookkeeping behind both clocks this file runs on a
// pull request's CI: the window an empty check list is held PENDING for
// (emptyChecks, defaultCheckRegistrationWindow) and the deadline an
// unfinished check is waited on before the queue gives up (pendingChecks,
// defaultCheckStallDeadline). Each remembers, per pull request, the head
// commit this process first saw the state it times and when it first saw
// it, alongside the duration itself, so a setter can change that without
// racing a cycle that is reading it.
//
// Deliberately in memory rather than in a task's Observation. Persisting
// either would mean a schema column, and neither fact is worth one:
// losing a sighting (a restart, a new leader) makes the next cycle start
// that clock afresh, which costs one more window of waiting and can never
// merge something early or give up on something sooner. Erring toward
// waiting is the whole point of both mechanisms.
//
// Both are keyed by pull request and hold the head sha alongside, so a
// push to a branch that already has a pull request open -- a fix task
// merging into it, a human pushing by hand, a redispatched task -- starts
// the clock again rather than inheriting the verdict of the commit before
// it. That push empties the check list, and sets CI running again, just
// as surely as the first one did.
type sightings struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]sighting
}

type sighting struct {
	headSHA string
	first   time.Time
}

func newSightings(window time.Duration) *sightings {
	return &sightings{window: window, seen: map[string]sighting{}}
}

// emptyChecks times how long ref's current head commit has had *no*
// checks reported against it (emptyChecksSettled).
var emptyChecks = newSightings(defaultCheckRegistrationWindow)

// pendingChecks times how long ref's current head commit has read
// PrPending without interruption (checksStalled). Interruption matters:
// the sighting is dropped the moment a cycle reads anything else, so a
// check re-run hours after the first round settled is timed from the
// re-run rather than from the commit's first ever pending read.
var pendingChecks = newSightings(defaultCheckStallDeadline)

// observe records now as the first moment this process saw ref at headSHA
// in the state the caller is timing -- unless it has already recorded one
// for that exact pair -- and returns that moment together with the window
// in effect. Both come back from under one lock, so a window changed
// mid-cycle is never compared against a sighting taken under the old one.
//
// now comes from the caller (SyncPullRequests') clock, the same one that
// recorded the sighting, so nothing here ever compares grain's clock
// against GitHub's -- which is the trap in measuring either of these from
// a timestamp GitHub stamped, like the pull request's own CreatedAt.
//
// A window of zero is off, and off records nothing: a caller with no
// clock to consult has no sighting to consult it against.
func (s *sightings) observe(ref model.PullRequestRef, headSHA string, now time.Time) (time.Time, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.window <= 0 {
		return time.Time{}, s.window
	}
	key := ref.String()
	seen, ok := s.seen[key]
	if !ok || seen.headSHA != headSHA {
		seen = sighting{headSHA: headSHA, first: now}
		s.seen[key] = seen
	}
	return seen.first, s.window
}

// forget drops ref's sighting, so the next observe starts its clock
// again. Called when a pull request closes -- the one moment this package
// knows it will never be synced again, and so where the map stops growing
// -- and, for pendingChecks, on any cycle that reads something other than
// the state it is timing. Anything that stops being tracked without ever
// reading closed leaks one small entry until the process restarts, which
// is a bound worth having over pruning the map against the live set every
// cycle: two daemons sharing a process (as tests do) would then each
// delete the other's clocks and leave every pull request starting afresh
// every cycle.
func (s *sightings) forget(ref model.PullRequestRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, ref.String())
}

// setWindow replaces the duration and returns the function that puts the
// old one back.
//
// Changing it clears the sightings taken under the old one. Measuring an
// existing sighting against a window it was not started for is
// meaningless in the shortening direction and wrong in the lengthening
// one, and starting every clock afresh is the conservative reset -- it
// can only make the next merge later, or the next escalation later, never
// either one sooner.
func (s *sightings) setWindow(d time.Duration) func() {
	s.mu.Lock()
	prev := s.window
	s.window = d
	s.seen = map[string]sighting{}
	s.mu.Unlock()
	return func() { s.setWindow(prev) }
}

// SetCheckRegistrationWindow replaces the window described on
// defaultCheckRegistrationWindow and returns the function that puts the
// old one back -- the same shape DisableBranchExistsSleep has, and used
// the same way.
//
// Zero disables the wait outright: an empty check list is believed the
// first time it is read, which is what this package did before the window
// existed. That is the right setting for a caller driving a simulated
// GitHub whose repos have no CI at all and never will -- every test in
// tests/e2e and cmd/grain is one -- where the only thing the window can
// do is make each of them sit out two minutes of wall clock for a signal
// that is never coming. It is the wrong setting for a deployment: see
// defaultCheckRegistrationWindow for what the wait is buying.
func SetCheckRegistrationWindow(d time.Duration) func() { return emptyChecks.setWindow(d) }

// SetCheckStallDeadline replaces the deadline described on
// defaultCheckStallDeadline and returns the function that puts the old
// one back, the same shape as SetCheckRegistrationWindow.
//
// Zero switches giving up off: PrPending is then waited on forever, which
// is what this package did before the deadline existed. Note that zero
// means the opposite direction to zero above -- one disables a wait, the
// other disables giving up on one -- and both are the setting that leaves
// the merge queue doing less on its own.
func SetCheckStallDeadline(d time.Duration) func() { return pendingChecks.setWindow(d) }

// emptyChecksSettled reports whether ref's current head commit has had an
// empty check list for longer than the registration window --
// healthFrom's checksSettled. Call it only when the check list actually
// is empty: the first such call for a given commit is what starts that
// commit's window.
//
// An empty headSHA is never settled, short of the window being off
// altogether. GitHub fills the head sha in asynchronously the same way it
// computes Mergeable (PullRequestDetail's own doc comments), so a read
// without one is a read of a pull request GitHub has not finished
// thinking about -- not a commit whose CI can be concluded absent.
func emptyChecksSettled(ref model.PullRequestRef, headSHA string, now time.Time) bool {
	first, window := emptyChecks.observe(ref, headSHA, now)
	// A window of zero is off, and off means every path here answers the
	// way it did before the window existed -- including the one below
	// that has nothing to do with elapsed time.
	if window <= 0 {
		return true
	}
	if headSHA == "" {
		return false
	}
	// Written as a comparison against the deadline rather than as
	// `now.Sub(first) >= window`, so a clock that jumped backwards reads
	// as "not settled yet": the same direction everything else here errs
	// in.
	return !now.Before(first.Add(window))
}

// forgetEmptyChecks drops ref's registration-window sighting; see
// sightings.forget.
func forgetEmptyChecks(ref model.PullRequestRef) { emptyChecks.forget(ref) }

// checksStalled reports whether ref's current head commit has read
// PrPending for longer than defaultCheckStallDeadline, and the deadline
// it was measured against (which escalateStalledChecks puts in what it
// tells the task's own thread). Call it only on a cycle that actually
// read PrPending, and call forgetPendingChecks on every cycle that did
// not: the first such call for a given commit is what starts its clock,
// and forgetting is what keeps that clock measuring one unbroken run of
// pending reads rather than the span between two of them.
//
// Unlike emptyChecksSettled an empty headSHA is timed rather than
// excused. There the sha is the thing being reasoned about -- no commit,
// nothing whose CI can be concluded absent -- while here the pull request
// is plainly stuck whatever GitHub is or is not naming as its head, and a
// head sha that never arrives is one more way for PENDING to last
// forever rather than a reason to allow it.
func checksStalled(ref model.PullRequestRef, headSHA string, now time.Time) (bool, time.Duration) {
	first, deadline := pendingChecks.observe(ref, headSHA, now)
	if deadline <= 0 {
		return false, 0
	}
	return !now.Before(first.Add(deadline)), deadline
}

// forgetPendingChecks drops ref's stall-deadline sighting; see
// sightings.forget.
func forgetPendingChecks(ref model.PullRequestRef) { pendingChecks.forget(ref) }

// workflowFallbackOnce keeps the "falling back to Actions" notice to one
// line per process: like checksUnavailable below it is a standing
// property of the deployment's credential, not an event.
var workflowFallbackOnce sync.Once

// checksUnavailableOnce keeps the "no Checks access" notice below to one
// line per process rather than one per pull request per cycle: it is a
// standing property of the deployment's credential, not an event, and it
// never resolves on its own while that credential is in use.
var checksUnavailableOnce sync.Once

// checksUnavailable backs ChecksUnavailable -- set alongside the log line
// checksUnavailableOnce prints, so a caller outside this package can ask
// the same question without grepping server logs for it.
var checksUnavailable atomic.Bool

// ChecksUnavailable reports whether this process has ever seen GitHub
// refuse a check-runs read with 403 (checkRunsFor's own doc comment).
// Once true it stays true for the rest of the process's life -- the same
// permanent-until-restart fact the "no Checks access" log line already
// reports -- which is what lets pkg/ui surface a "submit is queued but
// will never actually merge" notice on this deployment's own /api/config
// (bwsalmon/agents#483) instead of an operator having to notice a task's
// AutoMerge never resolving and go searching logs to learn why.
func ChecksUnavailable() bool {
	return checksUnavailable.Load()
}

// checkRunsFor reads ref's CI state, reporting whether the answer is
// known at all. It tries two endpoints, because no single one is
// readable by every credential type a deployment might be configured
// with:
//
//  1. The Checks API, which sees every check on the commit whoever
//     reported it. Needs the `repo` scope on a classic PAT; a GitHub App
//     installation token has it too.
//  2. Failing that with a permission error, the Actions API, which sees
//     GitHub Actions workflow runs and nothing else. This is the only one
//     of the two a fine-grained PAT can reach -- "Checks" cannot be
//     granted to one at all, so a deployment on a fine-grained token
//     needs "Actions" read or it has no CI signal whatsoever.
//
// A permission error from both is a fact about the credential rather
// than a failure to handle: it will repeat on every call, forever, so
// failing the sync on it would leave every tracked pull request erroring
// every cycle. The health of those PRs goes unknown instead -- which
// costs auto-merge, and nothing else: dispatch, the push, and opening
// the PR are a separate reconciler that never reads CI.
//
// Every other error is still an error, and that deliberately includes
// the other conditions GitHub answers 403 for -- rate limits, SAML
// enforcement, an organization IP allow list. Those clear, on their own
// or with a change an operator can make, so swallowing one as "this
// credential cannot read CI" would switch auto-merge off until the next
// restart over something that had already fixed itself (checksUnavailable
// below never clears within a process). github.IsPermissionDenied draws
// that line by message, not by status.
//
// Both endpoints are scoped to headSHA -- the commit the caller read off
// the pull request a moment ago, and the commit whose health it is about
// to decide. The Checks API takes any ref, including the head branch's
// name, and a branch-scoped read is a read of whatever that branch points
// at *now*: a push landing between the caller's GetPullRequest and this
// call answers for a commit the caller never saw, so the check list, the
// verdict computed from it and the registration-window sighting keyed on
// the caller's headSHA would each describe a different commit. Naming the
// commit costs nothing and keeps all three talking about one (a push
// during the cycle then simply makes the *next* cycle start that new
// commit's window afresh, which is what sighting.headSHA is for).
//
// head, the branch name, is the fallback for the one case there is no sha
// to name: headSHA may be empty on a PR read before GitHub filled it in.
// A branch-scoped read is then better than no CI signal at all, since the
// caller's own headSHA is empty too and emptyChecksSettled already
// refuses to conclude anything from an empty list without one. The
// Actions fallback below has no such fallback of its own -- it takes a
// commit and nothing else -- so it is skipped rather than widened.
func checkRunsFor(client github.Client, ref model.PullRequestRef, head, headSHA string) ([]github.CheckRun, bool, error) {
	commit := headSHA
	if commit == "" {
		commit = head
	}
	checks, err := client.ListCheckRuns(ref.Repo.Owner, ref.Repo.Name, commit)
	if err == nil {
		return checks, true, nil
	}
	if !github.IsPermissionDenied(err) {
		return nil, false, fmt.Errorf("orchestrator: reading check runs for %s: %w", ref, err)
	}
	checksErr := err

	if headSHA != "" {
		runs, err := client.ListWorkflowRuns(ref.Repo.Owner, ref.Repo.Name, headSHA)
		if err == nil {
			workflowFallbackOnce.Do(func() {
				log.Printf("orchestrator: this deployment's GitHub credential cannot read check " +
					"runs, so pull request health comes from GitHub Actions workflow runs " +
					"instead. CI reported by anything other than Actions is invisible -- grant " +
					"the `repo` scope (classic PAT) or install a GitHub App if this deployment " +
					"has checks from another provider.")
			})
			return runs, true, nil
		}
		if !github.IsPermissionDenied(err) {
			return nil, false, fmt.Errorf("orchestrator: reading workflow runs for %s: %w", ref, err)
		}
	}

	checksUnavailableOnce.Do(func() {
		checksUnavailable.Store(true)
		// GitHub's own refusal goes in the line: it is the only place an
		// operator can see which permission is missing, and this is the
		// one time the process ever reports it.
		log.Printf("orchestrator: this deployment's GitHub credential can read neither check "+
			"runs nor Actions workflow runs -- pull request health stays unknown and nothing "+
			"is auto-merged. Everything else is unaffected. Grant the `repo` scope (classic "+
			"PAT) or \"Actions\" read (fine-grained PAT), then restart. GitHub said: %v", checksErr)
	})
	return nil, false, nil
}

// queueEntry is one still-open tracked pull request, everything syncEntry
// needs about it gathered up front so SyncPullRequests can compute every
// repo's queue head before acting on any single one.
type queueEntry struct {
	task model.Task
	obs  *model.Observation
	ref  model.PullRequestRef
}

// isQueueMember reports whether e's task is a member of its target repo's
// merge queue at all -- see queueHeads. A task never asked to be merged
// automatically (no /auto-merge) is not in any queue: the merge queue is
// opt in, the same "submit" step the issue this implements
// (bwsalmon/agents#283) describes, reusing the directive that already
// meant "merge me once I'm clean" rather than adding a second field that
// would mean the same thing. A fix task the queue itself filed
// (Origin.Reason == ReasonFix) is deliberately excluded too: it merges
// into the task it repairs, not into the repo's own base branch, so it is
// not a queue entry in its own right -- see syncEntry's own eligibility
// check for why it still merges unconditionally. A review task
// (isReviewTask, grain/task-284) is excluded on exactly the same
// grounds: it is stacked on the branch of the task it reviews and merges
// back into it, not into the repo's base. A task the queue has
// already given up on (obs.MergeQueueBlockedAt set) is excluded as well,
// which is what lets the queue move on to the next task rather than
// waiting forever on one that needs a human.
func isQueueMember(e queueEntry) bool {
	if !e.task.AutoMerge || e.task.Origin.Reason == model.ReasonFix || isReviewTask(e.task) {
		return false
	}
	return e.obs == nil || e.obs.MergeQueueBlockedAt == nil
}

// queueOrder is every entry isQueueMember accepts, across all repos at
// once, in the merge queue's own order: the backlog's, ascending
// Task.OrderKey with the task ID as the tiebreak, which is the order
// Store.ListTasks shows a human and Store.Ready dispatches in.
//
// The queue used to have an order of its own -- earliest submitted, by
// Task.CreatedAt -- computed here, inside one cycle, and written down
// nowhere. Reading position off the backlog instead means there is one
// ordering rather than two, and it is the one already on screen:
// showQueueAtFrontOfBacklog keeps these entries at the front of that
// backlog, in this order, so what a list shows and what merges next
// cannot drift apart, and a human who disagrees with the order can drag a
// row rather than discover there was nothing to drag. For a deployment
// that has never reordered anything this is the same answer CreatedAt
// gave: Store.OrderKeyForNewTask files each new task behind the last.
func queueOrder(entries []queueEntry) []queueEntry {
	members := make([]queueEntry, 0, len(entries))
	for _, e := range entries {
		if isQueueMember(e) {
			members = append(members, e)
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].task.OrderKey != members[j].task.OrderKey {
			return members[i].task.OrderKey < members[j].task.OrderKey
		}
		return members[i].task.ID < members[j].task.ID
	})
	return members
}

// queueHeads returns, for every target repo with at least one queued
// task, the ID of the one task in front: the first of that repo's
// entries in queueOrder. Only a repo's head task is ever merged or
// auto-fixed on a given cycle -- everything behind it waits, which is the
// whole property that makes this a queue rather than "merge every clean
// PR in whatever order GitHub happens to return them in": a fix filed for
// the second task while the first is still being repaired would very
// likely need refiling the moment the first merges and changes what the
// second is based against.
func queueHeads(members []queueEntry) map[string]string {
	heads := map[string]string{}
	for _, e := range members {
		repo := e.ref.Repo.String()
		if _, ok := heads[repo]; !ok {
			heads[repo] = e.task.ID
		}
	}
	return heads
}

// showQueueAtFrontOfBacklog moves every task waiting on the merge queue to
// the front of the backlog, in the order the queue will act on them
// (queueOrder), so grain's internal ordering is something a person can
// read off the task list rather than something only a cycle knows.
//
// Reconciled every cycle rather than written once when a task joins the
// queue: membership is a derived fact (isQueueMember, over auto-merge,
// fix-task and blocked-at state that all move on their own), so the only
// way for the front of the backlog to keep agreeing with it is to
// recompute both together. Store.MoveToFrontOfBacklog writes nothing when
// the order already holds, which is almost every cycle.
//
// Failing is not fatal to the sync it runs inside: what is lost is the
// backlog showing the right order for one cycle, and every merge decision
// here is made from queueOrder directly rather than from what was
// written.
func showQueueAtFrontOfBacklog(ctx context.Context, store *model.Store, members []queueEntry) error {
	if len(members) == 0 {
		return nil
	}
	ids := make([]string, len(members))
	for i, e := range members {
		ids[i] = e.task.ID
	}
	if err := store.MoveToFrontOfBacklog(ctx, ids); err != nil {
		return fmt.Errorf("orchestrator: moving the merge queue to the front of the backlog: %w", err)
	}
	return nil
}

// SyncPullRequests refreshes every completed task's tracked pull request,
// advances each target repo's merge queue by one step, and closes out the
// tasks whose PR finished -- the other half of core.py's _close_finished_prs
// this package owns, plus the merge queue bwsalmon/agents#283 asked for in
// place of core.py's own _suggest_fix (a suggestion a human had to
// approve before the agent set would attempt it). See queueOrder,
// queueHeads and syncEntry for the queue itself, and
// showQueueAtFrontOfBacklog for where it puts itself so a person can see
// it; this function only gathers the state every entry's decision needs
// before any of them act, so a decision about task N never depends on an
// in-progress change queueHeads has not seen yet.
func SyncPullRequests(ctx context.Context, store *model.Store, client github.Client, now time.Time) error {
	links, err := store.OpenPullRequestLinks(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading open pull request links: %w", err)
	}

	// The gather loop is not isolated per entry the way the act loop
	// below is, and deliberately so: queueHeads decides who is at the
	// front of each repo's queue by comparing entries against each other,
	// so acting on a set with an entry silently missing from it could
	// promote the task behind the real head and merge two changes in the
	// wrong order. A store read failing here is systemic anyway (the same
	// database answers every one of these), so the whole reconciler backs
	// off to the next tick rather than advancing a queue it cannot see all
	// of.
	//
	// A link whose ref will not parse is the one exception, because it is
	// not a transient failure: nothing can ever be done with a pull
	// request whose repo and number are unreadable, so it can never be
	// anyone's head, and letting one malformed row wedge every other
	// repo's queue forever is strictly worse than skipping it and
	// reporting it every cycle.
	entries := make([]queueEntry, 0, len(links))
	var errs []error
	for _, link := range links {
		task, err := store.GetTask(ctx, link.TaskID)
		if err != nil {
			return fmt.Errorf("orchestrator: reading task %s: %w", link.TaskID, err)
		}
		if task == nil {
			continue
		}
		obs, err := store.GetObservation(ctx, link.TaskID)
		if err != nil {
			return fmt.Errorf("orchestrator: reading observation for %s: %w", link.TaskID, err)
		}
		ref, err := model.ParsePullRequestRef(link.PullRequest)
		if err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: task %s: %w", link.TaskID, err))
			continue
		}
		entries = append(entries, queueEntry{task: *task, obs: obs, ref: ref})
	}

	// The queue, in the order it will act: computed once against the whole
	// set, and put where a person can see it before anything acts on it.
	members := queueOrder(entries)
	heads := queueHeads(members)
	if err := showQueueAtFrontOfBacklog(ctx, store, members); err != nil {
		errs = append(errs, err)
	}

	// Acting on one entry is isolated, because head-of-queue was already
	// decided above against the complete set: an entry that fails here
	// cannot make another entry merge that would not have merged anyway
	// (syncEntry merges an ordinary queue member only when it is its
	// repo's head), so the worst a failure costs the others is a cycle of
	// latency -- which is what returning early used to cost all of them.
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := syncEntry(ctx, store, client, e, heads, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncEntry is SyncPullRequests' per-task decision: refresh e's PR health,
// act on it, and close the task out if the PR itself is done.
func syncEntry(ctx context.Context, store *model.Store, client github.Client,
	e queueEntry, heads map[string]string, now time.Time) error {

	task, ref := e.task, e.ref

	detail, err := client.GetPullRequest(ref.Repo.Owner, ref.Repo.Name, ref.Number)
	if err != nil {
		return fmt.Errorf("orchestrator: reading %s: %w", ref, err)
	}

	// A closed PR's head branch is routinely gone by the time this runs --
	// GitHub deletes it on merge when a repo has that setting on, and
	// nothing stops a human deleting it after closing without merging
	// either. healthFrom's first line already answers PrClosed off
	// detail.State alone with no need for checks, so skip checkRunsFor
	// entirely here rather than have a 404 on a ref that no longer
	// resolves turn a task that is plainly done into one syncEntry can
	// never close: this same task_id keeps recurring in
	// OpenPullRequestLinks every cycle, so an error here would repeat
	// forever, not just once.
	var checks []github.CheckRun
	checksKnown := true
	checksSettled := true
	if detail.State != "closed" {
		checks, checksKnown, err = checkRunsFor(client, ref, detail.HeadRef, detail.HeadSHA)
		if err != nil {
			return err
		}
		// Only an empty list needs the window, and asking only for one
		// is what keeps the sighting map to pull requests that are
		// actually in that state (emptyChecksSettled starts a commit's
		// window on the first call for it).
		//
		// A list that is *not* empty drops the sighting, the same way
		// forgetPendingChecks below drops the other clock the moment its
		// own state stops holding: checks have registered, so there is
		// nothing left to time, and a sighting left lying around is one
		// that could outlive the state it was timing.
		if checksKnown {
			if len(checks) == 0 {
				checksSettled = emptyChecksSettled(ref, detail.HeadSHA, now)
			} else {
				forgetEmptyChecks(ref)
			}
		}
	}
	health := healthFrom(detail, checks, checksKnown, checksSettled)

	// The clock on PENDING, kept for every entry rather than only for the
	// one at the head of its queue: a pull request whose checks hang has
	// been hanging for as long as it has been hanging, whether or not
	// something ahead of it happened to be occupying the queue in the
	// meantime. Starting it only on promotion would make a repo with
	// three stalled pull requests take three deadlines to clear rather
	// than one.
	stalled, stallDeadline := false, time.Duration(0)
	if health == model.PrPending {
		stalled, stallDeadline = checksStalled(ref, detail.HeadSHA, now)
	} else {
		// Any other answer -- clean, failing, conflicted, unknown, closed
		// -- is CI having reported, or having stopped being the question,
		// so whatever pending run was being timed is over. A later
		// PENDING on this same commit (a re-run of a check, hours after
		// the first round finished) is timed from itself.
		forgetPendingChecks(ref)
	}

	isFixTask := task.Origin.Reason == model.ReasonFix
	isHead := heads[ref.Repo.String()] == task.ID
	blocked := e.obs != nil && e.obs.MergeQueueBlockedAt != nil
	// stacked: a task filed onto another task's branch and merged back
	// into it rather than into the repo's own base -- an automatic fix,
	// or a review (grain/task-284). isQueueMember excludes both, so
	// neither ever waits for a head position, and neither is a head that
	// could be repaired or escalated.
	stacked := isFixTask || isReviewTask(task)

	// The review this task declared, if it has one and it has not landed
	// yet (SyncReviews is what files it). A change waiting on its own
	// review does not merge, clean or not and head or not: attaching a
	// review means it happens before the merge rather than after.
	//
	// Only for an ordinary queue member. A stacked task is never itself
	// reviewed, and one the queue has already given up on is held by
	// nothing -- blocked means exactly that this task merges the moment
	// it reads clean and waits on nobody.
	var review reviewState
	if !stacked && !blocked {
		review, err = reviewStateOf(ctx, store, task, e.obs, now)
		if err != nil {
			return err
		}
	}

	// A stacked task -- an automatic fix, or a review -- always merges
	// into the branch it was filed against the moment it
	// reads clean, unconditionally, the same as every AutoMerge task did
	// before this package had a queue at all -- it is not itself a queue
	// entry (isQueueMember excludes it), so it is never "blocking" a repo
	// the way a stuck top-level task would. A task the queue already gave
	// up on (blocked) gets the same unconditional treatment: it stopped
	// being anyone's queue head so it cannot hold the queue back, but it
	// still lands the moment a human's own push makes it clean. Anything
	// else -- an ordinary queue member -- only merges once it is actually
	// the head: merging task 2 while task 1 is still ahead of it would
	// defeat the reason to queue at all.
	//
	// PrPending matches none of these arms until the last one, and
	// PrUnknown matches none of them at all: CI is still running, or has
	// not reported for the first time yet, or its verdict is unreadable
	// -- so in every case there is nothing to merge yet and nothing to
	// file a fix for. The entry is simply left where it is -- still the
	// head of its queue, holding the position -- and the next cycle asks
	// GitHub again. That is what makes a merge wait for the tests rather
	// than race them, and it is deliberately the same "do nothing, look
	// again" the asynchronous Mergeable read has always taken.
	//
	// "Look again" has to stop somewhere, though, and the last arm is
	// where: a head whose checks have been unfinished for longer than
	// defaultCheckStallDeadline is one nothing is going to resolve on its
	// own, so the queue says so and moves on rather than holding its own
	// head position forever with nothing said to anyone. PrUnknown has no
	// such arm on purpose -- the thing that produces it for cycle after
	// cycle is a credential that cannot read checks, which is one fact
	// about the whole deployment (ChecksUnavailable, and the one log line
	// that reports it) rather than something wrong with any one of the
	// pull requests it would otherwise comment on individually.
	switch {
	case review.overdue:
		// The review is not arriving. Say so, give up the wait, and leave
		// the merge itself to the next cycle -- which reads a task the
		// store now says is blocked, and merges it the moment it reads
		// clean the way it does any other blocked task.
		if err := escalateUnfinishedReview(ctx, store, task, ref, review.taskID, now); err != nil {
			return err
		}

	case review.pending:
		// Nothing to do but wait: the review is still running, or its own
		// pull request has not merged back into this branch yet. Holding
		// the head position is the point -- this change is next in its
		// repo, and it is not ready. No fix is filed and no stall
		// escalated either, deliberately: the agent already reading this
		// branch was sent to fix what is wrong with it, so a second one
		// filed onto the same branch at the same time would be two
		// stacked repairs of one change.

	case health == model.PrClean && task.AutoMerge && (stacked || isHead || blocked):
		// Pinned to the commit the verdict above was computed for, not
		// left to land on whatever the head branch points at by the time
		// GitHub processes this. The two are the same commit right up
		// until they are not: a push arriving between the reads above and
		// this call -- a fix task merging into this branch, a human's own
		// "push a fix by hand" that escalateToUser asked for, a
		// redispatched task pushing again -- would otherwise merge code
		// whose CI this cycle never read, which is the one thing waiting
		// for CI at all exists to prevent.
		//
		// GitHub refuses that with 409 (MergePullRequest's own doc
		// comment), and refusing is the whole point: the error goes back
		// to SyncPullRequests, which is already built to have one entry
		// fail without disturbing the others, and the next cycle reads
		// the commit that is there now and judges it on its own CI.
		//
		// An empty HeadSHA merges unpinned, as before. There is nothing
		// to pin to, and it is very nearly unreachable anyway: a PR
		// GitHub has not filled the sha in for reads PENDING on an empty
		// check list (emptyChecksSettled) rather than clean.
		if err := client.MergePullRequest(ref.Repo.Owner, ref.Repo.Name, ref.Number, detail.HeadSHA); err != nil {
			return fmt.Errorf("orchestrator: auto-merging %s (head %q): %w", ref, detail.HeadSHA, err)
		}
		// The merge above may have already settled the PR; re-read rather
		// than assume, since GitHub applies it asynchronously the same way
		// it computes Mergeable asynchronously (detail.Mergeable's own doc
		// comment).
		detail, err = client.GetPullRequest(ref.Repo.Owner, ref.Repo.Name, ref.Number)
		if err != nil {
			return fmt.Errorf("orchestrator: re-reading %s after merge: %w", ref, err)
		}
		health = healthFrom(detail, checks, checksKnown, checksSettled)

	case isHead && !stacked && !blocked && (health == model.PrConflicted || health == model.PrFailing):
		if err := advanceMergeQueueHead(ctx, store, client, task, e.obs, ref, detail, health, checks, now); err != nil {
			return err
		}

	case isHead && !stacked && !blocked && stalled:
		if err := escalateStalledChecks(ctx, store, task, ref, checks, stallDeadline, now); err != nil {
			return err
		}
	}

	// Closing out is one write now. It used to be two -- close the task's
	// GitHub issue, then record the closure -- with the issue closed first
	// and the store told second, so a crash in between left a closed issue
	// that grain still believed was open. recordPullRequestEvents widens
	// that one write rather than adding a second: PrOpenedAt can also need
	// recording on a cycle that closes nothing (an open PR seen for the
	// first time), and PrMergedAt/PrClosedAt only ever get set alongside
	// ClosedAt, so there is never a cycle needing both writes at once.
	if health == model.PrMerged || health == model.PrClosed {
		forgetEmptyChecks(ref)
	}
	return recordPullRequestEvents(ctx, store, task.ID, e.obs, detail, health, now)
}

// recordPullRequestEvents stamps whichever of a task's own pull-request
// timeline moments (bwsalmon/agents#493: "PR opened", "PR merged", "PR
// closed unmerged") this cycle can newly answer, and closes the task out
// once the pull request itself is done.
//
// PrOpenedAt, off detail's own CreatedAt, is written the first cycle this
// runs for a given task -- the moment a human (or EnsurePullRequest)
// actually opened the PR, which can predate this task completing by
// however long a found (rather than freshly opened) PR had already been
// open before grain started tracking it. Written once: unlike Health, an
// opening moment never changes, so re-observing it every cycle a PR
// happens to stay open would be a write with nothing new to say.
//
// PrMergedAt or PrClosedAt -- mutually exclusive, per Observation's own
// doc comment -- are set the one cycle health first reads PrMerged or
// PrClosed, alongside ClosedAt: once written, this task drops out of
// OpenPullRequestLinks (task_state reads 'closed', rather than either of
// the two post-run states that query names, once closed_at is set) and
// syncEntry never runs for it again, so there is no
// second cycle for either field to be overwritten on.
func recordPullRequestEvents(ctx context.Context, store *model.Store, taskID string,
	obs *model.Observation, detail github.PullRequestDetail, health model.PrHealth, now time.Time) error {

	needsOpenedAt := (obs == nil || obs.PrOpenedAt == nil) && !detail.CreatedAt.IsZero()
	closing := health == model.PrClosed || health == model.PrMerged
	if !needsOpenedAt && !closing {
		return nil
	}

	return observeField(ctx, store, taskID, now, func(o *model.Observation) {
		if needsOpenedAt {
			openedAt := detail.CreatedAt
			o.PrOpenedAt = &openedAt
		}
		if !closing {
			return
		}
		o.ClosedAt = &now
		if health != model.PrMerged {
			o.PrClosedAt = &now
			return
		}
		mergedAt := now
		if detail.MergedAt != nil {
			mergedAt = *detail.MergedAt
		}
		o.PrMergedAt = &mergedAt
	})
}

// advanceMergeQueueHead is what makes task -- the head of its repo's
// merge queue, its PR conflicted or failing checks -- progress: bring a
// stale branch up to date with its base and re-ask next cycle, file an
// automatic fix the first time a genuine failure happens, or notice the
// fix already filed has finished and decide whether that resolved things,
// or give up on a fix that has not finished and is not going to
// (defaultFixTaskDeadline).
func advanceMergeQueueHead(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, obs *model.Observation, ref model.PullRequestRef, detail github.PullRequestDetail,
	health model.PrHealth, checks []github.CheckRun, now time.Time) error {

	fixTaskID, hasFix := fixTaskLink(task)
	if !hasFix {
		outcome, err := refreshStaleHead(ctx, store, client, task, obs, ref, detail, now)
		if err != nil {
			return err
		}
		if outcome == refreshUpdated {
			// CI is running again against a tree somebody would merge.
			// Decide next cycle, on that answer rather than this one.
			return nil
		}
		return fileFixTask(ctx, store, client, task, ref, detail, health, checks, outcome, now)
	}

	fixState, err := store.State(ctx, fixTaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading fix task %s for %s: %w", fixTaskID, task.ID, err)
	}
	if fixState != model.StateClosed {
		// Still running, or its own PR is still open and being watched by
		// this same SyncPullRequests call -- nothing to do until it
		// finishes one way or the other, unless it has been not-finishing
		// for longer than any fix is waited on.
		overdue, err := fixTaskOverdue(ctx, store, fixTaskID, now)
		if err != nil {
			return err
		}
		if !overdue {
			return nil
		}
		return escalateUnfinishedFix(ctx, store, task, ref, fixTaskID, health, now)
	}
	// The fix task ran to completion (its own PR merged into ref's branch,
	// or was closed without merging) and yet ref itself still reads
	// broken this cycle: the automatic fix did not stick. One attempt is
	// the deployment's whole policy here -- see fileFixTask's own doc
	// comment on why a second attempt is not just retried outright.
	return escalateToUser(ctx, store, task, ref, health, now)
}

// refreshOutcome is what refreshStaleHead did about a broken queue head.
// It is also the queue's whole classification of "was this head simply
// behind its base": rather than guess from a signal that cannot answer it
// (see refreshStaleHead), the queue asks GitHub to make the merge and
// reads the answer off what GitHub does.
type refreshOutcome int

const (
	// refreshUpdated: the head really was behind, the merge landed, and CI
	// is re-running against a tree somebody would actually merge. Nothing
	// is filed this cycle -- the next one decides on the fresh answer.
	refreshUpdated refreshOutcome = iota
	// refreshUpToDate: GitHub's own 204. The head branch already contained
	// its base, so whatever CI reported, it reported about the tree that
	// would merge. Nothing was stale and the failure is genuine.
	refreshUpToDate
	// refreshConflicted: GitHub's own 409. Behind *and* genuinely
	// conflicting, which is exactly the case a fix task is for -- and now
	// one the queue has watched fail rather than inferred.
	refreshConflicted
	// refreshUnreachable: there was no merge to attempt. A pull request
	// from a fork has its head branch in another repository, which POST
	// /merges answers 404 for, and a detail with no branch names on it
	// (very nearly unreachable) has nothing to name in the call.
	refreshUnreachable
	// refreshAlreadyTried: this pull request has had the one attempt it
	// gets. See Observation.MergeQueueRefreshedAt.
	refreshAlreadyTried
)

// refreshStaleHead is the cheap half of repairing a broken queue head:
// before a fix task is filed and an agent dispatched, merge the pull
// request's base branch into its head branch and let the next cycle
// decide on CI that ran against a tree somebody would actually merge.
//
// Nearly every automatic fix this deployment has filed was resolved by
// exactly this commit -- a plain merge of main into the task's branch,
// with nothing to arbitrate (docs/merge-queue-staleness.md lists them).
// The shape is always the same: a branch whose checks last ran against a
// base that has since moved, so either GitHub can no longer compute a
// merge ref and every `on: pull_request` job dies at checkout (which
// healthFrom reads as PrFailing -- the jobs exist, they completed, they
// concluded `failure`), or the verdict that did report is about a tree
// nobody would ever merge. Either way the fix task's body would name
// failures that are not what is actually wrong with the branch, so the
// agent starts from a false description of its own job.
//
// The queue deliberately does not classify "behind and stale" against
// "genuinely failing" before acting, because neither available signal
// answers it. detail.MergeableState is one value where two independent
// facts are wanted, and `dirty` and `blocked` outrank `behind` -- so a
// pull request that is both behind and failing a required check reports
// `blocked` and never says "behind" at all, which is precisely this case.
// A check run carries no sha of the base it was merged onto, either; the
// staleness lives in the merge ref, which the Checks API does not name.
// So the queue asks the question it can get an authoritative answer to,
// at the moment it matters, with the write it wants to make anyway: 201
// means it was behind and is not now, 204 means nothing was stale, 409
// means it conflicts for real. The 204 is the whole answer to "is this
// failure genuine".
//
// One attempt per pull request, persisted
// (Observation.MergeQueueRefreshedAt), mirroring fileFixTask's own
// one-fix-then-a-person policy for the same reason: a refresh that lands
// and still leaves the branch red has told us the failure is real, and
// re-merging every time the base moves again would spend writes re-asking
// a question that has been answered. The attempt is recorded whether or
// not the merge landed -- what must not repeat is the *attempt*, not just
// its success.
//
// A merge, not a rebase, and made by GitHub rather than by a clone here:
// a rebase would need a force push to a branch an agent may still hold a
// clone of, would move the base out from under any stacked fix branch
// (fileFixTask's Base *is* this head branch), and would discard review
// history. The merge is also exactly what the agents were doing by hand.
func refreshStaleHead(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, obs *model.Observation, ref model.PullRequestRef,
	detail github.PullRequestDetail, now time.Time) (refreshOutcome, error) {

	if obs != nil && obs.MergeQueueRefreshedAt != nil {
		return refreshAlreadyTried, nil
	}
	if detail.HeadRef == "" || detail.BaseRef == "" {
		return refreshUnreachable, nil
	}

	result, err := client.MergeBranch(ref.Repo.Owner, ref.Repo.Name,
		detail.HeadRef, detail.BaseRef, refreshCommitMessage(detail))
	switch {
	case err == nil:
	case github.IsMergeConflict(err):
		if err := recordRefreshAttempt(ctx, store, task.ID, now); err != nil {
			return refreshConflicted, err
		}
		return refreshConflicted, nil
	case notFound(err):
		// The head branch is not in this repository -- a pull request from
		// a fork, which grain never opens itself (finishWithPullRequest
		// pushes into the target repo) but may well have found -- or it is
		// gone. Nothing to refresh, and the attempt is recorded so the
		// next cycle does not re-ask.
		if err := recordRefreshAttempt(ctx, store, task.ID, now); err != nil {
			return refreshUnreachable, err
		}
		return refreshUnreachable, nil
	default:
		// An ordinary failure, including a permission this credential does
		// not hold: not latched the way checksUnavailable is, since this
		// is a write on the same permission MergePullRequest already
		// needs, so a deployment that cannot make it has a broken merge
		// queue anyway and should say so loudly.
		return refreshUnreachable, fmt.Errorf(
			"orchestrator: merging %q into %q for %s: %w", detail.BaseRef, detail.HeadRef, ref, err)
	}

	// GitHub's 204: nothing was written because nothing was behind. No
	// attempt is recorded -- there was no merge to not repeat, and the fix
	// task filed this cycle is what stops the queue coming back here
	// anyway (advanceMergeQueueHead's own hasFix check).
	if !result.Merged {
		return refreshUpToDate, nil
	}

	if err := recordRefreshAttempt(ctx, store, task.ID, now); err != nil {
		return refreshUpdated, err
	}
	// Not optional: a human working in this branch who did not want a
	// merge commit has one now, and the only thing that tells them who
	// made it is this.
	comment := fmt.Sprintf(
		"%s last had its checks run against a `%s` that has moved since, so the merge "+
			"queue merged `%s` into `%s` rather than filing a fix for a failure that "+
			"may not be real. CI is re-running against a tree that could actually "+
			"merge; if it is still broken next cycle, a fix task is filed then.",
		ref, detail.BaseRef, detail.BaseRef, detail.HeadRef,
	)
	if err := queueComment(ctx, store, task.ID, comment, now); err != nil {
		return refreshUpdated, err
	}
	return refreshUpdated, nil
}

// refreshCommitMessage is what the merge queue's own merge commit says:
// who made it and why, since the first person to find it will be reading
// `git log` on a branch they thought only an agent had touched.
func refreshCommitMessage(detail github.PullRequestDetail) string {
	return fmt.Sprintf(
		"Merge %s into %s (grain merge queue)\n\n"+
			"Its checks last ran against a base that has moved since; re-running\n"+
			"them against a tree that could actually merge.",
		detail.BaseRef, detail.HeadRef)
}

// recordRefreshAttempt writes down that this pull request has had its one
// refresh -- see Observation.MergeQueueRefreshedAt for why this is
// persisted where the CI clocks above are not.
func recordRefreshAttempt(ctx context.Context, store *model.Store, taskID string, now time.Time) error {
	return observeField(ctx, store, taskID, now, func(o *model.Observation) { o.MergeQueueRefreshedAt = &now })
}

// notFound reports whether err is GitHub answering 404 -- for the merges
// endpoint, a branch that is not in this repository at all.
func notFound(err error) bool {
	var e *github.Error
	return errors.As(err, &e) && e.Status == 404
}

// fixTaskLink returns the ID task's own LinkFixTask names, if it has filed
// one already.
func fixTaskLink(task model.Task) (string, bool) {
	for _, l := range task.Links {
		if l.Kind == model.LinkFixTask {
			return l.Target, true
		}
	}
	return "", false
}

// defaultFixTaskDeadline is the third clock the merge queue runs, and the
// one that bounds advanceMergeQueueHead's wait: how long a queue head is
// left waiting on the fix task filed for it before the queue gives up and
// asks a person.
//
// Without it the wait is unbounded, and by a route neither of the two CI
// clocks above reaches. A head with a fix in flight reads PrConflicted or
// PrFailing, not PrPending, so defaultCheckStallDeadline never times it;
// and the fix task's own pull request, which may be the thing hanging, is
// never a queue head itself (isQueueMember excludes it), so nothing times
// that either. Anything that leaves the fix task open forever -- its own
// checks wedged, a dispatch that fails without closing it, an agent run
// that never comes back -- would otherwise hold its parent at the head of
// the repo's queue indefinitely, with everything behind it waiting: the
// same stall defaultCheckStallDeadline closes, reached from the other
// side.
//
// Six hours is chosen against what a fix task honestly needs: it has to
// wait its turn to be dispatched (briefly -- fileFixTask files it at the
// very head of the backlog, which is the order Store.Ready dispatches
// in), run an agent (capped at DefaultMaxRunRuntime, two hours), open a
// pull request and get its own CI through. That is comfortably inside
// six hours even when every stage takes longer than usual, and the cost
// of erring long is one that is paid once per stuck head rather than per
// cycle. Erring short is worse here than it is for CI: giving up throws
// away an automatic fix that might have been minutes from landing, and
// the queue never files a second one.
//
// Measured from the fix task's own CreatedAt, which fileFixTask stamps in
// the same cycle it writes LinkFixTask, rather than from an in-memory
// sighting like the CI clocks: this one is naturally persisted already,
// so a restart does not hand a stuck head another six hours.
const defaultFixTaskDeadline = 6 * time.Hour

// fixTaskOverdue reports whether the fix task filed for a queue head has
// been unfinished for longer than defaultFixTaskDeadline. Call it only
// for a fix task that has not reached StateClosed.
//
// A fix task the store has no row for counts as overdue immediately: the
// link names something that can never reach StateClosed, so waiting on it
// is waiting on nothing. That is unreachable today (nothing deletes
// tasks) and is here so the unreachable case fails toward asking a person
// rather than back into the stall this deadline exists to end. A row with
// no CreatedAt -- also unreachable, since fileFixTask always sets one --
// has no clock to consult and is waited on, the only direction that can
// be wrong without giving up on a fix that was going to work.
func fixTaskOverdue(ctx context.Context, store *model.Store, fixTaskID string, now time.Time) (bool, error) {
	fixTask, err := store.GetTask(ctx, fixTaskID)
	if err != nil {
		return false, fmt.Errorf("orchestrator: reading fix task %s: %w", fixTaskID, err)
	}
	if fixTask == nil {
		return true, nil
	}
	if fixTask.CreatedAt == nil {
		return false, nil
	}
	// Compared against the deadline rather than as `now.Sub(filed) >=
	// deadline`, so a clock that jumped backwards reads as "not overdue
	// yet" -- the same direction the CI clocks err in.
	return !now.Before(fixTask.CreatedAt.Add(defaultFixTaskDeadline)), nil
}

// healthReason renders why a PR is not mergeable, for a human or an
// agent reading the fix task's own body. A failing PR names the checks
// that failed (failingChecks) rather than saying only that something
// did: the agent sent to repair it starts from that list, and "one or
// more required checks are failing" left it to go and find out which.
//
// A conflict the queue has just watched GitHub refuse (refreshConflicted)
// is described as that rather than as the inference healthReason would
// otherwise make from detail.Mergeable: the queue tried the merge, so it
// can tell the agent what its job actually is -- a real resolution rather
// than the plain merge that fixes a merely stale branch.
func fixReason(outcome refreshOutcome, health model.PrHealth,
	detail github.PullRequestDetail, checks []github.CheckRun) string {

	if outcome == refreshConflicted {
		return fmt.Sprintf(
			"the merge queue tried to merge `%s` into `%s` and it conflicted, so this "+
				"needs a real resolution rather than a merge", detail.BaseRef, detail.HeadRef)
	}
	return healthReason(health, detail, checks)
}

func healthReason(health model.PrHealth, detail github.PullRequestDetail, checks []github.CheckRun) string {
	switch health {
	case model.PrConflicted:
		return fmt.Sprintf("it has conflicts with `%s`", detail.BaseRef)
	case model.PrFailing:
		failing := failingChecks(checks)
		if len(failing) == 0 {
			return "one or more required checks are failing"
		}
		return fmt.Sprintf("its checks are failing (`%s`)", strings.Join(failing, "`, `"))
	default:
		return "it is not mergeable"
	}
}

// failingJobLogs is the rest of what a fix task needs and healthReason
// cannot give it: not just which jobs are red, but what they printed.
//
// healthReason already names the failing checks, which is what a person
// reading the fix task needs to know where to look. An agent dispatched
// into a sandbox needs more than that, because the sandbox is not a CI
// runner -- it may not be able to run the failing suite at all -- so
// "which test is red" has to travel with the task rather than be
// rediscovered inside it. A job name plus the tail of its log is the
// difference between "one or more required checks are failing" and a
// stack trace.
//
// Best effort, deliberately: this is three extra GitHub reads
// (github.FailedJobLogs) against a credential that may not be allowed to
// make them, for a CI provider that may not be Actions at all, at the
// one moment the queue has finally decided to act on a broken head.
// Failing the whole cycle over the *annotation* on a fix task would cost
// the fix itself, so an error is logged and the body says what it said
// before this existed.
func failingJobLogs(client github.Client, ref model.PullRequestRef, headSHA string) string {
	logs, err := client.FailedJobLogs(ref.Repo.Owner, ref.Repo.Name, headSHA)
	if err != nil {
		log.Printf("orchestrator: reading the failing job logs for %s: %v -- "+
			"filing the fix task with the check names alone", ref, err)
		return ""
	}
	if len(logs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n## What CI printed\n\n" +
		"The end of each failing job's own log, copied here because the sandbox " +
		"this fix runs in is not the runner that produced it:")
	for _, l := range logs {
		fmt.Fprintf(&b, "\n\n### %s\n", l.Name)
		if l.URL != "" {
			fmt.Fprintf(&b, "\n%s\n", l.URL)
		}
		// Four backticks, so a log that itself contains a fenced block --
		// any Go test printing three backticks does -- cannot close this
		// one early.
		fmt.Fprintf(&b, "\n````\n%s\n````\n", github.JobLogExcerpt(l.Log))
		if l.Truncated {
			b.WriteString("\n(the tail of the log, not all of it)\n")
		}
	}
	return b.String()
}

// fixTaskTitle names a fix after the task it repairs -- "Resolve: Add
// pagination to the tasks API" -- where it used to be named after the
// pull request that went red ("🤖 grain: fix acme/widgets#104").
// The queue files one of these per broken head, and a list of them named
// by pull request number says only which numbers are broken, not what any
// of them is about; the source task's title is what a human scanning the
// queue already recognises. It is the fix's own pull request title too,
// since EnsurePullRequest takes Title verbatim, so it is read in the
// place a reviewer sees it as well. Nothing is lost by dropping the ref:
// the pull request, its URL and why it is broken all open the body, and
// LinkProposedBy is the machine-readable form of "after the source task".
//
// Fix tasks do not nest -- syncEntry files one only for a head that is
// not itself a fix -- so this cannot compound into "Resolve: Resolve:
// ...". The ref stays as the fallback for the one thing a title cannot
// cover: a task filed without one, which would otherwise leave a fix
// called just "Resolve:".
func fixTaskTitle(task model.Task, ref model.PullRequestRef) string {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = ref.String()
	}
	return "Resolve: " + title
}

// fileFixTask is bwsalmon/agents#283's replacement for core.py's
// _suggest_fix: where that filed a new issue labelled needs_approval_label
// and left it for a human to apply trigger_label (or comment /lgtm)
// before the agent set would touch it, this files the task straight into
// the store already approved -- Approval set, by PrincipalAutomation, so
// task_state reads it 'queued' immediately and the very next dispatch.Cycle
// dispatches it with no human in the loop. That is the issue's own "we
// will no longer suggest tasks the user needs to approve for this."
//
// The fix task's Base is ref's own head branch and its AutoMerge is set,
// the same stacked-branch trick core.py's own _suggest_fix used: a fresh
// branch built on top of ref's branch is a stacked PR, and AutoMerge is
// what lets syncEntry merge that stacked PR straight back into ref's
// branch once it reads clean, with no separate review of the fix itself.
// Both were directive lines written into an issue body before; they are
// columns set directly now. LinkFixTask on task is what stops this from running a
// second time next cycle (advanceMergeQueueHead checks it first), and what
// lets a later cycle find the fix task again once it finishes to decide
// whether ref is fixed. LinkProposedBy on fixTask itself is the same
// provenance relayProposedTasks records for a propose_task call: this
// task exists because task did, and the UI reads it the same way
// regardless of which path filed the task it is showing.
//
// The fix task is filed straight into the store, and a comment on the
// task it repairs says so -- both writes grain owns, where the GitHub
// version needed an issue created for the fix and a comment posted on the
// original task's own issue.
//
// Origin.Reason is ReasonFix, which is more than provenance: a run of a
// task filed here is a *merger* (model.OriginReason.Merger), so it draws
// on the capacity model.Limits.Mergers keeps back for exactly this --
// capacity ordinary work cannot reach, so a repair does not have to wait
// out whatever else the deployment is running. Filing at the head of the
// backlog decides when it runs; that decides whether there is room to.
//
// Its body carries the failure itself, not just the verdict: fixReason
// names the jobs that went red -- or, for a head refreshStaleHead has
// just watched GitHub refuse to merge, says that in place of the
// inference healthReason would have made -- and for a failing pull
// request failingJobLogs appends what those jobs printed. The comment on the
// parent task stays the one-line version -- it is read by a person
// watching the queue, who has the pull request itself a click away, where
// the agent dispatched into a sandbox has neither.
func fileFixTask(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, ref model.PullRequestRef, detail github.PullRequestDetail,
	health model.PrHealth, checks []github.CheckRun, outcome refreshOutcome, now time.Time) error {

	reason := fixReason(outcome, health, detail, checks)
	queue := model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: allocating an id for a fix task for %s: %w", task.ID, err)
	}
	// At the very head of the backlog: ahead of the queue this repairs,
	// which showQueueAtFrontOfBacklog has already put at the front of it,
	// and so ahead of everything else. That is bwsalmon/agents#389's "a
	// queue head's repair must not wait behind unrelated new work" said as
	// a position rather than as a sort rule inside Store.Ready, which is
	// where it used to live and where nobody could see it -- the backlog
	// now shows the repair first because it really is dispatched first.
	orderKey, err := store.OrderKeyForNewTask(ctx, true)
	if err != nil {
		return fmt.Errorf("orchestrator: placing a fix task for %s at the head of the backlog: %w", task.ID, err)
	}
	title := fixTaskTitle(task, ref)
	body := fmt.Sprintf(
		"Task %s opened %s (%s), but %s.\n\n"+
			"This is an automatic fix, filed by the merge queue: it works from "+
			"`%s` (the same branch) and, once it succeeds, its own pull request "+
			"is merged straight back into `%s` -- no approval needed, since the "+
			"merge queue dispatches it itself.",
		task.ID, ref, detail.HTMLURL, reason, detail.HeadRef, detail.HeadRef,
	)
	if health == model.PrFailing {
		body += failingJobLogs(client, ref, detail.HeadSHA)
	}

	fixTask := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  title,
		Body:   body,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: queue},
			Reason:      model.ReasonFix,
		},
		Approval:  &model.Attribution{Actor: queue},
		Target:    &ref.Repo,
		Binding:   model.BindingDirective,
		Base:      detail.HeadRef,
		AutoMerge: true,
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: task.ID}},
		CreatedAt: &now,
		OrderKey:  orderKey,
	}
	if err := store.PutTask(ctx, fixTask); err != nil {
		return fmt.Errorf("orchestrator: filing fix task %s: %w", fixTask.ID, err)
	}

	// Through UpdateTask, for the reason finishWithPullRequest gives.
	if err := store.UpdateTask(ctx, task.ID, func(t *model.Task) error {
		t.Links = append(t.Links, model.Link{Kind: model.LinkFixTask, Target: fixTask.ID})
		return nil
	}); err != nil {
		return fmt.Errorf("orchestrator: linking %s to its fix task %s: %w", task.ID, fixTask.ID, err)
	}

	comment := fmt.Sprintf(
		"%s %s -- filed task %s to fix it automatically. No approval needed: the "+
			"merge queue will run it and, if it succeeds, merge it straight back "+
			"into this branch.", ref, reason, fixTask.ID,
	)
	if err := queueComment(ctx, store, task.ID, comment, now); err != nil {
		return err
	}
	return nil
}

// queueComment records something the merge queue said about a task,
// attributed to it rather than to grain generally -- the merge queue is
// its own automation principal, and a human reading the conversation can
// tell its remarks from a relayed agent's.
func queueComment(ctx context.Context, store *model.Store, taskID, body string, now time.Time) error {
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    taskID,
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}},
		Body:      body,
		CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("orchestrator: commenting on %s: %w", taskID, err)
	}
	return nil
}

// blockMergeQueue is the merge queue's other exit besides merging, and
// bwsalmon/agents#283's "mark the task as needing user input and move
// onto the next task in the queue." Marking is obs.MergeQueueBlockedAt --
// which queueHeads reads to exclude task from ever being a queue head
// again, freeing whatever is behind it to become head next cycle instead
// of waiting on a task nothing further will fix by itself -- plus a
// comment on the task's own issue, since a label a human would need to
// know to look for is easy to miss and a comment lands wherever they're
// already watching.
//
// Blocked is not abandoned: syncEntry still merges such a task the moment
// it reads clean, so whatever the comment asked a person to do, doing it
// is enough. What is given up is the queue position and the automatic
// fix, not the merge.
//
// The three callers below are the three reasons the queue gives up, and
// they say different things to the person now holding it: escalateToUser
// for a fix that ran and did not take, escalateUnfinishedFix for one that
// never finished running at all, escalateStalledChecks for CI that never
// reported.
func blockMergeQueue(ctx context.Context, store *model.Store, taskID, comment string, now time.Time) error {
	if err := queueComment(ctx, store, taskID, comment, now); err != nil {
		return err
	}
	return observeField(ctx, store, taskID, now, func(o *model.Observation) { o.MergeQueueBlockedAt = &now })
}

// escalateToUser gives up on task because its automatic fix ran and
// finished and its PR is still broken. (A fix that has not finished at
// all is escalateUnfinishedFix's, and says so differently.)
//
// This never runs a second automatic fix for the same PR. core.py's own
// _suggest_fix reasoning ("suggesting a fix for a fix risks an unbounded
// chain") applies just as much to retrying a fix that already failed once
// without anything about the PR having changed in between.
func escalateToUser(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, health model.PrHealth, now time.Time) error {

	comment := fmt.Sprintf(
		"The automatic fix for %s didn't take -- %s -- so this needs a person. "+
			"Push a fix by hand (or resolve it directly on GitHub) and %s will "+
			"merge as soon as it reads clean. The merge queue has moved on to "+
			"the next task in %s.",
		ref, healthReasonSuffix(health), ref, ref.Repo,
	)
	return blockMergeQueue(ctx, store, task.ID, comment, now)
}

// escalateUnfinishedFix gives up on task because the fix filed for it has
// been neither finished nor abandoned for longer than
// defaultFixTaskDeadline, so the head has been holding its queue position
// on a repair that is not arriving.
//
// The fix task itself is left exactly as it is, running or queued or
// waiting on its own checks. Nothing here can tell which of those it is
// stuck in, and cancelling it would throw away work for no gain: if it
// does finish and go clean, syncEntry still merges it into ref's branch
// unconditionally (a fix task is never a queue member), and ref itself,
// now blocked rather than abandoned, still merges the moment it reads
// clean. What the queue gives up is the position and the waiting, not
// either merge.
func escalateUnfinishedFix(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, fixTaskID string,
	health model.PrHealth, now time.Time) error {

	comment := fmt.Sprintf(
		"The automatic fix for %s never finished -- task %s was filed to repair it "+
			"more than %s ago and still hasn't run to completion, and %s -- so this "+
			"needs a person. Have a look at %s (its run, or its own checks, may be "+
			"stuck); either way, pushing a fix by hand is enough, and %s will merge "+
			"as soon as it reads clean. The merge queue has moved on to the next "+
			"task in %s.",
		ref, fixTaskID, humanDuration(defaultFixTaskDeadline), healthReasonSuffix(health),
		fixTaskID, ref, ref.Repo,
	)
	return blockMergeQueue(ctx, store, task.ID, comment, now)
}

// escalateStalledChecks gives up on task for the third reason: its checks
// have been unfinished for longer than deadline (checksStalled), so the
// queue has been holding its own head position on a signal that is not
// coming.
//
// No fix task is filed first, which is the one way this differs in shape
// from every other way the queue gives up. A fix task exists to repair a
// pull request, and CI that never reports has said nothing about the pull
// request being broken -- the usual causes sit entirely outside it, and
// dispatching an agent to "fix" a workflow waiting on an approval would
// spend a run to produce, at best, nothing. So this goes straight to the
// person, naming what it was waiting on.
func escalateStalledChecks(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, checks []github.CheckRun,
	deadline time.Duration, now time.Time) error {

	comment := fmt.Sprintf(
		"%s has had %s for more than %s, so the merge queue has stopped waiting on "+
			"it and moved on to the next task in %s. Nothing has failed, so there is "+
			"no automatic fix to file -- a check that never finishes is usually "+
			"waiting on something outside the pull request: a workflow approval "+
			"nobody gave, a runner that never picked the job up, a provider that "+
			"reported \"queued\" and went away. Have a look at it (or merge %s by "+
			"hand); if the checks do finish and read clean, it will still merge on "+
			"its own.",
		ref, stalledChecksPhrase(checks), humanDuration(deadline), ref.Repo, ref,
	)
	return blockMergeQueue(ctx, store, task.ID, comment, now)
}

// stalledChecksPhrase names what the queue has been waiting on, for the
// comment above. The no-checks-at-all case is reachable despite the
// registration window: that window only settles a commit GitHub has named
// a head sha for (emptyChecksSettled), so a pull request GitHub never
// finishes thinking about reads PENDING with nothing to name.
func stalledChecksPhrase(checks []github.CheckRun) string {
	unfinished := unfinishedChecks(checks)
	if len(unfinished) == 0 {
		return "no check reported against it at all"
	}
	return fmt.Sprintf("checks that have not finished (`%s`)", strings.Join(unfinished, "`, `"))
}

// humanDuration renders d for a person reading a comment. Duration's own
// String is exact and unpleasant about round hours ("2h0m0s"), so the
// trailing zero units come off -- carefully, since "1h30m0s" ends in a
// "0m" that is part of the thirty.
func humanDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// healthReasonSuffix restates health without a PullRequestDetail in hand
// -- escalateToUser has already lost the one syncEntry read this cycle by
// the time it knows a fix task closed without success, and re-reading it
// only to name a base branch again is not worth another GitHub call.
func healthReasonSuffix(health model.PrHealth) string {
	if health == model.PrFailing {
		return "checks are still failing"
	}
	return "it's still conflicted"
}
