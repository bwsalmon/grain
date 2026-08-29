package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// firstToolCallArg returns the first argument named key from the first
// non-error call to name in result, and whether one was found. Reading
// straight off agent.Result.ToolCalls rather than a mcp.MockSink is
// deliberate: gemini.Framework.Run constructs and discards its own
// MockSink internally (its own doc comment says so), so ToolCalls is the
// only seam a caller outside that package has -- the same one
// v2/e2e/harness_test.go's askedQuestion/pushedOK helpers already use.
func firstToolCallArg(result *agent.Result, name, key string) (string, bool) {
	for _, c := range result.ToolCalls {
		if c.Name != name || c.IsError {
			continue
		}
		v, ok := c.Arguments[key].(string)
		return v, ok
	}
	return "", false
}

// proposedTaskCalls returns every non-error propose_task call's
// arguments.
func proposedTaskCalls(result *agent.Result) []map[string]any {
	var out []map[string]any
	for _, c := range result.ToolCalls {
		if c.Name == "propose_task" && !c.IsError {
			out = append(out, c.Arguments)
		}
	}
	return out
}

// ProcessResult is what a finished run's tool calls turn into: a comment
// on the task, a pull request, a proposed task -- v1's own "core.py
// writes a file, then turns it into a real effect" split, done here in
// one step since nothing else needs the intermediate file.
//
// Everything a run says now lands in the store. It used to land on the
// task's own GitHub issue, which is why this needed a task repo and an
// ExternalRef to relay to at all. A task has no issue any more (README,
// "Input is a model update, not a GitHub issue"), so a question is a
// model.Comment, a closing remark is a model.Comment, and a proposed task
// is a model.Task with no Approval. GitHub is still reached for the one
// thing that is genuinely GitHub's: the branch a run pushed, and the pull
// request opened for it.
//
// Order matters. A question ends the run's turn by contract (mcp's own
// ask_question doc comment: "after calling this, do not take any further
// actions") so it is checked, and returned on, before anything else --
// answering it is the whole reason the run stopped, and no PR exists yet
// for an ask_question turn to have opened regardless. Proposed tasks are
// relayed independent of how the run otherwise ended, since v1's own
// propose_task can accompany other work rather than replacing it.
//
// add_review_comment is not relayed here at all: doing so needs a PR
// already in hand to attach a draft review to, which only a /review-intent
// dispatch (not yet built -- see directives.go) would have. A run that
// calls it today gets ProcessResult's ordinary "nothing to act on" ending;
// nothing is lost, since ask_question/comment_on_issue/propose_task's own
// contracts already say a run should call one of the four, not several.
//
// runID is d.RunID, the very run result came from -- needed only for the
// "nothing to act on" ending below, to correct that run's own outcome
// (see the comment there for why RunDispatch's own guess is not good
// enough). Every other ending here already has an outcome RunDispatch got
// right without ProcessResult's help.
func ProcessResult(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, result *agent.Result, runID string, now time.Time) error {

	if err := relayProposedTasks(ctx, store, task, result, now); err != nil {
		return err
	}

	if question, ok := firstToolCallArg(result, "ask_question", "question"); ok && question != "" {
		commentID, err := relayComment(ctx, store, task, question, now)
		if err != nil {
			return fmt.Errorf("orchestrator: posting question for %s: %w", task.ID, err)
		}
		return observeField(ctx, store, task.ID, now, func(o *model.Observation) {
			o.PendingQuestionCommentID = &commentID
		})
	}

	pushed, err := client.BranchExists(task.Target.Owner, task.Target.Name, model.BranchName(task.ID))
	if err != nil {
		return fmt.Errorf("orchestrator: checking %s's branch: %w", task.ID, err)
	}

	if pushed {
		// task was read at the top of this cycle, before the run itself
		// finished, so it cannot see a close that landed while the run was
		// still live -- re-checking the observation here is what
		// model/state.go's StateOf precedence (ClosedAt outranks a live
		// run) actually means for a run that already pushed: nobody wants
		// this work merged, so the branch is left pushed but unopened
		// rather than turned into a real pull request on GitHub.
		closed, err := taskClosed(ctx, store, task.ID)
		if err != nil {
			return err
		}
		if closed {
			return nil
		}
		return finishWithPullRequest(ctx, store, client, task, now)
	}

	if comment, ok := firstToolCallArg(result, "comment_on_issue", "comment"); ok && comment != "" {
		if _, err := relayComment(ctx, store, task, comment, now); err != nil {
			return fmt.Errorf("orchestrator: posting closing comment for %s: %w", task.ID, err)
		}
		return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.CompletedAt = &now })
	}

	// Neither a push, a question, nor a closing comment: the run produced
	// nothing to act on (the failScript case in the e2e harness this
	// mirrors). Left running-less and un-observed, it is eligible for
	// another attempt the next time dispatch.Cycle looks at task_ready --
	// task_streak (schema.go) and dispatch.Cycle's own backoff are what
	// keep that bounded now rather than unconditional (bwsalmon/agents#403).
	//
	// That bound only works if this run's own outcome says so: RunDispatch
	// already called FinishRun before ProcessResult ever ran, using
	// outcomeOf's own guess -- "succeeded" the moment the agent made any
	// harmless tool call, since RunDispatch has no way to check GitHub or
	// task_observation itself. A run that, say, ran a few shell commands
	// and then gave up without pushing, asking, or leaving a comment would
	// otherwise sit in task_run as a permanent "succeeded" that never
	// counts toward the streak, dodging the very cap this ending exists to
	// feed. Overwriting it here, now that a push/question/comment has
	// actually been ruled out, is what keeps task_run's own outcome
	// column meaning what a human reading `grain get` would expect it to.
	if err := store.SetRunOutcome(ctx, runID, "no_action",
		"the run finished without pushing a branch, asking a question, or leaving a closing comment"); err != nil {
		return fmt.Errorf("orchestrator: recording %s's outcome: %w", task.ID, err)
	}
	return nil
}

// relayComment records something a dispatched run said, attributed as
// grain relaying an agent: (automation, on behalf of agent).
//
// That attribution is the whole reason model.Comment carries an
// Attribution rather than a bare Principal. v1 could only gesture at it
// by looking for a signature substring in an issue comment's body, since
// GitHub knows one entity -- the account the token belongs to -- and
// grain has three.
func relayComment(ctx context.Context, store *model.Store, task model.Task,
	body string, now time.Time) (int64, error) {

	agentPrincipal := model.Principal{Kind: model.PrincipalAgent, ID: task.ID}
	return store.AddComment(ctx, model.Comment{
		TaskID: task.ID,
		Author: model.Attribution{
			Actor:      model.Principal{Kind: model.PrincipalAutomation, ID: "grain"},
			OnBehalfOf: &agentPrincipal,
		},
		Body:      body,
		CreatedAt: now,
	})
}

// relayProposedTasks files each propose_task call as a real task with no
// Approval -- which model.StateOf reads as 'proposed', so it sits in the
// store waiting for a human to approve it and is never dispatchable
// meanwhile. That is exactly proposeTaskTool's own contract ("requires a
// human to ... before the agent set will ever attempt it"), enforced by
// the state machine now rather than by withholding a label.
//
// The proposal is linked back to the task that made it
// (model.LinkProposedBy, provenance only -- it blocks nothing), which the
// GitHub-issue version had no way to record.
//
// AutoMerge inherits from task, the job that proposed it, whenever the
// proposal asks for nothing beyond what task itself was granted --
// model.GrantsSubsetOf's own doc comment is the reasoning. A proposal
// cannot yet ask for a capability at all (proposeTaskTool's own input
// schema has no such field, so Grants is always empty here), which makes
// the check trivially true today; it is written as a real comparison
// rather than a bare `task.AutoMerge` so it stays correct the day a
// capability request is added to that schema. This only changes what
// happens once a human has approved and run the proposal and its PR
// merges cleanly -- it does not touch Approval, so a proposed task still
// needs a human to approve it before it ever runs, the same as any other.
//
// depends_on referring to another call's local `id` in the same run is
// still not resolved: doing that needs holding every proposal until the
// whole batch is filed and rewriting cross-references afterward, which is
// follow-on work. Today each proposal's depends_on stays in the body
// verbatim for a human to resolve by hand.
func relayProposedTasks(ctx context.Context, store *model.Store, task model.Task,
	result *agent.Result, now time.Time) error {

	for _, p := range proposedTaskCalls(result) {
		title, _ := p["title"].(string)
		body, _ := p["body"].(string)
		if title == "" || body == "" {
			continue
		}
		id, err := store.NewTaskID(ctx)
		if err != nil {
			return fmt.Errorf("orchestrator: allocating an id for proposed task %q: %w", title, err)
		}
		proposal := model.Task{
			ID:     id,
			Intent: model.IntentImplement,
			Title:  title,
			Body:   body,
			Origin: model.Origin{
				Attribution: model.Attribution{
					Actor:      model.Principal{Kind: model.PrincipalAutomation, ID: "grain"},
					OnBehalfOf: &model.Principal{Kind: model.PrincipalAgent, ID: task.ID},
				},
				Reason: model.ReasonDirect,
			},
			Target:    task.Target,
			Binding:   model.BindingDirective,
			Links:     []model.Link{{Kind: model.LinkProposedBy, Target: task.ID}},
			CreatedAt: &now,
		}
		proposal.AutoMerge = task.AutoMerge && model.GrantsSubsetOf(proposal.Grants, task.Grants)
		if err := store.PutTask(ctx, proposal); err != nil {
			return fmt.Errorf("orchestrator: filing proposed task %q: %w", title, err)
		}
	}
	return nil
}

// finishWithPullRequest records that task's run pushed a real branch:
// find or open its PR, link it onto the task, and observe the task
// completed -- StateOf's own precedence means a task with CompletedAt set
// and no ClosedAt yet reads as 'completed', exactly the state
// SyncPullRequests watches for.
func finishWithPullRequest(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, now time.Time) error {

	pr, err := EnsurePullRequest(client, task)
	if err != nil {
		return fmt.Errorf("orchestrator: opening a pull request for %s: %w", task.ID, err)
	}

	// Through UpdateTask rather than writing back the task this function
	// was handed: that copy was read at the top of the cycle, and a person
	// editing it from the UI in between would lose their edit to this
	// write. Re-checking the link inside the closure is also what makes it
	// idempotent across a retry.
	ref := model.PullRequestRef{Repo: *task.Target, Number: pr.Number}
	if err := store.UpdateTask(ctx, task.ID, func(t *model.Task) error {
		for _, l := range t.Links {
			if l.Kind == model.LinkFixes && l.Target == ref.String() {
				return nil
			}
		}
		t.Links = append(t.Links, model.Link{Kind: model.LinkFixes, Target: ref.String()})
		return nil
	}); err != nil {
		return fmt.Errorf("orchestrator: linking %s to %s: %w", task.ID, ref, err)
	}

	return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.CompletedAt = &now })
}

// taskClosed reports whether taskID's current observation already has
// ClosedAt set -- read fresh from the store rather than off the caller's
// task/result, since a close racing a still-live run is exactly the case
// this exists to catch.
func taskClosed(ctx context.Context, store *model.Store, taskID string) (bool, error) {
	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		return false, fmt.Errorf("orchestrator: checking whether %s closed while its run was live: %w", taskID, err)
	}
	return obs != nil && obs.ClosedAt != nil, nil
}

// observeField is Store.ObserveField with this package's error prefix.
// The read-modify-write itself moved to the store once the CLI and the UI
// needed the same thing -- Observe REPLACEs the whole row rather than
// patching one column (its own doc comment on task_observation's schema,
// and simulate_test.go's
// TestGitHubSyncObservationsReplaceTheWholeRowNotJustTheChangedField),
// so every caller changing one field without erasing the others has to
// read the row first, and that is not per-package logic.
func observeField(ctx context.Context, store *model.Store, taskID string, now time.Time,
	set func(*model.Observation)) error {

	if err := store.ObserveField(ctx, taskID, now, set); err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}
	return nil
}

// EnsurePullRequest finds task's already-open PR for its own branch, or
// opens one -- FindOpenPullRequestForBranch first, since GitHub allows at
// most one open PR per head branch and a retried finish (this cycle
// crashed after CreatePullRequest but before the link was recorded) must
// not try to open a second one.
func EnsurePullRequest(client github.Client, task model.Task) (github.PullRequest, error) {
	branch := model.BranchName(task.ID)
	if existing, err := client.FindOpenPullRequestForBranch(task.Target.Owner, task.Target.Name, branch); err != nil {
		return github.PullRequest{}, err
	} else if existing != nil {
		return *existing, nil
	}

	base := task.Base
	if base == "" {
		b, err := client.DefaultBranch(task.Target.Owner, task.Target.Name)
		if err != nil {
			return github.PullRequest{}, err
		}
		base = b
	}

	title := task.Title
	if title == "" {
		title = "grain: " + task.ID
	}
	// The task id, not an issue reference: a task has no issue to point a
	// reader at any more, and its id is what `grain get` takes.
	body := fmt.Sprintf("Automated change for grain task %s.", task.ID)
	return client.CreatePullRequest(task.Target.Owner, task.Target.Name, branch, base, title, body)
}
