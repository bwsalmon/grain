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
"""

from __future__ import annotations

from enum import Enum

from .github import Issue
from ..run import Runner


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


def _prompt(issue: Issue) -> str:
    return (
        f"You are working GitHub issue #{issue.number}: {issue.title}\n\n"
        f"{issue.body}\n\n"
        f"Issue URL: {issue.html_url}\n\n"
        "Push your work as a branch through the git remote already "
        "configured in this workspace. You have no GitHub API access from "
        "here — do not attempt to open a PR or comment directly."
    )


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


def dispatch(runner: Runner, sandbox: str, issue: Issue) -> str:
    """Starts the task on the sandbox `runner` targets. Returns the unit
    name — the caller records it in `AutomationState` to poll later.
    """
    unit = unit_name(sandbox)
    prompt_path = f"/tmp/{unit}.md"
    runner.run(["dd", f"of={prompt_path}", "status=none"], stdin=_prompt(issue))
    start_unit(runner, unit, f"claude -p --permission-mode acceptEdits < {prompt_path}")
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
