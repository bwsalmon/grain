package orchestrator

import (
	"context"
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

// SyncPullRequests refreshes every completed task's tracked pull request
// and closes out the ones that finished -- the other half of core.py's
// _close_finished_prs this package owns. A PrClean PR whose task asked for
// auto_merge is merged outright; nothing here files a fix task for a
// PrConflicted/PrFailing one yet (docs/data-model.md names that as
// TrackedPullRequest's own reason to exist, but it needs propose_task's
// depends_on resolved to a real issue first -- see finish.go's own note on
// that gap).
func SyncPullRequests(ctx context.Context, store *model.Store, client github.Client, now time.Time) error {
	links, err := store.OpenPullRequestLinks(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading open pull request links: %w", err)
	}

	for _, link := range links {
		if err := syncOne(ctx, store, client, link, now); err != nil {
			return err
		}
	}
	return nil
}

func syncOne(ctx context.Context, store *model.Store, client github.Client,
	link model.TaskPullRequestLink, now time.Time) error {

	ref, err := model.ParsePullRequestRef(link.PullRequest)
	if err != nil {
		return fmt.Errorf("orchestrator: task %s: %w", link.TaskID, err)
	}
	task, err := store.GetTask(ctx, link.TaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading task %s: %w", link.TaskID, err)
	}
	if task == nil {
		return nil
	}

	detail, err := client.GetPullRequest(ref.Repo.Owner, ref.Repo.Name, ref.Number)
	if err != nil {
		return fmt.Errorf("orchestrator: reading %s: %w", ref, err)
	}
	checks, err := client.ListCheckRuns(ref.Repo.Owner, ref.Repo.Name, detail.HeadRef)
	if err != nil {
		return fmt.Errorf("orchestrator: reading check runs for %s: %w", ref, err)
	}
	health := healthFrom(detail, checks)

	if health == model.PrClean && task.AutoMerge {
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
	}

	if health != model.PrClosed && health != model.PrMerged {
		return nil
	}

	repo, number, err := parseExternalRef(task.ExternalRef)
	if err != nil {
		return fmt.Errorf("orchestrator: closing out %s: %w", link.TaskID, err)
	}
	if err := client.CloseIssue(repo.Owner, repo.Name, number); err != nil {
		return fmt.Errorf("orchestrator: closing %s#%d: %w", repo, number, err)
	}
	return observeField(ctx, store, task.ID, now, func(o *model.Observation) { o.ClosedAt = &now })
}
