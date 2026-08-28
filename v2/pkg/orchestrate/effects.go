package orchestrate

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// firstToolCallArg returns the first argument named key from the first
// non-error call to name in result, and whether one was found. Reading
// straight off agent.Result.ToolCalls rather than a mcp.MockSink is
// deliberate: gemini.Framework.Run constructs and discards its own
// MockSink internally (its own doc comment says so), so ToolCalls is the
// only seam a caller outside that package has.
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

// reportOutcome turns a successfully finished run's escape-hatch tool
// calls into the real GitHub effect they imply -- the missing half
// v2/README.md names for this package: "the mcp.NewMockTools escape
// hatches ... are still discarded rather than posted anywhere real ...
// not while [the run] is live." Discovering them from
// agent.Result.ToolCalls once Run has already returned, rather than
// injecting a live sink into gemini.Framework.Run itself, is deliberate
// here too: pkg/orchestrator (finish.go's ProcessResult) already proved
// this is enough to post a real comment, file a real issue, and park a
// task on a real question, with no change needed inside agent/gemini at
// all. This is a second, independent copy of that same logic rather than
// a call into orchestrator -- see the package doc comment on why
// orchestrate and orchestrator are still two separate, not-yet-
// reconciled answers to "when to call GitHub."
//
// Relaying to GitHub at all needs somewhere to relay to: task.ExternalRef
// must name the tracking issue (model.ParseExternalRef), which only a
// task filed the way pkg/orchestrator's own PollIssues files one carries
// today. A task with no parseable ExternalRef still gets its pull
// request opened exactly as before -- openPullRequest's own GitHub/Target
// nil checks already make that a no-op when there is nothing to push --
// it just has no question to park or comment/issue to relay, the same
// "nowhere to relay to" case a nil r.cfg.GitHub already leaves silent.
func (r *Reconciler) reportOutcome(ctx context.Context, task model.Task, result *agent.Result, now time.Time) {
	repo, number, refErr := model.ParseExternalRef(task.ExternalRef)
	hasIssue := r.cfg.GitHub != nil && refErr == nil && result != nil

	if hasIssue {
		if err := r.relayProposedTasks(repo, result); err != nil {
			r.log.Printf("orchestrate: task %s: relaying proposed tasks: %v", task.ID, err)
		}

		if question, ok := firstToolCallArg(result, "ask_question", "question"); ok && question != "" {
			// A question ends the run's turn by contract (mcp's own
			// ask_question doc comment) -- no push happened for an
			// ask_question turn to have implied a pull request, so this
			// returns before ever calling openPullRequest.
			if err := r.parkForQuestion(ctx, task, repo, number, question, now); err != nil {
				r.log.Printf("orchestrate: task %s: posting question: %v", task.ID, err)
			}
			return
		}
	}

	if err := r.openPullRequest(ctx, task, result); err != nil {
		r.log.Printf("orchestrate: dispatch: opening pull request: %v", err)
	}

	if !hasIssue {
		return
	}

	pushed := false
	if task.Target != nil {
		if exists, err := r.cfg.GitHub.BranchExists(task.Target.Owner, task.Target.Name, model.BranchName(task.ID)); err == nil {
			pushed = exists
		}
	}
	if pushed {
		return
	}

	if comment, ok := firstToolCallArg(result, "comment_on_issue", "comment"); ok && comment != "" {
		if err := r.closeWithComment(ctx, task, repo, number, comment, now); err != nil {
			r.log.Printf("orchestrate: task %s: posting closing comment: %v", task.ID, err)
		}
	}
}

// relayProposedTasks files each propose_task call as a real (but
// unlabelled) GitHub issue on repo -- proposeTaskTool's own doc comment:
// "requires a human to apply the trigger label ... before the agent set
// will ever attempt it." depends_on referring to another call's local
// `id` in the same run is not resolved to a real issue number here, the
// same limitation pkg/orchestrator's own relayProposedTasks carries:
// resolving it needs holding every proposal until the whole batch is
// filed and rewriting cross-references afterward.
func (r *Reconciler) relayProposedTasks(repo model.RepoRef, result *agent.Result) error {
	for _, p := range proposedTaskCalls(result) {
		title, _ := p["title"].(string)
		body, _ := p["body"].(string)
		if title == "" || body == "" {
			continue
		}
		if _, err := r.cfg.GitHub.CreateIssue(repo.Owner, repo.Name, title, body, nil); err != nil {
			return fmt.Errorf("filing proposed task %q: %w", title, err)
		}
	}
	return nil
}

// parkForQuestion posts question as a real comment on the task's
// tracking issue and records it as an Observation's
// PendingQuestionCommentID -- which, per model.StateOf, is what makes
// the task read as awaiting_reply rather than the queued state
// task_ready requires, so loop.Cycle stops offering it for another
// attempt until a human replies and pkg/orchestrator's own
// requeueIfAwaitingReply (or an equivalent for this package) clears it.
func (r *Reconciler) parkForQuestion(ctx context.Context, task model.Task, repo model.RepoRef, number int, question string, now time.Time) error {
	commentID, err := r.cfg.GitHub.CreateComment(repo.Owner, repo.Name, number, question)
	if err != nil {
		return fmt.Errorf("posting question: %w", err)
	}
	id64 := int64(commentID)
	return r.observeField(ctx, task.ID, now, func(o *model.Observation) { o.PendingQuestionCommentID = &id64 })
}

// closeWithComment posts comment as a real closing comment on the task's
// tracking issue and marks the task closed outright -- the
// comment_on_issue path for a task whose run answered with a comment
// rather than a push. Unlike a pushed task (marked merely CompletedAt,
// left for syncGitHub to close once its pull request disappears), this
// sets ClosedAt in the same write: a comment-only task has no pull
// request for syncGitHub to ever find, and model.Store.TrackedTargets
// re-derives "is this still open" from branch/PR existence alone (no
// per-task memory of *why* a task completed), so leaving it merely
// CompletedAt would make the very next sync pass see "no PR" and
// mistake never-had-one for merged-or-closed -- a misleading state even
// though StateOf would still land on the same answer either way.
func (r *Reconciler) closeWithComment(ctx context.Context, task model.Task, repo model.RepoRef, number int, comment string, now time.Time) error {
	if _, err := r.cfg.GitHub.CreateComment(repo.Owner, repo.Name, number, comment); err != nil {
		return fmt.Errorf("posting closing comment: %w", err)
	}
	return r.observeField(ctx, task.ID, now, func(o *model.Observation) {
		o.CompletedAt, o.ClosedAt = &now, &now
	})
}

// observeField reads a task's current observation (or starts a fresh one
// if it has none), applies set, and writes it back -- Store.Observe
// REPLACEs the whole row rather than patching one column, so every
// caller that wants to change one field without erasing the others has
// to read-modify-write, the same discipline pkg/orchestrator's own
// observeField follows.
func (r *Reconciler) observeField(ctx context.Context, taskID string, now time.Time, set func(*model.Observation)) error {
	obs, err := r.cfg.Store.GetObservation(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reading observation for %s: %w", taskID, err)
	}
	if obs == nil {
		obs = &model.Observation{TaskID: taskID}
	}
	set(obs)
	obs.ObservedAt = &now
	if err := r.cfg.Store.Observe(ctx, *obs); err != nil {
		return fmt.Errorf("observing %s: %w", taskID, err)
	}
	return nil
}
