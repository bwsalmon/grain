"""Runs `claude -p` as a transient systemd unit on the **controller**, and
checks on it later. The dispatched agent still does all its actual work
(editing files, running commands, git) inside the assigned sandbox — but it
reaches the sandbox only through the narrow MCP tool surface in
`mcp_server.py`, over SSH, never by running there itself.

**Why the agent doesn't run on the sandbox anymore (docs/roadmap.md item
8's "Update").** It used to: `claude -p` ran on the sandbox with a real
Claude Code OAuth credential, and Claude Code's own sandbox/permission
system (`sandbox.enabled`, `--permission-mode`, `network.allowedDomains`)
tried to restrict what it could do. A full live-debugging session found
this fundamentally broken — the credential leaks into any unsandboxed Bash
subprocess's environment trivially (confirmed live via a plain `env`), the
agent readily discovers `dangerouslyDisableSandbox: true` on its own to get
there, and Claude Code's own sandboxing caused a full day of unrelated
breakage (denied Edit/Write under `dontAsk`, a `.git/config.lock`
write-block breaking `git commit` outright, "requires approval" prompts a
headless run can never answer). Landlock — kernel-level, immune to
`dangerouslyDisableSandbox` — can protect a *file*, but has no concept of
environment variables at all, so it cannot close the credential-leak gap
either. The only real fix: never put the credential inside the untrusted
execution environment in the first place. `claude -p` now runs on the
controller with its entire native tool roster disabled (`--tools ""`) and
replaced by `mcp_server.py`'s four tools, which are the *only* way the
agent can touch the sandbox. Confirmed live: `--tools "" --mcp-config
<file> --allowedTools <names>` genuinely empties the roster (the advertised
`tools` list in the `system/init` event shrinks to exactly what's named),
`--allowedTools` alone does not (it's a permission hint, not a roster
filter — both flags are required together), and a `Task`-spawned subagent
inherits the same restriction (an explicit system denial confirmed this,
not just self-report) so `Task` is safe to leave enabled for delegation.

systemd, not tmux: both the controller and every sandbox already have
systemd as PID 1 (Debian, not the container image
`openhands-agent-server` assumes — see `provision/controller.sh`/
`provision/sandbox.sh`), so tracking a dispatched task needs nothing beyond
what's already provisioned, and status survives the orchestrator's own
process restarting between cron invocations — the run keeps going, and
`unit_status` picks it back up next time.

`sudo`, not a bare `systemd-run`: found live — starting or stopping a
*system* (not `--user`) transient unit is a privileged D-Bus call, and an
unprivileged session has no polkit authentication agent to satisfy it, so a
bare call fails with "Interactive authentication required" even though
`--uid=` only asks to run the unit's own command as that user. Debian's
cloud images (and the controller's own provisioning) grant the invoking
user passwordless sudo, which is enough to make the *manager* call, while
`--uid=` still keeps the dispatched command itself unprivileged. Read-only
queries (`systemctl show`) need no such elevation.

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
only variable components are unit-derived paths, never issue content.

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
  ever need to carry it. It also sets a fixed git identity (`user.name`/
  `user.email`) — found live: a fresh sandbox has *no* identity configured
  anywhere (global, system, or local), which makes `git commit` fail
  outright the first time anything tries to commit, agent or otherwise.

docs/roadmap.md item 9 adds a second entry point, `dispatch_pr()`, for
pointing an agent at an *existing* PR rather than a labelled issue — to
address review feedback, fix CI, or continue work already in flight. The
sandbox side is genuinely unchanged: still only `git`, still credentials via
the same helper, still a prompt over stdin never argv. Two things differ:

- **`ensure_workspace()` gained an optional `branch`.** A fresh issue
  dispatch always lands on the remote's own default branch (`origin/HEAD`);
  a PR dispatch must land on the *PR's own existing branch* instead — the
  agent is continuing that branch's history, not starting a new one — so
  `branch` swaps the reset target from `origin/HEAD` to `origin/<branch>`
  and checks it out as a real local branch of the same name (`checkout -f -B
  <branch> origin/<branch>`), not detached, so `git push origin HEAD:<branch>`
  (the same push instruction the issue prompt already uses) pushes exactly
  where the agent's `HEAD` already sits.
- **The prompt is PR-shaped (`_pr_prompt`), not issue-shaped.** It has to
  tell the agent this is *not* a fresh task — the branch already carries
  commits and review history — and it carries the PR's review comments
  (`GitHubClient.list_review_comments`) as the feedback to address, in place
  of an issue's title/body.

docs/roadmap.md item 10 adds one more piece to the unit's own command line:
`--output-format stream-json --verbose`, redirected to `transcript_path(unit)`.
This is the session browser's raw material — see `grain/automation/capture.py`'s
docstring for what was actually checked (not assumed) about what `claude -p`
leaves behind on disk, and why this project captures a redirected stream
rather than depending on Claude Code's own session-persistence file location
(now doubly true: that default path lives under the *controller's* shared
`grain-agent` account, accumulating across every dispatch forever with no
per-sandbox VM recreation to bound it — `--no-session-persistence` turns it
off outright rather than letting it grow unbounded).
`transcript_path()` is computed the same "once, shared" way `branch_name()`
already is: `dispatch()` uses it to build the redirect, `capture.py` uses the
identical function to know what to read back before the sandbox's slot is
freed.
"""

from __future__ import annotations

import json
import shlex
from dataclasses import dataclass
from enum import Enum
from urllib.parse import urlsplit

from .github import Issue, PullRequestDetail, ReviewComment
from ..run import Runner

# Fixed across every dispatch to a sandbox — long-lived sandboxes reuse the
# same checkout rather than getting a fresh one per task; see
# ensure_workspace()'s docstring.
WORKSPACE_PATH = "/home/debian/workspace"
_CREDENTIALS_PATH = "/home/debian/.git-credentials"
_GIT_IDENTITY_NAME = "grain agent"
_GIT_IDENTITY_EMAIL = "grain-agent@localhost"

# The unprivileged, dedicated controller-side account `claude -p` (and the
# MCP server it spawns as a child) runs as — never root, never the account
# `grain-automation.service` itself runs as. See provision/controller.sh.
CONTROLLER_AGENT_USER = "grain-agent"

# Where a unit's controller-local files (prompt, MCP config, transcript)
# live — under /data, not /tmp: the controller is now a shared, multi-
# tenant machine running every concurrent dispatch's agent process, not a
# disposable per-task VM, so its state belongs in the same durable,
# already-backed-up location as everything else under /data/state.
_UNIT_STATE_DIR = "/data/state/automation/units"


class UnitState(Enum):
    ACTIVE = "active"
    DONE_SUCCESS = "done_success"
    DONE_FAILED = "done_failed"
    ABSENT = "absent"


@dataclass(frozen=True)
class SandboxTarget:
    """What `mcp_server.py` needs to reach the assigned sandbox on its own
    behalf — it builds its own independent `SshRunner` rather than reusing
    the caller's, since it runs as a separate process (a child of the
    controller-side `claude -p` unit), so this has to travel as data (baked
    into the per-dispatch MCP config JSON), not as an already-constructed
    `Runner` object. `core.py` builds one from the exact same
    address/user/key it already uses to build `sandbox_runner`.
    """

    address: str
    ssh_user: str
    ssh_key_path: str
    workspace: str = WORKSPACE_PATH


def unit_name(sandbox: str) -> str:
    # One task per sandbox at a time (the design's max_concurrent_runs=1
    # trade, carried over from the OpenHands path) — a fixed name per
    # sandbox rather than one per task, reused across dispatches.
    return f"grain-task-{sandbox}"


def _unit_dir(unit: str) -> str:
    return f"{_UNIT_STATE_DIR}/{unit}"


def transcript_path(unit: str) -> str:
    """The fixed path this dispatch's `claude -p --output-format
    stream-json` output is redirected to (docs/roadmap.md item 10) —
    computed once here so `_start_task`'s own redirect and
    `capture.py`'s later read of the same path can never disagree,
    the same "compute once, share" shape `branch_name()` already uses.
    Reused verbatim by the next dispatch to this sandbox (`unit_name()` is
    fixed per sandbox, not per task), which is exactly why `capture.py`'s
    docstring calls capture-before-slot-reuse load-bearing, not a nicety.
    """
    return f"{_unit_dir(unit)}/transcript.jsonl"


def _prompt_path(unit: str) -> str:
    return f"{_unit_dir(unit)}/prompt.md"


def _mcp_config_path(unit: str) -> str:
    return f"{_unit_dir(unit)}/mcp-config.json"


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
        f"{workspace} in your assigned sandbox, with its git remote already "
        "configured — do your work there, using the tools available to "
        "you.\n\n"
        "When you are done, commit your changes and push them with exactly "
        "this command:\n"
        f"    git push origin HEAD:{branch}\n"
        "The controller opens the pull request itself once it sees that "
        "branch — you have no GitHub API access from here, so do not "
        "attempt to open a PR or comment directly."
    )


def _format_review_comment(comment: ReviewComment) -> str:
    location = f"{comment.path}:{comment.line}" if comment.line is not None else comment.path
    return f"- {comment.user} on {location}:\n  {comment.body}"


def _pr_prompt(pr: PullRequestDetail, comments: list[ReviewComment], workspace: str) -> str:
    feedback = (
        "\n\n".join(_format_review_comment(c) for c in comments)
        if comments else "(no inline review comments)"
    )
    return (
        f"You are continuing existing work on GitHub pull request "
        f"#{pr.number}: {pr.title}\n\n"
        f"{pr.body}\n\n"
        f"PR URL: {pr.html_url}\n\n"
        f"A clone of the target repository is already checked out at "
        f"{workspace} in your assigned sandbox, on the PR's existing branch "
        f"({pr.head_ref!r}) with its git remote already configured. This is "
        "NOT a fresh task: the branch already has commits and review "
        "history behind it — your job is to continue that work (address "
        "the feedback below, fix CI, finish what was started), not to "
        "start over or open a competing branch.\n\n"
        "Review feedback on this pull request so far:\n\n"
        f"{feedback}\n\n"
        "When you are done, commit your changes and push them with exactly "
        "this command:\n"
        f"    git push origin HEAD:{pr.head_ref}\n"
        "The controller is already tracking this pull request — you have no "
        "GitHub API access from here, so do not attempt to comment on or "
        "modify the PR directly."
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
    so neither the clone URL nor any command run there ever needs to carry
    it — docs/design.md: "git consumes it via a credential helper, so
    agents never handle it." The token reaches the sandbox over the same
    stdin-not-argv channel the prompt uses, so it is never a literal argv
    element either (an argv element would land in `ps` output and this
    runner's own command logging).

    Also sets a fixed git identity (`user.name`/`user.email`), idempotently,
    every dispatch — found live: a fresh sandbox has none configured
    anywhere (global, system, or local), which makes `git commit` fail
    outright ("Author identity unknown") the first time anything tries to
    commit. Unrelated to the credential-helper concern above, but fixed
    here since it is exactly this same "make the sandbox ready to commit
    and push" step, and it would otherwise block every single dispatch.
    """
    runner.run(["git", "config", "--global", "credential.helper", "store"])
    runner.run(
        ["dd", f"of={_CREDENTIALS_PATH}", "status=none"],
        stdin=_credential_line(remote_url, token),
    )
    runner.run(["chmod", "600", _CREDENTIALS_PATH])
    runner.run(["git", "config", "--global", "user.name", _GIT_IDENTITY_NAME])
    runner.run(["git", "config", "--global", "user.email", _GIT_IDENTITY_EMAIL])


def ensure_workspace(runner: Runner, remote_url: str, path: str = WORKSPACE_PATH,
                      *, branch: str | None = None) -> None:
    """Makes sure `path` holds a checkout of `remote_url`, cloning on a
    sandbox's first dispatch and fetching-plus-resetting on every one after.
    With `branch` unset, resets to the remote's current default branch
    (`origin/HEAD`) — the fresh-issue-task shape. With `branch` given
    (docs/roadmap.md item 9's PR-continuation dispatch), resets to that
    branch's own tip (`origin/<branch>`) and checks it out as a real local
    branch of the same name instead — the agent must land on the PR's
    existing branch, with its existing history, not the default branch.

    Sandboxes are long-lived (docs/design.md, "sandbox lifecycle: long-lived,
    recreated on demand") — nothing resets `path` between tasks on its own,
    so a reused sandbox's workspace can hold whatever branch, commit, and
    untracked files the previous task left behind. `git clean -fdx` plus a
    forced checkout discards all of that unconditionally, regardless of what
    local state existed, rather than trying to reconcile it — the design's
    own tradeoff (correctness over isolation for *sequential* tasks on one
    sandbox) says a clean, known starting point matters more here than
    preserving anything left over. `origin/HEAD` (not a hardcoded branch name
    like `main`) is what makes the default-branch path agnostic to the
    target repo's actual default branch.
    """
    target = f"origin/{branch}" if branch else "origin/HEAD"
    if branch:
        checkout_line = (
            f"  git -C {shlex.quote(path)} checkout -f -B "
            f"{shlex.quote(branch)} {shlex.quote(target)}\n"
        )
    else:
        checkout_line = f"  git -C {shlex.quote(path)} checkout -f --detach {target}\n"
    # origin/HEAD only means anything for the default-branch path; a branch
    # dispatch doesn't need it, and `remote set-head` would just be a wasted
    # network round trip.
    head_sync_line = (
        "" if branch else f"  git -C {shlex.quote(path)} remote set-head origin -a\n"
    )
    lines = [
        "set -eu",
        f"if [ -d {shlex.quote(path)}/.git ]; then",
        f"  git -C {shlex.quote(path)} remote set-url origin {shlex.quote(remote_url)}",
        f"  git -C {shlex.quote(path)} fetch --prune origin",
    ]
    if head_sync_line:
        lines.append(head_sync_line.rstrip("\n"))
    lines.append(f"  git -C {shlex.quote(path)} clean -fdx")
    lines.append(checkout_line.rstrip("\n"))
    lines.append("else")
    lines.append(f"  git clone {shlex.quote(remote_url)} {shlex.quote(path)}")
    if branch:
        # A plain clone lands on the remote's default branch, not the PR's
        # own — the same checkout used on the reset path also has to run
        # here, or a sandbox's very first dispatch to a PR would silently
        # start on the wrong branch.
        lines.append(checkout_line.rstrip("\n"))
    lines.append("fi")
    script = "\n".join(lines) + "\n"
    runner.run(["bash", "-c", script])


def start_unit(runner: Runner, unit: str, command: str, *, uid: str = "debian") -> None:
    """Starts `command` (a shell string, run via `bash -c`) as a transient
    systemd unit named `unit`, run as `uid`. `dispatch()` specializes this
    for `claude -p` on the controller, as `CONTROLLER_AGENT_USER` — pulled
    out on its own because it's what a live test exercises against a real
    VM without needing a real Claude Code login: any inert stand-in command
    proves the same systemd-run/SSH mechanism `dispatch()` relies on,
    against whichever `uid` a given test cares about.
    """
    runner.run([
        "sudo", "systemd-run", f"--unit={unit}", f"--uid={uid}",
        "--property=RemainAfterExit=yes", "--",
        "bash", "-c", command,
    ])


def _mcp_config_json(target: SandboxTarget) -> str:
    """The `--mcp-config` file `claude -p` loads on the controller —
    `mcp_server.py` is invoked as its own process, per dispatch, with the
    assigned sandbox's connection details baked into its argv here. Nothing
    in a tool call itself can ever change this: the agent only ever sees
    tool names and their declared parameters (`command`, `file_path`, ...),
    never `target` — this JSON is the only place the sandbox gets named.
    """
    return json.dumps({
        "mcpServers": {
            "grain-sandbox": {
                "command": "python3",
                "args": [
                    "-m", "grain.automation.mcp_server",
                    "--address", target.address,
                    "--user", target.ssh_user,
                    "--key-path", target.ssh_key_path,
                    "--workspace", target.workspace,
                ],
            },
        },
    })


_ALLOWED_TOOLS = (
    "mcp__grain-sandbox__run_command,mcp__grain-sandbox__read_file,"
    "mcp__grain-sandbox__edit_file,mcp__grain-sandbox__write_file,"
    "TodoWrite,Task"
)


def _start_task(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
                 target: SandboxTarget, prompt: str, *, remote_url: str, token: str,
                 branch: str | None = None) -> str:
    """The common tail of both `dispatch()` and `dispatch_pr()`. Two
    runners now, not one (docs/roadmap.md item 8's "Update"): `sandbox_runner`
    still prepares the workspace and credentials on the sandbox itself,
    exactly as before; `controller_runner` is new and does everything that
    now happens on the controller instead — writing the prompt and MCP
    config, and starting the unit. `branch` unset gets the fresh-issue
    workspace (default branch); `branch` given gets `ensure_workspace`'s
    PR-continuation path (docs/roadmap.md item 9) — see its docstring.
    """
    configure_git_credentials(sandbox_runner, remote_url, token)
    ensure_workspace(sandbox_runner, remote_url, target.workspace, branch=branch)

    unit = unit_name(sandbox)
    unit_dir = _unit_dir(unit)
    # `sudo`, matching start_unit's/reap's own existing pattern below —
    # controller_runner isn't guaranteed to already be root (found live:
    # even in production, relying on that is fragile), and the directory
    # has to end up owned by CONTROLLER_AGENT_USER specifically, not
    # whoever controller_runner happens to be: `claude -p`'s own
    # `--output-format stream-json` redirect creates transcript_path(unit)
    # as that unprivileged user, and a root-owned 0755 directory (mkdir's
    # own default) would deny it write access to create that file at all
    # — found live, the unit's own systemd-run wrapper failing outright
    # with "Permission denied" on the redirect before claude -p ever ran.
    controller_runner.run(["sudo", "mkdir", "-p", unit_dir])
    controller_runner.run(["sudo", "chown", CONTROLLER_AGENT_USER, unit_dir])
    p_path = _prompt_path(unit)
    controller_runner.run(["sudo", "dd", f"of={p_path}", "status=none"], stdin=prompt)
    m_path = _mcp_config_path(unit)
    controller_runner.run(
        ["sudo", "dd", f"of={m_path}", "status=none"], stdin=_mcp_config_json(target),
    )
    out_path = transcript_path(unit)
    start_unit(
        controller_runner, unit,
        f"cd /opt/grain && claude -p "
        f"--tools '' "
        f"--mcp-config {shlex.quote(m_path)} --strict-mcp-config "
        f"--allowedTools {shlex.quote(_ALLOWED_TOOLS)} "
        f"--no-session-persistence "
        f"--output-format stream-json --verbose "
        f"< {shlex.quote(p_path)} > {shlex.quote(out_path)}",
        uid=CONTROLLER_AGENT_USER,
    )
    return unit


def dispatch(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
             target: SandboxTarget, issue: Issue, *, remote_url: str, token: str) -> str:
    """Starts an issue-triggered task. `sandbox_runner` prepares the
    workspace on the sandbox `target` describes; `controller_runner` starts
    `claude -p` on the controller, pointed at that same sandbox via
    `target`. Returns the unit name — the caller records it in
    `AutomationState` to poll later.

    `remote_url` is the git-proxy URL for the target repo
    (`http://<controller>:<port>/<owner>/<repo>.git`) and `token` is this
    sandbox's own git-proxy bearer token (`grain/proxy/tokens.py`'s
    `SandboxTokenStore.ensure_token` mints it on first use) — both supplied
    by `core.py`, which is the only layer that knows the controller's
    address and holds the token store.
    """
    branch = branch_name(issue.number)
    return _start_task(
        sandbox_runner, controller_runner, sandbox, target,
        _prompt(issue, branch, target.workspace),
        remote_url=remote_url, token=token,
    )


def dispatch_pr(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
                 target: SandboxTarget, pr: PullRequestDetail,
                 comments: list[ReviewComment], *, remote_url: str, token: str) -> str:
    """Starts a PR-triggered task (docs/roadmap.md item 9): same mechanism as
    `dispatch()`, but the workspace lands on the PR's *own* existing branch
    (`pr.head_ref`) instead of the default branch, and the prompt carries the
    PR's title/body/review-comments instead of an issue's title/body — see
    `_pr_prompt` and `ensure_workspace`'s `branch` parameter.

    `pr` and `comments` come from `GitHubClient.get_pull_request`/
    `list_pull_requests` and `list_review_comments` respectively — `core.py`
    reads both before calling this, same division of labour as `dispatch()`
    (all GitHub API work stays on the controller; the sandbox only ever
    sees git, and only through the tools `mcp_server.py` exposes).
    """
    return _start_task(
        sandbox_runner, controller_runner, sandbox, target,
        _pr_prompt(pr, comments, target.workspace),
        remote_url=remote_url, token=token, branch=pr.head_ref,
    )


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
