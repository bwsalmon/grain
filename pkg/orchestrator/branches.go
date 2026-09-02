package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// SyncBranches performs the GitHub-side effect every requested branch
// still declares but has not yet had carried out -- bwsalmon/agents#638's
// own "create a new branch on a repo... This should just create the given
// branch in github": ui.Client.CreateBranch only ever writes a row, and
// this is what actually creates it, at the repo's current default branch
// tip, the same declarative-intent-then-reconcile split SyncReleases
// already holds pkg/ui to.
func SyncBranches(ctx context.Context, store *model.Store, client github.Client, now time.Time) error {
	var errs []error

	branches, err := store.PendingBranches(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading pending branches: %w", err)
	}
	for _, b := range branches {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := syncBranch(ctx, store, client, b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncBranch creates b.Name on GitHub at repo's current default branch
// tip. A GitHub call failing here -- including a name already in use,
// which CreateBranch's own doc comment says GitHub answers with a 422 --
// is recorded onto b's own LastError and reported as nil, not returned,
// the same "one pending row's own failure must not cost every other
// pending one this cycle" reasoning syncCandidate already documents.
func syncBranch(ctx context.Context, store *model.Store, client github.Client, b model.Branch) error {
	owner, repo := b.Repo.Owner, b.Repo.Name

	def, err := client.DefaultBranch(owner, repo)
	if err != nil {
		return recordBranchError(ctx, store, b.ID, fmt.Errorf("reading default branch: %w", err))
	}
	head, err := client.GetBranchHead(owner, repo, def)
	if err != nil {
		return recordBranchError(ctx, store, b.ID, fmt.Errorf("reading default branch %q: %w", def, err))
	}
	if head == nil {
		return recordBranchError(ctx, store, b.ID, fmt.Errorf("default branch %q does not exist", def))
	}
	if err := client.CreateBranch(owner, repo, b.Name, head.SHA); err != nil {
		return recordBranchError(ctx, store, b.ID, fmt.Errorf("creating branch %q: %w", b.Name, err))
	}

	if err := store.MarkBranchCreated(ctx, b.ID); err != nil {
		return fmt.Errorf("orchestrator: marking branch %d created: %w", b.ID, err)
	}
	return nil
}

// recordBranchError saves err onto the branch's own LastError for a UI to
// surface, and reports success to its own caller -- see syncBranch's own
// doc comment on why one branch's own failure must not cost every other
// pending one this cycle.
func recordBranchError(ctx context.Context, store *model.Store, id int64, err error) error {
	if markErr := store.MarkBranchError(ctx, id, err.Error()); markErr != nil {
		return fmt.Errorf("orchestrator: recording error on branch %d (%v): %w", id, err, markErr)
	}
	return nil
}
