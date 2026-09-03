package orchestrator

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

// CheckoutDir is where prepareCheckout leaves a dispatched task's clone,
// relative to the sandbox's own working directory -- the same "work" the
// scripted agents in e2e have always cloned into by hand, kept so a
// transcript from before this existed still reads the same way.
const CheckoutDir = "work"

// A sandbox starts empty. HostSandboxes hands out a bare directory
// (orchestrator.go's own doc comment) and KonturSandboxes a fresh VM;
// neither has the task's repo in it, and until prepareCheckout the agent
// was left to work that out for itself from a prompt that never said so
// -- BuildPrompt names the repo and the branch to push, and mentions
// cloning only for the read-only repos in task.Reads. A live run showed
// what that costs: the agent's first tool call was a git command in the
// empty directory, which failed with "not a git repository", and it gave
// up there rather than cloning. The work only happened on the redispatch
// after it, which had a sandbox with the previous attempt's leftovers in
// it to go on.
//
// It also had no way to know where to clone *from*. The remote is not
// github.com but this deployment's own git proxy (cmd/grain/daemon.go's
// startGitProxy), which is what holds the credential the sandbox
// authenticates with and what refuses a push to anything but the task's
// target (gitproxy/authorize.go) -- an address that reaches the agent
// nowhere else, since ConfigureGitCredentials writes only the proxy's
// host into .git-credentials, never a URL to clone.
//
// So the clone happens here, before the agent's first turn, against the
// proxy URL the daemon already has in hand.

// gitSafe is what an owner, repo name, branch or base has to match to be
// interpolated into the shell script below. Everything reaching it is
// grain's own (model.BranchName) or came through directives.go's own
// `/repo`/`/base` parse, which accepts any non-space token -- so a task
// body could otherwise put a quote or a backtick into a command this
// package composes. The sandbox is where the agent runs arbitrary shell
// anyway, so this is not a privilege boundary; it is the difference
// between a bad ref failing with git's own error and a bad ref producing
// a command that does something else entirely.
var gitSafe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// CloneURL is the address a sandbox clones repo from: not github.com, but
// base -- this deployment's git proxy, which authenticates the sandbox by
// its own token and authorizes what it may fetch and push.
func CloneURL(base string, repo model.RepoRef) string {
	return strings.TrimSuffix(base, "/") + "/" + repo.Owner + "/" + repo.Name + ".git"
}

// prepareCheckout clones task.Target into CheckoutDir inside the slot's
// sandbox and leaves task's own branch checked out, returning the
// directory it prepared ("" when there is nothing to prepare, which is
// what a deployment or test with no proxy URL configured gets).
//
// It runs through the sandbox's own run_command tool rather than
// exec.Command or SSH, which is what makes it work for either backend:
// the tool a HostSandboxes slot exposes runs `bash -c` in that slot's
// directory, and the one a KonturSandboxes slot exposes runs it inside
// that slot's VM (mcp/ssh_tools.go), so this is the one call that is
// already the right call in both places.
//
// An existing remote branch for this task is checked out and continued
// rather than branched over: a redispatch is usually the second attempt
// at the same task, and its push is a fast-forward of what the first
// attempt pushed instead of a rejected non-fast-forward.
func prepareCheckout(ctx context.Context, tools []mcp.Tool, remoteBase string, task model.Task) (string, error) {
	if remoteBase == "" || task.Target == nil {
		return "", nil
	}
	branch := model.BranchName(task.ID)
	for _, v := range []string{task.Target.Owner, task.Target.Name, branch} {
		if !gitSafe.MatchString(v) {
			return "", fmt.Errorf("orchestrator: %q is not a usable git name", v)
		}
	}
	if task.Base != "" && !gitSafe.MatchString(task.Base) {
		return "", fmt.Errorf("orchestrator: %q is not a usable base branch", task.Base)
	}

	// The base is checked out only in the branch-does-not-exist-yet case:
	// with the branch already on the remote, its own history is what the
	// next attempt continues, and re-rooting it on the base is exactly
	// what would throw the first attempt's commits away.
	//
	// Its existence, though, is checked either way -- see baseCheck.
	rootOnBase := ""
	if task.Base != "" {
		rootOnBase = fmt.Sprintf("git checkout --quiet '%s'\n  ", task.Base)
	}
	script := fmt.Sprintf(`set -e
rm -rf '%[1]s'
git clone --quiet '%[2]s' '%[1]s'
cd '%[1]s'
%[4]sif git rev-parse --verify --quiet 'refs/remotes/origin/%[3]s' >/dev/null; then
  git checkout --quiet -b '%[3]s' 'origin/%[3]s'
else
  %[5]sgit checkout --quiet -b '%[3]s'
fi`, CheckoutDir, CloneURL(remoteBase, *task.Target), branch, baseCheck(task, branch), rootOnBase)

	run, ok := runCommandTool(tools)
	if !ok {
		return "", fmt.Errorf("orchestrator: slot's sandbox exposes no run_command tool to clone %s with", task.Target)
	}
	result := run(ctx, map[string]any{"command": script})
	if result.IsError {
		return "", fmt.Errorf("orchestrator: cloning %s into %s: %s", task.Target, CheckoutDir, strings.TrimSpace(result.Text))
	}
	// The survivable half of baseCheck: the run is going ahead on a branch
	// that already exists, and the only record of what this found is
	// whatever it printed, which nothing else reads. Said in the daemon's
	// own journal so that the first place the missing base turns up is the
	// start of the attempt rather than the pull request refused at the end
	// of it. Only grain's own lines, never git's chatter: the marker is a
	// prefix this package composed a few lines up.
	for _, line := range strings.Split(result.Text, "\n") {
		if said, ok := strings.CutPrefix(strings.TrimSpace(line), baseGoneMarker); ok {
			log.Printf("orchestrator: task %s: %s", task.ID, strings.TrimSpace(said))
		}
	}
	return CheckoutDir, nil
}

// commitMarker prefixes each commit line branchCommits has git print, so
// those lines can be picked back out of a run_command result that also
// carries the tool's own `exit=`/`stdout:`/`stderr:` framing and whatever
// git said on the way -- the same trick baseGoneMarker plays for
// prepareCheckout's own diagnosis, and the reason neither has to parse
// that framing.
const commitMarker = "grain-commit:"

// branchCommits lists the commits already on task's own branch and not on
// its base -- `git log <base>..HEAD` in the checkout prepareCheckout has
// just made, newest first, one "<abbrev> <subject>" per entry.
//
// This is the other half of what a redispatch needs to make sense of the
// branch it wakes up on (History, previousAttemptsSection): the store
// says how each earlier attempt ended, and this says what those attempts
// actually left behind. It is read here because here is the only place
// that has the checkout at all -- by the time BuildPrompt runs there is a
// string, not a repository.
//
// Best effort, and never fatal: a missing base, a branch with nothing on
// it, a git that refuses for a reason nobody predicted, and a sandbox
// whose run_command is gone all come back as no commits. Orientation
// that could fail a dispatch would be a worse trade than the
// re-diagnosis it exists to save -- the commits are on the branch either
// way, and the agent has `git log` of its own.
//
// The base is resolved to whichever of origin/<task.Base> and origin/HEAD
// exists, in that order: a task with no base branched off whatever the
// clone left at origin/HEAD (prepareCheckout), and a task whose base has
// since been merged and deleted still has a branch worth describing --
// the same survivable case baseCheck carries on through.
func branchCommits(ctx context.Context, tools []mcp.Tool, task model.Task, limit int) []string {
	branch := model.BranchName(task.ID)
	if !gitSafe.MatchString(branch) {
		return nil
	}
	refs := []string{"origin/HEAD"}
	if task.Base != "" && gitSafe.MatchString(task.Base) {
		refs = []string{"origin/" + task.Base, "origin/HEAD"}
	}
	run, ok := runCommandTool(tools)
	if !ok {
		return nil
	}
	script := fmt.Sprintf(`cd '%s' || exit 0
for ref in %s; do
  if git rev-parse --verify --quiet "$ref^{commit}" >/dev/null 2>&1; then
    git log --format='%s %%h %%s' -n %d "$ref..HEAD" 2>/dev/null
    exit 0
  fi
done`, CheckoutDir, "'"+strings.Join(refs, "' '")+"'", commitMarker, limit)

	result := run(ctx, map[string]any{"command": script})
	if result.IsError {
		log.Printf("orchestrator: task %s: reading what earlier attempts left on %s: %s",
			task.ID, branch, strings.TrimSpace(result.Text))
		return nil
	}
	var commits []string
	for _, line := range strings.Split(result.Text, "\n") {
		if said, ok := strings.CutPrefix(strings.TrimSpace(line), commitMarker); ok {
			if said = strings.TrimSpace(said); said != "" {
				commits = append(commits, said)
			}
		}
	}
	return commits
}

// baseGoneMarker prefixes what baseCheck prints when a task's base is not
// on the remote, so prepareCheckout can pick its own words back out of a
// command's output and log them (there is nowhere else for the surviving
// case's diagnosis to go) without also logging git's.
const baseGoneMarker = "grain:"

// baseCheck is the shell that runs before either checkout arm and reports
// a base branch that is not on the remote.
//
// It is checked on both arms, not only the one that uses it. A base that
// is simply gone is the ordinary end of a branch's life -- it merged, and
// GitHub deleted it -- and a task can easily outlive one: New task
// prefills Base from the repo's last task (bwsalmon/agents#641), so a
// branch that merges between one task being filed and the next being
// dispatched leaves a task pointed at nothing. Checking only the arm that
// checks the base out meant that from the second attempt onward -- once
// the run's own branch was on the remote -- nothing looked at the base
// again until GitHub refused the pull request at the very end, with a 422
// nobody saw.
//
// What it does about it differs by arm, and that is the point:
//
//   - No branch on the remote yet: fatal. The base is what a fresh branch
//     would be rooted on, no work exists to lose, and a human retargeting
//     the task is cheap here and never gets cheaper. git's own answer
//     ("error: pathspec 'x' did not match any file(s) known to git")
//     names neither the base nor the repo it was looked for in and reads
//     like a corrupt checkout, so the message says which branch and
//     where.
//   - The branch is already there: not fatal. The base is not used on
//     this arm at all -- the branch's own history is what is continued --
//     and failing here would strand commits that are already pushed:
//     RunDispatch never reaches the agent, so runOne never salvages the
//     branch, and the task that was one refused pull request away from
//     finishing gets nothing at all instead. It says what it found and
//     carries on; EnsurePullRequest retargets at the default branch when
//     the run ends, and says so on the task.
func baseCheck(task model.Task, branch string) string {
	if task.Base == "" {
		return ""
	}
	return fmt.Sprintf(`if ! git rev-parse --verify --quiet 'refs/remotes/origin/%[1]s' >/dev/null; then
  echo "%[4]s base branch '%[1]s' does not exist on %[2]s -- it may have been merged and deleted; retarget this task at a branch that exists" >&2
  if ! git rev-parse --verify --quiet 'refs/remotes/origin/%[3]s' >/dev/null; then
    exit 1
  fi
  echo "%[4]s continuing '%[3]s', which is already on the remote; this task's pull request will open against the repository's default branch instead" >&2
fi
`, task.Base, task.Target, branch, baseGoneMarker)
}

// runCommandTool picks the run_command handler out of the tools a slot's
// sandbox exposes, so prepareCheckout can call it the same way the agent
// itself would -- by name, through the same handler, with no second path
// into the sandbox to keep in step with the first.
func runCommandTool(tools []mcp.Tool) (func(context.Context, map[string]any) mcp.Result, bool) {
	for _, t := range tools {
		if t.Name == "run_command" && t.Handler != nil {
			return t.Handler, true
		}
	}
	return nil, false
}
