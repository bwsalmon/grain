// statemanager.go implements pkg/ui's StateRepoManager: the daemon's
// side of the bootstrap pane, which is the same three choices `grain
// state` offers from a terminal (state.go) reached from the UI instead.
//
// One manager per daemon, holding the one open repository the sync loop
// also uses, because adopting a different repository swaps that
// repository out from under the loop -- so both have to go through the
// same lock rather than through two independent handles on one working
// tree.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

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

	mu   sync.Mutex
	repo *staterepo.Repo
	// lastErr is the most recent sync failure, reported in the pane so an
	// expired credential or an unreachable remote is visible where the
	// remote is configured rather than only in the daemon's journal.
	lastErr error
}

func newStateManager(dataDir string, db *sql.DB, repo *staterepo.Repo, secretStore *secrets.Store) *stateManager {
	return &stateManager{dataDir: dataDir, db: db, repo: repo, secrets: secretStore}
}

// sync is what the daemon's timer calls, through the manager rather than
// directly, so a sync cannot run while an adopt is halfway through
// replacing the repository it would have written to.
func (m *stateManager) sync(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed, err := staterepo.Sync(ctx, m.repo, m.db, model.SchemaVersion)
	m.lastErr = err
	return changed, err
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
		} else {
			out.Error = err.Error()
		}
	}
	if m.lastErr != nil {
		out.Error = m.lastErr.Error()
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
func (m *stateManager) Adopt(ctx context.Context, remote, branch, token string) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(remote) == "" {
		return ui.StateRepoStatus{}, errors.New("a remote is required")
	}
	settings := staterepo.Settings{Remote: strings.TrimSpace(remote), Branch: strings.TrimSpace(branch)}
	if token != "" {
		path, err := writeStateRepoToken(m.dataDir, token)
		if err != nil {
			return ui.StateRepoStatus{}, err
		}
		settings.TokenFile = path
	}
	// The existing working tree is a clone of a different repository, and
	// no change of URL makes it a clone of this one. Moved aside rather
	// than deleted: an operator who adopts the wrong repository has lost
	// nothing, and the secrets file inside it is not regenerable.
	if err := archiveStateRepo(m.dataDir); err != nil {
		return ui.StateRepoStatus{}, err
	}
	if err := staterepo.SaveSettings(m.dataDir, settings); err != nil {
		return ui.StateRepoStatus{}, err
	}
	repo, err := openStateRepo(ctx, m.dataDir)
	if err != nil {
		return ui.StateRepoStatus{}, fmt.Errorf("opening %s: %w", remote, err)
	}
	m.repo = repo
	if err := staterepo.Load(ctx, repo, m.db, model.SchemaVersion); err != nil {
		return ui.StateRepoStatus{}, fmt.Errorf("loading %s: %w", remote, err)
	}
	m.lastErr = nil
	return m.status(ctx), nil
}

func (m *stateManager) Sync(ctx context.Context) (ui.StateRepoStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := staterepo.Sync(ctx, m.repo, m.db, model.SchemaVersion)
	m.lastErr = err
	if err != nil {
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
