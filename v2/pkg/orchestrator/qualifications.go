package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// SyncQualifications is bwsalmon/agents#518's own scheduling step: the
// moment a candidate's own branch first goes live on GitHub (releases.
// go's own cutOnGitHub advancing it to CandidateActive), any
// qualification plan configured for its repo is instantiated as real
// tasks targeting that branch, and a run that has since succeeded is
// promoted automatically if its plan's own AutoPromote says so.
//
// Level-triggered like every other reconciler here: QualifiableActive
// Candidates only ever names a candidate this cycle still has something
// to do for (create a run, or check one already running), so a candidate
// with neither a plan nor a run is never even read, and one this cycle
// cannot yet promote is simply read again next cycle once its tasks have
// moved on. Unlike SyncReleases, this never talks to GitHub itself --
// everything it acts on and everything it decides lives in the store --
// so it takes a *model.Store directly rather than a whole Deps, the same
// shape SyncPullRequests and SyncReleases already have.
func SyncQualifications(ctx context.Context, store *model.Store, now time.Time) error {
	candidates, err := store.QualifiableActiveCandidates(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: listing qualifiable candidates: %w", err)
	}
	var errs []error
	for _, c := range candidates {
		if err := syncQualification(ctx, store, c, now); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: qualifying candidate %d: %w", c.ID, err))
		}
	}
	return errors.Join(errs...)
}

// syncQualification advances one candidate's own qualification by
// exactly the step it is missing: a run, if its repo has a plan and none
// exists yet, or a promotion, if one does exist, has succeeded, and its
// plan says to promote automatically.
func syncQualification(ctx context.Context, store *model.Store, c model.Candidate, now time.Time) error {
	run, err := store.CandidateQualificationRun(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("reading qualification run: %w", err)
	}
	plan, err := store.GetQualificationPlan(ctx, c.Repo)
	if err != nil {
		return fmt.Errorf("reading qualification plan: %w", err)
	}

	if run == nil {
		if plan == nil || len(plan.Items) == 0 {
			return nil
		}
		if _, err := store.CreateQualificationRun(ctx, c, *plan, now); err != nil {
			return fmt.Errorf("creating qualification run: %w", err)
		}
		return nil
	}

	if plan == nil || !plan.AutoPromote {
		return nil
	}
	if model.QualificationStatus(run.Tasks) != model.QualificationSucceeded {
		return nil
	}

	// PromoteCandidate re-reads the repo's current candidate itself, so
	// this always acts on c even though it is only ever named by repo --
	// c is Active (QualifiableActiveCandidates' own WHERE clause) and a
	// repo can only ever have one unpromoted candidate at a time
	// (Store.CutCandidate's own ErrCandidateActive), so c is necessarily
	// that repo's current one.
	if _, err := store.PromoteCandidate(ctx, c.Repo); err != nil {
		// ErrCandidateActive/ErrCandidateNotReady/ErrAlreadyPromoted all
		// mean a human (or an earlier cycle) already acted on this same
		// candidate between the read above and this write -- benign, and
		// left for the next cycle to see the outcome of rather than
		// reported as a failure of this one.
		if errors.Is(err, model.ErrCandidateActive) || errors.Is(err, model.ErrCandidateNotReady) ||
			errors.Is(err, model.ErrAlreadyPromoted) || errors.Is(err, model.ErrNoReleaseConfig) {
			return nil
		}
		return fmt.Errorf("auto-promoting candidate %d: %w", c.ID, err)
	}
	return nil
}
