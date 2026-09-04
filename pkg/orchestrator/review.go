package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// reviewer is the automation principal every review task is attributed
// to -- its own actor, distinct from "schedule" (a schedule's firing),
// "merge-queue" (automatic PR repair) and "grain" (relayed agent
// output), so a human reading a task's conversation or origin can tell
// which of grain's own mechanisms filed it.
var reviewer = model.Principal{Kind: model.PrincipalAutomation, ID: "review"}

// SyncReviews files the review a task declares (Task.ReviewTemplateID)
// once that task's own work is done -- grain/task-284's "when the task
// has completed a new agent should be spun up on the same branch with
// instructions to review and fix bugs in the proposed code."
//
// "Completed" is read off the same query the merge queue reads,
// Store.OpenPullRequestLinks: a task whose run is over, whose pull
// request is open, and which has not closed. That is the moment there is
// code to review at all, and it is also the moment before the merge --
// syncEntry holds the pull request back while the review it filed is
// still in flight (reviewPending), so the sequence a task with a review
// attached goes through is work, review, merge, rather than work, merge,
// review-of-something-already-landed.
//
// Level-triggered like every other reconciler here, and for the same
// reason SyncQualifications gives: what to do is a function of what the
// store says right now, so a cycle that fails files the review on the
// next one instead, and a review attached to a task that finished
// yesterday is filed the first cycle after somebody attaches it rather
// than never. Exactly one is ever filed per task -- LinkReviewTask is
// the record of that, checked before filing and written with it -- so a
// review that ran and found nothing is not re-run every cycle for the
// rest of the task's life.
//
// It takes a *model.Store and no github.Client because it needs neither:
// the branch to review is derived (model.BranchName), the template is a
// row, and the task it files is a row. SyncPullRequests is what talks to
// GitHub about the same pull requests.
func SyncReviews(ctx context.Context, store *model.Store, now time.Time) error {
	links, err := store.OpenPullRequestLinks(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading open pull request links: %w", err)
	}

	var errs []error
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		task, err := store.GetTask(ctx, link.TaskID)
		if err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: reading task %s: %w", link.TaskID, err))
			continue
		}
		if task == nil || !reviewDue(*task) {
			continue
		}
		ref, err := model.ParsePullRequestRef(link.PullRequest)
		if err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: task %s: %w", link.TaskID, err))
			continue
		}
		if err := fileReviewTask(ctx, store, *task, ref, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reviewDue reports whether task is one SyncReviews still owes a review:
// it has one attached, it has a repo to file the review against, and no
// review has been filed for it yet.
//
// A task with no Target is unreachable here -- it could not have a pull
// request to be read out of OpenPullRequestLinks in the first place --
// and is checked anyway because the review task's own Target is taken
// straight from it.
func reviewDue(task model.Task) bool {
	if task.ReviewTemplateID == "" || task.Target == nil {
		return false
	}
	_, filed := reviewTaskLink(task)
	return !filed
}

// reviewTaskLink returns the ID task's own LinkReviewTask names, if a
// review has already been filed for it -- the counterpart of the
// LinkFixTask read the merge queue kept while it still filed a separate
// repair task, and read the same two ways: SyncReviews checks it so
// exactly one review is ever filed, and syncEntry checks it to know
// whose landing it is waiting on.
func reviewTaskLink(task model.Task) (string, bool) {
	for _, l := range task.Links {
		if l.Kind == model.LinkReviewTask {
			return l.Target, true
		}
	}
	return "", false
}

// isReviewTask reports whether task is one filed by SyncReviews -- a
// review stacked on the branch of the task it reviews, rather than a
// change of its own. syncEntry treats it exactly the way it treats a fix
// task: never a queue entry in its own right, and merged into the branch
// it was filed against the moment it reads clean.
func isReviewTask(task model.Task) bool { return task.Origin.Reason == model.ReasonReview }

// fileReviewTask files one review of task, from the template task names.
//
// The shape is the one the merge queue's own repair task had while it
// was still filed separately (model.LinkFixTask), because the situation
// is the same one: a second agent is wanted on a branch a first agent
// has already pushed, and what it does there has to end up back on that
// branch. So the
// review task's Base is task's own branch (model.BranchName -- derived,
// never self-reported, so this and the checkout agree without either
// asking the other), its AutoMerge is set, and the pull request it opens
// is merged straight back into the branch under review by syncEntry the
// moment it reads clean. Stacking is also what makes "the same branch"
// true in the only sense grain can offer it: a run gets a branch of its
// own (BranchName is a function of the task id), so a review that works
// on task's branch is one whose own branch starts there and returns
// there.
//
// It is filed already approved, by automation, for the reason
// fireTaskSchedule gives about a schedule: attaching a review to a task
// is itself a human's standing instruction to run it, and asking them to
// approve the thing they asked for would leave every review sitting as a
// proposal until somebody noticed. It is filed at the head of the
// backlog for the reason the merge queue's own repairs reach the head of
// it (Store.Ready): the change it reviews is waiting on it, and
// everything behind that change in the repo's merge queue is waiting on
// both.
//
// Three of the template's fields are used and three are deliberately
// not. Title, Body and Grants (plus Reads) are the reusable content --
// the instructions "read this branch and fix what is wrong with it"
// written once and improved once. Target and Base are not the
// template's to give (model.Template's own doc comment on why), and here
// they are decided by the task under review. AutoMerge is not the
// template's either, for a reason that is this function's own: a stacked
// review branch that never merges back into the branch it reviewed is
// work nobody will ever see, so it is set here regardless of what the
// template says about the tasks it files anywhere else.
//
// Origin.Reason is ReasonReview, which is what isReviewTask reads and
// therefore what keeps the review out of its repo's merge queue as an
// entry in its own right. It is deliberately not ReasonFix: that reason
// draws on the capacity model.Limits.Mergers keeps back for the merge
// queue's own repairs, and a review is ordinary work -- it runs an agent
// over a whole change rather than finishing one that is already nearly
// landed.
//
// The task's own body ends with what only grain can supply: which task
// is being reviewed, which branch its code is on, and where its pull
// request is. The template supplies the instructions; this supplies the
// subject.
func fileReviewTask(ctx context.Context, store *model.Store, task model.Task,
	ref model.PullRequestRef, now time.Time) error {

	tmpl, err := store.GetTemplate(ctx, task.ReviewTemplateID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading review template %s for %s: %w",
			task.ReviewTemplateID, task.ID, err)
	}
	if tmpl == nil {
		// A template deleted out from under a task -- ui.Client.
		// DeleteTemplate refuses this while a task still names one, so
		// it takes a row disappearing some other way. Reported every
		// cycle rather than swallowed, and never fatal to the tasks
		// beside it (SyncReviews collects), the same way a schedule
		// whose template has gone fails its own firing.
		return fmt.Errorf("orchestrator: reviewing %s: template %s no longer exists",
			task.ID, task.ReviewTemplateID)
	}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: allocating an id for a review of %s: %w", task.ID, err)
	}
	orderKey, err := store.OrderKeyForNewTask(ctx, true)
	if err != nil {
		return fmt.Errorf("orchestrator: placing a review of %s at the head of the backlog: %w", task.ID, err)
	}

	branch := model.BranchName(task.ID)
	reviewTask := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  reviewTaskTitle(*tmpl, task),
		Body:   reviewTaskBody(*tmpl, task, ref, branch),
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: reviewer},
			Reason:      model.ReasonReview,
		},
		Approval:  &model.Attribution{Actor: reviewer},
		Target:    task.Target,
		Binding:   model.BindingDirective,
		Base:      branch,
		Reads:     tmpl.Reads,
		Grants:    tmpl.Grants,
		AutoMerge: true,
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: task.ID}},
		CreatedAt: &now,
		OrderKey:  orderKey,
	}
	if err := store.PutTask(ctx, reviewTask); err != nil {
		return fmt.Errorf("orchestrator: filing review task %s: %w", reviewTask.ID, err)
	}

	// Through UpdateTask, for the reason finishWithPullRequest gives: the
	// task was read at the top of this cycle and something else may have
	// written to it since, so the link is appended to whatever is there
	// now rather than to the copy in hand.
	if err := store.UpdateTask(ctx, task.ID, func(t *model.Task) error {
		t.Links = append(t.Links, model.Link{Kind: model.LinkReviewTask, Target: reviewTask.ID})
		return nil
	}); err != nil {
		return fmt.Errorf("orchestrator: linking %s to its review task %s: %w", task.ID, reviewTask.ID, err)
	}

	comment := fmt.Sprintf(
		"Filed task %s to review this one's own code, from the %q template. It works "+
			"from `%s` (the same branch) and its own pull request merges straight back "+
			"into it, so %s waits to merge until the review has landed.",
		reviewTask.ID, tmpl.Name, branch, ref,
	)
	return reviewComment(ctx, store, task.ID, comment, now)
}

// reviewTaskTitle names a review after the template it came from, since
// that is what the person who attached it wrote and what they will
// recognise in a list. A template with no title of its own falls back to
// naming the task under review -- fixTaskTitle's own fallback reasoning,
// for the one thing a template cannot cover.
//
// Reviews do not nest (a review task is filed with no review template of
// its own, and only a task with one is ever reviewed), so this cannot
// compound into "Review: Review: ...".
func reviewTaskTitle(tmpl model.Template, task model.Task) string {
	if title := strings.TrimSpace(tmpl.Title); title != "" {
		return title
	}
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = task.ID
	}
	return "Review: " + title
}

// reviewTaskBody is the template's instructions with the subject of the
// review appended: the task, the branch its code is on, and its pull
// request.
//
// Appended rather than prepended, and separated by a rule, so the
// template's own opening line is still the first thing the dispatched
// agent reads -- the instructions are what the run is for, and this is
// the context they are carried out against.
func reviewTaskBody(tmpl model.Template, task model.Task, ref model.PullRequestRef, branch string) string {
	var b strings.Builder
	if body := strings.TrimSpace(tmpl.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n---\n\n")
	}
	fmt.Fprintf(&b,
		"You are reviewing task %s (%q), whose work is on `%s` and proposed in %s.\n\n"+
			"Your own branch is based on `%s`, so what you find you can also fix: commit "+
			"the fixes and push, and your pull request is merged straight back into `%s` "+
			"once it reads clean. Task %s does not merge until that has happened, so this "+
			"is the last look anybody gets at the change before it lands.",
		task.ID, task.Title, branch, ref, branch, branch, task.ID)
	return b.String()
}

// reviewComment records something the review machinery said about a
// task, attributed to it rather than to grain generally -- queueComment's
// counterpart, and its own automation principal for the same reason.
func reviewComment(ctx context.Context, store *model.Store, taskID, body string, now time.Time) error {
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    taskID,
		Author:    model.Attribution{Actor: reviewer},
		Body:      body,
		CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("orchestrator: commenting on %s: %w", taskID, err)
	}
	return nil
}

// defaultReviewTaskDeadline bounds how long a task's own merge waits on
// the review filed for it, and exists for the reason
// defaultRepairDeadline exists: without it the wait is unbounded, and
// by the same route. A task holding its merge for a review reads
// PrClean, not PrPending, so defaultCheckStallDeadline never times it,
// and the review task's own pull request is never a queue head itself
// (isQueueMember excludes it), so nothing times that either. Anything
// that leaves the review open forever -- its checks wedged, a dispatch
// that fails without closing it, a run that never comes back -- would
// otherwise hold its parent at the head of the repo's queue
// indefinitely, with everything behind it waiting.
//
// Six hours, the same as a repair's, and chosen against the same
// budget: a review waits briefly to be dispatched (it is filed at the
// head of the backlog), runs an agent (capped at DefaultMaxRunRuntime,
// two hours), opens a pull request and gets its own CI through. Erring
// long costs one wait per stuck review; erring short throws away a
// review that may have been minutes from landing.
//
// Measured from the review task's own CreatedAt, which fileReviewTask
// stamps in the same cycle it writes LinkReviewTask, so a restart does
// not hand a stuck review another six hours.
const defaultReviewTaskDeadline = 6 * time.Hour

// reviewState is what syncEntry needs to know about a queue entry's own
// review: whether the change is still waiting on one, and whether it has
// been waiting longer than anyone should.
type reviewState struct {
	// taskID is the review task's own ID, empty both when no review has
	// been filed yet and when the one filed has finished -- pending is
	// what tells those two apart.
	taskID string
	// pending is true while this task's merge is waiting on a review:
	// either one is owed and SyncReviews has not filed it yet, or the
	// one filed has not reached StateClosed. A review closes when its
	// own pull request merges into the branch under review
	// (recordPullRequestEvents), which is exactly the moment the change
	// is worth merging on.
	//
	// The owed case is deliberately part of it. Without it, whether a
	// reviewed change merged before its review was even filed would
	// depend on which of "reviews" and "sync" ran first in a cycle
	// (Reconcilers), and a merge is not a decision worth making on an
	// ordering -- so the declaration alone (Task.ReviewTemplateID) holds
	// the merge, and filing the review is what starts the clock rather
	// than what starts the wait.
	pending bool
	// overdue is true when pending has been true for longer than
	// defaultReviewTaskDeadline.
	overdue bool
}

// reviewStateOf reads what task's own review means for merging it now.
//
// The clock it measures against is whichever start the wait actually
// has: the review task's own CreatedAt once one has been filed, and the
// task's own CompletedAt while one is merely owed -- the moment there
// was something to review, and so the moment a review that never turns
// up started costing something.
//
// A review task the store has no row for counts as finished rather than
// pending: the link names something that can never reach StateClosed, so
// waiting on it is waiting on nothing. That is unreachable today
// (nothing deletes tasks) and errs toward merging rather than toward the
// indefinite hold this whole clock exists to prevent -- the opposite
// direction from fixTaskOverdue's own unreachable case, because what is
// at stake here is a merge that would otherwise never happen rather than
// a person who would otherwise never be asked.
func reviewStateOf(ctx context.Context, store *model.Store, task model.Task,
	obs *model.Observation, now time.Time) (reviewState, error) {

	reviewTaskID, filed := reviewTaskLink(task)
	if !filed {
		if task.ReviewTemplateID == "" {
			return reviewState{}, nil
		}
		s := reviewState{pending: true}
		if obs != nil && obs.CompletedAt != nil {
			s.overdue = overdue(*obs.CompletedAt, now)
		}
		return s, nil
	}

	reviewTask, err := store.GetTask(ctx, reviewTaskID)
	if err != nil {
		return reviewState{}, fmt.Errorf("orchestrator: reading review task %s: %w", reviewTaskID, err)
	}
	if reviewTask == nil {
		return reviewState{}, nil
	}
	state, err := store.State(ctx, reviewTaskID)
	if err != nil {
		return reviewState{}, fmt.Errorf("orchestrator: reading review task %s's state: %w", reviewTaskID, err)
	}
	if state == model.StateClosed {
		return reviewState{}, nil
	}
	s := reviewState{taskID: reviewTaskID, pending: true}
	if reviewTask.CreatedAt != nil {
		s.overdue = overdue(*reviewTask.CreatedAt, now)
	}
	return s, nil
}

// overdue reports whether a wait that started at from has run past
// defaultReviewTaskDeadline. Written as a comparison against the
// deadline rather than as `now.Sub(from) >= deadline`, so a clock that
// jumped backwards reads as "not overdue yet" -- the same direction
// fixTaskOverdue and the CI clocks err in.
func overdue(from, now time.Time) bool {
	return !now.Before(from.Add(defaultReviewTaskDeadline))
}

// escalateUnfinishedReview gives up on waiting for task's review, which
// has been neither finished nor abandoned for longer than
// defaultReviewTaskDeadline.
//
// blockMergeQueue, the same exit escalateUnfinishedFix takes, and with
// the same consequences: task stops being anyone's queue head, so the
// tasks behind it move, and it still merges the moment it reads clean --
// which, since its own checks are what were clean all along, is very
// likely the next cycle. That is the deliberate choice between the two
// ways to be wrong here: merging a change whose review never arrived,
// having said so on the task, is recoverable, where holding a finished
// change out of the repo forever on a review that is not coming is not.
//
// The review task itself is left exactly as it is, for the reason
// escalateUnfinishedFix leaves a fix task alone: nothing here can tell
// which stage it is stuck in, cancelling it would throw work away, and
// if it does finish it still merges into the branch it reviewed
// unconditionally.
func escalateUnfinishedReview(ctx context.Context, store *model.Store,
	task model.Task, ref model.PullRequestRef, reviewTaskID string, now time.Time) error {

	var comment string
	if reviewTaskID == "" {
		// The other way the wait can run out: no review task was ever
		// filed at all, so SyncReviews has been failing for this task for
		// the whole deadline -- a template deleted out from under it is
		// the one way that happens on purpose.
		comment = fmt.Sprintf(
			"%s has been waiting more than %s for the review attached to it, and no "+
				"review task was ever filed -- the template it names (`%s`) may be gone. "+
				"The merge queue has stopped waiting and moved on to the next task in "+
				"%s; %s merges as soon as it reads clean, reviewed or not. Check the "+
				"daemon's log for what filing the review failed on.",
			ref, humanDuration(defaultReviewTaskDeadline), task.ReviewTemplateID, ref.Repo, ref,
		)
	} else {
		comment = fmt.Sprintf(
			"The review filed for %s never finished -- task %s was filed to review it more "+
				"than %s ago and still hasn't run to completion -- so the merge queue has "+
				"stopped waiting on it and moved on to the next task in %s. Have a look at "+
				"%s (its run, or its own checks, may be stuck); %s itself merges as soon as "+
				"it reads clean, reviewed or not, so close or finish the review depending on "+
				"which you want.",
			ref, reviewTaskID, humanDuration(defaultReviewTaskDeadline), ref.Repo,
			reviewTaskID, ref,
		)
	}
	return blockMergeQueue(ctx, store, task.ID, comment, now)
}
