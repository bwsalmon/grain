package gitproxy

// What replaces the static repo-allowlist.json from grain/proxy: a
// sandbox may touch exactly the repos its own currently-live task
// declares -- its Target for read and write, its Reads for read only --
// answered by asking model's store rather than a file an operator
// keeps in sync by hand. A sandbox with no live run authorizes nothing,
// which is the same fail-closed default a missing allowlist file gave,
// arrived at without one existing.

import (
	"context"
	"fmt"

	"github.com/bwsalmon/grain/pkg/model"
)

// Authorizer decides whether a sandbox may perform action against
// owner/repo through the proxy.
type Authorizer interface {
	Authorize(ctx context.Context, sandbox, owner, repo, action string) (bool, error)
}

// Refuser is an Authorizer that refuses some repos to every sandbox and
// can say why. Optional: Handle asks for it and carries on without it,
// so a test stub or a future Authorizer that has no such repos need not
// implement anything. ModelAuthorizer does, for the one repo that has to
// be refused this way -- grain's own state, when it holds the encrypted
// secrets file.
type Refuser interface {
	Refusal(owner, repo string) (reason string, refused bool)
}

// ScopeLookup resolves a sandbox to its current write target and
// read-only repos. model.Store implements this today via GitScope; a
// test supplies a stub instead.
type ScopeLookup interface {
	GitScope(ctx context.Context, sandbox string) (*model.RepoRef, []model.RepoRef, error)
}

// CredentialOverrideLookup resolves a sandbox to the named credential its
// live task asks the proxy to use in place of the owner/repo ladder --
// bwsalmon/agents#52's `grain-github-<name>` label, by way of
// model.GitCredentialGrant. model.Store implements this via
// GitCredentialOverride; a test supplies a stub instead. Kept separate
// from ScopeLookup because it answers a different question (which
// credential, not which repo) that GitProxy.Handle only needs to ask
// once authorization has already allowed the request through.
type CredentialOverrideLookup interface {
	GitCredentialOverride(ctx context.Context, sandbox string) (name string, ok bool, err error)
}

// ModelAuthorizer is the Authorizer backed by the live task model.
type ModelAuthorizer struct {
	Store ScopeLookup
	// Forbidden names repos no sandbox may reach through this proxy, in
	// any way, whatever its task scope says -- checked before the scope
	// is even read, so being a task's own Target does not get past it.
	//
	// It exists for one repository: grain's own state (pkg/staterepo)
	// when that repository holds, or has ever held, the encrypted
	// secrets file. Dispatching a task at the state repository is a
	// supported and intended thing to do -- it is how a settings change
	// becomes a pull request -- but authorization here is per repository
	// and cannot be anything else: this proxy streams a packfile it does
	// not parse, so a sandbox allowed to fetch a repository is a sandbox
	// that gets every object in it, including a file deleted long ago.
	// A state repository written by a build that kept secrets.enc in it
	// is therefore one no sandbox may clone, and saying so here is the
	// only place that can be enforced. cmd/grain's startGitProxy decides
	// which repo, if any, that is; see staterepo.Repo.HasSecrets.
	Forbidden []model.RepoRef
}

// ForbiddenReason is the message a request for a Forbidden repo is
// refused with. It is returned to the caller, so it says what to do
// rather than only that the answer is no: an agent whose task really
// does target that repository would otherwise read a bare denial as
// grain being broken.
const ForbiddenReason = "grain's own state repository holds (or once held) its encrypted secrets file, " +
	"so no sandbox may fetch or push it: everything a sandbox can clone is everything it can read. " +
	"A state repository that has never held that file can be dispatched against normally"

// forbids reports whether owner/repo is one of the repos no sandbox may
// reach at all.
func (a *ModelAuthorizer) forbids(owner, repo string) bool {
	for _, r := range a.Forbidden {
		if canonicalMatches(r, owner, repo) {
			return true
		}
	}
	return false
}

// Refusal implements Refuser: it is how the denial above reaches the
// caller as an explanation rather than as the generic "not in scope"
// every other refusal gets.
func (a *ModelAuthorizer) Refusal(owner, repo string) (string, bool) {
	if a.forbids(canonicalizeRepo(owner, repo)) {
		return ForbiddenReason, true
	}
	return "", false
}

// Authorize reports whether sandbox may perform action against owner/repo.
// git-receive-pack (push) is allowed only against the task's own write
// Target -- a Reads repo grants nothing, matching Task.Reads' own
// contract in model/task.go. Every other action (info/refs,
// git-upload-pack) is allowed against the Target or any Reads entry.
func (a *ModelAuthorizer) Authorize(ctx context.Context, sandbox, owner, repo, action string) (bool, error) {
	// Before the scope is read at all: a forbidden repo is forbidden to
	// every sandbox, and a request for one must not depend on a store
	// read that could fail open if this were ordered the other way.
	if a.forbids(canonicalizeRepo(owner, repo)) {
		return false, nil
	}
	target, reads, err := a.Store.GitScope(ctx, sandbox)
	if err != nil {
		return false, fmt.Errorf("gitproxy: resolving scope for %s: %w", sandbox, err)
	}
	owner, repo = canonicalizeRepo(owner, repo)

	matchesTarget := target != nil && canonicalMatches(*target, owner, repo)
	if action == "git-receive-pack" {
		return matchesTarget, nil
	}
	if matchesTarget {
		return true, nil
	}
	for _, r := range reads {
		if canonicalMatches(r, owner, repo) {
			return true, nil
		}
	}
	return false, nil
}

func canonicalMatches(r model.RepoRef, owner, repo string) bool {
	rOwner, rRepo := canonicalizeRepo(r.Owner, r.Name)
	return rOwner == owner && rRepo == repo
}
