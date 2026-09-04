package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// reviewFinding is one add_review_comment call: what the run wants said,
// and -- when it gave both path and line -- where on the diff it wants it
// said. The tool refuses one without the other, so Located is the whole
// of the distinction between an inline comment and a general remark.
type reviewFinding struct {
	Body    string
	Path    string
	Line    int
	Located bool
}

// reviewFindings recovers every non-error add_review_comment call from a
// finished run, in the order the run made them -- the same recovery off
// agent.Result.ToolCalls that firstToolCallArg does for the hatches that
// only ever fire once, except that every call counts here: a review is a
// list of findings, not one answer.
//
// A call whose arguments do not survive the trip (an empty body, a path
// with no line) is dropped rather than guessed at. pkg/mcp's own handler
// already refused those with IsError set, so what reaches here in that
// shape is a client that made the call some other way.
func reviewFindings(result *agent.Result) []reviewFinding {
	var out []reviewFinding
	for _, c := range result.ToolCalls {
		if c.Name != "add_review_comment" || c.IsError {
			continue
		}
		body, _ := c.Arguments["body"].(string)
		if strings.TrimSpace(body) == "" {
			continue
		}
		finding := reviewFinding{Body: body}
		path, _ := c.Arguments["path"].(string)
		line, hasLine := argInt(c.Arguments["line"])
		if path != "" && hasLine {
			finding.Path, finding.Line, finding.Located = path, line, true
		}
		out = append(out, finding)
	}
	return out
}

// argInt reads a tool argument that is meant to be a whole number. JSON
// has one number type, so a line arrives as a float64 through an ordinary
// decode and as a json.Number through a decoder that was asked to keep
// the literal -- both are the same integer, and a relay that understood
// only one of them would drop half the inline comments an agent wrote.
func argInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// relayReviewFeedback turns a run's add_review_comment calls into a draft
// review on the pull request that run was reviewing, and reports how many
// findings it posted there.
//
// This is the hatch that used to be relayed nowhere. ProcessResult's own
// doc comment said why: attaching a review needs a pull request in hand,
// and no dispatch had one. A review task does (grain/task-284): it is
// filed against a task whose work is already proposed in a pull request,
// it carries model.LinkProposedBy back to that task, and that task
// carries model.LinkFixes to the pull request itself. So the pull request
// is two links away from the run, and the findings have somewhere to go.
//
// A draft, not a submitted review, for the reason pkg/github's own
// package doc comment gives and this call does not revisit: grain's
// credential wrote the change under review, so a submitted review from it
// would be grain approving its own work. What a draft costs is
// visibility -- GitHub keeps a PENDING review to the credential that
// created it until a human opens it on github.com and submits it -- and
// that cost is paid twice over below: the same findings are relayed into
// the reviewed task's own conversation, in grain's UI, which is where the
// person the feedback is for is actually looking. The draft is what makes
// them line-anchored on the diff; the comment is what makes them read.
//
// A run that made these calls with no pull request to attach them to --
// an ordinary task's run reaching for the wrong tool, a review whose
// parent has since lost its link -- is not silently dropped either: the
// findings go into that run's own task conversation instead. The words
// exist nowhere else once agent.Result is discarded, which is the same
// argument the comment_on_issue relay makes for being unconditional.
//
// Nothing here fails the finish. A draft review GitHub refused is worth
// one log line and the fallback comment; it is not worth losing the pull
// request the same ProcessResult is about to open, or the question the
// same run is about to park on.
func relayReviewFeedback(ctx context.Context, store *model.Store, client github.Client,
	task model.Task, result *agent.Result, now time.Time) (int, error) {

	findings := reviewFindings(result)
	if len(findings) == 0 {
		return 0, nil
	}

	reviewed, ref, ok, err := reviewedPullRequest(ctx, store, task)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, relayFindingsToConversation(ctx, store, task, findings,
			"This run recorded review feedback, but it is not reviewing a pull request, so "+
				"there was nothing to attach a review to. The findings are here instead:", now)
	}

	if _, err := client.CreateReview(ref.Repo.Owner, ref.Repo.Name, ref.Number,
		draftReviewBody(task, findings), inlineComments(findings)); err != nil {

		log.Printf("orchestrator: task %s could not post its %d review finding(s) as a draft review on %s (%v); "+
			"relaying them into the task's conversation instead", task.ID, len(findings), ref, err)
		return 0, relayFindingsToConversation(ctx, store, task, findings,
			fmt.Sprintf("Posting these findings as a review on %s failed (%v), so they are "+
				"recorded here instead:", ref, err), now)
	}

	if err := noteFindingsOnReview(ctx, store, task, ref, findings, now); err != nil {
		return 0, err
	}
	return len(findings), noteFindingsOnReviewedTask(ctx, store, task, *reviewed, ref, findings, now)
}

// reviewedPullRequest resolves the pull request task is a review of: the
// task it was filed against (model.LinkProposedBy, written by
// fileReviewTask) and that task's own pull request (model.LinkFixes,
// written by linkPullRequest when the run under review finished).
//
// Only for a review task. LinkProposedBy is also what a propose_task
// proposal carries back to the run that proposed it, and a proposal is
// not a review of anything -- Origin.Reason is what tells the two apart
// (isReviewTask), the same field syncEntry reads to keep a review out of
// its repo's merge queue.
//
// Every way of not finding one is "no pull request", not an error: a
// review whose parent has been deleted, or whose parent's pull request
// link is malformed, is a review whose findings have to land somewhere
// else, and failing the finish over it would lose them along with
// everything else the run did.
func reviewedPullRequest(ctx context.Context, store *model.Store, task model.Task) (
	*model.Task, model.PullRequestRef, bool, error) {

	if !isReviewTask(task) {
		return nil, model.PullRequestRef{}, false, nil
	}
	var parentID string
	for _, l := range task.Links {
		if l.Kind == model.LinkProposedBy {
			parentID = l.Target
			break
		}
	}
	if parentID == "" {
		return nil, model.PullRequestRef{}, false, nil
	}
	reviewed, err := store.GetTask(ctx, parentID)
	if err != nil {
		return nil, model.PullRequestRef{}, false,
			fmt.Errorf("orchestrator: reading the task %s reviews: %w", task.ID, err)
	}
	if reviewed == nil {
		return nil, model.PullRequestRef{}, false, nil
	}
	for _, l := range reviewed.Links {
		if l.Kind != model.LinkFixes {
			continue
		}
		ref, err := model.ParsePullRequestRef(l.Target)
		if err != nil {
			continue
		}
		return reviewed, ref, true, nil
	}
	return nil, model.PullRequestRef{}, false, nil
}

// inlineComments is the half of the findings GitHub can anchor to the
// diff. The rest are in the review's own body (draftReviewBody), because
// a comments[] entry with no path is a request GitHub rejects outright --
// and a rejected call would take the located findings down with it.
func inlineComments(findings []reviewFinding) []github.NewReviewComment {
	var out []github.NewReviewComment
	for _, f := range findings {
		if !f.Located {
			continue
		}
		out = append(out, github.NewReviewComment{Path: f.Path, Line: f.Line, Body: f.Body})
	}
	return out
}

// draftReviewBody is what the review itself says, above the inline
// comments: who wrote it and why it is a draft, then any finding that
// named no line of its own.
//
// The provenance line is grain's rather than the agent's on purpose. A
// draft review shows up under the account grain's credential belongs to,
// which is the same account that pushed the change being reviewed, and a
// human opening it is owed the sentence that tells those two apart.
func draftReviewBody(task model.Task, findings []reviewFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review from grain task %s. This is a draft: nobody but grain's own "+
		"credential sees it until you submit it here on GitHub.", task.ID)

	var general []reviewFinding
	for _, f := range findings {
		if !f.Located {
			general = append(general, f)
		}
	}
	if len(general) > 0 {
		b.WriteString("\n\n")
		b.WriteString(findingsList(general))
	}
	return b.String()
}

// findingsList renders findings as one markdown bullet each, prefixed
// with the line they are about when they name one. Multi-line bodies are
// indented under their own bullet so a finding that quotes code is still
// one item rather than the list ending at its first newline.
func findingsList(findings []reviewFinding) string {
	var b strings.Builder
	for _, f := range findings {
		body := strings.TrimSpace(f.Body)
		if f.Located {
			body = fmt.Sprintf("`%s:%d` -- %s", f.Path, f.Line, body)
		}
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(body, "\n", "\n  "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// relayFindingsToConversation is where findings go when no draft review
// can hold them: the conversation of the task whose run wrote them,
// attributed as grain relaying an agent, exactly like the closing comment
// beside it.
func relayFindingsToConversation(ctx context.Context, store *model.Store, task model.Task,
	findings []reviewFinding, preamble string, now time.Time) error {

	body := preamble + "\n\n" + findingsList(findings)
	if _, err := relayComment(ctx, store, task, body, now); err != nil {
		return fmt.Errorf("orchestrator: recording %s's review findings: %w", task.ID, err)
	}
	return nil
}

// noteFindingsOnReview records, in the review task's own conversation,
// that its findings went to GitHub -- so the review's own thread says
// what the run produced rather than reading as a run that did nothing.
// It is the review machinery talking about itself, so it is attributed to
// the reviewer principal rather than relayed on the agent's behalf.
func noteFindingsOnReview(ctx context.Context, store *model.Store, task model.Task,
	ref model.PullRequestRef, findings []reviewFinding, now time.Time) error {

	body := fmt.Sprintf(
		"Posted %s as a draft review on %s. A draft is visible only to grain's own "+
			"credential until somebody submits it on GitHub, so the same findings are on "+
			"the task under review as well:\n\n%s",
		countOf(len(findings), "finding"), ref, findingsList(findings))
	return reviewComment(ctx, store, task.ID, body, now)
}

// noteFindingsOnReviewedTask is the other half, and the half that decides
// something: the change under review stops merging by itself.
//
// A review is dispatched to fix what it finds, and its own branch merges
// back into the branch under review for exactly that (fileReviewTask).
// What reaches this function is the remainder -- the findings it wrote
// down instead of fixing, which are by construction the ones it judged
// not to be its to make: a design question, a trade-off, something that
// needs whoever asked for the change. Letting the change merge itself
// past those the moment the review's own pull request lands would answer
// every one of them with "ship it", and would do so within a cycle or
// two, while the draft review sits unread.
//
// So auto-merge is withdrawn, and only that. The task keeps its pull
// request, its branch and its place; model.AwaitsSubmit reads a completed
// task with a pull request and no auto-merge as 'awaiting_submit', which
// is the state grain already has a button for. One click on Submit
// (ui.Client.Submit sets AutoMerge back) and the change merges the moment
// it reads clean, so the cost of being wrong here is a click, where the
// cost of the other way round is a merged change nobody read the review
// of.
//
// It is deliberately not blockMergeQueue: that gives up the queue
// position and then merges anyway on the next clean cycle, which is the
// opposite of what findings ask for. And it is deliberately not a hold
// that expires -- nothing here starts a clock, because the thing being
// waited on is a person, and defaultReviewTaskDeadline exists for the
// case where nobody was ever asked.
//
// A task that has already closed -- its pull request merged or closed
// while the review was still running -- is left alone: there is nothing
// left to hold, and a comment about withdrawing a merge that already
// happened would only mislead. The draft review is still posted, since a
// finding about code that just landed is still worth having.
func noteFindingsOnReviewedTask(ctx context.Context, store *model.Store, review, reviewed model.Task,
	ref model.PullRequestRef, findings []reviewFinding, now time.Time) error {

	closed, err := taskClosed(ctx, store, reviewed.ID)
	if err != nil {
		return err
	}
	if closed {
		return nil
	}

	withdrawn := false
	if err := store.UpdateTask(ctx, reviewed.ID, func(t *model.Task) error {
		if t.AutoMerge {
			t.AutoMerge, withdrawn = false, true
		}
		return nil
	}); err != nil {
		return fmt.Errorf("orchestrator: holding %s for its review's findings: %w", reviewed.ID, err)
	}

	body := fmt.Sprintf(
		"The review of this change (task %s) left %s on %s rather than fixing them:\n\n%s\n\n"+
			"They are also on %s as a draft review, on the lines they are about -- open it "+
			"there and submit it if you want them on the record for everyone.",
		review.ID, countOf(len(findings), "finding"), ref, findingsList(findings), ref)
	if withdrawn {
		body += fmt.Sprintf(
			"\n\nThis task is no longer merging automatically: %s waits for you now. Press "+
				"Submit once you have decided what the findings are worth -- the change merges "+
				"as soon as it reads clean after that -- or reply here to have another run "+
				"deal with them first.", ref)
	}
	if err := reviewComment(ctx, store, reviewed.ID, body, now); err != nil {
		return err
	}
	if withdrawn {
		log.Printf("orchestrator: task %s's review (task %s) left %d finding(s) on %s, so %s "+
			"no longer merges automatically and waits on a human's Submit",
			reviewed.ID, review.ID, len(findings), ref, reviewed.ID)
	}
	return nil
}

// countOf is "1 finding" / "3 findings" -- a count read by a human,
// which is why it is not fmt's own "%d finding(s)".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
