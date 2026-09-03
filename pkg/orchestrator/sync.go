package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// Two minutes is chosen against what it costs on each side. On a repo
// that does have CI it costs nothing at all: the checks appear within
// seconds and the very next tick sees them. On a repo with genuinely no
// CI it is the whole cost -- every merge waits this out, once, per head
// commit -- so it wants to be small enough not to feel like a stall and
// large enough to cover GitHub being slow to dispatch a workflow, which
// is measured in seconds but not reliably in single digits.
const defaultCheckRegistrationWindow = 2 * time.Minute

// emptyChecks remembers, per pull request, the head commit this process
// first saw an empty check list for and when it first saw it -- what
// emptyChecksSettled measures the window from -- alongside the window
// itself, so SetCheckRegistrationWindow can change it without racing a
// cycle that is reading it.
//
// Deliberately in memory rather than in a task's Observation. Persisting
// it would mean a schema column, and the fact is not worth one: losing it
// (a restart, a new leader) makes the next cycle start the window afresh,
// which costs one more window of waiting and can never merge something
// early. Erring toward waiting is the whole point of the mechanism.
//
// It is keyed by pull request and holds the head sha alongside, so a push
// to a branch that already has a pull request open -- a fix task merging
// into it, a human pushing by hand, a redispatched task -- restarts the
// window rather than inheriting the settled verdict of the commit before
// it. That push resets the check list to empty just as surely as the
// first one did.
var emptyChecks = struct {
	sync.Mutex
	window time.Duration
	seen   map[string]emptyCheckSighting
}{window: defaultCheckRegistrationWindow, seen: map[string]emptyCheckSighting{}}

type emptyCheckSighting struct {
	headSHA string
	first   time.Time
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
//
// Changing the window clears the sightings taken under the old one.
// Measuring an existing sighting against a window it was not started for
// is meaningless in the shortening direction and wrong in the lengthening
// one, and starting every window afresh is the conservative reset -- it
// can only make the next merge later, never earlier.
func SetCheckRegistrationWindow(d time.Duration) func() {
	emptyChecks.Lock()
	defer emptyChecks.Unlock()
	prev := emptyChecks.window
	emptyChecks.window = d
	emptyChecks.seen = map[string]emptyCheckSighting{}
	return func() { SetCheckRegistrationWindow(prev) }
}

// emptyChecksSettled reports whether ref's current head commit has had an
// empty check list for longer than the registration window --
// healthFrom's checksSettled. Call it only when the check list actually
// is empty: the first such call for a given commit is what starts that
// commit's window.
//
// now comes from the caller (SyncPullRequests') clock, the same one that
// recorded the sighting, so this never compares grain's clock against
// GitHub's -- which is the trap in measuring the window from a timestamp
// GitHub stamped, like the pull request's own CreatedAt.
//
// An empty headSHA is never settled, short of the window being off
// altogether. GitHub fills the head sha in asynchronously the same way it
// computes Mergeable (PullRequestDetail's own doc comments), so a read
// without one is a read of a pull request GitHub has not finished
// thinking about -- not a commit whose CI can be concluded absent.
func emptyChecksSettled(ref model.PullRequestRef, headSHA string, now time.Time) bool {
	emptyChecks.Lock()
	defer emptyChecks.Unlock()
	// A window of zero is off, and off means every path here answers the
	// way it did before the window existed -- including the one below
	// that has nothing to do with elapsed time.
	if emptyChecks.window <= 0 {
		return true
	}
	if headSHA == "" {
		return false
	}
	key := ref.String()
	sighting, ok := emptyChecks.seen[key]
	if !ok || sighting.headSHA != headSHA {
		sighting = emptyCheckSighting{headSHA: headSHA, first: now}
		emptyChecks.seen[key] = sighting
	}
	// Written as a comparison against the deadline rather than as
	// `now.Sub(first) >= window`, so a clock that jumped backwards reads
	// as "not settled yet": the same direction everything else here errs
	// in.
	return !now.Before(sighting.first.Add(emptyChecks.window))
}

// forgetEmptyChecks drops ref's sighting, called when its pull request
// closes: that is the one moment this package knows a pull request will
// never be synced again, so it is where the map stops growing. Anything
// that stops being tracked without ever reading closed leaks one small
// entry until the process restarts, which is a bound worth having over
// pruning the map against the live set every cycle -- two daemons sharing
// a process (as tests do) would then each delete the other's windows and
// leave every pull request waiting afresh every cycle.
func forgetEmptyChecks(ref model.PullRequestRef) {
	emptyChecks.Lock()
	defer emptyChecks.Unlock()
	delete(emptyChecks.seen, ref.String())
}

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
// headSHA may be empty on a PR read before GitHub filled it in; the
// Actions fallback needs a commit to scope to, so it is skipped rather
// than widened to a branch-scoped read that could return an older
// commit's runs.
func checkRunsFor(client github.Client, ref model.PullRequestRef, head, headSHA string) ([]github.CheckRun, bool, error) {
	checks, err := client.ListCheckRuns(ref.Repo.Owner, ref.Repo.Name, head)
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
// check for why it still merges unconditionally. A task the queue has
// already given up on (obs.MergeQueueBlockedAt set) is excluded as well,
// which is what lets the queue move on to the next task rather than
// waiting forever on one that needs a human.
func isQueueMember(e queueEntry) bool {
	if !e.task.AutoMerge || e.task.Origin.Reason == model.ReasonFix {
		return false
	}
	return e.obs == nil || e.obs.MergeQueueBlockedAt == nil
}

// queueHeads returns, for every target repo with at least one queued
// task, the ID of the one task in front: the earliest submitted (by
// Task.CreatedAt, ties broken by ID so the choice is deterministic) among
// entries for which isQueueMember is true. Only a repo's head task is
// ever merged or auto-fixed on a given cycle -- everything behind it
// waits, which is the whole property that makes this a queue rather than
// "merge every clean PR in whatever order GitHub happens to return them
// in": a fix filed for the second task while the first is still being
// repaired would very likely need refiling the moment the first merges
// and changes what the second is based against.
func queueHeads(entries []queueEntry) map[string]string {
	heads := map[string]string{}
	earliest := map[string]time.Time{}
	for _, e := range entries {
		if !isQueueMember(e) {
			continue
		}
		repo := e.ref.Repo.String()
		created := time.Time{}
		if e.task.CreatedAt != nil {
			created = *e.task.CreatedAt
		}
		cur, ok := heads[repo]
		if !ok || created.Before(earliest[repo]) || (created.Equal(earliest[repo]) && e.task.ID < cur) {
			heads[repo] = e.task.ID
			earliest[repo] = created
		}
	}
	return heads
}

// SyncPullRequests refreshes every completed task's tracked pull request,
// advances each target repo's merge queue by one step, and closes out the
// tasks whose PR finished -- the other half of core.py's _close_finished_prs
// this package owns, plus the merge queue bwsalmon/agents#283 asked for in
// place of core.py's own _suggest_fix (a suggestion a human had to
// approve before the agent set would attempt it). See queueHeads and
// syncEntry for the queue itself; this function only gathers the state
// every entry's decision needs before any of them act, so a decision
// about task N never depends on an in-progress change queueHeads has not
// seen yet.
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

	// Acting on one entry is isolated, because head-of-queue was already
	// decided above against the complete set: an entry that fails here
	// cannot make another entry merge that would not have merged anyway
	// (syncEntry merges an ordinary queue member only when it is its
	// repo's head), so the worst a failure costs the others is a cycle of
	// latency -- which is what returning early used to cost all of them.
	heads := queueHeads(entries)
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
		if checksKnown && len(checks) == 0 {
			checksSettled = emptyChecksSettled(ref, detail.HeadSHA, now)
		}
	}
	health := healthFrom(detail, checks, checksKnown, checksSettled)

	isFixTask := task.Origin.Reason == model.ReasonFix
	isHead := heads[ref.Repo.String()] == task.ID
	blocked := e.obs != nil && e.obs.MergeQueueBlockedAt != nil

	// A fix task always merges into the branch it repairs the moment it
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
	// PrPending matches neither arm on purpose, and neither does
	// PrUnknown: CI is still running, or has not reported for the first
	// time yet, or its verdict is unreadable -- so in every case there
	// is nothing to merge yet and nothing to file a fix for. The entry
	// is simply left where it is -- still the head of its queue, holding
	// the position -- and the next cycle asks GitHub again. That is what
	// makes a merge wait for the tests rather than race them,
	// and it is deliberately the same "do nothing, look again" the
	// asynchronous Mergeable read has always taken.
	switch {
	case health == model.PrClean && task.AutoMerge && (isFixTask || isHead || blocked):
		if err := client.MergePullRequest(ref.Repo.Owner, ref.Repo.Name, ref.Number); err != nil {
			return fmt.Errorf("orchestrator: auto-merging %s: %w", ref, err)
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

	case isHead && !isFixTask && !blocked && (health == model.PrConflicted || health == model.PrFailing):
		if err := advanceMergeQueueHead(ctx, store, task, ref, detail, health, checks, now); err != nil {
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
// OpenPullRequestLinks (task_state stops reading 'completed' once
// closed_at is set) and syncEntry never runs for it again, so there is no
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
// merge queue, its PR conflicted or failing checks -- progress: file an
// automatic fix the first time this happens, or notice the fix already
// filed has finished and decide whether that resolved things.
func advanceMergeQueueHead(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, detail github.PullRequestDetail,
	health model.PrHealth, checks []github.CheckRun, now time.Time) error {

	fixTaskID, hasFix := fixTaskLink(task)
	if !hasFix {
		return fileFixTask(ctx, store, task, ref, detail, health, checks, now)
	}

	fixState, err := store.State(ctx, fixTaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading fix task %s for %s: %w", fixTaskID, task.ID, err)
	}
	if fixState != model.StateClosed {
		// Still running, or its own PR is still open and being watched by
		// this same SyncPullRequests call -- nothing to do until it
		// finishes one way or the other.
		return nil
	}
	// The fix task ran to completion (its own PR merged into ref's branch,
	// or was closed without merging) and yet ref itself still reads
	// broken this cycle: the automatic fix did not stick. One attempt is
	// the deployment's whole policy here -- see fileFixTask's own doc
	// comment on why a second attempt is not just retried outright.
	return escalateToUser(ctx, store, task, ref, health, now)
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

// healthReason renders why a PR is not mergeable, for a human or an
// agent reading the fix task's own body. A failing PR names the checks
// that failed (failingChecks) rather than saying only that something
// did: the agent sent to repair it starts from that list, and "one or
// more required checks are failing" left it to go and find out which.
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
func fileFixTask(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, detail github.PullRequestDetail,
	health model.PrHealth, checks []github.CheckRun, now time.Time) error {

	reason := healthReason(health, detail, checks)
	queue := model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: allocating an id for a fix task for %s: %w", task.ID, err)
	}
	title := fmt.Sprintf("\U0001F916 grain: fix %s", ref)
	body := fmt.Sprintf(
		"Task %s opened %s (%s), but %s.\n\n"+
			"This is an automatic fix, filed by the merge queue: it works from "+
			"`%s` (the same branch) and, once it succeeds, its own pull request "+
			"is merged straight back into `%s` -- no approval needed, since the "+
			"merge queue dispatches it itself.",
		task.ID, ref, detail.HTMLURL, reason, detail.HeadRef, detail.HeadRef,
	)

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

// escalateToUser is the merge queue's other exit besides merging: task's
// automatic fix ran and finished, but its PR is still broken, so this is
// bwsalmon/agents#283's "mark the task as needing user input and move
// onto the next task in the queue." Marking is obs.MergeQueueBlockedAt --
// which queueHeads reads to exclude task from ever being a queue head
// again, freeing whatever is behind it to become head next cycle instead
// of waiting on a task nothing further will fix by itself -- plus a
// comment on the task's own issue, since a label a human would need to
// know to look for is easy to miss and a comment lands wherever they're
// already watching.
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
	if err := queueComment(ctx, store, task.ID, comment, now); err != nil {
		return err
	}
	return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.MergeQueueBlockedAt = &now })
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
