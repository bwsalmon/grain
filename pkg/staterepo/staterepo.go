// Package staterepo keeps grain's database in a git repository.
//
// The store itself is still the embedded SQLite database pkg/model/sqlite
// opens -- every query in pkg/model is unchanged, and nothing above this
// package knows a repository exists. What changes is where that database
// comes from and where it goes: the repository's working tree holds the
// same rows as text (Export/Import, in dump.go), so the source of truth
// an operator backs up, and an agent reads, is a directory of diffable
// files rather than an opaque binary an agent cannot open.
//
// That is the whole point of it. grain's settings -- templates, suites,
// repo config, prompt extensions -- are the kind of thing a task is
// forever asking to change, and until now the only way to change one was
// a hand edit through the UI, because a row in a SQLite file is not
// something an agent can propose a diff to. As text in a repository they
// go through the mechanism grain already has for changing anything else:
// a branch, a pull request, a review, a merge.
//
// Concurrency is deliberately not modelled. The daemon is the only writer
// (see cmd/grain's daemon.go: the UI and the CLI reach it over REST), so
// a sync cycle is "pull, write what the database says, commit it, push
// it" with no merge to resolve. An agent's change arrives the other way,
// as a merged pull request, and Pull brings it back down: Apply imports
// the settings tables of it into a daemon that is already running, which
// is as much of a wholesale replacement as is safe to do underneath live
// runs, and Load imports the same tables at a start. The whole dump is
// imported in one case only, a clone this host has never loaded, which
// is the restore.
//
// The remote is optional, and that is not a fallback but a supported
// deployment: Config.Remote left empty gives a repository with no origin
// at all -- `git init` in the data directory, commits that go nowhere.
// A local install therefore needs no GitHub account, no credential and
// no answer from the operator, and can be pointed at a real remote later
// (Adopt/PublishTo) without anything being reformatted.
package staterepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBranch is the branch a state repository lives on when Config
// does not say otherwise. Named here rather than assumed at each call
// site: `git init` picks a branch name from the invoking user's own git
// configuration (init.defaultBranch), which is exactly the sort of thing
// that differs between a developer's laptop and a deployed host, and a
// repository whose branch name depends on who created it is one a push
// cannot find later.
const DefaultBranch = "main"

// Config says where the state repository lives and, optionally, which
// remote it is a clone of.
type Config struct {
	// Dir is the working tree. It need not exist yet.
	Dir string
	// Remote is the URL of the repository this one tracks, or "" for a
	// local-only install. An https URL is what Token authenticates
	// against; an ssh URL authenticates however the host's own ssh does,
	// and Token is not consulted.
	Remote string
	// Branch is the branch state lives on; DefaultBranch if empty.
	Branch string
	// AuthorName and AuthorEmail identify grain's own commits. Both are
	// given to git explicitly rather than left to the host's git config,
	// which on a deployed machine is usually not set at all -- and an
	// unset user.email is a commit that fails, not one that is merely
	// anonymous.
	AuthorName  string
	AuthorEmail string
	// Token, if set, is called for the credential to push and fetch an
	// https remote with. It is called per operation rather than once, so
	// a token that expires (a GitHub App installation token lasts an
	// hour) is re-read rather than cached until it fails.
	Token func(ctx context.Context) (string, error)
	// CheckImage is the container image the CI workflow grain installs
	// runs `grain state check` from; DefaultCheckImage when empty. The
	// check refuses a dump stamped with any other schema, so what belongs
	// here is the build this deployment is running -- which is what
	// cmd/grain passes, from the reference stamped into its own image
	// (cmd/grain/grainimage.go), unless an operator has named one in
	// state-repo.json.
	//
	// It is not only written into a workflow that is missing: an
	// installed one that is still grain's own rendering -- this build's
	// or an earlier grain's -- is repointed here as well, so an upgraded
	// deployment's check follows it. See StaleWorkflow.
	CheckImage string
	// NoWorkflow stops grain from installing that workflow at all: the
	// opt-out for a deployment whose state repository is checked
	// somewhere else, or by something other than GitHub Actions. It is a
	// switch rather than "delete the file", because deleting a file grain
	// writes when it is missing is not a decision that stays made -- see
	// installWorkflow in format.go, which also leaves a workflow somebody
	// has edited exactly as they left it.
	NoWorkflow bool
	// ChurnInterval is how often the tier-churn tables are written out;
	// DefaultChurnInterval if zero. Every other table is written out on
	// every Sync. See tier.go for which tables those are and why they get
	// a clock of their own.
	ChurnInterval time.Duration
	// Now is the clock ChurnInterval is measured against, and nil means
	// time.Now. It exists for the measurement in growth_test.go, which
	// drives a simulated day through this package in a few minutes of
	// wall clock and would otherwise be measuring its own runtime rather
	// than the cadence it set out to.
	Now func() time.Time
}

// DefaultChurnInterval is how often grain's own record of what it did --
// runs, leases, observations, read marks -- reaches the repository.
//
// An hour, against a 30-second sync, because the two halves are answers
// to different questions. Settings have to be current: the repository is
// where they are read and reviewed, and a change made in the UI that is
// not in the repository yet is a change nobody can see. Churn only has
// to be recoverable, and the database on disk is the live copy of it --
// the repository is the off-host backup, so the cost of an hour's lag is
// an hour of run history lost in a total loss of the host, which is a
// price worth paying to stop that history from being rewritten 2,880
// times a day.
const DefaultChurnInterval = time.Hour

// Repo is an opened state repository. Build one with Open.
type Repo struct {
	cfg    Config
	branch string
}

// Dir returns the working tree.
func (r *Repo) Dir() string { return r.cfg.Dir }

// Branch returns the branch state lives on.
func (r *Repo) Branch() string { return r.branch }

// Remote returns the tracked remote, or "" for a local-only repository.
func (r *Repo) Remote() string { return r.cfg.Remote }

// now is the clock Config.Now names.
func (r *Repo) now() time.Time { return r.cfg.Now() }

// Open prepares the state repository, creating it if it is not there.
//
// Three cases, and only three: a working tree that is already a git
// repository is used as it is (its remote reconciled to Config.Remote,
// so changing the remote in the bootstrap does not mean re-cloning);
// a Config with a Remote and no repository yet is cloned; a Config with
// neither is `git init`ed and left with no origin.
//
// A clone of a remote that exists but is empty -- which is what "create
// a repository on GitHub and point grain at it" produces -- is not an
// error and not a special case an operator has to know about: git clone
// of an empty repository succeeds and leaves a working tree with no
// commits, which Ensure then seeds exactly as it seeds a local one.
func Open(ctx context.Context, cfg Config) (*Repo, error) {
	if cfg.Dir == "" {
		return nil, errors.New("staterepo: no directory configured")
	}
	branch := cfg.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	if cfg.AuthorName == "" {
		cfg.AuthorName = "grain"
	}
	if cfg.AuthorEmail == "" {
		cfg.AuthorEmail = "grain@localhost"
	}
	if cfg.ChurnInterval <= 0 {
		cfg.ChurnInterval = DefaultChurnInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Repo{cfg: cfg, branch: branch}

	exists, err := isRepo(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("staterepo: preparing %s: %w", cfg.Dir, err)
		}
		empty, err := isEmptyDir(cfg.Dir)
		if err != nil {
			return nil, err
		}
		switch {
		case cfg.Remote != "" && empty:
			if err := r.clone(ctx); err != nil {
				return nil, err
			}
		default:
			// A remote with files already in the directory inits instead
			// of cloning, because `git clone` refuses a destination that
			// is not empty ("destination path already exists and is not
			// an empty directory") -- and that directory is the ordinary
			// case on a deployed host, not a corner one: scripts/setup.sh
			// writes the encrypted secrets file into this tree before the
			// daemon has ever started (its minter-key seed), and carries
			// it across by hand when a schema bump moves the old tree
			// aside. Refusing there would leave a deployment that cannot
			// start over a file grain itself put there.
			//
			// Nothing is lost by not cloning: configure adds origin
			// below, and Load's first Pull fetches the branch and resets
			// a repository with no commits onto it -- which brings the
			// remote's own copy of every file down over whatever was
			// sitting here, exactly as a clone would have.
			if err := r.init(ctx); err != nil {
				return nil, err
			}
		}
	}
	if err := r.configure(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// isRepo reports whether dir already holds a git repository. A directory
// that exists but holds no .git is not one -- Open clones or inits into
// it, which git is happy to do as long as it is empty of anything but
// dotfiles it does not mind.
func isRepo(dir string) (bool, error) {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err == nil {
		return info.IsDir() || info.Mode().IsRegular(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("staterepo: inspecting %s: %w", dir, err)
}

// isEmptyDir reports whether dir holds nothing at all, which is the
// question `git clone` asks of a destination it is given.
func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("staterepo: inspecting %s: %w", dir, err)
	}
	return len(entries) == 0, nil
}

func (r *Repo) init(ctx context.Context) error {
	if _, err := r.git(ctx, "init", "--initial-branch="+r.branch, r.cfg.Dir); err != nil {
		return err
	}
	return nil
}

func (r *Repo) clone(ctx context.Context) error {
	// Cloned into the directory itself rather than under it, and with
	// --branch left off: an empty remote has no branch to ask for, and
	// asking for one that does not exist is a clone that fails.
	_, err := r.gitAuthed(ctx, "clone", "--quiet", r.cfg.Remote, r.cfg.Dir)
	if err != nil {
		return fmt.Errorf("staterepo: cloning %s: %w", r.cfg.Remote, err)
	}
	// A clone of an empty remote lands on whatever branch the remote's
	// HEAD advertises, which for a fresh GitHub repository is main but
	// need not be. Naming it ourselves keeps the local branch the one
	// Config asked for either way.
	if head, err := r.currentBranch(ctx); err == nil && head != r.branch {
		if empty, _ := r.isEmpty(ctx); empty {
			_, _ = r.git(ctx, "checkout", "-q", "-b", r.branch)
		}
	}
	return nil
}

// configure writes the identity and the remote into the repository's own
// config, so every later command inherits them and nothing has to pass
// -c on each invocation.
func (r *Repo) configure(ctx context.Context) error {
	for _, kv := range [][2]string{
		{"user.name", r.cfg.AuthorName},
		{"user.email", r.cfg.AuthorEmail},
		// grain's commits are its own; signing is the operator's business
		// and a host with commit.gpgsign set globally would otherwise
		// break every automated commit with a passphrase prompt.
		{"commit.gpgsign", "false"},
		// How many loose objects `gc --auto` (maintain) will tolerate
		// before packing them. git's own default is 6700, which is tuned
		// for a repository whose objects are source files a person wrote:
		// this one's are whole database dumps, tens of megabytes each, so
		// 6700 of them is hundreds of gigabytes of disk sitting unpacked.
		// A few dozen commits' worth is the right order here, and packing
		// that often is cheap because it is exactly what git is good at.
		{"gc.auto", "128"},
		// Synchronously, not in a background process that outlives the
		// call. The daemon runs in a container that can be stopped between
		// one sync and the next, and a detached gc killed halfway is a
		// repository nobody was watching over. Run inline it is bounded by
		// the same timeout every other git command here has.
		{"gc.autoDetach", "false"},
	} {
		if _, err := r.git(ctx, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return r.setRemote(ctx, r.cfg.Remote)
}

// setRemote reconciles origin with url: added, updated or removed so the
// repository tracks exactly what Config says. Removing it is what
// converting a remote install back to a local one means, and it leaves
// every commit already made in place.
func (r *Repo) setRemote(ctx context.Context, url string) error {
	current, err := r.git(ctx, "remote")
	if err != nil {
		return err
	}
	has := false
	for _, name := range strings.Fields(current) {
		if name == "origin" {
			has = true
		}
	}
	switch {
	case url == "" && has:
		_, err = r.git(ctx, "remote", "remove", "origin")
	case url != "" && has:
		_, err = r.git(ctx, "remote", "set-url", "origin", url)
	case url != "":
		_, err = r.git(ctx, "remote", "add", "origin", url)
	}
	if err != nil {
		return err
	}
	r.cfg.Remote = url
	return nil
}

// SetRemote points an already-opened repository at a different remote (or
// at none). This is the bootstrap's "adopt a repository" step: the
// working tree, its history and its contents are untouched, only where
// they push to changes.
func (r *Repo) SetRemote(ctx context.Context, url string) error { return r.setRemote(ctx, url) }

// isEmpty reports whether the repository has no commits yet.
func (r *Repo) isEmpty(ctx context.Context) (bool, error) {
	_, err := r.git(ctx, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return true, nil
	}
	return false, nil
}

// currentBranch is the branch HEAD is on, including the unborn one a
// clone of an empty repository lands on.
//
// `git branch --show-current` rather than `rev-parse --abbrev-ref HEAD`,
// which is what this used to run: rev-parse resolves HEAD to a commit
// first, so on a branch with no commits yet it fails outright ("unknown
// revision"). That is exactly the case clone's caller has to answer --
// adopting an *empty* remote -- and the failure there was silent, since
// the rename to Config.Branch is skipped on any error. A remote whose
// HEAD advertised "master" (a `git init --bare` with no
// init.defaultBranch, or an older repository) therefore left every
// commit on master while every push named main, and adopting it failed
// with git's own "src refspec main does not match any" (found by hand,
// task 244).
func (r *Repo) currentBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Commit stages everything in the working tree and commits it, reporting
// whether there was anything to commit. A no-op sync is the common case
// -- the daemon exports on a timer -- so "nothing changed" is a false and
// a nil error rather than something a caller has to recognise in an
// error string.
func (r *Repo) Commit(ctx context.Context, message string) (bool, error) {
	if _, err := r.git(ctx, "add", "--all", "."); err != nil {
		return false, err
	}
	status, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := r.git(ctx, "commit", "--quiet", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// Push sends the branch to origin. A repository with no remote is not an
// error: a local-only install calls the same sync path as a remote one,
// and this is where that stops.
func (r *Repo) Push(ctx context.Context) error {
	if r.cfg.Remote == "" {
		return nil
	}
	if empty, _ := r.isEmpty(ctx); empty {
		return nil
	}
	if _, err := r.gitAuthed(ctx, "push", "--quiet", "origin", r.branch); err != nil {
		return fmt.Errorf("staterepo: pushing %s: %w", r.branch, err)
	}
	return nil
}

// ErrUnreachable marks the one failure in this package that says nothing
// at all about what the repository holds: the fetch itself did not
// complete.
//
// A network blip, an installation token that expired an hour ago, a
// repository renamed on GitHub -- git reports them differently and they
// mean the same thing here, which is that grain knows exactly what it
// knew before it asked. That is a different fact from "the repository
// holds something this build must not overwrite" (ErrSchemaTooNew and
// its neighbours in bind.go), and only the second is worth refusing to
// start over; the sentinel exists so that the difference is something a
// caller keys off rather than something it matches an error string for.
// Load, and cmd/grain's own run(), are where that decision is written
// down.
var ErrUnreachable = errors.New("the state repository's remote could not be reached")

// Pull fast-forwards the working tree to the remote's branch, reporting
// whether anything arrived.
//
// Fast-forward only, on purpose. The daemon is the only writer of these
// files, so a remote that is ahead of us means one thing -- a pull
// request was merged -- and there is nothing to merge. Divergence means
// something happened that this package's model does not describe, and
// failing loudly with the two commits named beats resolving a conflict
// in a database dump by guesswork.
//
// That refusal is marked with ErrDiverged rather than left as whatever
// git said, because it is the one failure here a caller can do something
// about: RecoverDiverged (diverge.go) clears the case where the only
// commits in the way are grain's own exports, which is what a push that
// failed before a merge landed leaves behind.
//
// A fetch that never got off the ground is marked too, with
// ErrUnreachable, and for the opposite reason: there is nothing to do
// about it and nothing to act on, so a caller that has a working tree
// already can carry on with it and ask again on the next tick.
func (r *Repo) Pull(ctx context.Context) (bool, error) {
	if r.cfg.Remote == "" {
		return false, nil
	}
	if _, err := r.gitAuthed(ctx, "fetch", "--quiet", "origin", r.branch); err != nil {
		// A remote with no such branch yet (a repository created empty and
		// never pushed to) is not a failure to pull, it is nothing to pull.
		if r.remoteBranchMissing(ctx) {
			return false, nil
		}
		// Marked, because a fetch that did not happen is the one failure
		// here a caller can carry on from: see ErrUnreachable. Everything
		// below this point is local git against a tree we did reach, and
		// keeps its own words.
		return false, fmt.Errorf("%w: fetching %s: %w", ErrUnreachable, r.branch, err)
	}
	before, _ := r.git(ctx, "rev-parse", "HEAD")
	if empty, _ := r.isEmpty(ctx); empty {
		if _, err := r.git(ctx, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := r.git(ctx, "merge", "--ff-only", "--quiet", "FETCH_HEAD"); err != nil {
		// A merge that will not fast-forward is usually divergence and is
		// not always: a working tree with local modifications refuses the
		// same way. Asked rather than assumed, so that the sentinel a
		// caller keys recovery off means what it says.
		if ahead, aerr := r.commitsIn(ctx, "FETCH_HEAD..HEAD"); aerr == nil && len(ahead) > 0 {
			return false, fmt.Errorf("%w: %s and origin/%s have both moved past their common "+
				"parent; grain will not merge a database dump by guesswork: %w",
				ErrDiverged, r.branch, r.branch, err)
		}
		return false, fmt.Errorf("staterepo: fast-forwarding %s to origin/%s: %w", r.branch, r.branch, err)
	}
	after, _ := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(before) != strings.TrimSpace(after), nil
}

// remoteState is where origin's branch sits relative to this working
// tree, and it is both of the questions a sync cycle has to ask about
// the remote rather than one, because one ls-remote answers them both.
//
// ahead means origin holds commits this working tree does not -- which,
// for this repository, means exactly one thing: a pull request against
// grain's own state was merged. behind means the opposite: this host
// holds commits origin has not got, which is what an export that was
// committed and could not be pushed leaves behind. Neither is the
// ordinary tick, where the two agree.
//
// known is false when there was nothing to ask -- a local-only install
// with no remote -- and when the asking itself failed: a network blip, a
// credential that expired. Nothing at all is known then, so a caller
// carries on exactly as it would have without this check, and the push
// reports the network in its own words. That is also why an unreachable
// remote is not reported as an error here: no answer and "no, neither"
// lead to the same place, and the pull half of the cycle is where an
// operator is told the remote has gone (Apply, and ErrUnreachable
// above).
type remoteState struct {
	known  bool
	ahead  bool
	behind bool
}

// remoteState asks origin where its branch is, in one ls-remote and
// without fetching an object.
//
// The ahead half is asked before every export (see sync) because of what
// would otherwise happen. The daemon would commit its own dump on top of
// a history the remote has moved past, the push would be rejected as a
// non-fast-forward on that tick and on every tick after it, and the next
// start would find the two diverged and refuse to load at all -- a grain
// that will not come up because somebody merged the change it asked
// them to. Noticing first costs one ls-remote and turns all of that into
// a sentence an operator can act on.
//
// The behind half is what that same answer says about the other
// direction, for free, and sync uses it to decide whether to push: see
// the note there on why "did this cycle commit something" is the wrong
// question to hang a push on.
func (r *Repo) remoteState(ctx context.Context) remoteState {
	if r.cfg.Remote == "" {
		return remoteState{}
	}
	out, err := r.gitAuthed(ctx, "ls-remote", "--heads", "origin", r.branch)
	if err != nil {
		return remoteState{}
	}
	empty, _ := r.isEmpty(ctx)
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		// No such branch on the remote yet: an empty repository, which is
		// behind us by every commit we have, and behind us by nothing at
		// all if we have none either.
		return remoteState{known: true, behind: !empty}
	}
	head := fields[0]
	// A commit this clone has never fetched cannot be one of ours, and a
	// working tree with no commits at all has nothing it could be.
	if empty {
		return remoteState{known: true, ahead: true}
	}
	if _, err := r.git(ctx, "cat-file", "-e", head+"^{commit}"); err != nil {
		return remoteState{known: true, ahead: true}
	}
	// We have it: ahead only if it is not already in our history.
	if _, err := r.git(ctx, "merge-base", "--is-ancestor", head, "HEAD"); err != nil {
		return remoteState{known: true, ahead: true}
	}
	// It is in our history, so we are level with it or ahead of it: the
	// second exactly when it is not where we are standing.
	ours, err := r.Head(ctx)
	if err != nil {
		return remoteState{known: true}
	}
	return remoteState{known: true, behind: ours != head}
}

func (r *Repo) remoteBranchMissing(ctx context.Context) bool {
	out, err := r.gitAuthed(ctx, "ls-remote", "--heads", "origin", r.branch)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == ""
}

// Head returns the commit the working tree is on, or "" if there are no
// commits yet. The UI reports it, so an operator can see at a glance
// whether what grain is running matches what is on the remote.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// HasSecrets reports whether this repository holds, or has ever held,
// the encrypted secrets file.
//
// "Has ever" is the whole point, and it is why this asks git rather than
// the filesystem. A sandbox that may clone a repository may read every
// object in it: a `git log -p` reaches a file deleted three commits ago
// exactly as easily as one still in the tree. So a state repository an
// earlier build wrote secrets.enc into stays a repository grain must not
// let a sandbox near, however long ago the file was removed, and the
// only remedy is a repository that never held one.
//
// A directory that is not a repository, or a repository with no commits,
// answers on the working tree alone -- there is no history to ask.
func (r *Repo) HasSecrets(ctx context.Context) (bool, error) {
	if _, err := os.Stat(filepath.Join(r.cfg.Dir, SecretsFile)); err == nil {
		return true, nil
	}
	if empty, _ := r.isEmpty(ctx); empty {
		return false, nil
	}
	// --all rather than the current branch: a repository whose secrets
	// file only ever existed on a branch somebody else pushed is one a
	// sandbox can still fetch it from.
	out, err := r.git(ctx, "log", "--all", "--max-count=1", "--format=%H", "--", SecretsFile)
	if err != nil {
		return false, fmt.Errorf("staterepo: looking for %s in %s's history: %w", SecretsFile, r.cfg.Dir, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// loadedHeadFile is where the commit this host last loaded or wrote is
// recorded: inside the git directory, so it is neither part of the
// working tree nor something a commit, a clone or a push can carry. That
// placement is the point -- the marker answers "has this repository moved
// under *this* host", which is a question about the host and not about
// the repository, and a clone onto a new machine must arrive without one.
const loadedHeadFile = "grain-loaded-head"

// loadedHead reads the marker, reporting (commit, identity, error).
func (r *Repo) loadedHead(ctx context.Context) (string, string, error) {
	path, err := r.loadedHeadPath(ctx)
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("staterepo: reading %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 {
		return lines[0], "", nil
	}
	return lines[0], lines[1], nil
}

func (r *Repo) setLoadedHead(ctx context.Context, commit string, identity string) error {
	path, err := r.loadedHeadPath(ctx)
	if err != nil {
		return err
	}
	content := commit + "\n"
	if identity != "" {
		content += identity + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("staterepo: writing %s: %w", path, err)
	}
	return nil
}

// recordLoadedHead records wherever the working tree is now.
func (r *Repo) recordLoadedHead(ctx context.Context, identity string) error {
	head, err := r.Head(ctx)
	if err != nil {
		return err
	}
	return r.setLoadedHead(ctx, head, identity)
}

// loadedHeadPath asks git where the git directory is rather than
// assuming <dir>/.git: that is a file, not a directory, in a worktree,
// and writing into it would corrupt the repository.
func (r *Repo) loadedHeadPath(ctx context.Context) (string, error) {
	return r.gitDirFile(ctx, loadedHeadFile)
}

func (r *Repo) gitDirFile(ctx context.Context, name string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(out), name), nil
}

// churnExportedFile records when the tier-churn tables were last written
// out. It sits beside loadedHead, inside the git directory, for the same
// reason: it is a fact about this host's own clock, not about the
// repository, so it must not be committed and must not travel to a
// remote. A clone that arrives without one exports churn on its first
// sync, which is the right answer -- a fresh working tree has no idea how
// stale its churn files are.
const churnExportedFile = "grain-churn-exported"

// churnDue reports whether the churn tier is due to be written out. A
// marker that is missing, empty or unreadable means yes: erring towards
// one extra export costs one commit, where erring the other way would
// silently stop exporting run history altogether.
func (r *Repo) churnDue(ctx context.Context, now time.Time) bool {
	path, err := r.gitDirFile(ctx, churnExportedFile)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	return !now.Before(last.Add(r.cfg.ChurnInterval))
}

func (r *Repo) recordChurnExport(ctx context.Context, at time.Time) error {
	path, err := r.gitDirFile(ctx, churnExportedFile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("staterepo: writing %s: %w", path, err)
	}
	return nil
}

// maintain lets git pack what has accumulated.
//
// Every commit here writes a whole new blob for each file it touched --
// git has no concept of appending to one -- so a repository that is
// committed to on a timer accumulates loose objects, each a complete
// zlib copy of a dump that is mostly identical to the one before it.
// Packing turns that pile into deltas, and it is dramatic rather than
// marginal: a measured day of a busy deployment is gigabytes loose and
// tens of megabytes packed (see growth_test.go).
//
// git's own `gc --auto` is what does it, at the threshold configure()
// sets, so the policy is git's and the only thing this adds is a place
// to call it from. Errors are deliberately dropped: housekeeping that
// could not run is a repository that is larger than it needs to be, and
// failing a sync -- and so a push of real state -- over that would be
// the worse outcome by far.
func (r *Repo) maintain(ctx context.Context) {
	_, _ = r.git(ctx, "gc", "--auto", "--quiet")
}

// git runs one git command in the working tree.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, nil, args...)
}

// gitAuthed runs a git command that talks to the remote, with whatever
// credential Config.Token yields.
//
// The token goes into a temporary askpass script rather than into the
// URL or onto the command line: anything in argv is readable by any
// other process on the host through `ps`, and a token baked into a
// remote URL is worse still -- it persists in .git/config, which is a
// file inside the very repository this package pushes to a remote.
func (r *Repo) gitAuthed(ctx context.Context, args ...string) (string, error) {
	if r.cfg.Token == nil || !strings.HasPrefix(r.cfg.Remote, "http") {
		return r.run(ctx, nil, args...)
	}
	token, err := r.cfg.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("staterepo: no credential for %s: %w", r.cfg.Remote, err)
	}
	if token == "" {
		return r.run(ctx, nil, args...)
	}
	dir, err := os.MkdirTemp("", "grain-staterepo-askpass-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "askpass.sh")
	// git asks twice: once for the username, once for the password. The
	// prompt it passes as $1 is what tells them apart.
	body := "#!/bin/sh\ncase \"$1\" in\n*[Uu]sername*) printf '%s' 'x-access-token' ;;\n*) printf '%s' " +
		shellQuote(token) + " ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		return "", err
	}
	env := []string{
		"GIT_ASKPASS=" + script,
		// Without this, a git that cannot use the askpass falls through to
		// a terminal prompt and hangs the daemon forever instead of failing.
		"GIT_TERMINAL_PROMPT=0",
	}
	return r.run(ctx, env, args...)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (r *Repo) run(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	// A git operation against a remote can hang on a network that
	// half-answers; every call here gets a ceiling so a sync cycle cannot
	// wedge the daemon's timer.
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	// clone names its own destination and must not run inside a directory
	// that is not a repository yet.
	if args[0] != "clone" && args[0] != "init" {
		cmd.Dir = r.cfg.Dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(redact(args), " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// redact keeps a remote URL's userinfo out of an error message, for the
// case where an operator pasted a URL with a token in it during the
// bootstrap. gitAuthed never builds one, but a human can.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if at := strings.Index(a, "@"); at > 0 && strings.Contains(a[:at], "://") {
			scheme := a[:strings.Index(a, "://")+3]
			out[i] = scheme + "***@" + a[at+1:]
			continue
		}
		out[i] = a
	}
	return out
}

const gitTimeout = 2 * time.Minute
