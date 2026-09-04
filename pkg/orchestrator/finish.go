package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// firstToolCallArg returns the first argument named key from the first
// non-error call to name in result, and whether one was found. Reading
// straight off agent.Result.ToolCalls rather than a mcp.MockSink is
// deliberate: a Framework.Run constructs and discards its own
// MockSink internally (its own doc comment says so), so ToolCalls is the
// only seam a caller outside that package has -- the same one that
// tests/e2e/harness_test.go's askedQuestion/pushedOK helpers use.
//
// name is matched exactly, against the bare tool name pkg/mcp registers
// ("ask_question", not "mcp__grain-sandbox__ask_question"). That is
// agent.ToolCall.Name's own documented contract, enforced by each
// Framework putting a reported name through mcp.BareToolName -- both CLIs
// namespace an MCP server's tools by the key they loaded it under, and a
// Framework that skipped that step would match nothing here and silently
// cost every run its question and its closing comment (mcp.BareToolName's
// own doc comment has the history). Each framework package has a test on
// that invariant rather than this function tolerating both spellings: a
// prefix leaking this far would also be in the log line and the
// no_action detail below, so the place to stop it is where it is read.
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
// actions") so it is what the task ends up parked on, and is returned on
// before anything else -- answering it is the whole reason the run
// stopped, and no PR exists yet for an ask_question turn to have opened
// regardless. A request_secret call parks the task on the same terms and
// through the same field, and is relayed in the same step
// (relayParkingCalls), which is what keeps a run that made both calls
// from having one of them silently dropped. Proposed tasks are relayed independent of how the run
// otherwise ended, since v1's own propose_task can accompany other work
// rather than replacing it.
//
// A comment_on_issue call is therefore relayed before either of those
// two returns rather than after both of them, which is what a run that
// commented *and* did something else used to lose. Both combinations are
// ones the tools themselves invite: comment_on_issue's own description
// says a pull request is opened for pushed commits "regardless of whether
// you also call this", and a run that reports what it found before asking
// what to do next is saying two different things, not the same thing
// twice. Under the old order the question's return and the pushed
// branch's return each came first, and the comment was dropped silently
// and irrecoverably -- agent.Result is never persisted, so the words
// existed nowhere else, and neither path reaches the "nothing to act on"
// log line that would at least have recorded that the call happened.
// Relaying costs one row and puts the words in front of the human reading
// the task and the redispatched run reading the thread back (run.go's
// commentThreadSection).
//
// Only the relaying moved. What the comment means for the task's state is
// still decided by what else the run did: it is a closing note that
// completes the task only when there was no question and no branch, and
// parking is still keyed to the question, whose comment id is the one
// PendingQuestionCommentID names.
//
// add_review_comment is not relayed here at all: doing so needs a PR
// already in hand to attach a draft review to, which only a /review-intent
// dispatch (not yet built -- see directives.go) would have. A run that
// calls it today gets ProcessResult's ordinary "nothing to act on"
// ending, which at least names the call in the run's own outcome detail
// (noActionDetail) rather than losing it in silence.
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

	// Relayed up front, before either ending that used to return past it.
	// What a comment_on_issue call means for the task's *state* depends on
	// what else the run did -- a closing note on its own, a remark
	// alongside a pull request, a finding in front of a question -- but
	// the words themselves are the run's own and belong in the
	// conversation on every one of those paths.
	comment, ok := firstToolCallArg(result, "comment_on_issue", "comment")
	hasComment := ok && comment != ""
	if hasComment {
		if _, err := relayComment(ctx, store, task, comment, now); err != nil {
			return fmt.Errorf("orchestrator: posting comment for %s: %w", task.ID, err)
		}
	}

	parked, err := relayParkingCalls(ctx, store, task, result, now)
	if err != nil {
		return err
	}
	if parked {
		return nil
	}

	handled, err := salvagePushedBranch(ctx, store, client, task, now)
	if empty, ok := emptyBranch(err); ok {
		// The task has already been told, and parked (noteEmptyBranch).
		// What is left is this run's own row, and it must not say
		// "finish-failed": that word means "try again", which is what
		// this ending exists to stop. no_action is what actually
		// happened -- a run that left nothing anyone can act on -- and
		// returning nil is what keeps noteFinishFailure from writing
		// over it on the way out.
		if serr := store.SetRunOutcome(ctx, runID, "no_action", emptyBranchDetail(empty)); serr != nil {
			return fmt.Errorf("orchestrator: recording %s's outcome: %w", task.ID, serr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	if hasComment {
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
	// tool call at all, since RunDispatch has no way to check GitHub or
	// task_observation itself. A run that, say, ran a few shell commands
	// and then gave up without pushing, asking, or leaving a comment would
	// otherwise sit in task_run as a permanent "succeeded" that never
	// counts toward the streak, dodging the very cap this ending exists to
	// feed. Overwriting it here, now that a push/question/comment has
	// actually been ruled out, is what keeps task_run's own outcome
	// column meaning what a human reading `grain get` would expect it to.
	// Log what the run *did* do, because nothing else does. agent.Result
	// carries FinalText and ToolCalls, neither of which is persisted --
	// SetRunOutcome stores an outcome string and nothing more -- and
	// neither this package nor the agent framework logs them. So a run
	// ending here left no record at all of the agent's behaviour, and
	// "finished without pushing a branch, asking a question, or leaving a
	// closing comment" was the entire evidence available for diagnosing
	// why: it says what did not happen and nothing about what did.
	//
	// This branch specifically is where that hurts. Every other ending
	// leaves an artefact a human can read -- a pull request, a question, a
	// comment. This one is defined by leaving none.
	//
	// Tool names are a fixed vocabulary and safe to log in full. FinalText
	// is model output and is bounded hard: an agent that read a file is
	// free to quote it back, and a sandbox working directory can hold a
	// .git-credentials. A truncated prefix is enough to tell "I could not
	// find anything to do" from "I hit an API error" without turning the
	// journal into a transcript store.
	log.Printf("orchestrator: task %s run %s ended with no action: %d tool call(s)%s; final text (%d bytes): %s",
		task.ID, runID, len(result.ToolCalls), toolCallSummary(result), len(result.FinalText),
		truncate(result.FinalText, 500))

	if err := store.SetRunOutcome(ctx, runID, "no_action", noActionDetail(result)); err != nil {
		return fmt.Errorf("orchestrator: recording %s's outcome: %w", task.ID, err)
	}
	return nil
}

// salvagePushedBranch turns a branch the run left on GitHub into a pull
// request, reporting whether there was one to turn. It is the only part
// of a run's outcome that survives the run itself: the sandbox is
// recreated and agent.Result is never persisted, but a pushed branch is
// on GitHub whatever happened to the process that pushed it.
//
// Three callers need exactly this and used to have two-and-a-half copies
// of it: ProcessResult for a run that ended cleanly, recoverRun for one
// whose process died, and runOne for one whose framework returned an
// error -- the case that had no copy at all, and stranded every branch a
// run pushed before it ran out of turns.
//
// The close re-check is not incidental. task was read at the top of this
// cycle, before the run finished, so it cannot see a close that landed
// while the run was still live; re-reading the observation here is what
// model/state.go's StateOf precedence (ClosedAt outranks a live run)
// means for a run that already pushed. Nobody wants a closed task's work
// merged, so nothing is opened here: the branch is left pushed, and left
// unopened unless the run itself already opened a pull request for it.
// It still counts as handled either way -- there was a branch, and the
// decision about it has been made.
//
// That qualifier is what a run calling open_pull_request (pullrequest.go)
// mid-flight changed, and it is a real case: the close can land after the
// tool call and before this runs, on all three of this function's callers.
// The pull request then already exists on GitHub and is already linked to
// the task, and this function undoes neither. Nothing in grain closes a
// pull request, and the commits on the branch are real work whichever way
// the human went on the task, so it is left open and left linked, for a
// person to merge or close by hand.
//
// What that leaves behind is bounded, which is why it is the choice made:
// grain itself never touches that pull request again.
// Store.OpenPullRequestLinks only returns links belonging to a task whose
// run is over and which has not closed, so a closed task never becomes a
// merge queue entry and SyncPullRequests never so much as reads it. The thing "nobody wants a closed task's work merged" is protecting
// is protected either way; all that remains is a pull request visible on
// a task somebody just closed.
//
// What remains is now also *said*, on the task and on the pull request
// itself, rather than only being true: noteOrphanedPullRequests, on both
// sides of the finish -- for a close this function found, and for one
// that landed just after it looked. The decision is unchanged (nothing is
// closed, nothing is merged, nothing is unlinked); what changed is that
// nobody has to work it out for themselves from a pull request that has
// simply gone quiet.
//
// It is also the answer already given to the same question one moment
// later in a task's life -- a task closed after its pull request was
// opened at the finish keeps that pull request open and unmerged forever
// (tests/e2e's TestClosingATaskWithAnOpenPullRequestDropsItFromTheMerge
// QueueForGood). A run opening its own pull request only moved when that
// moment can arrive. pullrequest_test.go's
// TestClosingATaskLeavesThePullRequestItsRunAlreadyOpenedAlone pins both
// halves of it here.
func salvagePushedBranch(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, now time.Time) (bool, error) {

	if task.Target == nil {
		return false, nil
	}
	pushed, err := branchExistsSettled(client, task.Target.Owner, task.Target.Name, model.BranchName(task.ID))
	if err != nil {
		return false, fmt.Errorf("orchestrator: checking %s's branch: %w", task.ID, err)
	}
	if !pushed {
		return false, nil
	}
	closed, err := taskClosed(ctx, store, task.ID)
	if err != nil {
		return true, err
	}
	if closed {
		return true, noteOrphanedPullRequests(ctx, store, client, task.ID, now)
	}
	if err := finishWithPullRequest(ctx, store, client, task, now); err != nil {
		// A branch with nothing on it is the one failure here that is not
		// worth reporting as a failure: it is answered, on the task, by
		// noteEmptyBranch. The error is still handed back afterwards, so
		// each caller can record what it means for the *run* -- see
		// ProcessResult, RunCycle and recoverRun, which want three
		// different outcomes on the row and the same thing on the task.
		if empty, ok := emptyBranch(err); ok {
			if noteErr := noteEmptyBranch(ctx, store, task, empty, now); noteErr != nil {
				// Deliberately not wrapping err: the task was *not*
				// told, so this is an ordinary failure to report and
				// retry, not the settled ending below.
				return true, noteErr
			}
		}
		return true, err
	}
	return true, noteOrphanedIfClosed(ctx, store, client, task.ID, now)
}

// noActionDetail says what the run did, not only what it did not do.
//
// This overwrites whatever RunDispatch's outcomeOf already recorded, and
// the sentence alone used to throw that away. outcomeOf distinguishes a
// run that called tools and achieved nothing from one that "made no tool
// calls at all" -- a real diagnosis, since an agent that never called a
// tool did not fail at the work so much as never attempt it -- and
// replacing it with a fixed string about pushes and questions left both
// looking identical to whoever read `grain get` afterwards.
//
// Names and counts only. The detail is a stored column read in a task
// listing, not a transcript: the run's final text goes to the log line
// above, bounded, for the same reason.
func noActionDetail(result *agent.Result) string {
	const ending = "finished without pushing a branch, asking a question, or leaving a closing comment"
	if len(result.ToolCalls) == 0 {
		return "the run made no tool calls at all, and " + ending
	}
	return fmt.Sprintf("the run made %d tool call(s)%s, and %s",
		len(result.ToolCalls), toolCallSummary(result), ending)
}

// toolCallSummary names every tool the run called and how often, in a
// stable order, or empty when it called none -- which is itself the most
// informative case, since an agent that called nothing at all did not
// fail to act so much as never start.
func toolCallSummary(result *agent.Result) string {
	if len(result.ToolCalls) == 0 {
		return ""
	}
	counts := map[string]int{}
	var order []string
	for _, c := range result.ToolCalls {
		name := c.Name
		if c.IsError {
			name += "(error)"
		}
		if _, seen := counts[name]; !seen {
			order = append(order, name)
		}
		counts[name]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s x%d", name, counts[name]))
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

// truncate bounds model output on its way to the journal. Marks that it
// cut rather than silently eliding, so a short final text is never
// mistaken for a truncated one.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<empty>"
	}
	if len(s) <= max {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:max]) + "... (truncated)"
}

// branchExistsRetries and branchExistsBackoff bound how long a negative
// answer is re-checked before it is believed. Small: this is covering a
// read that trails a write by a moment, not an outage.
var (
	branchExistsRetries = 3
	branchExistsBackoff = 2 * time.Second
	branchExistsSleep   = time.Sleep
)

// branchExistsSettled re-checks a "no such branch" answer before acting
// on it, because this call runs moments after the agent's own `git push`
// and GitHub's REST reads can trail a push briefly.
//
// The asymmetry is the whole point. A false positive here is caught
// immediately -- finishWithPullRequest asks GitHub to open a pull request
// against a branch, and GitHub refuses if it is not there. A false
// negative is silent and permanent: the run is recorded no_action, the
// task counts another failed attempt toward its streak cap, and the work
// the agent actually did sits pushed on the remote with nothing pointing
// at it. That happened to a task on the v2 staging deployment whose
// branch was on GitHub, at the right commit, with a push timestamp one
// second after the commit it carried.
//
// So a positive is taken at once and only a negative is paid for, at a
// few seconds against a dispatch that took minutes. An error is not
// retried at all: BranchExists already distinguishes 404 (a real "not
// there") from every other status, and a 500 or a refused connection is
// the caller's to report rather than to sit on.
func branchExistsSettled(client github.Client, owner, repo, branch string) (bool, error) {
	for attempt := 0; attempt < branchExistsRetries; attempt++ {
		if attempt > 0 {
			branchExistsSleep(branchExistsBackoff)
		}
		exists, err := client.BranchExists(owner, repo, branch)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}

	// Every attempt said no. Confirm the client can see the repository at
	// all before believing it: BranchExists reads a 404 as "no such
	// branch", and every other way of failing to reach a repo -- a
	// transport aimed at the wrong host, a revoked or unscoped token, a
	// repo turned private -- 404s in exactly the same way. A negative
	// that means "I cannot look" is then indistinguishable from one that
	// means "I looked and it is not there", and this caller acts on the
	// difference: the second ends the task as no_action, the first throws
	// away work the agent really did push.
	//
	// That is not hypothetical. github.NewRealTransport pointed the REST
	// client at github.com rather than api.github.com, so every API path
	// 404'd, so every branch read as unpushed, and runs that had just
	// pushed were recorded as having done nothing at all -- with no error
	// anywhere to say so. A cheap read of the repo itself, on the
	// negative path only, is what turns that class of failure from a
	// wrong answer into a reported one.
	if _, err := client.DefaultBranch(owner, repo); err != nil {
		return false, fmt.Errorf(
			"orchestrator: cannot read %s/%s, so %q cannot be treated as unpushed: %w",
			owner, repo, branch, err)
	}
	return false, nil
}

// relayParkingCalls relays the two escape hatches that park a task on a
// human -- request_secret and ask_question -- and reports whether it
// parked it. Both write a comment and both leave the task in
// awaiting_reply through the same Observation.PendingQuestionCommentID,
// so they are decided together rather than in two branches that would
// each return past the other.
//
// A run is meant to call at most one of them: each says "after calling
// this, do not take any further actions". A run that called both is
// nonetheless relayed both, because a call that reached this far and is
// dropped is dropped in silence -- agent.Result is never persisted, and
// the words exist nowhere else (the same argument the comment_on_issue
// relay above makes). The task then parks on the question, which is the
// later of the two and the one that ended the run's turn, while the
// secret is still recorded and its box still offered: setting it or
// replying in words both un-park the task, and either is a human having
// answered.
//
// Ordering inside is what a reader of the conversation gets: the secret
// request first, the question after it, both stamped now.
func relayParkingCalls(ctx context.Context, store *model.Store, task model.Task,
	result *agent.Result, now time.Time) (bool, error) {

	secret, _ := firstToolCallArg(result, "request_secret", "secret")
	question, _ := firstToolCallArg(result, "ask_question", "question")
	if secret == "" && question == "" {
		return false, nil
	}

	var pending int64
	if secret != "" {
		reason, _ := firstToolCallArg(result, "request_secret", "reason")
		commentID, err := relayComment(ctx, store, task, secretRequestBody(secret, reason), now)
		if err != nil {
			return false, fmt.Errorf("orchestrator: posting the secret request for %s: %w", task.ID, err)
		}
		pending = commentID
	}
	if question != "" {
		commentID, err := relayComment(ctx, store, task, question, now)
		if err != nil {
			return false, fmt.Errorf("orchestrator: posting question for %s: %w", task.ID, err)
		}
		pending = commentID
	}

	if err := observeField(ctx, store, task.ID, now, func(o *model.Observation) {
		o.PendingQuestionCommentID = &pending
		if secret != "" {
			o.PendingSecret = secret
		}
	}); err != nil {
		return false, err
	}
	return true, nil
}

// secretRequestBody is what a request_secret call reads as in the task's
// conversation: the run's own reason first, in its own words, then
// grain's sentence about what the box underneath does with what is typed
// into it.
//
// The second half is grain's to say and not the agent's, because it is
// the part a human has to be able to trust: that the value is not going
// into the thread they are reading, and not back to the run that asked.
// An agent could claim it; only grain can mean it.
func secretRequestBody(secret, reason string) string {
	body := fmt.Sprintf(
		"**This run asked for the secret `%s`.** Set it in the box on this task: the value "+
			"goes straight into grain's encrypted secret store, is never shown to the agent "+
			"that asked for it, and never appears in this conversation. Setting it -- or "+
			"replying here instead -- puts this task back in the queue.", secret)
	if reason == "" {
		return body
	}
	return reason + "\n\n" + body
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
// capability request is added to that schema. An explicit auto_merge
// argument overrides that inheritance in one direction only: false opts a
// proposal out (separate work an agent thinks deserves its own review),
// while true still cannot exceed task's own setting, since a run that
// could mark its own proposals auto-merge would be granting itself the
// unreviewed merge a human withheld. This only changes what happens once
// a human has approved and run the proposal and its PR merges cleanly --
// it does not touch Approval, so a proposed task still needs a human to
// approve it before it ever runs, the same as any other.
//
// depends_on becomes real model.LinkDependsOn links (proposedDependency
// resolves each entry), so a proposal that names work it has to follow is
// blocked until that work is done rather than dispatchable the moment a
// human approves it. Resolution is against tasks that already exist --
// including task itself, the usual case for a piece split out of the run
// doing the splitting -- and against the local `id` of an earlier
// propose_task call in the same run. Earlier, not any: a batch resolved
// in call order cannot contain a cycle, and a cycle in LinkDependsOn is
// two tasks neither of which is ever dispatchable again.
//
// Each proposal joins the backlog at whichever end model.Config.NewestFirst
// says new work joins -- Store.OrderKeyForNewTask, the same call
// ui.CreateTask makes for a task a human files -- so by default (that
// setting's own false) a proposal is filed at the end of the backlog,
// behind everything already queued. It used to be filed with no OrderKey
// at all, which is not "no position" but position zero: ahead of every
// task filed since the backlog started, so a proposal an agent split out
// as an afterthought was dispatched, once approved, before work that had
// been waiting for it.
func relayProposedTasks(ctx context.Context, store *model.Store, task model.Task,
	result *agent.Result, now time.Time) error {

	proposals := proposedTaskCalls(result)
	if len(proposals) == 0 {
		return nil
	}
	atFront, err := newestFirst(ctx, store)
	if err != nil {
		return err
	}

	// Local `id` of a propose_task call -> the task id it was filed
	// under, filled in as each proposal lands so only earlier calls are
	// in reach. First claim on an id wins: a run that reuses one has
	// already made the reference ambiguous, and picking the earlier
	// keeps this pass order-dependent in only one direction.
	filed := map[string]string{}

	for _, p := range proposals {
		title, _ := p["title"].(string)
		body, _ := p["body"].(string)
		if title == "" || body == "" {
			continue
		}
		id, err := store.NewTaskID(ctx)
		if err != nil {
			return fmt.Errorf("orchestrator: allocating an id for proposed task %q: %w", title, err)
		}
		links := []model.Link{{Kind: model.LinkProposedBy, Target: task.ID}}
		deps, unresolved, err := proposedDependencies(ctx, store, p, filed)
		if err != nil {
			return fmt.Errorf("orchestrator: resolving depends_on for proposed task %q: %w", title, err)
		}
		links = append(links, deps...)
		if len(unresolved) > 0 {
			body += unresolvedDependencyNote(unresolved)
		}
		// Once per proposal rather than once for the batch: each one is
		// past the previous one's key by then, so a run that proposes
		// several files them in call order behind each other rather than
		// all on the same key with the id left to break the tie.
		orderKey, err := store.OrderKeyForNewTask(ctx, atFront)
		if err != nil {
			return fmt.Errorf("orchestrator: placing proposed task %q in the backlog: %w", title, err)
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
			Links:     links,
			CreatedAt: &now,
			OrderKey:  orderKey,
		}
		proposal.AutoMerge = proposedAutoMerge(p, task) &&
			model.GrantsSubsetOf(proposal.Grants, task.Grants)
		if err := store.PutTask(ctx, proposal); err != nil {
			return fmt.Errorf("orchestrator: filing proposed task %q: %w", title, err)
		}
		if local, _ := p["id"].(string); local != "" {
			if _, taken := filed[local]; !taken {
				filed[local] = proposal.ID
			}
		}
	}
	return nil
}

// newestFirst is model.Config.NewestFirst, read fresh from the store --
// ui.Client's own method of the same name, for the same reason it reads
// it per call there: a setting the settings pane toggles is expected to
// decide where the very next task lands, not where the next process's
// first one does. A fresh deployment with no config row (nil) reads as
// false, the backlog order grain has always defaulted to.
func newestFirst(ctx context.Context, store *model.Store) (bool, error) {
	cfg, err := store.GetConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("orchestrator: reading the backlog order: %w", err)
	}
	return cfg != nil && cfg.NewestFirst, nil
}

// proposedAutoMerge reads one propose_task call's auto_merge argument
// against task, the job that made it: unset inherits task's own setting,
// and an explicit answer is honoured only as far down as that -- see
// relayProposedTasks' own doc comment for why true cannot raise it.
func proposedAutoMerge(args map[string]any, task model.Task) bool {
	asked, ok := args["auto_merge"].(bool)
	if !ok {
		return task.AutoMerge
	}
	return asked && task.AutoMerge
}

// proposedDependencies turns one propose_task call's depends_on into
// links, alongside every entry it could not resolve to a task at all.
//
// Unresolved entries are returned rather than dropped: an agent naming a
// task that does not exist (a GitHub issue number, say, from the v1
// spelling of this tool, or a local `id` it only used later in the run)
// has said something about the order this work has to happen in, and
// silently filing the proposal as unblocked would lose it. It goes into
// the proposal's body instead, where a human approving it can see it --
// unresolvedDependencyNote.
func proposedDependencies(ctx context.Context, store *model.Store,
	args map[string]any, filed map[string]string) (links []model.Link, unresolved []string, err error) {

	seen := map[string]bool{}
	for _, ref := range argStrings(args["depends_on"]) {
		target, ok, err := proposedDependency(ctx, store, ref, filed)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			unresolved = append(unresolved, ref)
			continue
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		links = append(links, model.Link{Kind: model.LinkDependsOn, Target: target})
	}
	return links, unresolved, nil
}

// proposedDependency resolves one depends_on entry to a real task id.
//
// A local `id` from an earlier propose_task call in the same run wins
// over a task id that happens to read the same, since a run that named
// one of its own proposals "12" meant that proposal, not task 12.
// Otherwise the entry has to name a task that already exists: this is the
// one place a proposal's own claim about ordering is checked against
// something, and a link to a task id that was never real would block the
// proposal forever with nothing to unblock it.
//
// A leading "#" is tolerated because the tool's v1 spelling asked for
// issue numbers and agents still write them that way.
func proposedDependency(ctx context.Context, store *model.Store, ref string,
	filed map[string]string) (string, bool, error) {

	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ref == "" {
		return "", false, nil
	}
	if id, ok := filed[ref]; ok {
		return id, true, nil
	}
	existing, err := store.GetTask(ctx, ref)
	if err != nil {
		return "", false, err
	}
	if existing == nil {
		return "", false, nil
	}
	return ref, true, nil
}

// unresolvedDependencyNote is what a human approving the proposal reads
// in place of a link grain could not make -- attributed to grain, since
// the rest of the body is the agent's own words.
func unresolvedDependencyNote(unresolved []string) string {
	return fmt.Sprintf("\n\n---\n\ngrain: the run that proposed this said it depends on %s, "+
		"which named no task that exists and no other proposal from the same run. "+
		"Nothing blocks this task on them -- resolve them by hand if they matter.",
		"`"+strings.Join(unresolved, "`, `")+"`")
}

// argStrings reads a tool argument that should have arrived as an array
// of strings, skipping anything in it that did not. Tool arguments reach
// here as map[string]any straight out of the transcript, so this repeats
// mcp.argStringSlice rather than importing it: agent.ToolCall.Arguments
// is whatever the framework reported, not something mcp validated.
func argStrings(v any) []string {
	if already, ok := v.([]string); ok {
		return already
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// finishWithPullRequest records that task's run pushed a real branch:
// find or open its PR, link it onto the task, and observe the task
// completed -- StateOf's own precedence means a task with CompletedAt set
// and no ClosedAt yet reads as one of the two post-run states
// ('awaiting_submit' until somebody submits it, 'completed' once they
// have), both of which SyncPullRequests watches.
func finishWithPullRequest(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, now time.Time) error {

	pr, err := EnsurePullRequest(ctx, store, client, task, now)
	if err != nil {
		return fmt.Errorf("orchestrator: opening a pull request for %s: %w", task.ID, err)
	}

	// linkPullRequest (pullrequest.go), not an UpdateTask written out
	// here: a run that opened its own pull request mid-flight through
	// open_pull_request has already recorded this very link, and both
	// paths writing it the same idempotent way is what keeps the second
	// one a no-op rather than a duplicate.
	ref := model.PullRequestRef{Repo: *task.Target, Number: pr.Number}
	if err := linkPullRequest(ctx, store, task.ID, ref); err != nil {
		return err
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
//
// Either way it ends with a description built from the branch's own
// commit messages (description.go): written at creation, and refreshed on
// a pull request that was already there, since the one a run opened
// mid-flight was described before it had finished pushing. Neither can
// fail this call -- see describeBranch and refreshDescription on why a
// description is never worth failing a finish over.
//
// The store is here for one reason: a base branch that is no longer
// there. pullRequestBase retargets that at the repo's default branch and
// noteBaseRetarget writes down that it did, on the task itself -- see
// both of their doc comments for why the work is opened rather than
// refused, and why the retarget is never silent.
//
// The other 422 GitHub answers forever -- a head that carries no commits
// its base does not already have -- comes back as a *noCommitsError
// rather than as GitHub's own words, both when the compare above saw it
// coming and when only the refusal did. Nothing is retargeted and nothing
// is opened, because there is nothing to open: what to do about it is the
// caller's, and the two callers answer differently (salvagePushedBranch
// ends the task, OpenPullRequestForTask tells the run to commit
// something).
func EnsurePullRequest(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, now time.Time) (github.PullRequest, error) {

	branch := model.BranchName(task.ID)
	if existing, err := client.FindOpenPullRequestForBranch(task.Target.Owner, task.Target.Name, branch); err != nil {
		return github.PullRequest{}, err
	} else if existing != nil {
		refreshDescription(client, task, existing.Number)
		return *existing, nil
	}

	base, gone, err := pullRequestBase(client, task)
	if err != nil {
		return github.PullRequest{}, err
	}
	if gone != "" {
		if err := noteBaseRetarget(ctx, store, task, gone, base, now); err != nil {
			return github.PullRequest{}, err
		}
	}

	// One compare, read twice: for what the branch proposes, and for
	// whether it proposes anything at all. GitHub refuses a pull request
	// whose head adds nothing to its base -- see noCommitsError -- and
	// asking here is what turns that refusal into something this
	// package can explain rather than something it repeats. A compare
	// that could not be read (known false) is not an empty branch: the
	// pull request is attempted anyway, and the 422 below is the second
	// line of defence.
	commits, known := branchCommits(client, *task.Target, base, branch)
	if known && len(commits) == 0 {
		return github.PullRequest{}, &noCommitsError{Repo: *task.Target, Branch: branch, Base: base}
	}

	title := task.Title
	if title == "" {
		title = "grain: " + task.ID
	}
	body := pullRequestBody(describeCommits(commits), task.ID)
	pr, err := client.CreatePullRequest(task.Target.Owner, task.Target.Name, branch, base, title, body)
	if err != nil && github.IsNoCommitsBetween(err) {
		// The same condition, learnt from GitHub instead of from the
		// compare: the read above failed, or the branch moved between the
		// two calls (a run's last push landing after this cycle looked).
		// Either way the answer is the one below, not a bare 422 nobody
		// downstream can act on.
		return github.PullRequest{}, &noCommitsError{Repo: *task.Target, Branch: branch, Base: base}
	}
	return pr, err
}

// noCommitsError is a pushed branch that carries no commits its base does
// not already have. GitHub answers that with a 422 every time, so it is
// the vanished base's sibling: permanent, unaffected by retrying, and --
// until it was recognised here -- invisible except as a run detail
// quoting GitHub's own words.
//
// A typed error rather than a sentinel because the branch and the base
// are what the task has to be told (noteEmptyBranch): "no commits between
// main and grain/task-158" is a fact about two branches, and neither is
// in reach of the callers that have to act on it.
type noCommitsError struct {
	Repo   model.RepoRef
	Branch string
	Base   string
}

func (e *noCommitsError) Error() string {
	return fmt.Sprintf("%s on %s carries no commits %q does not already have, "+
		"so GitHub will not open a pull request for it", e.Branch, e.Repo, e.Base)
}

// emptyBranch reports whether err is that, anywhere in its chain -- the
// one question every caller of the finish path asks about an error it
// gets back, since this is the single failure that will not come out
// differently next time.
func emptyBranch(err error) (*noCommitsError, bool) {
	var e *noCommitsError
	ok := errors.As(err, &e)
	return e, ok
}

// pullRequestBase is the branch task's pull request opens against: its
// own Base when that is still a branch on the remote, and otherwise the
// repo's default branch -- returned alongside the base it replaced ("" on
// the ordinary path, where nothing was replaced).
//
// task.Base used to be passed to CreatePullRequest unread, and GitHub
// answers a base that does not exist with a 422 every time. A task
// pointed at a dead base could then never be finished at all: the agent
// cloned, worked, committed and pushed, the branch was real on the
// remote, and the one call that would turn it into a pull request was
// refused -- so the task stayed un-completed, dispatch offered it again
// after its backoff, and the next attempt did the same thing on top of
// the same branch, forever.
//
// A task pointed at a dead base is not exotic. New task prefills Base
// from the repo's last task (bwsalmon/agents#641), so a branch that
// merges between one task being filed and the next being dispatched
// leaves a task pointed at nothing -- prepareCheckout's own doc comment
// makes the same point about the clone.
//
// Retargeting rather than refusing is a decision about *when* this runs.
// By the time anything here is called the work exists: it is committed
// and pushed to a branch on the remote, and refusing it forever helps
// nobody. The overwhelmingly likely reason a base vanished is that it
// merged, in which case its commits are in the default branch already and
// the pull request's diff is the same either way. When it is not -- a
// misspelled base, or work that was abandoned -- the pull request is
// aimed at the wrong branch, which is exactly why noteBaseRetarget says
// so on the task rather than letting this be a quiet fallback. The place
// to catch that case cheaply is before any work happens, and
// prepareCheckout still does: it refuses to start a run whose base is
// gone and there is nothing pushed yet to lose.
//
// The DefaultBranch call is also what keeps a broken client from being
// read as a vanished branch. BranchExists reports a 404 as "no such
// branch", and every other way of failing to reach a repo -- a transport
// aimed at the wrong host, a token that lost its scope, a repo turned
// private -- 404s identically (branchExistsSettled's own doc comment has
// the history). A client that cannot see the repo cannot read its default
// branch either, so that read fails and this reports the failure instead
// of retargeting a base that is perfectly fine.
func pullRequestBase(client github.Client, task model.Task) (base, retargetedFrom string, err error) {
	if task.Base != "" {
		exists, err := client.BranchExists(task.Target.Owner, task.Target.Name, task.Base)
		if err != nil {
			return "", "", fmt.Errorf("orchestrator: checking %s's base branch %q: %w", task.ID, task.Base, err)
		}
		if exists {
			return task.Base, "", nil
		}
	}
	def, err := client.DefaultBranch(task.Target.Owner, task.Target.Name)
	if err != nil {
		return "", "", err
	}
	if task.Base == "" {
		return def, "", nil
	}
	return def, task.Base, nil
}

// noteBaseRetarget records that task's pull request was opened against
// `to` because `from`, the base it asked for, is no longer on the remote.
//
// Both halves matter. The task's own row is changed, so that everything
// downstream of it agrees with the pull request that is about to exist:
// BuildPrompt tells a run to keep merging its base into its branch and
// would otherwise send the next attempt at a branch that is not there,
// prepareCheckout would refuse that attempt outright, and the task's Base
// as shown in the UI would name a branch nobody can look at. And a
// comment says what happened in the human's own conversation, because
// changing a field somebody set, without saying so, is how a task quietly
// ends up meaning something other than what it was filed as. The comment
// names the branch that went missing, so the change is legible and
// reversible by hand.
//
// It is attributed to grain itself rather than on behalf of the agent
// (relayComment's attribution): the run said nothing about any of this,
// grain decided it.
//
// The row is only rewritten while it still says `from`, and the comment
// only follows a row that this call actually changed -- so a retried
// finish that reaches here twice leaves one comment, not two.
func noteBaseRetarget(ctx context.Context, store *model.Store, task model.Task,
	from, to string, now time.Time) error {

	changed := false
	if err := store.UpdateTask(ctx, task.ID, func(t *model.Task) error {
		if t.Base == from {
			t.Base, changed = to, true
		}
		return nil
	}); err != nil {
		return fmt.Errorf("orchestrator: retargeting %s at %q: %w", task.ID, to, err)
	}
	if !changed {
		return nil
	}

	log.Printf("orchestrator: task %s's base branch %q is gone from %s; opening its pull request against %q instead",
		task.ID, from, task.Target, to)

	body := fmt.Sprintf(
		"This task's base branch `%s` no longer exists on %s, so its pull request "+
			"was opened against `%s`, the repository's default branch, and the task's "+
			"own base was changed to match.\n\n"+
			"A base branch usually disappears because it merged and was deleted, in "+
			"which case its commits are in `%s` already and this pull request's diff "+
			"is the same either way. If `%s` went missing for some other reason -- a "+
			"typo, or work that was abandoned -- then this pull request is aimed at "+
			"the wrong branch: retarget it on GitHub, or close it and point this task "+
			"at a branch that exists.",
		from, task.Target, to, to, from,
	)
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    task.ID,
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
		Body:      body,
		CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("orchestrator: recording %s's retargeted base: %w", task.ID, err)
	}
	return nil
}

// noteEmptyBranch is what a task gets instead of a pull request when the
// branch its run pushed carries no commits its base does not already
// have: an explanation in its own conversation, and a stop.
//
// The stop is the judgement call, and it is a different one from the
// vanished base's. There, work existed and only its target was wrong, so
// pullRequestBase retargets and the pull request opens. Here there is no
// work: nothing to salvage, nothing to retarget, nothing for a reviewer
// to look at. Left alone, the task stayed un-completed and dispatch
// offered it again after its backoff, and every attempt continued the
// same empty branch and was refused in exactly the same way -- five full
// agent runs (model.MaxConsecutiveFailures) burnt on a call that cannot
// succeed, ending in a task that reads 'failed' with GitHub's own 422 as
// the only account of why.
//
// So it stops after one, by parking the task on this comment
// (Observation.PendingQuestionCommentID -- the same field ask_question's
// own handling sets, and selfrepair.Confirm's). Parking rather than
// closing, because which of the three ways a branch ends up empty this
// was -- a run that reverted its own work, one that committed only what
// the base already had, one that pushed without committing at all -- is
// not something grain can tell, and only one of them means the task
// itself is finished. Parking costs nothing and is undone by one reply:
// any comment on the task clears the pending question (ui.Client's own
// Reply) and it is queued again, with this explanation in the thread the
// next run reads back. Closing would have thrown a live task away on
// grain's own guess; another four attempts would have thrown money at a
// refusal nobody was watching.
//
// A closing comment from the run itself does not change the answer,
// though it is relayed first and sits above this one in the thread. A run
// that signs off while the branch it pushed carries nothing has made two
// claims that do not agree, and a person reading both is the point of
// parking; completing the task on the strength of the sign-off alone
// would file "done" against a remote where nothing landed.
//
// Attributed to grain itself rather than on behalf of the agent, for
// noteBaseRetarget's reason: the run said none of this, grain worked it
// out.
func noteEmptyBranch(ctx context.Context, store *model.Store, task model.Task,
	empty *noCommitsError, now time.Time) error {

	log.Printf("orchestrator: task %s pushed %s to %s with no commits over %q; "+
		"no pull request can be opened for it, so the task is parked rather than retried",
		task.ID, empty.Branch, empty.Repo, empty.Base)

	body := fmt.Sprintf(
		"This task's run pushed `%s` to %s, but that branch carries no commits `%s` "+
			"does not already have -- GitHub refuses to open a pull request for it "+
			"(\"No commits between %s and %s\"), and refuses it the same way every time.\n\n"+
			"There is nothing here to open, retarget or merge. A branch ends up like this "+
			"when the run reverted its own work, committed only changes `%s` already had, or "+
			"pushed without committing anything at all. Another attempt would continue this "+
			"same branch, so grain has stopped rather than repeating the refusal.\n\n"+
			"Reply on this task to have it dispatched again -- saying what should be "+
			"different is worth more than \"try again\", since the next run reads this "+
			"conversation -- or close it if the change turned out not to be needed.",
		empty.Branch, empty.Repo, empty.Base, empty.Base, empty.Branch, empty.Base,
	)
	commentID, err := store.AddComment(ctx, model.Comment{
		TaskID:    task.ID,
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
		Body:      body,
		CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("orchestrator: recording that %s's branch is empty: %w", task.ID, err)
	}
	return observeField(ctx, store, task.ID, now, func(o *model.Observation) {
		o.PendingQuestionCommentID = &commentID
	})
}

// emptyBranchDetail is the same finding on the run's own row, where
// `grain get` shows it: short, and about this run rather than about the
// task, since the task has the comment above.
func emptyBranchDetail(empty *noCommitsError) string {
	return fmt.Sprintf("the run pushed %s, but it carries no commits %q does not already have, "+
		"so there was no pull request to open; the task is parked on grain's own comment about it",
		empty.Branch, empty.Base)
}
