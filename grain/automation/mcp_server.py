"""A small MCP server, run on the controller as the `claude -p` session's
*only* available tool surface, that lets the dispatched agent do real work
inside its assigned sandbox without the process holding the Claude Code
credential ever running there itself.

Why this exists: see docs/roadmap.md item 8's "Update" -- a full live
session found that running `claude -p` inside the sandbox leaks its own
OAuth credential into any unsandboxed Bash subprocess's environment
trivially (confirmed live via a plain `env`), and that the agent readily
discovers `dangerouslyDisableSandbox: true` on its own to get there.
Claude Code's own sandbox/permission settings cannot close that gap --
`--dangerously-skip-permissions` was tried and made things worse, and
Landlock (kernel-level, immune to `dangerouslyDisableSandbox`) can protect a
*file* but has no concept of environment variables at all. The only real
fix is to never put the credential inside the untrusted execution
environment in the first place: `claude -p` now runs on the controller,
with every native tool disabled (`--tools ""`) and replaced by the four
tools this module implements, which reach the sandbox over SSH instead.

`file_path` on every tool always resolves against the *assigned* sandbox's
workspace -- never the controller, and never model-controllable. The target
sandbox (address/user/key/workspace) is fixed at process startup via CLI
args `dispatch.py` bakes into the per-dispatch `--mcp-config` JSON; nothing
in a tool call's own arguments can redirect it elsewhere.

Newline-delimited JSON-RPC over stdin/stdout, no `Content-Length` framing --
that is the real MCP stdio transport, not a simplification. Hand-rolled
rather than pulled from the `mcp` package: this project has no third-party
runtime dependencies (see pyproject.toml), and the protocol surface this
server actually needs -- `initialize`, `notifications/initialized`,
`tools/list`, `tools/call` -- is small enough that adding a dependency
would cost more than it saves.

Tool schemas deliberately mirror Claude Code's own native `Bash`/`Read`/
`Edit`/`Write` tools field-for-field (mirror-image logic in `read_file`'s
line-numbered output too, not just its parameters) rather than an
OpenAI-Codex-style `apply_patch`/V4A diff -- the agent here is Claude, which
was never trained to produce V4A, but has extensive trained behavior around
its own native tool shapes.

A fifth tool, `ask_question` (docs/roadmap.md item 12), is not like the
other four: it never touches the sandbox at all, so it takes no `runner`.
The agent's only way to reach a human is through GitHub issue/PR comments,
and only `core.py` -- on the controller, holding the real GitHub credential
-- can post one (docs/design.md's split surface: the agent never gets API
access of its own, here or anywhere else). So this tool just records the
question to a plain local file at a fixed path `dispatch.py` computes once
per unit (the same "compute once, share" shape `transcript_path`/
`branch_name` already use) and tells the agent to stop; `core.py`'s sweep
reads that file back before deciding how a finished run resolved, and is
the one that actually posts the comment.

A sixth tool, `comment_on_issue` (bwsalmon/agents#50, reworked by
bwsalmon/agents#89), is the same shape as `ask_question` for a related
reason: some tasks only ever needed an answer, an investigation, or a
recommendation, never a code change, and forcing one through the branch/PR
path just to say so is a poor fit -- see `docs/roadmap.md` item 2's
"verify, don't trust" for why a pushed branch is otherwise the *only*
signal a fresh-issue task is done. Like `ask_question`, it records its
argument to a fixed per-unit local file rather than touching the sandbox or
GitHub itself.

It used to be called `complete_analysis` and worked by *skipping* the
branch check outright whenever the agent called it -- which meant an agent
that got confused about which tool to call at the end of a task (a real,
common failure mode, not a hypothetical) could push real commits and then
still make `core.py` treat the run as a branchless analysis, silently
discarding the PR that should have opened for them. `core.py`'s sweep now
always checks whether the branch dispatch() told the agent to push to
actually has anything on it, `comment_on_issue` or not; the file this tool
writes only ever supplies the *comment* to post, and only ever suppresses a
PR on the one occasion a pushed branch can't: when the agent never pushed
anything at all.

A seventh tool, `read_grain_logs` (bwsalmon/agents#62), is unlike every
tool above in one specific way: it reads from the *controller*, where this
process already runs, rather than reaching the sandbox over SSH -- there is
no `Runner`-over-SSH hop to make, since `journalctl` for grain's own
services already runs locally. It exists so a task can debug a bug in
*grain itself* (a wedged dispatch loop, a git-proxy auth failure) rather
than only ever the target repo's own code. Advertised and answered only
when this process is started with `--self-debug`, which `dispatch.py` only
ever passes when the task issue actually carried `self_debug_label` --
every other task never sees this tool exists. Strictly read-only: the
`journalctl` invocation behind it takes no mutating flag, and `unit` is
checked against a fixed allowlist of the two long-running services this
deployment actually runs (`provision/controller.sh`), never passed through
as a raw systemd unit name.

A third `unit` value, `grain-task` (bwsalmon/agents#97), reads the journal
for the transient unit this very dispatch runs under -- `dispatch.py`'s
`unit_name()`, the same `grain-task-<sandbox>` `systemd-run` gave the
`claude -p` process this MCP server is a child of. It exists to close a
real blind spot: `_start_task` only ever redirects `claude -p`'s stdout
(its `--output-format stream-json` transcript, what `capture.py` later
reads) to a file -- stderr is never redirected, so both `claude -p`'s own
stderr and whatever this server writes to its own (inherited, same as any
child process's unless explicitly piped) fall through to the unit's
journal instead. `capture.py`'s docstring already names "a `claude -p`
that crashed before writing anything" as a real, expected case its
transcript capture returns `None` for -- before this, that crash had no
diagnosis path at all beyond direct operator SSH access. A session cannot
read its own mid-crash log the instant this server itself goes down, since
that is the same tool surface a dead server can no longer answer -- but the
unit name is fixed per sandbox and reused across dispatches
(`unit_name()`'s own docstring), so the *next* dispatch to that sandbox, if
also self-debug-labelled, can still read what an earlier crash on the same
box left behind. Resolved server-side from `--task-unit`, which
`dispatch.py` bakes into this process's own argv the same way it already
does the sandbox address -- `unit` in a tool call only ever selects among
the three fixed options, never supplies a systemd unit name itself.

Three more tools, `check_grain_health`, `read_grain_config`, and
`read_automation_audit_log` (bwsalmon/agents#86), round out the
self-debug surface -- same gating as `read_grain_logs` (advertised and
answered only under `--self-debug`), same "read-only, fixed allowlist,
never a raw path from the model" discipline:

- `check_grain_health` runs `health.py`'s existing `check_health` --
  ssh/systemd/docker/disk -- against either the assigned sandbox
  (`self.runner`) or the controller itself (`self.local_runner`), the same
  checks `grain host health` already exposes to an operator, now available
  to the agent for triaging a sandbox that's gone degraded or unreachable
  mid-task.
- `read_grain_config` reads one of the deployment's own config files under
  `/data/config` on the controller -- `automation.json`,
  `repo-allowlist.json`, `gemini-key.json`, `metadata-server.json`,
  `sandbox-github-key.json`. Every one of those is already non-secret by
  construction (`configure.py` never writes a token or key under
  `/data/config`; every credential lives under `/data/secrets` instead,
  which this tool has no path to reach at all) -- `file` is checked
  against a fixed allowlist of those five names, not a raw path, so this
  can never be pointed anywhere else on the controller's filesystem.
- `read_automation_audit_log` reads recent lines of `audit.py`'s own
  `FileAuditLog` output (`/data/state/automation/audit.log`) -- one JSON
  line per dispatch/sweep decision the state machine in `core.py` made
  (a task dispatched, skipped, succeeded, failed, stranded, and why), the
  durable record of *why* the orchestrator did or didn't act, not just
  what it's doing right now.

Four more tools, `restart_grain_service`, `reboot_sandbox`,
`reformat_sandbox`, and `reboot_controller` (bwsalmon/agents#99), are the
mutating half of the self surface -- gated by a *second* label,
`grain-self-repair`, kept deliberately separate from `grain-self-debug`
above: every tool that label turns on is read-only by construction, and a
human who wants an agent to look at grain's own state should not have to
also hand it the power to restart services, reboot VMs, or wipe a
sandbox's docker/kind state. `main()`'s `--self-repair` flag (distinct
from `--self-debug`) gates all four the same way -- advertised and
answered only then, checked again inside `_dispatch_tool` the same way
`self_debug` already is, never trusted from the input schema alone.

**What these do and don't reach, and why.** `docs/design.md`'s "One host
machine runs everything" split matters here: the controller and every
sandbox are guest VMs the *host* machine's hypervisor manages
(`grain/adapter/base.py`'s `HostAdapter`), and the host is a different
machine the controller has no credential or network path to at all
(`provision/controller.sh`: "The public half does NOT reach the host by
anything this script can do"). That rules out the two operations an
operator would actually reach for first -- `grain host recreate
<sandbox>` (rebuild the VM from its image) and `grain host bootstrap`
(re-converge the whole deployment) -- neither is exposed here, and neither
can be without a new host-reachable channel this deployment doesn't have
today; see `docs/runbook.md`'s "Enabling grain-self-repair" for the gap
spelled out in full rather than silently faked. What *is* reachable from
the controller with the runners `McpServer` already holds:

- `restart_grain_service` restarts one of the same two units
  `read_grain_logs` already reads the journal of -- `grain-automation
  .service` or `grain-git-proxy.service` -- via `local_runner`, for a
  service that's wedged short of needing the whole VM rebooted. The same
  fixed `_SELF_DEBUG_UNITS` allowlist as `read_grain_logs`, not a raw unit
  name.
- `reboot_sandbox` reboots the one sandbox this task was ever assigned
  (`self.runner` -- never a name the model supplies, the same "the sandbox
  is fixed at process startup" invariant every other sandbox-facing tool
  here already holds). The blast radius is exactly one sandbox: nothing
  else in the pool is touched, and the orchestrator's own sweep already
  tolerates a sandbox that drops off mid-task (`docs/design.md`'s
  stranded-work handling).
- `reformat_sandbox` runs `cleanup.py`'s existing between-task hygiene
  (`kind delete clusters --all`, `docker system prune -af --volumes`) --
  the same routine `grain host cleanup`/the sweeper's own post-task pass
  already run, just callable mid-task instead of only between tasks. Short
  of the VM rebuild this deployment's controller can't reach, this is the
  deepest reset available.
- `reboot_controller` is the one genuinely drastic tool here: it reboots
  the controller VM `claude -p` and this very MCP server are running on.
  Its own tool description says as much -- like `ask_question`, calling it
  ends the turn, because the process making the call is about to die.
  Unlike `reboot_sandbox`, the blast radius is the whole controller: every
  other task dispatched concurrently on it is interrupted too. That is a
  real cost, not a hidden one, but it is a *recoverable* one by
  construction, not a new risk this tool introduces -- `docs/next-session
  .md` documents `AutomationState` being persisted incrementally, before
  each GitHub side effect that would otherwise make a mid-flight crash
  unrecoverable, specifically so "the controller VM can be restarted or
  recreated at any moment" without losing a task. `reboot_controller`
  exercises that already-built recovery path deliberately, as a
  last-resort tool for a controller that is wedged in a way no service
  restart fixes (a hung kernel thread, a filled root disk, `systemd`
  itself gone `degraded` in a way `systemctl restart` alone can't clear).
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import shlex
import sys
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import TextIO

from ..run import Runner, RealRunner
from .cleanup import cleanup
from .health import check_health
from .ssh import SshRunner

PROTOCOL_VERSION = "2024-11-05"
SERVER_NAME = "grain-sandbox"

TOOLS = [
    {
        "name": "run_command",
        "description": "Run a shell command in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "command": {"type": "string"},
                "timeout": {
                    "type": "number",
                    "description": "Timeout in milliseconds, max 600000",
                },
                "description": {
                    "type": "string",
                    "description": "Short description of what this command does",
                },
            },
            "required": ["command"],
        },
    },
    {
        "name": "read_file",
        "description": "Read a file from your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "offset": {"type": "integer", "description": "Line number to start from"},
                "limit": {"type": "integer", "description": "Number of lines to read"},
            },
            "required": ["file_path"],
        },
    },
    {
        "name": "edit_file",
        "description": "Replace an exact string in a file in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "old_string": {"type": "string"},
                "new_string": {"type": "string"},
                "replace_all": {"type": "boolean", "default": False},
            },
            "required": ["file_path", "old_string", "new_string"],
        },
    },
    {
        "name": "write_file",
        "description": "Write (create or overwrite) a file in your assigned sandbox workspace.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "file_path": {"type": "string"},
                "content": {"type": "string"},
            },
            "required": ["file_path", "content"],
        },
    },
    {
        "name": "ask_question",
        "description": (
            "Ask the human a clarifying question when you cannot safely or "
            "correctly proceed without their input. This ends your turn: the "
            "question is posted as a comment on the GitHub issue or pull "
            "request, the task is taken out of the queue, and a human must "
            "reply and re-apply the trigger label before another attempt "
            "runs. Do not call this for routine progress updates or when you "
            "can reasonably proceed on your own judgment -- only when you are "
            "genuinely blocked. After calling this, do not take any further "
            "actions."
        ),
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "question": {"type": "string"},
            },
            "required": ["question"],
        },
    },
    {
        "name": "comment_on_issue",
        "description": (
            "Leave a comment on the task's GitHub issue. Use this when the "
            "task only asked for an answer, an investigation, or a "
            "recommendation -- not a code change: if you never push a "
            "branch, this comment becomes the task's closing note and no "
            "pull request is opened. If you do push commits, a pull "
            "request is opened for them regardless of whether you also "
            "call this -- calling it does not by itself prevent a pull "
            "request from opening. After calling this, do not take any "
            "further actions unless you still intend to push commits."
        ),
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "comment": {"type": "string"},
            },
            "required": ["comment"],
        },
    },
]

# The exact two long-running controller services this deployment ever runs
# (provision/controller.sh) -- an allowlist, not a free-form unit name, so
# `read_grain_logs` can never be pointed at an arbitrary systemd unit on
# the controller.
_SELF_DEBUG_UNITS = {
    "grain-automation": "grain-automation.service",
    "grain-git-proxy": "grain-git-proxy.service",
}

# bwsalmon/agents#97: a third, dynamic `unit` value -- unlike the two
# above, there is no single fixed service name for it, since it names
# whichever `grain-task-<sandbox>` unit *this* dispatch happens to be
# running under (`McpServer.task_unit`, threaded from `dispatch.py`'s
# `--task-unit`). Kept out of `_SELF_DEBUG_UNITS` because that dict's
# values are literal, deployment-wide service names; this one is resolved
# per-instance instead. Still just as fixed a choice from the model's point
# of view -- `read_grain_logs`'s `unit` enum below names it as a literal
# alongside the other two, never as a free-form unit string.
_TASK_UNIT_KEY = "grain-task"

# bwsalmon/agents#62: kept out of `TOOLS` above -- `McpServer` only ever
# advertises this one when started with `--self-debug` (`main()`), which
# `dispatch.py` only passes for a task whose issue carried
# `self_debug_label`. Every other task's `tools/list` never mentions it.
_READ_GRAIN_LOGS_TOOL = {
    "name": "read_grain_logs",
    "description": (
        "Read recent journal log entries for one of grain's own "
        "controller services, or for this dispatch's own controller-side "
        "process (grain-task) -- for triaging a bug in grain itself, not "
        "the target repo's own code. grain-task is the only place to find "
        "claude -p's own stderr, which never reaches the transcript file; "
        "useful when a redispatch to this same sandbox needs to see why "
        "an earlier run on it crashed. Read-only: this can only read the "
        "journal, never change anything about the controller. Only "
        "available on a task whose issue carries the grain-self-debug "
        "label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "unit": {
                "type": "string",
                "enum": sorted(_SELF_DEBUG_UNITS) + [_TASK_UNIT_KEY],
                "description": (
                    "Which unit's log to read: one of grain's own "
                    "controller services, or grain-task for this "
                    "dispatch's own claude -p process."
                ),
            },
            "lines": {
                "type": "integer",
                "description": "Number of most recent lines to return (default 200).",
            },
        },
        "required": ["unit"],
    },
}

# bwsalmon/agents#86: which machine `check_grain_health` can be pointed at
# -- an enum, not a free-form address, so it can never be redirected at
# anything other than the two machines already in play for this dispatch.
_HEALTH_TARGETS = ("sandbox", "controller")

_CHECK_GRAIN_HEALTH_TOOL = {
    "name": "check_grain_health",
    "description": (
        "Run grain's own health checks -- SSH reachability, systemd "
        "state, docker responsiveness, disk usage -- against either your "
        "assigned sandbox or the controller grain's own services run on. "
        "The same checks `grain host health` reports to an operator, for "
        "triaging a sandbox or controller that has gone degraded or "
        "unreachable, not the target repo's own code. Read-only. Only "
        "available on a task whose issue carries the grain-self-debug "
        "label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "target": {
                "type": "string",
                "enum": list(_HEALTH_TARGETS),
                "description": (
                    "Which machine to check: 'sandbox' (your assigned "
                    "sandbox) or 'controller' (where grain's own services "
                    "run)."
                ),
            },
        },
        "required": ["target"],
    },
}

# bwsalmon/agents#86: the exact non-secret config files `configure.py`
# writes under `/data/config` -- an allowlist, not a free-form path, so
# `read_grain_config` can never be pointed at `/data/secrets` or anywhere
# else on the controller's filesystem.
_SELF_DEBUG_CONFIG_FILES = {
    "automation": "automation.json",
    "repo-allowlist": "repo-allowlist.json",
    "gemini-key": "gemini-key.json",
    "metadata-server": "metadata-server.json",
    "sandbox-github-key": "sandbox-github-key.json",
}

_DATA_CONFIG_DIR = "/data/config"

_READ_GRAIN_CONFIG_TOOL = {
    "name": "read_grain_config",
    "description": (
        "Read one of grain's own non-secret configuration files from "
        "/data/config on the controller: automation.json, "
        "repo-allowlist.json, gemini-key.json, metadata-server.json, or "
        "sandbox-github-key.json -- for triaging a bug in grain itself, "
        "not the target repo's own code. Every credential and token this "
        "deployment holds lives under /data/secrets instead, which this "
        "tool has no path to reach at all: only these five names are "
        "readable. Reports a file as unconfigured rather than erroring if "
        "this deployment never wrote it (gemini-key.json and "
        "sandbox-github-key.json are both optional). Only available on a "
        "task whose issue carries the grain-self-debug label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "file": {
                "type": "string",
                "enum": sorted(_SELF_DEBUG_CONFIG_FILES),
                "description": "Which config file to read.",
            },
        },
        "required": ["file"],
    },
}

# bwsalmon/agents#86: `audit.py`'s own FileAuditLog output path -- a fixed
# constant, not a parameter, since there is exactly one audit log on a
# deployment and no reason a tool call should ever name a different one.
_AUDIT_LOG_PATH = "/data/state/automation/audit.log"

_READ_AUTOMATION_AUDIT_LOG_TOOL = {
    "name": "read_automation_audit_log",
    "description": (
        "Read recent entries from grain's own dispatch/sweep audit log -- "
        "one JSON line per state-machine decision the orchestrator in "
        "core.py made (a task dispatched, skipped, succeeded, failed, or "
        "stranded, and why) -- for triaging why the orchestrator did or "
        "didn't act on a task, not the target repo's own code. "
        "Read-only. Only available on a task whose issue carries the "
        "grain-self-debug label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "lines": {
                "type": "integer",
                "description": "Number of most recent lines to return (default 200).",
            },
        },
        "required": [],
    },
}

# bwsalmon/agents#86: the full self-debug roster -- `tools/list` appends
# this whole list, rather than each tool individually, so a fifth
# self-debug tool later needs only one line added here.
_SELF_DEBUG_TOOLS = [
    _READ_GRAIN_LOGS_TOOL,
    _CHECK_GRAIN_HEALTH_TOOL,
    _READ_GRAIN_CONFIG_TOOL,
    _READ_AUTOMATION_AUDIT_LOG_TOOL,
]

# bwsalmon/agents#99: gated by `grain-self-repair`, a *separate* label from
# `grain-self-debug` above -- see the module docstring for why the two are
# kept apart. `_RESTART_GRAIN_SERVICE_TOOL` reuses `_SELF_DEBUG_UNITS`
# directly rather than a second copy of the same two-service allowlist.
_RESTART_GRAIN_SERVICE_TOOL = {
    "name": "restart_grain_service",
    "description": (
        "Restart one of grain's own controller services -- for a service "
        "that's wedged or stuck in a bad state short of needing the whole "
        "controller rebooted. Runs `systemctl restart` on the controller "
        "itself, not the target repo's code. Only available on a task "
        "whose issue carries the grain-self-repair label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {
            "unit": {
                "type": "string",
                "enum": sorted(_SELF_DEBUG_UNITS),
                "description": "Which controller service to restart.",
            },
        },
        "required": ["unit"],
    },
}

_REBOOT_SANDBOX_TOOL = {
    "name": "reboot_sandbox",
    "description": (
        "Reboot your assigned sandbox VM -- for a sandbox that's wedged in "
        "a way `reformat_sandbox` alone won't clear (a hung docker daemon, "
        "a degraded systemd). Only ever targets the one sandbox this task "
        "was dispatched to, never named by you. The SSH connection this "
        "runs over will itself be cut by the reboot, so a broken-pipe-like "
        "error back from this call is the expected outcome, not a sign it "
        "failed. Only available on a task whose issue carries the "
        "grain-self-repair label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {},
    },
}

_REFORMAT_SANDBOX_TOOL = {
    "name": "reformat_sandbox",
    "description": (
        "Reset your assigned sandbox's docker/kind state -- the same "
        "between-task hygiene (`kind delete clusters --all`, `docker "
        "system prune -af --volumes`) grain already runs automatically "
        "once a task finishes, callable mid-task instead of only between "
        "tasks. Does not touch your git workspace or reboot the sandbox. "
        "Only available on a task whose issue carries the grain-self-repair "
        "label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {},
    },
}

_REBOOT_CONTROLLER_TOOL = {
    "name": "reboot_controller",
    "description": (
        "Reboot the controller VM this task's own `claude -p` session and "
        "MCP server are running on -- the last-resort tool here, for a "
        "controller wedged in a way no service restart fixes. This ends "
        "your turn: the process making this call is about to be killed by "
        "the reboot it just triggered, and any other task running "
        "concurrently on this same controller is interrupted too (it will "
        "be recovered automatically once the controller comes back -- "
        "grain's stranded-work sweep exists exactly for this). Do not take "
        "any further actions after calling this. Only available on a task "
        "whose issue carries the grain-self-repair label."
    ),
    "inputSchema": {
        "type": "object",
        "additionalProperties": False,
        "properties": {},
    },
}

# bwsalmon/agents#99: same "one list, not four separate `if self.x:`
# branches" shape as `_SELF_DEBUG_TOOLS` above.
_SELF_REPAIR_TOOLS = [
    _RESTART_GRAIN_SERVICE_TOOL,
    _REBOOT_SANDBOX_TOOL,
    _REFORMAT_SANDBOX_TOOL,
    _REBOOT_CONTROLLER_TOOL,
]


@dataclass(frozen=True)
class ToolResult:
    text: str
    is_error: bool = False


def run_command(runner: Runner, workspace: str, command: str, *,
                 timeout: float | None = None, description: str | None = None) -> ToolResult:
    """`timeout` (milliseconds, matching native Bash's own unit) is applied
    with the `timeout` coreutil on the sandbox side rather than adding
    timeout support to `Runner`/`SshRunner` -- smaller, more surgical, and
    `Runner` stays a plain synchronous call/response abstraction other
    callers already depend on.
    """
    shell = f"cd {shlex.quote(workspace)} && {command}"
    if timeout is not None:
        seconds = max(1, int(timeout / 1000))
        shell = f"timeout {seconds} bash -c {shlex.quote(shell)}"
    result = runner.run(["bash", "-c", shell], check=False)
    text = f"exit={result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    return ToolResult(text=text, is_error=result.returncode != 0)


def _read_remote(runner: Runner, path: str) -> tuple[str | None, ToolResult | None]:
    result = runner.run(["cat", "--", path], check=False)
    if result.returncode != 0:
        return None, ToolResult(text=f"Error reading {path}: {result.stderr.strip()}", is_error=True)
    return result.stdout, None


def read_file(runner: Runner, workspace: str, file_path: str, *,
               offset: int | None = None, limit: int | None = None) -> ToolResult:
    content, error = _read_remote(runner, file_path)
    if error is not None:
        return error
    lines = content.splitlines()
    start = offset or 0
    end = start + limit if limit is not None else len(lines)
    # cat -n-style numbering: the model's own Edit-quoting behavior depends
    # on having seen this exact format, not just the raw file content.
    numbered = "\n".join(f"{i + start + 1:6d}\t{line}" for i, line in enumerate(lines[start:end]))
    return ToolResult(text=numbered)


def _write_remote(runner: Runner, path: str, content: str) -> ToolResult:
    result = runner.run(["dd", f"of={path}", "status=none"], stdin=content, check=False)
    if result.returncode != 0:
        return ToolResult(text=f"Error writing {path}: {result.stderr.strip()}", is_error=True)
    return ToolResult(text=f"Wrote {path}")


def edit_file(runner: Runner, workspace: str, file_path: str, old_string: str,
              new_string: str, *, replace_all: bool = False) -> ToolResult:
    content, error = _read_remote(runner, file_path)
    if error is not None:
        return error
    count = content.count(old_string)
    if count == 0:
        return ToolResult(text=f"String not found in file: {old_string!r}", is_error=True)
    if count > 1 and not replace_all:
        return ToolResult(
            text=(f"String appears {count} times in the file. Use replace_all: "
                  "true to replace every occurrence, or provide more surrounding "
                  "context to uniquely identify the instance you mean."),
            is_error=True,
        )
    new_content = content.replace(old_string, new_string, -1 if replace_all else 1)
    return _write_remote(runner, file_path, new_content)


def write_file(runner: Runner, workspace: str, file_path: str, content: str) -> ToolResult:
    parent = str(PurePosixPath(file_path).parent)
    mkdir_result = runner.run(["mkdir", "-p", parent], check=False)
    if mkdir_result.returncode != 0:
        return ToolResult(text=f"Error creating {parent}: {mkdir_result.stderr.strip()}", is_error=True)
    return _write_remote(runner, file_path, content)


def ask_question(question_path: str, question: str) -> ToolResult:
    """Records `question` for `core.py` to relay to a human as a GitHub
    comment (docs/roadmap.md item 12) -- a plain local file write, no
    `Runner`/SSH involved at all, unlike every other tool here: the
    question is for a human, not the sandbox, so there is nothing to reach
    over SSH for. Overwrites on a repeat call within the same dispatch --
    the last question asked is the one that matters; `dispatch.py` resets
    this file at the start of every dispatch so a leftover question can
    never leak into a later, unrelated one on the same reused sandbox.
    """
    Path(question_path).write_text(question)
    return ToolResult(
        text="Your question has been recorded and will be posted as a "
             "comment on the GitHub issue/PR for a human to answer. Do not "
             "take any further actions -- end your turn now."
    )


def comment_on_issue(comment_path: str, comment: str) -> ToolResult:
    """Records `comment` for `core.py` to post on the task issue
    (bwsalmon/agents#50, bwsalmon/agents#89) -- same shape as
    `ask_question`: a plain local file write, no `Runner`/SSH involved,
    since the comment is for a human via GitHub, not the sandbox.
    Overwrites on a repeat call within the same dispatch, and `dispatch.py`
    resets this file at the start of every dispatch, for the same reasons
    `ask_question` already documents for its own file.

    Unlike the `complete_analysis` tool this replaced, writing this file is
    no longer what decides whether a pull request opens -- `core.py` always
    checks the branch first (bwsalmon/agents#89) and only falls back to
    posting this as a closing comment when that branch turns out to have
    nothing on it.
    """
    Path(comment_path).write_text(comment)
    return ToolResult(
        text="Your comment has been recorded. If you never push a branch, "
             "it will be posted on the GitHub issue as the task's closing "
             "note; if you do push commits, a pull request opens for them "
             "regardless. Do not take any further actions unless you "
             "still intend to push commits."
    )


def read_grain_logs(local_runner: Runner, unit: str, *, lines: int | None = None,
                     task_unit: str | None = None) -> ToolResult:
    """Reads recent journal entries for one of grain's own controller
    services (bwsalmon/agents#62), or for this dispatch's own transient
    unit (bwsalmon/agents#97), for triaging a bug in grain itself.

    Run with `local_runner`, never `self.runner` (the `SshRunner` that
    reaches the *sandbox*): the journal this reads lives on the controller,
    where this process already runs, so there is no SSH hop to make, and
    `self.runner`'s sandbox has no visibility into it at all. `unit` is
    checked against `_SELF_DEBUG_UNITS` (plus the one dynamic
    `_TASK_UNIT_KEY` case below) before it ever reaches an argv -- the
    input schema already constrains it to that same enum, but this function
    does not trust the caller to have honoured it.

    `task_unit` is `McpServer.task_unit` passed straight through -- the
    literal `grain-task-<sandbox>` unit name `dispatch.py` baked into this
    process's own `--task-unit` argv at startup, never something a tool
    call can name itself. `None` only when a caller (a test, or a
    deployment mid-rollout before `dispatch.py` started passing
    `--task-unit`) never supplied one -- `grain-task` is then refused with
    an explanation rather than crashing on a `None` unit name, the same
    "absence is the off switch" treatment `gemini_key_config`/
    `metadata_launcher` already get on `Orchestrator`.
    """
    if unit == _TASK_UNIT_KEY:
        if task_unit is None:
            return ToolResult(
                text="grain-task is not available: this dispatch was never "
                     "given its own unit name.",
                is_error=True,
            )
        service = f"{task_unit}.service"
    else:
        service = _SELF_DEBUG_UNITS.get(unit)
        if service is None:
            allowed = sorted(_SELF_DEBUG_UNITS) + [_TASK_UNIT_KEY]
            return ToolResult(
                text=f"Unknown unit {unit!r}. Must be one of: {', '.join(allowed)}.",
                is_error=True,
            )
    n = lines if lines is not None else 200
    result = local_runner.run(
        ["journalctl", "-u", service, "-n", str(n), "--no-pager"], check=False
    )
    text = f"exit={result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    return ToolResult(text=text, is_error=result.returncode != 0)


def check_grain_health(runner: Runner, local_runner: Runner, target: str) -> ToolResult:
    """Runs `health.py`'s existing `check_health` (bwsalmon/agents#86)
    against whichever machine `target` names. `runner` (the sandbox's own
    `SshRunner`) and `local_runner` (the controller, same as
    `read_grain_logs`) are both already available on `McpServer`; this
    just picks between them rather than opening a third kind of
    connection. `target` is checked against `_HEALTH_TARGETS` before
    either runner is touched -- the input schema's `enum` already
    constrains it, but this function does not trust the caller to have
    honoured it, same discipline `read_grain_logs` already holds `unit`
    to.
    """
    if target not in _HEALTH_TARGETS:
        return ToolResult(
            text=f"Unknown target {target!r}. Must be one of: {', '.join(_HEALTH_TARGETS)}.",
            is_error=True,
        )
    chosen = runner if target == "sandbox" else local_runner
    report = check_health(chosen)
    text = f"status={report.status.value}\n{report.summary()}"
    return ToolResult(text=text, is_error=not report.ok)


def read_grain_config(local_runner: Runner, file: str) -> ToolResult:
    """Reads one of the fixed, non-secret config files under
    `/data/config` (bwsalmon/agents#86) -- run against `local_runner`, the
    controller, for the same reason `read_grain_logs` does: the files live
    there, so there is no SSH hop to the sandbox to make. `file` is
    resolved through `_SELF_DEBUG_CONFIG_FILES` before it ever reaches an
    argv, never joined onto `_DATA_CONFIG_DIR` directly from caller input.
    """
    filename = _SELF_DEBUG_CONFIG_FILES.get(file)
    if filename is None:
        return ToolResult(
            text=(f"Unknown file {file!r}. Must be one of: "
                  f"{', '.join(sorted(_SELF_DEBUG_CONFIG_FILES))}."),
            is_error=True,
        )
    path = f"{_DATA_CONFIG_DIR}/{filename}"
    result = local_runner.run(["cat", "--", path], check=False)
    if result.returncode != 0:
        # Most commonly a genuinely absent optional file (gemini-key.json,
        # sandbox-github-key.json) rather than a real failure -- reported
        # as plain informational text, not an error, so the agent doesn't
        # treat "this deployment never turned this feature on" as a tool
        # malfunction.
        return ToolResult(text=f"{filename} does not exist on this deployment.")
    return ToolResult(text=result.stdout)


def read_automation_audit_log(local_runner: Runner, *, lines: int | None = None) -> ToolResult:
    """Reads recent lines of `audit.py`'s `FileAuditLog` output
    (bwsalmon/agents#86) -- the durable, one-line-per-decision record of
    the dispatch/sweep state machine in `core.py`, run against
    `local_runner` since the file lives on the controller, same as
    `read_grain_logs`/`read_grain_config`.
    """
    n = lines if lines is not None else 200
    result = local_runner.run(["tail", "-n", str(n), "--", _AUDIT_LOG_PATH], check=False)
    text = f"exit={result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    return ToolResult(text=text, is_error=result.returncode != 0)


def restart_grain_service(local_runner: Runner, unit: str) -> ToolResult:
    """Restarts one of grain's own controller services (bwsalmon/agents#99)
    -- run against `local_runner`, the controller, same as every other
    self-* tool that reads or touches something on the controller itself
    rather than the sandbox. `unit` is resolved through `_SELF_DEBUG_UNITS`
    before it ever reaches an argv, the same discipline `read_grain_logs`
    already holds it to -- reused directly rather than a second copy of
    the same two-service allowlist.

    `sudo` here relies on the narrow, unconditional NOPASSWD grant
    `provision/controller.sh` gives `grain-agent` for exactly this command
    line (and no other) -- the mutating counterpart to the `systemd-journal`
    group membership that already makes `read_grain_logs` work.
    """
    service = _SELF_DEBUG_UNITS.get(unit)
    if service is None:
        return ToolResult(
            text=f"Unknown unit {unit!r}. Must be one of: {', '.join(sorted(_SELF_DEBUG_UNITS))}.",
            is_error=True,
        )
    result = local_runner.run(["sudo", "systemctl", "restart", service], check=False)
    text = f"exit={result.returncode}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    return ToolResult(text=text, is_error=result.returncode != 0)


def reboot_sandbox(runner: Runner) -> ToolResult:
    """Reboots the assigned sandbox (bwsalmon/agents#99) over `runner` --
    the same `SshRunner` `run_command`/`read_file`/etc. already use, so
    this can never be pointed at any sandbox other than the one this task
    was dispatched to. The sandbox's own cloud-init default user already
    carries passwordless sudo (the same assumption `cleanup.py`/`health.py`
    already make of it), so no new provisioning is needed for this one,
    unlike `restart_grain_service`'s controller-side grant.

    A reboot cuts the SSH session that issued it, so `runner.run` returning
    a nonzero code or a transport-level failure here is the expected shape
    of *success*, not a sign the reboot didn't happen -- reported as
    informational text either way rather than as an error, so the agent
    doesn't read its own successful reboot as a tool malfunction.
    """
    result = runner.run(["sudo", "reboot"], check=False)
    return ToolResult(
        text=(f"Reboot triggered (exit={result.returncode}). The SSH "
              "connection this ran over may already have dropped as a "
              "result -- that is expected, not a failure.")
    )


def reformat_sandbox(runner: Runner) -> ToolResult:
    """Runs `cleanup.py`'s existing between-task hygiene against the
    assigned sandbox (bwsalmon/agents#99) -- the same routine the sweeper
    already runs automatically once a task's slot frees, exposed here so
    an agent can reach for it mid-task instead of only ever getting it for
    free between tasks. Reuses `cleanup()` directly rather than
    reimplementing its steps, so the two never drift apart.
    """
    result = cleanup(runner)
    return ToolResult(text=f"status={'ok' if result.ok else 'FAIL'}\n{result.summary()}",
                       is_error=not result.ok)


def reboot_controller(local_runner: Runner) -> ToolResult:
    """Reboots the controller VM itself (bwsalmon/agents#99) -- the
    genuinely drastic tool in this roster; see the module docstring for
    the full reasoning on blast radius and why it's still safe to expose.
    Run against `local_runner`, since there is no sandbox to reach over
    SSH for this one -- the controller *is* where this process runs.

    Relies on the same `provision/controller.sh` NOPASSWD grant
    `restart_grain_service` does, for `systemctl reboot` specifically.
    Like that reboot, this call's own process is about to be killed by the
    action it just triggered, so the text returned here is unlikely to
    ever reach the model -- `_dispatch_tool`'s caller sends the response
    on a best-effort basis before the VM actually goes down.
    """
    local_runner.run(["sudo", "systemctl", "reboot"], check=False)
    return ToolResult(
        text="Controller reboot triggered. Do not take any further "
             "actions -- this process is about to be killed."
    )


class McpServer:
    """The JSON-RPC method dispatch, kept separate from stdio plumbing
    (`serve()`) so `handle()` can be exercised directly in tests with a
    plain dict in, dict out -- no subprocess, no real stdin/stdout.
    """

    def __init__(self, runner: Runner, workspace: str, *,
                 question_path: str | None = None,
                 comment_path: str | None = None,
                 self_debug: bool = False,
                 self_repair: bool = False,
                 local_runner: Runner | None = None,
                 task_unit: str | None = None) -> None:
        self.runner = runner
        self.workspace = workspace
        # None only in tests that don't care about ask_question -- `main()`
        # always supplies a real path in production.
        self.question_path = question_path
        # Same "None only in tests" treatment as question_path, for
        # comment_on_issue (bwsalmon/agents#50, bwsalmon/agents#89).
        self.comment_path = comment_path
        # bwsalmon/agents#62: whether this dispatch's task issue carried
        # `self_debug_label` -- `main()` sets this from `--self-debug`,
        # which `dispatch.py` only ever passes in that case. Gates both
        # whether `read_grain_logs` is advertised at all (`tools/list`
        # below) and whether it does anything if called anyway.
        self.self_debug = self_debug
        # bwsalmon/agents#99: the same "task issue carried the label"
        # record as `self_debug`, for `self_repair_label`/`--self-repair`
        # -- a deliberately separate flag, not folded into `self_debug`,
        # since these four tools mutate state instead of only reading it.
        self.self_repair = self_repair
        # bwsalmon/agents#97: this dispatch's own `grain-task-<sandbox>`
        # unit name, from `--task-unit` -- `read_grain_logs`'s dynamic
        # `grain-task` case resolves against this, never a name a tool call
        # supplies itself. `None` only in tests that don't exercise that
        # case; every real dispatch supplies it via `_mcp_config_json`.
        self.task_unit = task_unit
        # Deliberately a *different* Runner than `self.runner`: that one is
        # an `SshRunner` pointed at the assigned *sandbox*, but the journal
        # `read_grain_logs` reads lives on the controller, where this
        # process already runs -- `RealRunner()` runs it right here, no SSH
        # hop. `None` (every real dispatch) builds a real one lazily-ish
        # here rather than in `read_grain_logs` itself, so a test can still
        # inject a `FakeRunner` the same way it already does for `runner`.
        self.local_runner = local_runner if local_runner is not None else RealRunner()

    def handle(self, msg: dict) -> dict | None:
        method = msg.get("method")
        msg_id = msg.get("id")
        if method == "initialize":
            return {
                "jsonrpc": "2.0", "id": msg_id,
                "result": {
                    "protocolVersion": PROTOCOL_VERSION,
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": SERVER_NAME, "version": "0.1.0"},
                },
            }
        if method == "notifications/initialized":
            return None
        if method == "tools/list":
            tools = (
                TOOLS
                + (_SELF_DEBUG_TOOLS if self.self_debug else [])
                + (_SELF_REPAIR_TOOLS if self.self_repair else [])
            )
            return {"jsonrpc": "2.0", "id": msg_id, "result": {"tools": tools}}
        if method == "tools/call":
            return self._handle_call(msg_id, msg.get("params") or {})
        if msg_id is not None:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32601, "message": f"unknown method {method}"}}
        return None

    def _handle_call(self, msg_id, params: dict) -> dict:
        name = params.get("name")
        args = params.get("arguments") or {}
        try:
            result = self._dispatch_tool(name, args)
        except KeyError as exc:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32602, "message": f"missing argument {exc}"}}
        if result is None:
            return {"jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32602, "message": f"unknown tool {name}"}}
        return {
            "jsonrpc": "2.0", "id": msg_id,
            "result": {"content": [{"type": "text", "text": result.text}],
                       "isError": result.is_error},
        }

    def _dispatch_tool(self, name: str, args: dict) -> ToolResult | None:
        if name == "run_command":
            return run_command(self.runner, self.workspace, args["command"],
                                timeout=args.get("timeout"), description=args.get("description"))
        if name == "read_file":
            return read_file(self.runner, self.workspace, args["file_path"],
                              offset=args.get("offset"), limit=args.get("limit"))
        if name == "edit_file":
            return edit_file(self.runner, self.workspace, args["file_path"],
                              args["old_string"], args["new_string"],
                              replace_all=args.get("replace_all", False))
        if name == "write_file":
            return write_file(self.runner, self.workspace, args["file_path"], args["content"])
        if name == "ask_question":
            if self.question_path is None:
                return ToolResult(
                    text="ask_question is not configured for this session.",
                    is_error=True,
                )
            return ask_question(self.question_path, args["question"])
        if name == "comment_on_issue":
            if self.comment_path is None:
                return ToolResult(
                    text="comment_on_issue is not configured for this session.",
                    is_error=True,
                )
            return comment_on_issue(self.comment_path, args["comment"])
        if name == "read_grain_logs":
            if not self.self_debug:
                return ToolResult(
                    text="read_grain_logs is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-debug label.",
                    is_error=True,
                )
            return read_grain_logs(self.local_runner, args["unit"], lines=args.get("lines"),
                                    task_unit=self.task_unit)
        if name == "check_grain_health":
            if not self.self_debug:
                return ToolResult(
                    text="check_grain_health is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-debug label.",
                    is_error=True,
                )
            return check_grain_health(self.runner, self.local_runner, args["target"])
        if name == "read_grain_config":
            if not self.self_debug:
                return ToolResult(
                    text="read_grain_config is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-debug label.",
                    is_error=True,
                )
            return read_grain_config(self.local_runner, args["file"])
        if name == "read_automation_audit_log":
            if not self.self_debug:
                return ToolResult(
                    text="read_automation_audit_log is not enabled for this "
                         "task -- only available when the task issue carries "
                         "the grain-self-debug label.",
                    is_error=True,
                )
            return read_automation_audit_log(self.local_runner, lines=args.get("lines"))
        if name == "restart_grain_service":
            if not self.self_repair:
                return ToolResult(
                    text="restart_grain_service is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-repair label.",
                    is_error=True,
                )
            return restart_grain_service(self.local_runner, args["unit"])
        if name == "reboot_sandbox":
            if not self.self_repair:
                return ToolResult(
                    text="reboot_sandbox is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-repair label.",
                    is_error=True,
                )
            return reboot_sandbox(self.runner)
        if name == "reformat_sandbox":
            if not self.self_repair:
                return ToolResult(
                    text="reformat_sandbox is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-repair label.",
                    is_error=True,
                )
            return reformat_sandbox(self.runner)
        if name == "reboot_controller":
            if not self.self_repair:
                return ToolResult(
                    text="reboot_controller is not enabled for this task -- "
                         "only available when the task issue carries the "
                         "grain-self-repair label.",
                    is_error=True,
                )
            return reboot_controller(self.local_runner)
        return None


def serve(runner: Runner, workspace: str, *, question_path: str | None = None,
          comment_path: str | None = None, self_debug: bool = False,
          self_repair: bool = False, task_unit: str | None = None,
          stdin: TextIO = sys.stdin, stdout: TextIO = sys.stdout) -> None:
    server = McpServer(runner, workspace, question_path=question_path,
                        comment_path=comment_path, self_debug=self_debug,
                        self_repair=self_repair, task_unit=task_unit)
    for line in stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        response = server.handle(msg)
        if response is not None:
            stdout.write(json.dumps(response) + "\n")
            stdout.flush()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--address", required=True)
    parser.add_argument("--user", required=True)
    parser.add_argument("--key-path", required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--question-path", required=True)
    parser.add_argument("--comment-path", required=True)
    # bwsalmon/agents#62: off unless `dispatch.py`'s `_mcp_config_json`
    # added it, which only happens for a task whose issue carried
    # `self_debug_label`.
    parser.add_argument("--self-debug", action="store_true")
    # bwsalmon/agents#99: same shape as --self-debug, for `self_repair_label`
    # -- a separate flag, since the two labels (and the tool rosters they
    # gate) are deliberately independent.
    parser.add_argument("--self-repair", action="store_true")
    # bwsalmon/agents#97: this dispatch's own `grain-task-<sandbox>` unit
    # name -- `dispatch.py`'s `unit_name()`, the same one `systemd-run`
    # started this whole `claude -p` process under. Optional, not required
    # like `--address` et al., so a test invoking `main()` directly without
    # it still gets `McpServer.task_unit=None` (the "not supplied" case
    # `read_grain_logs` already handles) rather than an argparse error.
    parser.add_argument("--task-unit")
    args = parser.parse_args()
    runner = SshRunner(
        inner=RealRunner(), user=args.user,
        address=ipaddress.IPv4Address(args.address), key_path=Path(args.key_path),
    )
    serve(runner, args.workspace, question_path=args.question_path,
          comment_path=args.comment_path, self_debug=args.self_debug,
          self_repair=args.self_repair, task_unit=args.task_unit)


if __name__ == "__main__":
    main()
