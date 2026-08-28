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

// ProcessResult is what a finished run's tool calls turn into on GitHub:
// v1's own "core.py writes a file, then turns it into a real GitHub call"
// split, done here in one step since nothing else needs the intermediate
// file. task must be the same Task RunDispatch was given -- its
// ExternalRef names the task-repo issue every relayed comment lands on.
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
func ProcessResult(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, result *agent.Result, now time.Time) error {

	repo, number, err := model.ParseExternalRef(task.ExternalRef)
	if err != nil {
		return fmt.Errorf("orchestrator: processing result for %s: %w", task.ID, err)
	}

	if err := relayProposedTasks(client, repo, result); err != nil {
		return err
	}

	if question, ok := firstToolCallArg(result, "ask_question", "question"); ok && question != "" {
		commentID, err := client.CreateComment(repo.Owner, repo.Name, number, question)
		if err != nil {
			return fmt.Errorf("orchestrator: posting question for %s: %w", task.ID, err)
		}
		id64 := int64(commentID)
		return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.PendingQuestionCommentID = &id64 })
	}

	pushed, err := client.BranchExists(task.Target.Owner, task.Target.Name, model.BranchName(task.ID))
	if err != nil {
		return fmt.Errorf("orchestrator: checking %s's branch: %w", task.ID, err)
	}

	if pushed {
		return finishWithPullRequest(ctx, store, client, task, now)
	}

	if comment, ok := firstToolCallArg(result, "comment_on_issue", "comment"); ok && comment != "" {
		if _, err := client.CreateComment(repo.Owner, repo.Name, number, comment); err != nil {
			return fmt.Errorf("orchestrator: posting closing comment for %s: %w", task.ID, err)
		}
		return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.CompletedAt = &now })
	}

	// Neither a push, a question, nor a closing comment: the run produced
	// nothing to act on (the failScript case in the e2e harness this
	// mirrors). Left running-less and un-observed, it is eligible for
	// another attempt the next time dispatch.Cycle looks at task_ready --
	// nothing here forces a retry, but nothing prevents one either.
	return nil
}

// relayProposedTasks files each propose_task call as a real (but
// unlabelled) GitHub issue -- proposeTaskTool's own doc comment: "requires
// a human to apply the trigger label ... before the agent set will ever
// attempt it." depends_on referring to another call's local `id` in the
// same run is not resolved to a real issue number here -- doing that
// needs holding every proposal until the whole batch is filed and
// rewriting cross-references afterward, which is follow-on work; today
// each proposal's depends_on is posted into the new issue's body verbatim
// for a human to resolve by hand.
func relayProposedTasks(client github.Client, taskRepo model.RepoRef, result *agent.Result) error {
	for _, p := range proposedTaskCalls(result) {
		title, _ := p["title"].(string)
		body, _ := p["body"].(string)
		if title == "" || body == "" {
			continue
		}
		if _, err := client.CreateIssue(taskRepo.Owner, taskRepo.Name, title, body, nil); err != nil {
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

	ref := model.PullRequestRef{Repo: *task.Target, Number: pr.Number}
	linked := false
	for _, l := range task.Links {
		if l.Kind == model.LinkFixes && l.Target == ref.String() {
			linked = true
			break
		}
	}
	if !linked {
		task.Links = append(task.Links, model.Link{Kind: model.LinkFixes, Target: ref.String()})
		if err := store.PutTask(ctx, task); err != nil {
			return fmt.Errorf("orchestrator: linking %s to %s: %w", task.ID, ref, err)
		}
	}

	return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.CompletedAt = &now })
}

// observeField reads a task's current observation (or starts a fresh one
// if it has none), applies set, and writes it back. Store.Observe REPLACEs
// the whole row rather than patching one column -- its own doc comment on
// task_observation's schema, and simulate_test.go's
// TestGitHubSyncObservationsReplaceTheWholeRowNotJustTheChangedField pins
// it down -- so every caller that wants to change one field without
// erasing the others has to read-modify-write, this being the one place
// in this package that does it more than once.
func observeField(ctx context.Context, store *model.Store, taskID string, now time.Time,
	set func(*model.Observation)) error {

	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading observation for %s: %w", taskID, err)
	}
	if obs == nil {
		obs = &model.Observation{TaskID: taskID}
	}
	set(obs)
	obs.ObservedAt = &now
	if err := store.Observe(ctx, *obs); err != nil {
		return fmt.Errorf("orchestrator: observing %s: %w", taskID, err)
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
	body := fmt.Sprintf("Automated change for %s.", task.ExternalRef)
	return client.CreatePullRequest(task.Target.Owner, task.Target.Name, branch, base, title, body)
}
