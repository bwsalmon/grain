package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PullRequestRef identifies a pull request in the *target* repo, never the
// task repo — RepoRef alone is ambiguous between the two, which is exactly
// the split docs/data-model.md's "task repo vs target repo" section exists
// to keep straight.
type PullRequestRef struct {
	Repo   RepoRef
	Number int
}

// String is PullRequestRef's own representation as a task_link row's
// target: "owner/name#123". '#' cannot appear in either half of a GitHub
// repo name, so this round-trips through ParsePullRequestRef unambiguously
// with no escaping.
func (r PullRequestRef) String() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// ParsePullRequestRef reverses PullRequestRef.String.
func ParsePullRequestRef(s string) (PullRequestRef, error) {
	repoPart, numPart, ok := strings.Cut(s, "#")
	if !ok {
		return PullRequestRef{}, fmt.Errorf("pull request ref must be owner/name#number, got %q", s)
	}
	repo, err := ParseRepo(repoPart)
	if err != nil {
		return PullRequestRef{}, fmt.Errorf("pull request ref %q: %w", s, err)
	}
	number, err := strconv.Atoi(numPart)
	if err != nil {
		return PullRequestRef{}, fmt.Errorf("pull request ref %q: bad number: %w", s, err)
	}
	return PullRequestRef{Repo: repo, Number: number}, nil
}

// PrHealth is GitHub's own view of one pull request, as far as grain has
// last observed it — docs/data-model.md's PrHealth enum.
//
// UNKNOWN keeps the distinction that matters and that a close-out decision
// depends on: GitHub computes mergeability asynchronously, so a fresh push
// reading UNKNOWN means "check again next cycle," never "conflicted."
//
// PrMerged exists for documentation parity with docs/data-model.md, but
// nothing in this codebase can produce it yet: github.RESTClient.
// GetPullRequest's own doc comment records the deliberate choice not to
// distinguish a merged PR from a closed-without-merging one at the wire
// layer, since every caller here treats them identically (neither one
// takes any more commits). Reaching that distinction later costs adding a
// field there, not changing this enum.
type PrHealth string

const (
	PrUnknown    PrHealth = "unknown"
	PrClean      PrHealth = "clean"
	PrConflicted PrHealth = "conflicted"
	PrFailing    PrHealth = "failing"
	PrMerged     PrHealth = "merged"
	PrClosed     PrHealth = "closed"
)

// TrackedPullRequest is a PR grain opened (or found) for a task and is
// still watching, per docs/data-model.md's "PullRequestRef and the tracked
// PR." It is assembled, never stored: Ref/TaskID/Branch/Base/AutoMerge are
// already grain's own fields, living on the Task and its LinkFixes link
// (see orchestrator.EnsurePullRequest), and Health/Head/ObservedAt are read
// fresh from GitHub every cycle by orchestrator.SyncPullRequests — a
// second table to keep those in sync would just be a cache that could go
// stale, which is exactly what ObservedAt's own purpose (saying how stale
// this snapshot already is) argues against building.
type TrackedPullRequest struct {
	Ref       PullRequestRef
	TaskID    string
	Branch    string
	Base      string
	AutoMerge bool

	Health     PrHealth
	Head       string
	ObservedAt *time.Time
}
