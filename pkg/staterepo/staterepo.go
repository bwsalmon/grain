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
// as a merged pull request, and Pull brings it back down: Load imports
// the whole of it at startup, and Apply imports the settings tables of
// it into a daemon that is already running, which is as much of a
// wholesale replacement as is safe to do underneath live runs.
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
}

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
	r := &Repo{cfg: cfg, branch: branch}

	exists, err := isRepo(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("staterepo: preparing %s: %w", cfg.Dir, err)
		}
		if cfg.Remote != "" {
			if err := r.clone(ctx); err != nil {
				return nil, err
			}
		} else if err := r.init(ctx); err != nil {
			return nil, err
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

func (r *Repo) currentBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
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

// Pull fast-forwards the working tree to the remote's branch, reporting
// whether anything arrived.
//
// Fast-forward only, on purpose. The daemon is the only writer of these
// files, so a remote that is ahead of us means one thing -- a pull
// request was merged -- and there is nothing to merge. Divergence means
// something happened that this package's model does not describe, and
// failing loudly with the two commits named beats resolving a conflict
// in a database dump by guesswork.
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
		return false, fmt.Errorf("staterepo: fetching %s: %w", r.branch, err)
	}
	before, _ := r.git(ctx, "rev-parse", "HEAD")
	if empty, _ := r.isEmpty(ctx); empty {
		if _, err := r.git(ctx, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := r.git(ctx, "merge", "--ff-only", "--quiet", "FETCH_HEAD"); err != nil {
		return false, fmt.Errorf("staterepo: %s and origin/%s have diverged; "+
			"grain will not merge a database dump by guesswork: %w", r.branch, r.branch, err)
	}
	after, _ := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(before) != strings.TrimSpace(after), nil
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

// loadedHeadFile is where the commit this host last loaded or wrote is
// recorded: inside the git directory, so it is neither part of the
// working tree nor something a commit, a clone or a push can carry. That
// placement is the point -- the marker answers "has this repository moved
// under *this* host", which is a question about the host and not about
// the repository, and a clone onto a new machine must arrive without one.
const loadedHeadFile = "grain-loaded-head"

// loadedHead reads the marker, reporting "" when there is none.
func (r *Repo) loadedHead(ctx context.Context) (string, error) {
	path, err := r.loadedHeadPath(ctx)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("staterepo: reading %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (r *Repo) setLoadedHead(ctx context.Context, commit string) error {
	path, err := r.loadedHeadPath(ctx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(commit+"\n"), 0o600); err != nil {
		return fmt.Errorf("staterepo: writing %s: %w", path, err)
	}
	return nil
}

// recordLoadedHead records wherever the working tree is now.
func (r *Repo) recordLoadedHead(ctx context.Context) error {
	head, err := r.Head(ctx)
	if err != nil {
		return err
	}
	return r.setLoadedHead(ctx, head)
}

// loadedHeadPath asks git where the git directory is rather than
// assuming <dir>/.git: that is a file, not a directory, in a worktree,
// and writing into it would corrupt the repository.
func (r *Repo) loadedHeadPath(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(out), loadedHeadFile), nil
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
