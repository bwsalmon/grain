"""Runs `claude -p` on a sandbox as a transient systemd unit, and checks on
it later.

systemd, not tmux: every sandbox already has systemd as PID 1 (Debian, not
the container image `openhands-agent-server` assumes — see
`provision/sandbox.sh`), so tracking a dispatched task needs nothing beyond
what's already provisioned, and status survives the orchestrator's own SSH
connection dropping between cron invocations — the run keeps going, and
`unit_status` picks it back up next time.

`sudo`, not a bare `systemd-run`: found live — starting or stopping a
*system* (not `--user`) transient unit is a privileged D-Bus call, and an
unprivileged SSH session has no polkit authentication agent to satisfy it,
so a bare call fails with "Interactive authentication required" even
though `--uid=debian` only asks to run the unit's own command as `debian`.
Debian's cloud images grant the default user passwordless sudo, which is
enough to make the *manager* call, while `--uid=debian` still keeps the
dispatched command itself unprivileged. Read-only queries (`systemctl
show`) need no such elevation.

`--property=RemainAfterExit=yes`, found live the hard way: a plain
transient unit's *default* behaviour — with no `--collect` in sight — is to
auto-unload itself once its command exits zero. `LoadState` goes straight
to `not-found` within a couple of seconds of success, indistinguishable
from a unit that never started; a periodic sweep would read every
successful run as stranded. A *failed* unit does not get this treatment —
it stays `loaded`/`failed` until `reset-failed` — which is what earlier
reasoning (wrongly) generalized to the success case too. `RemainAfterExit`
keeps a finished unit `active`/`exited` instead of letting it vanish, so
`unit_status` can tell "succeeded" from "never dispatched" at all; `reap()`
is still what actually clears it for the next dispatch.

The issue's title/body reaches `claude -p` over stdin, not argv: `dd
of=<path>` written from `Runner.run`'s own `stdin` parameter, then the unit
redirects that file into `claude -p`'s stdin (which reads the prompt from
stdin when no positional argument is given). Untrusted issue content never
becomes a shell-interpolated argument anywhere in this path — the only
thing built as a shell string is the fixed `bash -c` wrapper below, and its
only variable component is a unit-derived path, never issue content.

Two more pieces landed here for docs/roadmap.md item 2, both upstream of
`start_unit`:

- **The branch name is decided here, not reported by the agent.** `core.py`
  must verify a branch exists before opening a PR against it, and the only
  thing worth trusting for that check is a name the controller itself
  picked — never the agent's own claim about what it pushed, since the
  prompt it received came from untrusted issue content. `branch_name()` is
  a pure function of the issue number so both `dispatch()` (to put in the
  prompt) and `core.py` (to verify) compute the identical name with no
  round-trip through agent output.
- **`dispatch()` ensures the workspace itself.** Sandboxes are long-lived
  (docs/design.md, "sandbox lifecycle") — provisioning does not leave a
  freshly cloned repo waiting, and a reused sandbox already has whatever the
  previous task left in it. `ensure_workspace()` clones on a sandbox's first
  task and fetches-plus-resets on every one after, always through the git
  proxy (docs/design.md, "GitHub access"), never GitHub directly — the same
  caution `docs/design.md`'s dispatch-mechanism section already flagged
  ("the old `openhands-resolver` embedded the GitHub token directly in the
  clone URL... hasn't been checked against a sandbox with a real git remote
  configured"). `configure_git_credentials()` is what keeps the token out of
  that URL: a `git-credential-store` line delivered over the same
  stdin-not-argv channel the prompt uses, consumed by git's own credential
  helper machinery, so neither the clone URL nor the agent's own commands
  ever need to carry it.
"""

from __future__ import annotations

import shlex
from enum import Enum
from urllib.parse import urlsplit

from .github import Issue
from ..run import Runner

# Fixed across every dispatch to a sandbox — long-lived sandboxes reuse the
# same checkout rather than getting a fresh one per task; see
# ensure_workspace()'s docstring.
WORKSPACE_PATH = "/home/debian/workspace"
_CREDENTIALS_PATH = "/home/debian/.git-credentials"


class UnitState(Enum):
    ACTIVE = "active"
    DONE_SUCCESS = "done_success"
    DONE_FAILED = "done_failed"
    ABSENT = "absent"


def unit_name(sandbox: str) -> str:
    # One task per sandbox at a time (the design's max_concurrent_runs=1
    # trade, carried over from the OpenHands path) — a fixed name per
    # sandbox rather than one per task, reused across dispatches.
    return f"grain-task-{sandbox}"


def branch_name(issue: int) -> str:
    """The exact branch a dispatch for this issue must push to.

    Deterministic and derived from the issue number alone — never the
    agent's own report of what it did (docs/roadmap.md item 2). Both
    `dispatch()` (to put in the prompt) and `core.py` (to verify the branch
    exists before opening a PR) call this, so they can never disagree.
    """
    return f"grain/issue-{issue}"


def _prompt(issue: Issue, branch: str, workspace: str) -> str:
    return (
        f"You are working GitHub issue #{issue.number}: {issue.title}\n\n"
        f"{issue.body}\n\n"
        f"Issue URL: {issue.html_url}\n\n"
        f"A clone of the target repository is already checked out at "
        f"{workspace}, with its git remote already configured — do your "
        "work there.\n\n"
        "When you are done, commit your changes and push them with exactly "
        "this command:\n"
        f"    git push origin HEAD:{branch}\n"
        "The controller opens the pull request itself once it sees that "
        "branch — you have no GitHub API access from here, so do not "
        "attempt to open a PR or comment directly."
    )


def _credential_line(remote_url: str, token: str) -> str:
    """One `git-credential-store` line covering the proxy's origin.

    The store format matches on protocol+host+port, not path, so one line
    covers every repo this sandbox might be pointed at through the same
    proxy. The username is a fixed placeholder — `grain/proxy/tokens.py`'s
    `extract_basic_auth_token` ignores it; only the token (the password
    half) identifies the sandbox.
    """
    split = urlsplit(remote_url)
    netloc = f"{split.hostname}:{split.port}" if split.port else split.hostname
    return f"{split.scheme}://sandbox:{token}@{netloc}\n"


def configure_git_credentials(runner: Runner, remote_url: str, token: str) -> None:
    """Points the sandbox's git at its proxy token via a credential helper,
    so neither the clone URL nor any command the agent runs ever needs to
    carry it — docs/design.md: "git consumes it via a credential helper, so
    agents never handle it." The token reaches the sandbox over the same
    stdin-not-argv channel `dispatch()` already uses for the prompt, so it
    is never a literal argv element either (an argv element would land in
    `ps` output and this runner's own command logging).
    """
    runner.run(["git", "config", "--global", "credential.helper", "store"])
    runner.run(
        ["dd", f"of={_CREDENTIALS_PATH}", "status=none"],
        stdin=_credential_line(remote_url, token),
    )
    runner.run(["chmod", "600", _CREDENTIALS_PATH])


def ensure_workspace(runner: Runner, remote_url: str,
                      path: str = WORKSPACE_PATH) -> None:
    """Makes sure `path` holds a checkout of `remote_url`'s current default
    branch, cloning on a sandbox's first dispatch and fetching-plus-resetting
    on every one after.

    Sandboxes are long-lived (docs/design.md, "sandbox lifecycle: long-lived,
    recreated on demand") — nothing resets `path` between tasks on its own,
    so a reused sandbox's workspace can hold whatever branch, commit, and
    untracked files the previous task left behind. `git clean -fdx` plus a
    forced detached checkout of `origin/HEAD` discards all of that
    unconditionally, regardless of what local state existed, rather than
    trying to reconcile it — the design's own tradeoff (correctness over
    isolation for *sequential* tasks on one sandbox) says a clean, known
    starting point matters more here than preserving anything left over.
    `origin/HEAD` (not a hardcoded branch name like `main`) is what makes
    this agnostic to the target repo's actual default branch.
    """
    script = (
        "set -eu\n"
        f"if [ -d {shlex.quote(path)}/.git ]; then\n"
        f"  git -C {shlex.quote(path)} remote set-url origin {shlex.quote(remote_url)}\n"
        f"  git -C {shlex.quote(path)} fetch --prune origin\n"
        f"  git -C {shlex.quote(path)} remote set-head origin -a\n"
        f"  git -C {shlex.quote(path)} clean -fdx\n"
        f"  git -C {shlex.quote(path)} checkout -f --detach origin/HEAD\n"
        "else\n"
        f"  git clone {shlex.quote(remote_url)} {shlex.quote(path)}\n"
        "fi\n"
    )
    runner.run(["bash", "-c", script])


def start_unit(runner: Runner, unit: str, command: str) -> None:
    """Starts `command` (a shell string, run via `bash -c`) as a transient
    systemd unit named `unit`. The primitive `dispatch()` specializes for
    `claude -p` — pulled out on its own because it's what a live test
    exercises against a real sandbox without needing a real Claude Code
    login: any inert stand-in command proves the same systemd-run/SSH
    mechanism `dispatch()` relies on.
    """
    runner.run([
        "sudo", "systemd-run", f"--unit={unit}", "--uid=debian",
        "--property=RemainAfterExit=yes", "--",
        "bash", "-c", command,
    ])


def dispatch(runner: Runner, sandbox: str, issue: Issue, *,
             remote_url: str, token: str) -> str:
    """Starts the task on the sandbox `runner` targets. Returns the unit
    name — the caller records it in `AutomationState` to poll later.

    `remote_url` is the git-proxy URL for the target repo
    (`http://<controller>:<port>/<owner>/<repo>.git`) and `token` is this
    sandbox's own git-proxy bearer token (`grain/proxy/tokens.py`'s
    `SandboxTokenStore.ensure_token` mints it on first use) — both supplied
    by `core.py`, which is the only layer that knows the controller's
    address and holds the token store.
    """
    configure_git_credentials(runner, remote_url, token)
    ensure_workspace(runner, remote_url)
    branch = branch_name(issue.number)
    unit = unit_name(sandbox)
    prompt_path = f"/tmp/{unit}.md"
    runner.run(
        ["dd", f"of={prompt_path}", "status=none"],
        stdin=_prompt(issue, branch, WORKSPACE_PATH),
    )
    start_unit(
        runner, unit,
        f"cd {WORKSPACE_PATH} && claude -p --permission-mode acceptEdits < {prompt_path}",
    )
    return unit


def _parse_show(stdout: str) -> dict[str, str]:
    return dict(line.split("=", 1) for line in stdout.splitlines() if "=" in line)


def unit_status(runner: Runner, unit: str) -> UnitState:
    result = runner.run(
        ["systemctl", "show", unit, "-p", "LoadState,ActiveState,SubState,Result"],
        check=False,
    )
    if result.returncode != 0:
        return UnitState.ABSENT
    fields = _parse_show(result.stdout)
    if fields.get("LoadState") != "loaded":
        return UnitState.ABSENT
    active_state = fields.get("ActiveState", "")
    sub_state = fields.get("SubState", "")
    if active_state == "failed":
        return UnitState.DONE_FAILED
    if active_state == "active" and sub_state == "exited":
        # RemainAfterExit's steady state for a finished command: ActiveState
        # stays "active" rather than moving to "inactive", so SubState is
        # what actually distinguishes "still running" from "done".
        return (UnitState.DONE_SUCCESS if fields.get("Result", "success") == "success"
                else UnitState.DONE_FAILED)
    if active_state in ("active", "activating", "reloading", "deactivating"):
        return UnitState.ACTIVE
    # Reachable only if something started this unit without
    # RemainAfterExit=yes — defensive, not the path start_unit takes.
    if fields.get("Result", "success") != "success":
        return UnitState.DONE_FAILED
    return UnitState.DONE_SUCCESS


def reap(runner: Runner, unit: str) -> None:
    """Clears a finished unit so its name is safe to reuse for the next
    dispatch to this sandbox.
    """
    runner.run(["sudo", "systemctl", "stop", unit], check=False)
    runner.run(["sudo", "systemctl", "reset-failed", unit], check=False)
