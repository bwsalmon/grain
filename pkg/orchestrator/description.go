package orchestrator

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// A pull request grain opens has to say what the change does, and the
// only account of that grain holds is the one the agent already wrote:
// the messages of the commits on the branch. Everything in this file
// exists to turn those into a description and to keep that description
// true as the branch moves.
//
// It is a restoration, not an invention. v1 seeded a freshly opened pull
// request's body with the pushed branch's tip commit message
// (github.BranchHead's own doc comment here still says so, unused until
// now), and the port to this repository dropped it: EnsurePullRequest
// wrote one metadata line and nothing else, which is why grain's own
// pull requests went back to reading as description-free.
//
// Two things about how runs work now shaped what came back:
//
//   - A run pushes several times. BuildPrompt sends every run round a
//     push/check/repair loop, so the tip commit is routinely "Fix the vet
//     warning" rather than the commit that explains the change. So the
//     description is built from every commit the branch carries over its
//     base (github.Client.CompareCommits), not from the tip alone.
//   - A run can open its own pull request before it is done
//     (OpenPullRequestForTask). Whatever it pushed by then is all the
//     description could have been built from, and the commits that
//     followed are the ones a reviewer needs. So the description is
//     rewritten, not just written once -- see refreshDescription.

// describeCommits is the description itself: what the agent wrote about
// its own change, arranged for a reviewer.
//
// The lead commit -- whose message becomes the description proper -- is
// the first one that has a body paragraph, not simply the first or the
// last. A message with a body is one written to explain a change rather
// than to name it, and on a branch of "Add the parser" / "Fix lint" the
// difference between picking that one and picking either end is the
// whole point of doing this at all. Every other commit is listed by its
// summary line underneath, so nothing on the branch goes unmentioned.
//
// Merge commits are dropped outright: "Merge branch 'main' into
// grain/task-12" is a fact about the branch's shape, not about the
// change, and a run told to merge its base back in when it conflicts
// (BuildPrompt again) produces them routinely.
//
// Empty means there was nothing to say -- no commits, or none that were
// not merges. Callers treat that as "leave the description alone" rather
// than writing an empty one.
func describeCommits(commits []github.Commit) string {
	var written []github.Commit
	for _, c := range commits {
		if !c.Merge && strings.TrimSpace(c.Message) != "" {
			written = append(written, c)
		}
	}
	if len(written) == 0 {
		return ""
	}
	lead := 0
	for i, c := range written {
		if commitBody(c.Message) != "" {
			lead = i
			break
		}
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(written[lead].Message))
	if len(written) > 1 {
		b.WriteString("\n\nAlso on this branch:\n")
		for i, c := range written {
			if i == lead {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", commitSummary(c.Message))
		}
	}
	return capDescription(strings.TrimSpace(b.String()))
}

// maxDescriptionBytes leaves room under GitHub's own 65536-byte limit on
// a pull request body for the footer and a little slack. Nothing grain
// writes should come close -- a branch would need a book's worth of
// commit messages -- but the failure if one did is a 422 on the call
// that opens the pull request, which would strand a finished run's work
// on a branch nobody was told about. A truncated description is a far
// better outcome than that.
const maxDescriptionBytes = 60000

func capDescription(description string) string {
	if len(description) <= maxDescriptionBytes {
		return description
	}
	// ToValidUTF8 rather than a bare slice: cutting at a byte offset can
	// land inside a rune, and half a rune is what GitHub renders as a
	// replacement character.
	return strings.ToValidUTF8(description[:maxDescriptionBytes], "") +
		"\n\n(Truncated -- the rest is in this branch's own commit messages.)"
}

// commitSummary is a commit message's first line, and commitBody is
// whatever follows it -- git's own convention, which is also what makes
// "this message explains something" a question that can be asked at all.
func commitSummary(message string) string {
	summary, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return strings.TrimSpace(summary)
}

func commitBody(message string) string {
	_, body, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return strings.TrimSpace(body)
}

// descriptionFooter is the line grain signs a description with, and the
// whole of the description when there is nothing else to say -- the same
// sentence the body used to consist of, so a pull request grain opened
// before any of this existed is still recognised as its own work.
//
// The task id, not an issue reference: a task has no issue to point a
// reader at any more, and its id is what `grain get` takes.
func descriptionFooter(taskID string) string {
	return fmt.Sprintf("Automated change for grain task %s.", taskID)
}

// pullRequestBody is a description plus grain's own footer, which is
// both an attribution and the marker grainAuthored reads back.
func pullRequestBody(description, taskID string) string {
	footer := descriptionFooter(taskID)
	if description == "" {
		return footer
	}
	return description + "\n\n---\n" + footer
}

// grainAuthored reports whether body is one grain wrote -- its last
// non-empty line being the footer for this very task.
//
// It is what stands between refreshDescription and a human's own
// writing. A reviewer who rewrites a pull request's description, adds a
// test plan to it, or pastes in a decision made in review has said
// something grain cannot reconstruct from commit messages, and a refresh
// that overwrote it would destroy the more valuable of the two. An empty
// body counts as grain's: nothing is lost by filling one in, and a pull
// request grain opened before a run had committed anything worth
// describing is exactly the case that leaves one.
func grainAuthored(body, taskID string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return true
	}
	lines := strings.Split(trimmed, "\n")
	return strings.TrimSpace(lines[len(lines)-1]) == descriptionFooter(taskID)
}

// describeBranch reads the commits branch carries over base and turns
// them into a description, or returns "" if it cannot -- a description
// is worth an API call, never worth failing a finish over, so every
// failure here is logged and answered with "say nothing" rather than
// returned. The pull request itself is the thing that matters, and it
// opens either way.
func describeBranch(client github.Client, repo model.RepoRef, base, branch string) string {
	if base == "" {
		return ""
	}
	commits, err := client.CompareCommits(repo.Owner, repo.Name, base, branch)
	if err != nil {
		log.Printf("orchestrator: reading %s's commits over %s to describe them: %v -- "+
			"the pull request gets grain's plain one-line body instead", branch, base, err)
		return ""
	}
	return describeCommits(commits)
}

// refreshDescription brings an already-open pull request's description
// up to date with the commits its branch now carries.
//
// This is the half of the restoration that v1 never needed. A run that
// calls open_pull_request opens its pull request mid-flight, deliberately
// (OpenPullRequestForTask's own doc comment: the point is seeing CI while
// there are still turns left to fix it), and everything it pushes
// afterwards -- including the commit that fixed what CI said -- lands on
// a pull request whose description was written before any of it existed.
// The finish path calls EnsurePullRequest again, so this runs there too,
// which is what makes the description a reviewer eventually reads the
// description of the finished change.
//
// It writes nothing at all in three cases: a body a human has touched
// (grainAuthored), a branch whose commits say nothing (describeBranch
// returning "" -- a failed read must never downgrade a good description
// to the bare footer), and a description that already matches. Errors are
// logged rather than returned, for the same reason describeBranch's are.
func refreshDescription(client github.Client, task model.Task, number int) {
	repo := *task.Target
	detail, err := client.GetPullRequest(repo.Owner, repo.Name, number)
	if err != nil {
		log.Printf("orchestrator: reading %s/%s#%d to refresh its description: %v",
			repo.Owner, repo.Name, number, err)
		return
	}
	if !grainAuthored(detail.Body, task.ID) {
		return
	}
	base := detail.BaseRef
	if base == "" {
		base = task.Base
	}
	description := describeBranch(client, repo, base, model.BranchName(task.ID))
	if description == "" {
		return
	}
	body := pullRequestBody(description, task.ID)
	if body == detail.Body {
		return
	}
	if err := client.UpdatePullRequestBody(repo.Owner, repo.Name, number, body); err != nil {
		log.Printf("orchestrator: rewriting %s/%s#%d's description: %v -- "+
			"it still says what the branch looked like when it was opened",
			repo.Owner, repo.Name, number, err)
	}
}
