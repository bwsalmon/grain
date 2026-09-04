// statemanager.go implements pkg/ui's StateRepoManager: the daemon's
// side of the bootstrap pane, which is the same three choices `grain
// state` offers from a terminal (state.go) reached from the UI instead.
//
// One manager per daemon, holding the one open repository the sync loop
// also uses, because adopting a different repository swaps that
// repository out from under the loop -- so both have to go through the
// same lock rather than through two independent handles on one working
// tree.
//
// That lock is also what makes a live import safe to do at all: cycle,
// below, pulls a merged change down and imports the settings tables of
// it into the running database, and it does that holding the same mutex
// an adopt and an on-demand sync take, so no two of the three can be
// rewriting rows at once.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/staterepo"
	"github.com/bwsalmon/grain/pkg/ui"
)

// stateManager owns this deployment's state repository.
type stateManager struct {
	dataDir string
	db      *sql.DB
	secrets *secrets.Store
	// forbidden is the live set of repos the git proxy refuses to every
	// sandbox (gitproxy.ForbiddenSet). Held here because this is the one
	// thing in a running daemon that changes which repository grain's
	// state lives in, and therefore the one thing that changes the
	// answer: refreshForbidden pushes the new one in. nil for a manager
	// with no proxy behind it, which is a test and nothing else.
	forbidden *gitproxy.ForbiddenSet

	mu   sync.Mutex
	repo *staterepo.Repo
	// lastErr is the most recent sync failure, reported in the pane so an
	// expired credential or an unreachable remote is visible where the
	// remote is configured rather than only in the daemon's journal.
	lastErr error
}

func newStateManager(dataDir string, db *sql.DB, repo *staterepo.Repo, secretStore *secrets.Store, forbidden *gitproxy.ForbiddenSet) *stateManager {
	return &stateManager{dataDir: dataDir, db: db, repo: repo, secrets: secretStore, forbidden: forbidden}
}

// noteLoadFailure records a startup load that did not entirely work but
// was not a reason to stop -- an unreachable remote, in practice
// (fatalStateRepoLoad) -- so the pane says so from the first paint
// rather than only once the sync loop's first tick has failed too. A
// deployment running on the database it has, out of touch with its
// remote, is a thing an operator should be able to see the moment they
// look, and thirty seconds of an untroubled-looking State pane is thirty
// seconds of it looking like nothing is wrong.
//
// nil is a no-op, so the caller need not ask twice whether there was
// anything to record.
func (m *stateManager) noteLoadFailure(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErr = err
}

// refreshForbidden re-reads which repositories the git proxy must refuse
// to every sandbox and pushes the answer into the set it authorizes
// against, so that a repository this deployment has just started using
// is refused -- or served -- from the next request rather than from the
// next restart.
//
// Called by everything here that changes where grain's state lives, in
// both directions: forbiddenRepos answers about the repository the
// settings now name, so adopting one that carries the encrypted secrets
// file in its history forbids it, and adopting one that never held it
// (or dropping the remote entirely) lifts a refusal that no longer
// applies.
//
// remote is what was just adopted, and is only used if the question
// cannot be answered at all -- a settings file that has gone unreadable
// between saving it and reading it back. Then the repository is refused
// rather than served, the same fail-closed rule forbiddenRepos itself
// follows: "is grain's ciphertext reachable from here" unanswered is not
// a no.
func (m *stateManager) refreshForbidden(ctx context.Context, remote string) {
	if m.forbidden == nil {
		return
	}
	repos, err := forbiddenRepos(ctx, m.dataDir)
	if err == nil {
		m.forbidden.Set(repos)
		return
	}
	log.Printf("grain: cannot tell which repositories the git proxy must refuse after pointing this "+
		"installation at %q: %v", remote, err)
	if owner, name, ok := repoFromRemote(remote); ok {
		m.forbidden.Set([]model.RepoRef{{Owner: owner, Name: name}})
		return
	}
	m.forbidden.Set(nil)
}

// sync is what the daemon's timer calls, through the manager rather than
// directly, so a sync cannot run while an adopt is halfway through
// replacing the repository it would have written to.
func (m *stateManager) sync(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cycle(ctx, false)
}

// syncAll is sync with grain's own churn written out whether or not its
// slower clock says it is due (pkg/staterepo/tier.go) -- what a human
// asking for a sync means, and what the loop owes the repository on the
// way out.
func (m *stateManager) syncAll(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cycle(ctx, true)
}

// cycle is one pass over the repository, in both directions: pull what a
// merged pull request left on the remote and make its settings live
// (staterepo.Apply), then export the database, commit and push. It
// reports whether either direction had anything to carry.
//
// Pull first, and that order is load-bearing rather than incidental. The
// export commits, so a cycle that exported first would leave a local
// commit the remote has never seen; a merge that landed in the meantime
// is a commit on the other side of the same parent, and the two together
// are exactly the divergence Pull refuses to resolve. Pulled first, the
// working tree is still where the last push left it, and the merge is a
// fast-forward.
// all says whether the export writes grain's own churn out too, or
// leaves it to its own slower clock: false for the timer's own tick,
// true for a human asking for a sync outright.
func (m *stateManager) cycle(ctx context.Context, all bool) (bool, error) {
	applied, applyErr := staterepo.Apply(ctx, m.repo, m.db, model.SchemaVersion)
	// A divergence grain made itself -- an export it committed and could
	// not push, overtaken by a merge -- is cleared here and the pull tried
	// again, rather than repeated verbatim on every tick until an operator
	// goes to the host. The reset throws away commits that hold nothing
	// but this database's own dump; the export below writes it all back
	// out on top of what was merged, so both directions come back in the
	// same cycle. A divergence anybody else made is not cleared, and falls
	// through to be reported like any other failure.
	if recoverDivergedStateRepo(ctx, m.repo, applyErr) {
		applied, applyErr = staterepo.Apply(ctx, m.repo, m.db, model.SchemaVersion)
	}
	if errors.Is(applyErr, staterepo.ErrNotApplied) {
		// The working tree is at a commit the database has not taken up --
		// a dump this build cannot read, or rows it could not insert. The
		// export must not run: it would write the database over those
		// files, commit that, and push a silent revert of whatever was
		// merged. Stopping here leaves grain running on the database it
		// has, says why in the pane, and lets the next tick try again once
		// somebody has fixed the repository.
		m.lastErr = applyErr
		return false, applyErr
	}
	// Any other failure to pull -- an unreachable remote, an expired
	// credential -- is not a reason to stop exporting. The commits pile
	// up locally and go out with the next push that works, which is the
	// same thing the loop already did before it pulled at all.
	export := staterepo.Sync
	if all {
		export = staterepo.SyncAll
	}
	changed, syncErr := export(ctx, m.repo, m.db, model.SchemaVersion)
	m.lastErr = errors.Join(applyErr, syncErr)
	return applied || changed, m.lastErr
}

// settingsRepo is which repository this deployment's settings live in,
// as owner/name, for the one caller that needs it in that shape: the
// reconcile loop's orchestrator.Config.StateRepo, which is how every
// dispatched run is told whether the checkout it has been handed is this
// grain's own settings (orchestrator's settingsRepoSection).
//
// Read through the manager's lock, and per call rather than once, for
// the reason everything else here goes through it: an adopt replaces the
// repository under a running daemon, and a run dispatched after that
// must be told about the repository this deployment reads now -- not the
// one it read when the process started.
//
// The zero RepoRef for a local-only installation, and for a remote no
// owner/name can be picked out of. Both mean "nothing to name", which
// the prompt renders as saying nothing at all; a URL grain cannot parse
// is not something a run could file a pull request against either.
func (m *stateManager) settingsRepo() model.RepoRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repo == nil {
		return model.RepoRef{}
	}
	owner, name, ok := repoFromRemote(m.repo.Remote())
	if !ok {
		return model.RepoRef{}
	}
	return model.RepoRef{Owner: owner, Name: name}
}

func (m *stateManager) Status(ctx context.Context) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status(ctx), nil
}

// status is Status' body, for the callers that already hold the lock.
// Nothing here fails the request: a working tree that cannot answer
// (mid-adopt, or on a disk that has gone away) yields a status with the
// fields it could fill and an Error naming what it could not, which is
// more use to an operator than a 500.
func (m *stateManager) status(ctx context.Context) ui.StateRepoStatus {
	out := ui.StateRepoStatus{
		Available:          true,
		Mode:               "local",
		Branch:             m.repo.Branch(),
		Dir:                m.repo.Dir(),
		BuildSchemaVersion: model.SchemaVersion,
	}
	if remote := m.repo.Remote(); remote != "" {
		out.Mode = "remote"
		out.Remote = remote
	}
	out.Head, _ = m.repo.Head(ctx)
	if v, err := staterepo.ReadSchemaVersion(m.repo.Dir()); err == nil {
		out.SchemaVersion = v
	}
	if m.secrets != nil {
		out.SecretsKeyFile = m.secrets.KeyFile()
		if pub, err := m.secrets.PublicKey(); err == nil {
			out.SecretsPublicKey = pub
		}
		if recipient, err := m.secrets.FileRecipient(); err == nil {
			out.SecretsFileRecipient = recipient
		}
		// Reported whether or not the key is merely absent: "this host
		// cannot read its own secrets file" is one condition with one fix,
		// and splitting it into a missing-key case and a wrong-key case
		// would only make the pane say it twice.
		if err := m.secrets.Check(); err != nil {
			out.SecretsError = err.Error()
		}
	}
	// Read from the repository rather than from m.lastErr, because a
	// refused workflow is not one: installWorkflow undoes its commit and
	// lets the sync carry on, so nothing about it ever reaches the error
	// this manager remembers -- deliberately, since a deployment must not
	// stop pushing its settings over a file worth one CI step. The
	// repository keeps the fact in a marker of its own, and this is where
	// it comes back out into the pane.
	if at, refused := m.repo.WorkflowRefusedAt(ctx); refused {
		out.WorkflowRefused = true
		out.WorkflowFile = staterepo.WorkflowFile
		if !at.IsZero() {
			out.WorkflowRefusedAt = &at
		}
	}
	if m.lastErr != nil {
		// A merge waiting to be loaded is a state, not a failure, and the
		// pane says a different thing about it -- so it is reported as
		// itself rather than as the last error to have come out of git.
		switch {
		// Divergence first: a diverged repository is also a repository
		// whose remote is ahead, and of the two that is the one an
		// operator can do nothing about. It keeps its Error as well as
		// its flag, because the message names the commit in the way and
		// who wrote it, which is the whole of what there is to act on.
		case errors.Is(m.lastErr, staterepo.ErrDiverged):
			out.Diverged = true
			out.Error = m.lastErr.Error()
		case errors.Is(m.lastErr, staterepo.ErrRemoteAhead):
			out.RemoteAhead = true
		default:
			out.Error = m.lastErr.Error()
		}
	}
	return out
}

// UseLocal drops the remote and keeps everything else: the working tree,
// its history and the database are untouched, so an installation that
// was pushing somewhere and no longer wants to loses nothing.
func (m *stateManager) UseLocal(ctx context.Context) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings, err := staterepo.LoadSettings(m.dataDir)
	if err != nil {
		return ui.StateRepoStatus{}, err
	}
	settings.Remote, settings.TokenFile = "", ""
	if err := staterepo.SaveSettings(m.dataDir, settings); err != nil {
		return ui.StateRepoStatus{}, err
	}
	if err := m.repo.SetRemote(ctx, ""); err != nil {
		return ui.StateRepoStatus{}, err
	}
	// A repository with no remote has no owner/repo for a sandbox to ask
	// the proxy for, so there is nothing left to refuse -- and leaving a
	// stale refusal in place would go on denying whoever now owns that
	// name on GitHub.
	m.refreshForbidden(ctx, "")
	m.lastErr = nil
	return m.status(ctx), nil
}

// Adopt points this installation at remote, in the one direction that
// makes sense: if the repository already holds a dump, that dump becomes
// the database. It is the bootstrap's "point grain at an existing
// repository" and its "start from scratch with an empty one" at once,
// because an empty repository is the same operation with nothing to
// import and everything to seed.
//
// Destructive, deliberately, and best done before this deployment has
// runs in flight: the import replaces every row, including the tasks and
// runs a live reconcile loop is holding ids from. Nothing here tries to
// make that safe -- the pane says as much, and bwsalmon/grain#174 accepts
// it outright.
func (m *stateManager) Adopt(ctx context.Context, req ui.AdoptRequest) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	remote := strings.TrimSpace(req.Remote)
	if remote == "" {
		return ui.StateRepoStatus{}, errors.New("a remote is required")
	}
	// Parsed before anything moves. A key that is not a key should be
	// rejected with the installation untouched, rather than partway
	// through adopting a repository it then cannot read.
	var secretsKey secrets.Key
	if req.SecretsKey != "" {
		var err error
		if secretsKey, err = secrets.ParseKey(req.SecretsKey); err != nil {
			return ui.StateRepoStatus{}, err
		}
	}
	settings := adoptedSettings(m.dataDir, staterepo.Settings{
		Remote: remote, Branch: strings.TrimSpace(req.Branch),
	})
	if req.Token != "" {
		path, err := writeStateRepoToken(m.dataDir, req.Token)
		if err != nil {
			return ui.StateRepoStatus{}, err
		}
		settings.TokenFile = path
	}
	// The existing working tree is a clone of a different repository, and
	// no change of URL makes it a clone of this one. Moved aside rather
	// than deleted: an operator who adopts the wrong repository has lost
	// nothing. The secrets file is not in there to be archived with it
	// any more (secretsConfig), so adopting no longer moves the one
	// unregenerable thing this deployment has out from under itself.
	archived, err := archiveStateRepo(m.dataDir)
	if err != nil {
		return ui.StateRepoStatus{}, err
	}
	if archived != "" {
		log.Printf("grain: moved the previous state repository to %s", archived)
	}
	if err := staterepo.SaveSettings(m.dataDir, settings); err != nil {
		return ui.StateRepoStatus{}, err
	}
	repo, err := openStateRepo(ctx, m.dataDir)
	if err != nil {
		return ui.StateRepoStatus{}, fmt.Errorf("opening %s: %w", remote, err)
	}
	m.repo = repo
	// Before the import, which is the slow half and can fail: from the
	// moment the settings name this repository it is the one a sandbox
	// would be served, so the proxy has to know what to make of it now
	// rather than after a step that may never finish. The clone above
	// fetched every ref, so its history is all here to be asked about.
	m.refreshForbidden(ctx, remote)
	if req.SecretsKey != "" && m.secrets != nil {
		if err := m.secrets.ImportKey(secretsKey); err != nil {
			return ui.StateRepoStatus{}, err
		}
	}
	if err := staterepo.Load(ctx, repo, m.db, model.SchemaVersion); err != nil {
		return ui.StateRepoStatus{}, fmt.Errorf("loading %s: %w", remote, err)
	}
	m.lastErr = nil
	return m.status(ctx), nil
}

// ImportSecretsKey installs the operator's own key, on its own rather
// than as part of an adopt: a deployment can be moved onto this host
// before whoever runs it has gone and fetched the key out of wherever
// they keep it, and until it arrives the secrets file it was restored
// with is ciphertext this host cannot open.
func (m *stateManager) ImportSecretsKey(ctx context.Context, key string) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secrets == nil {
		return ui.StateRepoStatus{}, errors.New("this deployment has no secret store to import a key into")
	}
	parsed, err := secrets.ParseKey(key)
	if err != nil {
		return ui.StateRepoStatus{}, err
	}
	if err := m.secrets.ImportKey(parsed); err != nil {
		return ui.StateRepoStatus{}, err
	}
	return m.status(ctx), nil
}

// Sync is the pane's "Sync now": the same cycle the timer runs, on
// demand. Both directions, deliberately -- an operator who has just
// merged a change to a template presses this to have it now rather than
// in thirty seconds, and a button that only pushed would not give them
// that. And all of it, churn included: a human who presses Sync is
// asking for the repository to match the database now, not for the half
// of it that is due.
func (m *stateManager) Sync(ctx context.Context) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// "The remote holds a commit this deployment has not taken up" is an
	// answer to "sync now", not a failure of it: the pane gets a status
	// saying so rather than an error banner, since what it asks for next
	// is a restart and not another sync. A divergence grain would not
	// resolve is the same kind of answer -- the status carries both the
	// flag and the message, and pressing Sync again would only produce
	// the same one.
	if _, err := m.cycle(ctx, true); err != nil &&
		!errors.Is(err, staterepo.ErrRemoteAhead) && !errors.Is(err, staterepo.ErrDiverged) {
		return ui.StateRepoStatus{}, err
	}
	return m.status(ctx), nil
}

// writeStateRepoToken puts a pasted push credential where the settings
// can name it: a 0600 file under <data-dir>/secrets, outside the
// repository, never in the settings file itself -- that file is not
// encrypted, and a token written into it would be a credential in
// plaintext with nothing marking it as one.
func writeStateRepoToken(dataDir, token string) (string, error) {
	dir := secretsDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("preparing %s: %w", dir, err)
	}
	path := dir + "/state-repo-token"
	if err := os.WriteFile(path, []byte(strings.TrimSpace(token)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}
