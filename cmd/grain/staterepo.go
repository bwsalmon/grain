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
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/model"
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

// secretsConfig places the two halves of pkg/secrets, both under
// -data-dir/secrets: the private key, which has always been there, and
// the encrypted file, which has not.
//
// The encrypted file used to sit inside the state repository, so that a
// push carried a copy of it off the host. That stopped being safe the
// moment the state repository became somewhere agents are dispatched to
// work (grain/task-186): the git proxy authorizes per repository and
// streams a packfile it does not parse, so a sandbox that may clone the
// state repository gets every object in it -- the ciphertext included,
// and a file deleted three commits ago just as readily as one in the
// tree. Ciphertext an agent can carry off is still ciphertext an agent
// can carry off.
//
// Nothing is lost by moving it. The private key was never in the
// repository and is copied nowhere, so the off-host ciphertext could
// never be decrypted from a clone anyway: a restore always needed
// -data-dir/secrets, and now that directory is the whole of what a
// restore needs.
//
// Named once, here, because four call sites -- the daemon, the UI it
// serves, `grain secrets` and the controller -- have to agree on both
// paths for a key pasted in one to be the key another reads.
func secretsConfig(dataDir string) secrets.Config {
	return secrets.Config{
		File:    filepath.Join(secretsDir(dataDir), secrets.DefaultFileName),
		KeyFile: filepath.Join(secretsDir(dataDir), secrets.DefaultKeyFileName),
	}
}

// openSecrets opens this deployment's secret store, reporting once if it
// had to mint the key -- the one thing about a fresh install that is
// urgent, since a key nobody has copied anywhere is a key one disk
// failure away from taking every secret with it.
//
// It is also where an installation written by a build that kept the
// encrypted file in the state repository catches up: every path that
// reads or writes a secret comes through here, so the file is moved
// once, by whichever of them runs first, rather than by a migration step
// an operator has to know to run.
func openSecrets(dataDir string) *secrets.Store {
	moveSecretsOutOfStateRepo(dataDir)
	store := secrets.Open(secretsConfig(dataDir))
	if store.KeyCreated() {
		log.Printf("grain: generated a new secrets key at %s -- back this file up: "+
			"without it, %s cannot be decrypted by anyone, grain included",
			store.KeyFile(), store.File())
	}
	// A repository adopted from another installation arrives sealed to
	// that installation's key, and every secret in it is unreadable here
	// until the operator imports theirs. Said once at startup, where it
	// is a line about the deployment, rather than only later as a run
	// that could not resolve a credential.
	if err := store.Check(); err != nil {
		log.Printf("grain: this host cannot read %s: %v", store.File(), err)
		if recipient, rerr := store.FileRecipient(); rerr == nil && recipient != "" {
			log.Printf("grain: it is encrypted to %s -- install that key with "+
				"`grain state key import` or the Settings pane's State tab", recipient)
		}
	}
	return store
}

// moveSecretsOutOfStateRepo lifts an encrypted secrets file an earlier
// build left inside the state repository out to where secretsConfig now
// looks for it.
//
// A rename, not a copy: a copy would leave the ciphertext in the working
// tree for the next sync to commit again, which is the whole thing being
// undone. The state repository's next commit stages the removal, so the
// file leaves the tip of the branch too -- though not the history, which
// is why forbiddenRepos below still refuses a repository that ever held
// one.
//
// Failures are logged rather than returned. Every caller of this is on
// its way to open the store, and a deployment that cannot move the file
// is better off carrying on with the copy it has (and the proxy refusing
// the repository) than refusing to start.
func moveSecretsOutOfStateRepo(dataDir string) {
	from := filepath.Join(stateRepoDir(dataDir), secrets.DefaultFileName)
	if _, err := os.Stat(from); err != nil {
		return
	}
	to := secretsConfig(dataDir).File
	if _, err := os.Stat(to); err == nil {
		// Both exist: the one outside the repository is the one grain
		// reads, and guessing which is newer is not a guess worth making
		// with secrets. Said out loud so an operator can look.
		log.Printf("grain: %s still holds an encrypted secrets file, and %s exists too -- "+
			"grain reads the second and has left the first alone; delete it once you are sure", from, to)
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		log.Printf("grain: preparing %s: %v", filepath.Dir(to), err)
		return
	}
	if err := os.Rename(from, to); err != nil {
		log.Printf("grain: moving %s out of the state repository to %s: %v", from, to, err)
		return
	}
	log.Printf("grain: moved the encrypted secrets file out of the state repository, from %s to %s -- "+
		"it is beside the private key now, and %s is what a backup has to include",
		from, to, secretsDir(dataDir))
}

// forbiddenRepos is the set of repos this deployment's git proxy refuses
// to every sandbox, whatever a task's scope says
// (gitproxy.ModelAuthorizer.Forbidden).
//
// There is one candidate: this deployment's own state repository, and
// only when it holds -- or has ever held -- the encrypted secrets file.
// Dispatching a task at the state repository is the point of
// grain/task-186; a repository that carries grain's ciphertext in its
// history is the one case where that cannot be allowed, because a
// sandbox that may clone a repository may read every object in it and
// the proxy has no finer grain than the repository to refuse at.
//
// A local-only installation has nothing to add here: with no remote,
// there is no owner/repo for a sandbox to ask the proxy for in the first
// place.
//
// A repository whose history cannot be read fails closed, refused rather
// than allowed: the question is "is grain's ciphertext reachable from
// here", and an unanswered question is not a no.
func forbiddenRepos(ctx context.Context, dataDir string) ([]model.RepoRef, error) {
	settings, err := staterepo.LoadSettings(dataDir)
	if err != nil {
		return nil, err
	}
	owner, name, ok := repoFromRemote(settings.Remote)
	if !ok {
		return nil, nil
	}
	ref := model.RepoRef{Owner: owner, Name: name}
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		log.Printf("grain: cannot open the state repository to check it for secrets (%v); "+
			"the git proxy will refuse %s to every sandbox", err, ref)
		return []model.RepoRef{ref}, nil
	}
	held, err := repo.HasSecrets(ctx)
	if err != nil {
		log.Printf("grain: cannot tell whether %s holds grain's encrypted secrets file (%v); "+
			"the git proxy will refuse it to every sandbox", ref, err)
		return []model.RepoRef{ref}, nil
	}
	if !held {
		return nil, nil
	}
	log.Printf("grain: %s holds (or once held) grain's encrypted secrets file, so the git proxy "+
		"refuses it to every sandbox -- tasks cannot be dispatched against this state repository. "+
		"Removing the file does not undo it: a clone reads history. Adopt a repository that has "+
		"never held one to dispatch settings changes against it", ref)
	return []model.RepoRef{ref}, nil
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
		// Whether grain installs the CI step that runs `grain state
		// check` on pull requests against this repository, and which
		// image it runs it from. Both default to "yes, grain's published
		// image", so a deployment that has never heard of either ends up
		// with a state repository whose changes are checked.
		CheckImage: settings.CheckImage,
		NoWorkflow: settings.NoWorkflow,
	})
	if err != nil {
		return nil, err
	}
	if err := staterepo.EnsureIgnored(repo.Dir()); err != nil {
		return nil, err
	}
	return repo, nil
}

// recoverDivergedStateRepo answers one question for the two places that
// have to ask it -- the load at startup and every tick of the sync loop:
// err is a divergence, and is it one grain can clear by itself?
//
// It reports whether the caller should try again. True means the working
// tree has been reset onto the remote's branch and the commits that were
// in the way were grain's own exports, which carry only what the database
// still holds (staterepo.RecoverDiverged has the argument in full). False
// means either that err was nothing to do with divergence, or that
// somebody's own commit is in the way -- and that one is logged, because
// a deployment that has stopped syncing until a human looks at it is
// worth a line in the journal every time it is noticed.
func recoverDivergedStateRepo(ctx context.Context, repo *staterepo.Repo, err error) bool {
	if !errors.Is(err, staterepo.ErrDiverged) {
		return false
	}
	recovered, rerr := repo.RecoverDiverged(ctx)
	if rerr != nil {
		log.Printf("grain: the state repository has diverged from its remote and is not syncing: %v", rerr)
		return false
	}
	if !recovered {
		log.Printf("grain: the state repository has diverged from its remote and is not syncing: %v", err)
		return false
	}
	log.Printf("grain: the state repository had diverged from its remote -- every local commit was grain's "+
		"own export, so %s has been reset onto origin/%s and the database will be exported over it again",
		repo.Dir(), repo.Branch())
	return true
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
// The last one is a syncAll rather than a sync: an ordinary tick leaves
// grain's own churn for the churn interval to pick up
// (staterepo.DefaultChurnInterval), and "leaves nothing unwritten" has
// to mean nothing, including the runs that finished in the last few
// minutes.
//
// A failure is logged and the loop continues. A remote that is
// unreachable, or a credential that has expired, must not stop grain
// from running: the database is still the live state, and the next tick
// will push everything that accumulated in the meantime -- and will try
// the pull again.
func stateSyncLoop(ctx context.Context, sync, syncAll func(context.Context) (bool, error)) {
	ticker := time.NewTicker(stateSyncInterval)
	defer ticker.Stop()
	// The last thing logged, so a condition that persists -- an expired
	// credential, or a merged change waiting for a restart, which can
	// wait days -- is one line in the journal rather than one every
	// thirty seconds drowning everything else in it.
	var last string
	for {
		select {
		case <-ctx.Done():
			// Deliberately not ctx: it is already cancelled, and the last
			// sync is the one that must not be skipped.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSyncInterval)
			defer cancel()
			if _, err := syncAll(final); err != nil {
				log.Printf("grain: final state sync failed: %v", err)
			}
			return
		case <-ticker.C:
			_, err := sync(ctx)
			switch {
			case err == nil:
				last = ""
			case err.Error() != last:
				last = err.Error()
				log.Printf("grain: state sync failed: %v", err)
			}
		}
	}
}
