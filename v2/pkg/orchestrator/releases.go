package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// SyncReleases performs the GitHub-side effect every release candidate
// still declares but has not yet had carried out -- bwsalmon/agents#398's
// cut and promote, the same declarative-intent-then-reconcile split
// SyncPullRequests already holds pkg/ui to: ui.Client's CutCandidate and
// PromoteCandidate only ever write a model.Candidate row (pkg/ui's own
// package doc comment: nothing there talks to GitHub at all), and this is
// what actually creates and moves branches for one, once per cycle, until
// it reports success.
//
// Level-triggered and idempotent like every other reconciler here
// (cycle.go's own doc comment on Reconciler): a candidate's Status only
// ever advances once its GitHub-side effect is confirmed, so a cycle that
// fails partway through leaves nothing for the next cycle to undo, only
// to retry.
func SyncReleases(ctx context.Context, store *model.Store, client github.Client, now time.Time) error {
	candidates, err := store.PendingCandidates(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading pending release candidates: %w", err)
	}

	var errs []error
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := syncCandidate(ctx, store, client, c, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncCandidate advances one candidate by exactly the step its own
// Status names. A GitHub call failing here is recorded onto the
// candidate's own LastError and reported as nil, not returned: it is
// candidate-specific and, per Reconciler's own doc comment, must not cost
// every other pending candidate this cycle -- the next cycle simply tries
// the same step again. Only a store failure -- reading or writing the
// record of what to do next -- is a cycle-level error worth returning.
func syncCandidate(ctx context.Context, store *model.Store, client github.Client, c model.Candidate, now time.Time) error {
	cfg, err := store.GetReleaseConfig(ctx, c.Repo)
	if err != nil {
		return fmt.Errorf("orchestrator: reading release config for %s: %w", c.Repo, err)
	}
	if cfg == nil {
		// The config that named ProdBranch/RCBranch was deleted out from
		// under a candidate already mid-flight. Nothing to retry towards --
		// record it and wait for a human to reconfigure or recut.
		return recordCandidateError(ctx, store, c.ID,
			fmt.Errorf("no release configuration for %s", c.Repo))
	}

	switch c.Status {
	case model.CandidateCutting:
		return cutOnGitHub(ctx, store, client, *cfg, c, now)
	case model.CandidatePromoting:
		return promoteOnGitHub(ctx, store, client, *cfg, c, now)
	default:
		return nil
	}
}

// cutOnGitHub is the issue's own "creates an rc branch with the
// appropriate name and assigns it to the rc branch name": c.Branch is
// created at cfg.ProdBranch's current tip, then cfg.RCBranch -- the
// repo's own moving pointer -- is created or moved to that same commit.
func cutOnGitHub(ctx context.Context, store *model.Store, client github.Client,
	cfg model.ReleaseConfig, c model.Candidate, now time.Time) error {

	owner, repo := c.Repo.Owner, c.Repo.Name

	head, err := client.GetBranchHead(owner, repo, cfg.ProdBranch)
	if err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("reading prod branch %q: %w", cfg.ProdBranch, err))
	}
	if head == nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("prod branch %q does not exist", cfg.ProdBranch))
	}

	exists, err := client.BranchExists(owner, repo, c.Branch)
	if err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("checking candidate branch %q: %w", c.Branch, err))
	}
	if !exists {
		if err := client.CreateBranch(owner, repo, c.Branch, head.SHA); err != nil {
			return recordCandidateError(ctx, store, c.ID, fmt.Errorf("creating candidate branch %q: %w", c.Branch, err))
		}
	}

	if err := setBranch(client, owner, repo, cfg.RCBranch, head.SHA); err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("assigning rc branch %q: %w", cfg.RCBranch, err))
	}

	if err := store.MarkCandidateCut(ctx, c.ID); err != nil {
		return fmt.Errorf("orchestrator: marking candidate %d cut: %w", c.ID, err)
	}
	return nil
}

// promoteOnGitHub is the issue's own "promote the current rc to the prod
// branch... also create a release branch with the rc number": cfg.ProdBranch
// is moved to c.Branch's own tip, and c.ReleaseBranch (named by
// PromoteCandidate, from cfg's own prefix, when the promotion was
// requested) is created at that same commit if it does not exist yet.
func promoteOnGitHub(ctx context.Context, store *model.Store, client github.Client,
	cfg model.ReleaseConfig, c model.Candidate, now time.Time) error {

	owner, repo := c.Repo.Owner, c.Repo.Name

	head, err := client.GetBranchHead(owner, repo, c.Branch)
	if err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("reading candidate branch %q: %w", c.Branch, err))
	}
	if head == nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("candidate branch %q does not exist", c.Branch))
	}

	if err := setBranch(client, owner, repo, cfg.ProdBranch, head.SHA); err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("promoting to prod branch %q: %w", cfg.ProdBranch, err))
	}

	releaseExists, err := client.BranchExists(owner, repo, c.ReleaseBranch)
	if err != nil {
		return recordCandidateError(ctx, store, c.ID, fmt.Errorf("checking release branch %q: %w", c.ReleaseBranch, err))
	}
	if !releaseExists {
		if err := client.CreateBranch(owner, repo, c.ReleaseBranch, head.SHA); err != nil {
			return recordCandidateError(ctx, store, c.ID, fmt.Errorf("creating release branch %q: %w", c.ReleaseBranch, err))
		}
	}

	if err := store.MarkCandidatePromoted(ctx, c.ID, now); err != nil {
		return fmt.Errorf("orchestrator: marking candidate %d promoted: %w", c.ID, err)
	}
	return nil
}

// setBranch creates branch at sha if it does not exist yet, or force-moves
// it there if it does -- shared by both the rc branch's own repoint on
// every cut and the prod branch's own fast-forward on every promotion,
// each of which may find their target branch either freshly minted (a
// repo's very first cut or promotion) or already real.
func setBranch(client github.Client, owner, repo, branch, sha string) error {
	exists, err := client.BranchExists(owner, repo, branch)
	if err != nil {
		return err
	}
	if exists {
		return client.UpdateBranch(owner, repo, branch, sha, true)
	}
	return client.CreateBranch(owner, repo, branch, sha)
}

// recordCandidateError saves err onto the candidate's own LastError for a
// UI to surface, and reports success to its own caller -- see
// syncCandidate's own doc comment on why a candidate-specific failure
// must not read as a cycle-level one.
func recordCandidateError(ctx context.Context, store *model.Store, id int64, err error) error {
	if markErr := store.MarkCandidateError(ctx, id, err.Error()); markErr != nil {
		return fmt.Errorf("orchestrator: recording error on candidate %d (%v): %w", id, err, markErr)
	}
	return nil
}
