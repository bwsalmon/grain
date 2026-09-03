// staterepo.go is the daemon's side of pkg/staterepo: where the state
// repository lives under -data-dir, how it is opened, what authenticates
// its pushes, and the loop that keeps it up to date with the database.
//
// Traffic runs both ways on the timer (stateSyncLoop), but not
// symmetrically. grain is the only writer of these files while it is
// running -- the UI and the CLI reach it over REST rather than opening
// the store, which is this file's whole reason for being able to say "no
// merges" -- so the outward half is a commit and a push, never a
// resolve. The inward half is a fast-forward pull, and what it may do
// with what arrives depends on which rows changed: the whole database is
// replaced only at startup (staterepo.Load), because that replacement is
// what makes a merged deletion delete something and is not an operation
// to run underneath runs holding task and run ids. On a tick, only the
// settings tables an agent actually proposes changes to are imported
// (staterepo.Apply and its SettingsTables), which is enough for a merged
// settings change to take effect without a restart.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/staterepo"
)

// stateRepoDir is the working tree, under -data-dir beside the store it
// materialises. Both are state a redeploy must not lose, which is what
// -data-dir means.
func stateRepoDir(dataDir string) string { return filepath.Join(dataDir, "state-repo") }

// secretsDir is the private half of the layout: the key, the GitHub
// credential ladder, the sandbox token file -- everything that must stay
// on this host and out of the repository.
func secretsDir(dataDir string) string { return filepath.Join(dataDir, "secrets") }

// secretsConfig places the two halves of pkg/secrets: the encrypted file
// inside the state repository, where a push carries it off the host, and
// the private key outside it under -data-dir/secrets, where nothing
// commits it anywhere.
//
// Named once, here, because four call sites -- the daemon, the UI it
// serves, `grain secrets` and the controller -- have to agree on both
// paths for a key pasted in one to be the key another reads.
func secretsConfig(dataDir string) secrets.Config {
	return secrets.Config{
		File:    filepath.Join(stateRepoDir(dataDir), secrets.DefaultFileName),
		KeyFile: filepath.Join(secretsDir(dataDir), secrets.DefaultKeyFileName),
	}
}

// openSecrets opens this deployment's secret store, reporting once if it
// had to mint the key -- the one thing about a fresh install that is
// urgent, since a key nobody has copied anywhere is a key one disk
// failure away from taking every secret with it.
func openSecrets(dataDir string) *secrets.Store {
	store := secrets.Open(secretsConfig(dataDir))
	if store.KeyCreated() {
		log.Printf("grain: generated a new secrets key at %s -- back this file up: "+
			"without it, %s cannot be decrypted by anyone, grain included",
			store.KeyFile(), store.File())
	}
	return store
}

// openStateRepo prepares the state repository named by
// <data-dir>/state-repo.json -- cloning it if this host has not seen it
// before, and `git init`ing a local-only one if that file says nothing,
// which is what an install nobody has configured gets.
func openStateRepo(ctx context.Context, dataDir string) (*staterepo.Repo, error) {
	settings, err := staterepo.LoadSettings(dataDir)
	if err != nil {
		return nil, err
	}
	repo, err := staterepo.Open(ctx, staterepo.Config{
		Dir:    stateRepoDir(dataDir),
		Remote: settings.Remote,
		Branch: settings.Branch,
		// grain's own commits, attributed to grain: a reviewer of a
		// settings diff should be able to tell at a glance which changes
		// the daemon exported and which a human or an agent proposed.
		AuthorName:  "grain",
		AuthorEmail: "grain@" + hostnameOrLocalhost(),
		Token:       stateRepoToken(dataDir, settings),
	})
	if err != nil {
		return nil, err
	}
	if err := staterepo.EnsureIgnored(repo.Dir()); err != nil {
		return nil, err
	}
	return repo, nil
}

// stateRepoToken resolves the credential a push to an https remote
// authenticates with, per operation rather than once: a GitHub App
// installation token lasts an hour, and a daemon that cached one at
// startup would stop being able to push a day later.
//
// Two sources, in order. A token file named by the settings wins, which
// is what a bootstrap that was handed a token writes and what covers a
// state repository outside this deployment's ordinary reach. Otherwise
// the same GitHub credential ladder everything else here authenticates
// through (gitproxy.CredentialSet) answers for the remote's own
// owner/repo -- so a deployment whose state repository sits beside its
// target repositories needs no second credential configured at all.
func stateRepoToken(dataDir string, settings staterepo.Settings) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if settings.TokenFile != "" {
			data, err := os.ReadFile(settings.TokenFile)
			if err != nil {
				return "", fmt.Errorf("reading %s: %w", settings.TokenFile, err)
			}
			return strings.TrimSpace(string(data)), nil
		}
		owner, repo, ok := repoFromRemote(settings.Remote)
		if !ok {
			return "", nil
		}
		credentials, err := gitproxy.LoadCredentialSet(filepath.Join(dataDir, "secrets", "github"))
		if err != nil {
			return "", err
		}
		cred, ok := credentials.Select(owner, repo)
		if !ok || cred.Token == nil {
			return "", fmt.Errorf("no GitHub credential covers %s/%s", owner, repo)
		}
		return *cred.Token, nil
	}
}

// repoFromRemote pulls owner and repo out of a git URL, in either of the
// two forms an operator pastes: an https URL or scp-style ssh. It is
// only used to ask the credential ladder a question, so a URL it does
// not recognise is a "no answer" rather than an error.
func repoFromRemote(remote string) (owner, repo string, ok bool) {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if remote == "" {
		return "", "", false
	}
	path := remote
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		path = u.Path
	} else if at := strings.Index(remote, "@"); at >= 0 {
		if colon := strings.Index(remote[at:], ":"); colon >= 0 {
			path = remote[at+colon+1:]
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[len(parts)-2], parts[len(parts)-1], true
}

func hostnameOrLocalhost() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "localhost"
}

// stateSyncInterval is how often the daemon reconciles its database with
// the repository, in both directions.
//
// A timer rather than a hook on every write: the export is a full dump,
// which is cheap at grain's scale but not free, and batching a burst of
// writes into one commit produces a history a human can actually read --
// one commit per reconcile cycle's worth of change rather than one per
// row. Nothing is lost outbound by the delay, because the database, not
// the repository, is what the daemon reads from while it runs; inbound
// it is the upper bound on how long a merged settings change takes to
// become live, which at half a minute is a good deal less than the
// restart it used to take.
const stateSyncInterval = 30 * time.Second

// stateSyncLoop pulls, applies, exports, commits and pushes until ctx is
// cancelled, then does it once more on the way out so a clean shutdown
// leaves nothing unwritten.
//
// A failure is logged and the loop continues. A remote that is
// unreachable, or a credential that has expired, must not stop grain
// from running: the database is still the live state, and the next tick
// will push everything that accumulated in the meantime -- and will try
// the pull again.
func stateSyncLoop(ctx context.Context, sync func(context.Context) (bool, error)) {
	ticker := time.NewTicker(stateSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Deliberately not ctx: it is already cancelled, and the last
			// sync is the one that must not be skipped.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSyncInterval)
			defer cancel()
			if _, err := sync(final); err != nil {
				log.Printf("grain: final state sync failed: %v", err)
			}
			return
		case <-ticker.C:
			if _, err := sync(ctx); err != nil {
				log.Printf("grain: state sync failed: %v", err)
			}
		}
	}
}
