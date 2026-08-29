package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// healthFrom computes a model.PrHealth from a fresh GitHub read, the pure
// half of SyncPullRequests split out so it needs no store or client to
// test. See model.PrHealth's own doc comment for why PrMerged never comes
// back from here: detail.State folds a merged PR and a closed-without-
// merging one into the same "closed" string, which github.
// RESTClient.GetPullRequest's own doc comment already treats as one
// outcome rather than two.
//
// Only a "failure" conclusion reads as PrFailing. GitHub's Checks API also
// reports "cancelled", "timed_out", "action_required" and others
// CheckRun's own doc comment says are a caller's policy to interpret, not
// this package's; treating every non-"success" completed run as failing
// would make a merge queue's own "cancelled, will retry" check block a PR
// this package has no business blocking.
func healthFrom(detail github.PullRequestDetail, checks []github.CheckRun) model.PrHealth {
	if detail.State == "closed" {
		return model.PrClosed
	}
	if detail.Mergeable != nil && !*detail.Mergeable {
		return model.PrConflicted
	}
	for _, c := range checks {
		if c.Status != "completed" {
			continue
		}
		if c.Conclusion != nil && *c.Conclusion == "failure" {
			return model.PrFailing
		}
	}
	if detail.Mergeable == nil {
		return model.PrUnknown
	}
	return model.PrClean
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
	checks, err := client.ListCheckRuns(ref.Repo.Owner, ref.Repo.Name, detail.HeadRef)
	if err != nil {
		return fmt.Errorf("orchestrator: reading check runs for %s: %w", ref, err)
	}
	health := healthFrom(detail, checks)

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
		health = healthFrom(detail, checks)

	case isHead && !isFixTask && !blocked && (health == model.PrConflicted || health == model.PrFailing):
		if err := advanceMergeQueueHead(ctx, store, task, ref, detail, health, now); err != nil {
			return err
		}
	}

	if health != model.PrClosed && health != model.PrMerged {
		return nil
	}

	// Closing out is one write now. It used to be two -- close the task's
	// GitHub issue, then record the closure -- with the issue closed first
	// and the store told second, so a crash in between left a closed issue
	// that grain still believed was open.
	return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.ClosedAt = &now })
}

// advanceMergeQueueHead is what makes task -- the head of its repo's
// merge queue, its PR conflicted or failing checks -- progress: file an
// automatic fix the first time this happens, or notice the fix already
// filed has finished and decide whether that resolved things.
func advanceMergeQueueHead(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, detail github.PullRequestDetail,
	health model.PrHealth, now time.Time) error {

	fixTaskID, hasFix := fixTaskLink(task)
	if !hasFix {
		return fileFixTask(ctx, store, task, ref, detail, health, now)
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
// agent reading the fix task's own body.
func healthReason(health model.PrHealth, detail github.PullRequestDetail) string {
	switch health {
	case model.PrConflicted:
		return fmt.Sprintf("it has conflicts with `%s`", detail.BaseRef)
	case model.PrFailing:
		return "one or more required checks are failing"
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
	health model.PrHealth, now time.Time) error {

	reason := healthReason(health, detail)
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
