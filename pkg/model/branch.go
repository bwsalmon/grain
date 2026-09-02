package model

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Branch is one branch a human has asked grain to create on a repo --
// bwsalmon/agents#638's own "Add the ability to create a new branch on a
// repo from the repo page. This should just create the given branch in
// github." Name is exactly the ref a human typed; Status walks pending ->
// created, the same declarative-intent-then-reconcile shape Release
// already holds to.
type Branch struct {
	ID        int64
	Repo      RepoRef
	Name      string
	Status    BranchStatus
	CreatedAt time.Time
	// LastError is the branches reconciler's own account of why Name has
	// not been created on GitHub yet, cleared the instant that succeeds.
	LastError string
}

// BranchStatus is where one requested branch is in its own lifecycle.
type BranchStatus string

const (
	// BranchPending is a freshly requested branch, before the branches
	// reconciler has created it on GitHub.
	BranchPending BranchStatus = "pending"
	// BranchCreated is a branch confirmed live on GitHub.
	BranchCreated BranchStatus = "created"
)

// branchNameSafe is what a branch Name has to match to be safe to hand
// GitHub's own git-database API -- the same charset orchestrator's own
// gitSafe (checkout.go) already requires of a task's target branch,
// including '/' for a name like "feature/foo", unlike
// releaseNameSafe, which excludes it since a release name is always one
// path segment.
var branchNameSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidBranchName reports whether name is safe to create on GitHub as a
// ref -- non-empty, drawn from branchNameSafe's own charset, and free of
// the "..", leading/trailing or doubled "/" that a git ref itself
// refuses.
func ValidBranchName(name string) bool {
	if !branchNameSafe.MatchString(name) {
		return false
	}
	return !strings.Contains(name, "..") && !strings.Contains(name, "//") && !strings.HasSuffix(name, "/")
}

// ErrInvalidBranchName is returned by CreateBranch when name is empty or
// contains something unsafe to create on GitHub as a ref.
var ErrInvalidBranchName = errors.New("branch: invalid branch name")
