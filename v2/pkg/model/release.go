package model

import (
	"errors"
	"fmt"
	"time"
)

// ReleaseConfig is one repo's release settings -- bwsalmon/agents#398's
// prod branch, rc branch, release branch prefix and hand-edited major
// version. ProdBranch is the branch a promotion moves forward; RCBranch
// is the moving pointer a fresh cut repoints, kept distinct from the
// specific, never-moving branch a Candidate's own Branch names (docs on
// Candidate); ReleaseBranchPrefix names both, joined with a Candidate's
// own CandidateLabel. MajorVersion is edited by hand, never by grain --
// nothing here increments it -- so an operator moving to a new major line
// does so by changing this field before the next cut.
type ReleaseConfig struct {
	Repo                RepoRef
	ProdBranch          string
	RCBranch            string
	ReleaseBranchPrefix string
	MajorVersion        int
}

// CandidateStatus is where one cut release candidate is in its own
// lifecycle -- see Candidate's own doc comment for the sequence.
type CandidateStatus string

const (
	// CandidateCutting is a freshly requested cut, before the reconciler
	// has created its branch on GitHub.
	CandidateCutting CandidateStatus = "cutting"
	// CandidateActive is a cut candidate with its branch live on GitHub,
	// not yet promoted -- "the current rc" the issue's own promote button
	// acts on.
	CandidateActive CandidateStatus = "active"
	// CandidatePromoting is a requested promotion, before the reconciler
	// has moved the prod branch and cut the release branch.
	CandidatePromoting CandidateStatus = "promoting"
	// CandidatePromoted is a retired candidate: promoted once, and -- per
	// the issue's own "it cannot be promoted twice" -- never again.
	CandidatePromoted CandidateStatus = "promoted"
)

// Candidate is one release candidate cut for a repo.
//
// MajorVersion, Number and Version together are the three components the
// issue's own naming scheme asks for: MajorVersion comes from
// ReleaseConfig at the moment this candidate was cut, Number is an
// incrementing count of candidates ever cut for the repo (never reused,
// never reset by a MajorVersion edit), and Version is always 1 for now --
// the issue's own "we will add testing and rc fixes to the mix" is what
// will someday cut a second Version of the same Number in place, rather
// than a fresh Number, for a fix to an RC already out. CandidateLabel
// renders the three together.
//
// Branch is the specific branch this candidate's own cut created, named
// from CandidateLabel and never reassigned afterwards -- what the issue
// calls "an rc branch with the appropriate name". ReleaseBranch is set
// only once a promotion is requested: the permanent branch that cut
// records, named from MajorVersion and Number alone (no "-rcN" suffix,
// and no Version component -- a fix that recuts the same Number in place
// still promotes to the one release branch for that Number).
//
// Status walks cutting -> active -> promoting -> promoted, monotonically:
// nothing here ever moves a candidate backwards. LastError is the
// releases reconciler's own account of why its current step hasn't
// landed yet -- cleared the instant that step succeeds -- so a UI can
// show why a cut or promotion is taking longer than expected without
// polling GitHub itself.
type Candidate struct {
	ID            int64
	Repo          RepoRef
	MajorVersion  int
	Number        int
	Version       int
	Branch        string
	ReleaseBranch string
	Status        CandidateStatus
	CreatedAt     time.Time
	PromotedAt    *time.Time
	LastError     string
}

// CandidateLabel renders a candidate's identity as the issue's own naming
// scheme: the major release, the incrementing rc number, and the rc
// version -- "3.7-rc1".
func CandidateLabel(major, number, version int) string {
	return fmt.Sprintf("%d.%d-rc%d", major, number, version)
}

// ReleaseLabel renders the permanent release a candidate promotes to --
// "3.7", with no rc component, since a release branch outlives any one
// candidate's own Version.
func ReleaseLabel(major, number int) string {
	return fmt.Sprintf("%d.%d", major, number)
}

// ErrNoReleaseConfig is returned by CutCandidate and PromoteCandidate when
// nothing has configured a repo's prod/rc branches yet.
var ErrNoReleaseConfig = errors.New("release: repo has no release configuration")

// ErrCandidateActive is returned by CutCandidate when the repo already has
// a candidate that has not been promoted -- the issue's own "current rc"
// is singular, so a fresh cut has to wait for that one to promote first.
var ErrCandidateActive = errors.New("release: repo already has an unpromoted candidate")

// ErrNoCandidate is returned by PromoteCandidate when the repo has never
// had a candidate cut.
var ErrNoCandidate = errors.New("release: repo has no release candidate")

// ErrCandidateNotReady is returned by PromoteCandidate when the current
// candidate's branch has not finished cutting yet.
var ErrCandidateNotReady = errors.New("release: current candidate has not finished cutting yet")

// ErrAlreadyPromoted is returned by PromoteCandidate for the case the
// issue calls out by name: "it cannot be promoted twice."
var ErrAlreadyPromoted = errors.New("release: current candidate was already promoted")
