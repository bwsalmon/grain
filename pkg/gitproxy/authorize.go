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
}

// Authorize reports whether sandbox may perform action against owner/repo.
// git-receive-pack (push) is allowed only against the task's own write
// Target -- a Reads repo grants nothing, matching Task.Reads' own
// contract in model/task.go. Every other action (info/refs,
// git-upload-pack) is allowed against the Target or any Reads entry.
func (a *ModelAuthorizer) Authorize(ctx context.Context, sandbox, owner, repo, action string) (bool, error) {
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
