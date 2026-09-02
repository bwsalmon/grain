package orchestrator

import (
	"context"
	"fmt"
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
	// Its existence is checked first, and named when it fails. A base
	// that is simply gone is the ordinary end of a branch's life -- it
	// merged, and GitHub deleted it -- and a task can easily outlive one:
	// New task prefills Base from the repo's last task
	// (bwsalmon/agents#641), so a branch that merges between one task
	// being filed and the next being dispatched leaves a task pointed at
	// nothing. What git says about that on its own is "error: pathspec
	// 'x' did not match any file(s) known to git", which names neither
	// the base nor the repo it was looked for in, and reads like a
	// corrupt checkout rather than a branch that no longer exists.
	base := ""
	if task.Base != "" {
		base = fmt.Sprintf(`if ! git rev-parse --verify --quiet 'refs/remotes/origin/%[1]s' >/dev/null; then
    echo "base branch '%[1]s' does not exist on %[2]s -- it may have been merged and deleted; retarget this task at a branch that exists" >&2
    exit 1
  fi
  git checkout --quiet '%[1]s'
  `, task.Base, task.Target)
	}
	script := fmt.Sprintf(`set -e
rm -rf '%[1]s'
git clone --quiet '%[2]s' '%[1]s'
cd '%[1]s'
if git rev-parse --verify --quiet 'refs/remotes/origin/%[3]s' >/dev/null; then
  git checkout --quiet -b '%[3]s' 'origin/%[3]s'
else
  %[4]sgit checkout --quiet -b '%[3]s'
fi`, CheckoutDir, CloneURL(remoteBase, *task.Target), branch, base)

	run, ok := runCommandTool(tools)
	if !ok {
		return "", fmt.Errorf("orchestrator: slot's sandbox exposes no run_command tool to clone %s with", task.Target)
	}
	if result := run(ctx, map[string]any{"command": script}); result.IsError {
		return "", fmt.Errorf("orchestrator: cloning %s into %s: %s", task.Target, CheckoutDir, strings.TrimSpace(result.Text))
	}
	return CheckoutDir, nil
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
