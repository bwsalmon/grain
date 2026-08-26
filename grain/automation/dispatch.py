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
controller with almost its entire native tool roster disabled (`--tools`
naming only `_NATIVE_TOOLS`) and replaced by `mcp_server.py`'s four tools,
which are the *only* way the agent can touch the sandbox. Confirmed live:
`--tools '' --mcp-config <file> --allowedTools <names>` genuinely empties
the roster down to exactly the MCP tools (the advertised `tools` list in
the `system/init` event shrinks to exactly what's named), `--allowedTools`
alone does not (it's a permission hint, not a roster filter — both flags
are required together), and naming a native tool in `--allowedTools` alone
does not add it back once `--tools` excludes it either — found live, the
same real dispatch that finally proved this whole redesign end to end also
showed `Task` silently absent from the roster despite being listed in
`--allowedTools`, because `--tools ''` had excluded it from the registry
outright. `--tools 'Task'` (naming it directly) is what actually admits
it — confirmed safe to include, since a `Task`-spawned subagent was
separately confirmed live to inherit this same restricted roster, not
bypass it (an explicit system denial confirmed this, not just
self-report). `TodoWrite` was tried the same way and dropped: confirmed
live, twice, that no `--tools` syntax admits it in `-p`/headless mode at
all — most likely excluded there as a product decision, since a todo list
is a visible, ongoing tracker meant for an interactive session, which a
one-shot headless dispatch never has.

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
import secrets
import shlex
from dataclasses import dataclass
from enum import Enum
from urllib.parse import urlsplit

from .github import Comment, Issue, PullRequestDetail, ReviewComment
from ..run import Runner

# Fixed across every dispatch to a sandbox — long-lived sandboxes reuse the
# same checkout rather than getting a fresh one per task; see
# ensure_workspace()'s docstring.
WORKSPACE_PATH = "/home/debian/workspace"
_CREDENTIALS_PATH = "/home/debian/.git-credentials"
_GIT_IDENTITY_NAME = "grain agent"
_GIT_IDENTITY_EMAIL = "grain-agent@localhost"

# Where a minted Gemini API key (bwsalmon/agents#47, `gemini_keys.py`) lands
# in the sandbox -- outside WORKSPACE_PATH, same as _CREDENTIALS_PATH above,
# so it is never inside the git working tree a task's own `git add -A`-style
# command could sweep up and commit.
GEMINI_KEY_PATH = "/home/debian/.gemini-api-key"

# Where a minted GCP service-account key (bwsalmon/agents#126, `gcp_keys.py`)
# lands in the sandbox -- same "outside WORKSPACE_PATH" reasoning as
# GEMINI_KEY_PATH above, so it can never be swept up by a task's own
# `git add -A`-style command.
GCP_KEY_PATH = "/home/debian/.gcp-service-account.json"

# The unprivileged, dedicated controller-side account `claude -p` (and the
# MCP server it spawns as a child) runs as — never root, never the account
# `grain-automation.service` itself runs as. See provision/controller.sh.
CONTROLLER_AGENT_USER = "grain-agent"

# CONTROLLER_AGENT_USER's own copy of the controller's SSH key
# (provision/controller.sh), used by `mcp_server.py` (a child of the
# controller-side `claude -p` unit, so it runs as this same account) to
# reach the assigned sandbox. Deliberately a *separate* file from the
# orchestrator's own `AutomationConfig.ssh_key_path` (normally
# `/data/secrets/controller-ssh`, root-owned) rather than a shared,
# group-readable copy of it — found live: OpenSSH's client refuses to use
# *any* private key file it considers group-readable at all, regardless of
# how narrowly the group is scoped, so sharing one file across the two
# accounts (root, for grain-automation.service; grain-agent, for this)
# breaks the root side's own use of it. Two independently owner-only
# (0600) copies is the only shape that satisfies both.
CONTROLLER_AGENT_SSH_KEY_PATH = "/home/grain-agent/.ssh/controller-ssh"

# Where CONTROLLER_AGENT_USER's own Claude Code OAuth token lives (a bare
# `claude setup-token` value, placed by `grain/automation/configure.py`'s
# `configure_claude_token`, mode 600, owned by that account — kept separate
# from any personal `claude login` session on purpose, so this deployment's
# dispatch traffic never rides on an operator's own credential). Read into
# the unit's own environment at runtime (`export CLAUDE_CODE_OAUTH_TOKEN=
# "$(cat ...)"` in `_start_task`'s command string below), never passed as a
# `systemd-run --setenv=`/`--property=Environment=` argument — either of
# those would put the raw token in this process's own argv, and therefore
# in `ps` output, the exact class of leak this project avoids everywhere
# else (see `configure_git_credentials`'s docstring for the identical
# stdin-not-argv reasoning applied to the sandbox's own git-proxy token).
#
# A bare env var is safe here in a way it wasn't when `claude -p` ran on
# the sandbox (docs/roadmap.md item 8's "Update", found live: the token
# leaked into any unsandboxed Bash subprocess's environment trivially): the
# agent has no native Bash tool anymore (`--tools ""`), and its only
# execution surface (`mcp_server.py`'s `run_command`) runs exclusively on
# the *sandbox* over SSH, never in this process's own environment — there
# is no tool call that reads or forwards this process's env vars. The
# tradeoff, made deliberately rather than by default: this is a structural
# guarantee resting on `--tools ""` staying complete, not on the secret
# being structurally unreachable regardless of tool restrictions the way a
# credentials-file-only design would be -- chosen anyway so this
# deployment's own token stays separate from an operator's personal login.
CONTROLLER_AGENT_TOKEN_PATH = "/home/grain-agent/.claude-oauth-token"

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


def question_path(unit: str) -> str:
    """The fixed path `mcp_server.py`'s `ask_question` tool writes to, and
    `core.py`'s sweep reads back after a unit finishes (docs/roadmap.md item
    12) — same "compute once, share" shape `transcript_path` already uses,
    for the same reason: the writer (`mcp_server.py`, a separate process)
    and the reader (`core.py`) must never be able to disagree on where the
    question landed. Reused verbatim across dispatches to this sandbox
    (`unit_name()` is fixed per sandbox), which is exactly why `_start_task`
    resets it at the start of every dispatch — otherwise a question from
    one task could leak into a later, unrelated one that never asked
    anything.
    """
    return f"{_unit_dir(unit)}/question.txt"


def comment_path(unit: str) -> str:
    """The fixed path `mcp_server.py`'s `comment_on_issue` tool writes to,
    and `core.py`'s sweep reads back after a unit finishes (bwsalmon/agents#50,
    reworked by bwsalmon/agents#89) -- the same "compute once, share" shape
    `question_path` already uses, and reset the same way at the start of
    every dispatch to this sandbox so a comment from an earlier, unrelated
    task can never be misread as belonging to this one.
    """
    return f"{_unit_dir(unit)}/comment.txt"


def review_path(unit: str) -> str:
    """The fixed path `mcp_server.py`'s `add_review_comment` tool appends
    to, and `core.py`'s sweep reads back after a unit finishes
    (bwsalmon/agents#154) -- the same "compute once, share" shape
    `question_path`/`comment_path` already use, and reset the same way at
    the start of every dispatch to this sandbox so review comments from an
    earlier, unrelated task can never be misread as belonging to this one.
    """
    return f"{_unit_dir(unit)}/review.json"


def branch_name(issue: int) -> str:
    """The exact branch a dispatch for this issue must push to.

    Deterministic and derived from the issue number alone — never the
    agent's own report of what it did (docs/roadmap.md item 2). Both
    `dispatch()` (to put in the prompt) and `core.py` (to verify the branch
    exists before opening a PR) call this, so they can never disagree.
    """
    return f"grain/issue-{issue}"


def agent_id() -> str:
    """A short random hex id, minted fresh for each dispatch and handed to
    the agent in its prompt (`_prompt`/`_pr_prompt`) so it has something of
    its own to fold into any infrastructure it names as part of the task —
    a container, a cloud resource, a test database — without colliding with
    another agent's concurrently-running task. Unlike `branch_name()` and
    `transcript_path()`, nothing on the controller side ever needs to
    recompute this to agree with it, so it does not need to be a pure
    function of anything: `secrets.token_hex`, not a seeded/deterministic
    generator, is exactly right.
    """
    return secrets.token_hex(4)


def _format_comment(comment: Comment) -> str:
    return f"- {comment.user}:\n  {comment.body}"


def _conversation_section(comments: list[Comment]) -> str:
    """The plain top-level comment thread, formatted for a prompt — where a
    human's reply to a prior `ask_question` call (docs/roadmap.md item 12)
    would actually show up. Always included, blank state and all, matching
    `_pr_prompt`'s existing "(no inline review comments)" convention: a
    fresh issue with nothing to show is the common case, not an error.
    """
    return (
        "\n\n".join(_format_comment(c) for c in comments)
        if comments else "(no comments yet)"
    )


def _agent_id_line(agent_id_value: str) -> str:
    return (
        f"Your agent id is {agent_id_value}. If this task involves creating "
        "any infrastructure of your own — a container, a cloud resource, a "
        "test database, and the like — fold this id into its name. Other "
        "agents may be working concurrently, on this same host or others, "
        "and this id is yours alone to use so your infrastructure never "
        "collides with theirs."
    )


def _gemini_key_line() -> str:
    """Told to the agent only when a key was actually minted for this task
    (`core.py`'s `_dispatch`, gated on the task issue's `gemini_key_label`)
    -- the path, never the key value itself, so the raw secret never lands
    in the prompt file (which is written to disk under
    `/data/state/automation/units` and is part of the same transcript
    surface `dispatch.py`'s own docstring already treats as untrusted-but-
    not-secret).
    """
    return (
        f"A Gemini API key has been minted for this task and placed in "
        f"your sandbox at {GEMINI_KEY_PATH} (readable only by you). It "
        "will be revoked automatically once this task finishes, so it is "
        "only good for the duration of this session. To use it, read the "
        "file and export it yourself in whichever command needs it, for "
        "example:\n"
        f"    export GEMINI_API_KEY=\"$(cat {GEMINI_KEY_PATH})\"\n"
        "Do not print its contents, log it, or commit it anywhere."
    )


def _gcp_key_line() -> str:
    """Told to the agent only when this deployment mints a GCP service-
    account key for every dispatch (bwsalmon/agents#126, `core.py`'s
    `_dispatch`, gated on `Orchestrator.gcp_key_config` rather than a task
    label -- see `gcp_keys.py`'s own docstring for why this runs
    unconditionally, unlike the Gemini key above) -- the path, never the
    key material itself, for the same reason `_gemini_key_line` only ever
    names `GEMINI_KEY_PATH`.
    """
    return (
        f"A GCP service-account key has been minted for this task and "
        f"placed in your sandbox at {GCP_KEY_PATH} (readable only by "
        "you). It will be revoked automatically once this task finishes, "
        "so it is only good for the duration of this session. To use it "
        "with the gcloud CLI, run:\n"
        f"    gcloud auth activate-service-account --key-file={GCP_KEY_PATH}\n"
        "To use it with a Google client library instead, export it as "
        "application default credentials:\n"
        f"    export GOOGLE_APPLICATION_CREDENTIALS=\"{GCP_KEY_PATH}\"\n"
        "Do not print its contents, log it, or commit it anywhere."
    )


def _self_debug_line() -> str:
    """Told to the agent only when this task's issue carried
    `self_debug_label` (bwsalmon/agents#62, `core.py`'s `_resolve_target`)
    -- four MCP tools most tasks never see at all, for triaging a bug in
    grain itself rather than the target repo's own code (bwsalmon/agents#86
    added the last three).
    """
    return (
        "This task carries the grain-self-debug label, so you also have "
        "four extra tools, all strictly read-only -- there is no way to "
        "change anything about the controller or its own state through "
        "any of them:\n"
        "- read_grain_logs: recent journal entries from grain's own "
        "controller services (grain-automation, grain-git-proxy), or from "
        "grain-task, this dispatch's own claude -p process -- the only "
        "place to find its stderr, which never reaches the transcript "
        "file, useful if a previous run on this same sandbox crashed "
        "before doing anything.\n"
        "- check_grain_health: the same ssh/systemd/docker/disk checks "
        "`grain host health` reports, against either your assigned "
        "sandbox or the controller itself.\n"
        "- read_grain_config: one of grain's own non-secret config files "
        "under /data/config on the controller (automation.json, "
        "repo-allowlist.json, gemini-key.json, gcp-key.json, "
        "sandbox-github-key.json) -- no credential or token is ever "
        "reachable through it.\n"
        "- read_automation_audit_log: recent entries from the dispatch/"
        "sweep audit log -- one line per state-machine decision (a task "
        "dispatched, skipped, succeeded, failed, or stranded, and why).\n"
        "Use these to triage a bug in grain itself, as opposed to the "
        "target repo's own code."
    )


def _self_repair_line() -> str:
    """Told to the agent only when this task's issue carried
    `self_repair_label` (bwsalmon/agents#99) -- a separate, deliberately
    smaller set of tools than `_self_debug_line()`'s four: those are all
    read-only, these four mutate state (restart a service, reboot or
    reformat the sandbox, reboot the controller itself), which is exactly
    why they sit behind their own label rather than being folded into
    `grain-self-debug`.
    """
    return (
        "This task also carries the grain-self-repair label, so you have "
        "four more tools, all of which change something rather than just "
        "reading it -- use them to recover from a failure in grain itself, "
        "not to modify the target repo's own code:\n"
        "- restart_grain_service: restart grain-automation.service or "
        "grain-git-proxy.service on the controller.\n"
        "- reboot_sandbox: reboot your assigned sandbox VM.\n"
        "- reformat_sandbox: reset your assigned sandbox's docker/kind "
        "state (the same hygiene grain already runs between tasks).\n"
        "- reboot_controller: reboot the controller VM this session itself "
        "is running on -- a last resort; this ends your turn immediately, "
        "and interrupts any other task running concurrently on this "
        "controller too (grain's own stranded-work recovery picks it back "
        "up once the controller is back).\n"
        "None of these can rebuild a VM from scratch or re-run grain's own "
        "bootstrap -- this deployment's controller has no credential or "
        "network path to the host machine that would take, so that stays a "
        "human/operator action."
    )


def _prompt(issue: Issue, branch: str, workspace: str, comments: list[Comment] = (),
            *, task_repo: str = "", target_repo: str = "", agent_id_value: str = "",
            gemini_key: bool = False, gcp_key: bool = False, self_debug: bool = False,
            self_repair: bool = False) -> str:
    """`task_repo` is where the issue itself lives (the agent set's queue);
    `target_repo` is where the code is. Two different repos in the general
    case — the prompt says which is which, because "the repository" would
    otherwise be ambiguous to an agent that can see a task numbered against
    one repo and a checkout of another.

    `issue.body` arrives with its directive lines already stripped
    (`core.py` calls `directives.strip_directives`) — a `/repo` line is
    addressed to the orchestrator, not to the agent.

    `agent_id_value` is `agent_id()`'s output, generated once by `dispatch()`
    and passed through here rather than minted inline, so a test can pin it
    to a known value instead of parsing whatever `secrets.token_hex` picked.

    `gemini_key` is `dispatch()`'s own record of whether the task's
    `gemini_key_label` actually got a key minted (`core.py`'s
    `_dispatch`) — true only once a key genuinely landed in the sandbox, so
    the prompt never claims one exists when `configure_gemini_key` was
    never called. `self_debug` (bwsalmon/agents#62) is the same
    "true only once the label was actually seen" record, for
    `self_debug_label` -- `core.py`'s `_resolve_target` sets it straight
    from `issue.labels`, no minting step required. `gcp_key`
    (bwsalmon/agents#126) is the identical "true only once it actually
    landed" record for a GCP service-account key -- unlike `gemini_key`,
    never gated on a task label at all (see `gcp_keys.py`'s own docstring
    for why), so this is `core.py`'s record of whether
    `Orchestrator.gcp_key_config` was configured for this dispatch.

    bwsalmon/agents#79: the closing instructions ask for a real commit
    message because `core.py`'s `_finish_succeeded_issue` now builds the
    opened PR's body from the pushed branch's own head commit message
    (`GitHubClient.get_branch_head`) rather than from generic metadata --
    the previous body said nothing about the change itself, which is what
    left a number of generated PRs reading as description-free.
    """
    gemini_key_section = f"{_gemini_key_line()}\n\n" if gemini_key else ""
    gcp_key_section = f"{_gcp_key_line()}\n\n" if gcp_key else ""
    self_debug_section = f"{_self_debug_line()}\n\n" if self_debug else ""
    self_repair_section = f"{_self_repair_line()}\n\n" if self_repair else ""
    return (
        f"You are working {task_repo}#{issue.number}: {issue.title}\n\n"
        f"{issue.body}\n\n"
        f"Issue URL: {issue.html_url}\n\n"
        "Conversation on this issue so far (a prior attempt may have asked "
        "a question here, and a human may have already answered it):\n\n"
        f"{_conversation_section(list(comments))}\n\n"
        f"The task above is filed in {task_repo}, which is only the queue "
        f"this work was assigned from. The code you are changing is "
        f"{target_repo}: a clone of it is already checked out at "
        f"{workspace} in your assigned sandbox, with its git remote already "
        "configured — do your work there, using the tools available to "
        "you.\n\n"
        f"{_agent_id_line(agent_id_value)}\n\n"
        f"{gemini_key_section}"
        f"{gcp_key_section}"
        f"{self_debug_section}"
        f"{self_repair_section}"
        "When you are done, commit your changes with a clear, descriptive "
        "commit message -- a short summary line, then a blank line, then a "
        "paragraph explaining what changed and why. Your final commit "
        "message becomes the pull request's description verbatim, so write "
        "it for a human reviewer, not just for git log. Then push with "
        "exactly this command:\n"
        f"    git push origin HEAD:{branch}\n"
        "The controller opens the pull request itself once it sees that "
        "branch — you have no GitHub API access from here, so do not "
        "attempt to open a PR or comment directly.\n\n"
        "If this task doesn't need a code change at all -- it only asked "
        "for an answer, an investigation, or a recommendation -- call the "
        "comment_on_issue tool with your findings instead of pushing a "
        "branch. That posts your findings as a comment on this issue. If "
        "you do push commits, a pull request is opened for them "
        "regardless of whether you also call comment_on_issue -- it never "
        "prevents a pull request from opening on its own; it only stands "
        "in for one when you never pushed anything.\n\n"
        "If you are genuinely blocked and need the human's input, use the "
        "ask_question tool instead."
    )


def _format_review_comment(comment: ReviewComment) -> str:
    location = f"{comment.path}:{comment.line}" if comment.line is not None else comment.path
    return f"- {comment.user} on {location}:\n  {comment.body}"


def _pr_prompt(pr: PullRequestDetail, comments: list[ReviewComment], workspace: str,
               thread_comments: list[Comment] = (), *, task_repo: str = "",
               target_repo: str = "", task_issue: int | None = None,
               agent_id_value: str = "", gemini_key: bool = False,
               gcp_key: bool = False, self_debug: bool = False,
               self_repair: bool = False) -> str:
    """A PR-continuation dispatch. The PR lives in `target_repo`; the task
    that asked for the work — and the conversation a human is having about
    it — lives in `task_repo`, as issue `task_issue` (a `/pr` directive on
    that issue is what makes this a PR dispatch at all, see
    `directives.py`).

    `agent_id_value` is `_prompt`'s own parameter of the same name — see its
    docstring. `gemini_key`, `gcp_key`, and `self_debug` are likewise
    `_prompt`'s own parameters of the same names, unchanged.
    """
    feedback = (
        "\n\n".join(_format_review_comment(c) for c in comments)
        if comments else "(no inline review comments)"
    )
    task_line = (
        f"This work was assigned by {task_repo}#{task_issue}, the task queue "
        f"entry for it — a different repository from the one you are "
        f"changing, and where any conversation with a human happens.\n\n"
        if task_issue is not None else ""
    )
    gemini_key_section = f"{_gemini_key_line()}\n\n" if gemini_key else ""
    gcp_key_section = f"{_gcp_key_line()}\n\n" if gcp_key else ""
    self_debug_section = f"{_self_debug_line()}\n\n" if self_debug else ""
    self_repair_section = f"{_self_repair_line()}\n\n" if self_repair else ""
    return (
        f"You are continuing existing work on pull request "
        f"{target_repo}#{pr.number}: {pr.title}\n\n"
        f"{pr.body}\n\n"
        f"PR URL: {pr.html_url}\n\n"
        f"{task_line}"
        f"A clone of {target_repo} is already checked out at "
        f"{workspace} in your assigned sandbox, on the PR's existing branch "
        f"({pr.head_ref!r}) with its git remote already configured. This is "
        "NOT a fresh task: the branch already has commits and review "
        "history behind it — your job is to continue that work (address "
        "the feedback below, fix CI, finish what was started), not to "
        "start over or open a competing branch.\n\n"
        f"{_agent_id_line(agent_id_value)}\n\n"
        f"{gemini_key_section}"
        f"{gcp_key_section}"
        f"{self_debug_section}"
        f"{self_repair_section}"
        "Review feedback on this pull request so far:\n\n"
        f"{feedback}\n\n"
        "Conversation on this pull request so far (a prior attempt may have "
        "asked a question here, and a human may have already answered it):\n\n"
        f"{_conversation_section(list(thread_comments))}\n\n"
        "When you are done, commit your changes and push them with exactly "
        "this command:\n"
        f"    git push origin HEAD:{pr.head_ref}\n"
        "The controller is already tracking this pull request — you have no "
        "GitHub API access from here, so do not attempt to comment on or "
        "modify the PR directly."
    )


def _review_prompt(pr: PullRequestDetail, workspace: str, *, task_repo: str = "",
                    target_repo: str = "", task_issue: int | None = None,
                    agent_id_value: str = "", gemini_key: bool = False,
                    gcp_key: bool = False, self_debug: bool = False,
                    self_repair: bool = False) -> str:
    """A `/review`-directed dispatch (bwsalmon/agents#154): unlike
    `_pr_prompt`, the job is to *read* the PR's branch and leave feedback,
    never to push commits to it -- so this omits every "commit and push"
    instruction `_prompt`/`_pr_prompt` end on, and tells the agent about
    `add_review_comment` instead. `task_issue`/`agent_id_value`/
    `gemini_key`/`gcp_key`/`self_debug`/`self_repair` are all `_pr_prompt`'s
    own parameters of the same names, unchanged.
    """
    task_line = (
        f"This review was requested by {task_repo}#{task_issue}, the task "
        f"queue entry for it — a different repository from the one you are "
        f"reading, and where any conversation with a human happens.\n\n"
        if task_issue is not None else ""
    )
    gemini_key_section = f"{_gemini_key_line()}\n\n" if gemini_key else ""
    gcp_key_section = f"{_gcp_key_line()}\n\n" if gcp_key else ""
    self_debug_section = f"{_self_debug_line()}\n\n" if self_debug else ""
    self_repair_section = f"{_self_repair_line()}\n\n" if self_repair else ""
    return (
        f"You are reviewing pull request {target_repo}#{pr.number}: {pr.title}\n\n"
        f"{pr.body}\n\n"
        f"PR URL: {pr.html_url}\n\n"
        f"{task_line}"
        f"A clone of {target_repo} is already checked out at {workspace} in "
        f"your assigned sandbox, on the PR's branch ({pr.head_ref!r}), with "
        f"its git remote already configured. This is a REVIEW task, not a "
        f"fix -- read the changes this PR makes against its base branch "
        f"({pr.base_ref!r}; `git diff origin/{pr.base_ref}...HEAD` from the "
        "workspace shows exactly what it changes) and form an opinion of "
        "them. Do not push any commits, and do not modify the checkout as "
        "a way of demonstrating a fix -- if you see something worth "
        "changing, describe it as review feedback instead.\n\n"
        f"{_agent_id_line(agent_id_value)}\n\n"
        f"{gemini_key_section}"
        f"{gcp_key_section}"
        f"{self_debug_section}"
        f"{self_repair_section}"
        "Use the add_review_comment tool to leave feedback -- once per "
        "point: give path and line to attach a comment to a specific line "
        "of a specific file in the diff, or omit both for a general "
        "remark. Call it as many times as you have things to say, "
        "including zero times if you looked and genuinely have no "
        "feedback to leave. Everything you leave this way is collected "
        "into a single draft review on the pull request once you finish -- "
        "nothing is posted immediately, and nothing is visible to anyone "
        "until a human opens the draft and submits it themselves. You have "
        "no GitHub API access from here, so do not attempt to comment on "
        "or review the PR directly."
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


def configure_gemini_key(runner: Runner, key: str) -> None:
    """Writes a freshly minted Gemini API key (`core.py`'s `_dispatch`,
    via `gemini_keys.create_key`) to the sandbox at `GEMINI_KEY_PATH`, over
    the same stdin-not-argv channel `configure_git_credentials` already
    uses for the git-proxy token -- the raw key never becomes a
    shell-interpolated argument anywhere in this path, and never appears in
    the prompt file either (`_gemini_key_line` below only ever names the
    *path*, not the value).
    """
    runner.run(["dd", f"of={GEMINI_KEY_PATH}", "status=none"], stdin=key)
    runner.run(["chmod", "600", GEMINI_KEY_PATH])


def configure_gcp_key(runner: Runner, key_json: str) -> None:
    """Writes a freshly minted GCP service-account key (`core.py`'s
    `_dispatch`, via `gcp_keys.create_key`) to the sandbox at
    `GCP_KEY_PATH`, over the same stdin-not-argv channel
    `configure_gemini_key` already uses -- the raw key material never
    becomes a shell-interpolated argument anywhere in this path, and never
    appears in the prompt file either (`_gcp_key_line` only ever names the
    *path*, not the key itself).
    """
    runner.run(["dd", f"of={GCP_KEY_PATH}", "status=none"], stdin=key_json)
    runner.run(["chmod", "600", GCP_KEY_PATH])


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
    # `git clean` cannot remove a file whose *parent directory* the sandbox
    # user does not own, and a previous task that ran anything as root --
    # a `sudo` invocation, a container writing into the workspace -- leaves
    # exactly that behind. Found live: a root-owned `__pycache__` made
    # `clean` exit 1, and since this script is `set -eu` that failed
    # `ensure_workspace` itself, so every subsequent dispatch to that
    # sandbox died at its first step. Nothing would ever have cleared it:
    # the state that breaks the reset is the state the reset exists to
    # remove.
    #
    # Deliberately not `sudo git clean`: running git as root inside a
    # user-owned repository trips git's own dubious-ownership guard, and
    # any file it did create would be one more thing the next reset cannot
    # remove. Re-cloning is what a sandbox's first dispatch does anyway,
    # and it reaches the same known-clean state from any corruption, not
    # just this one.
    #
    # Scoped to `clean` failing rather than wrapping the whole reset: a
    # `fetch` that fails is usually the network or the proxy, and throwing
    # the checkout away to re-clone over that same network would turn a
    # retryable blip into a slower one.
    lines.append(f"  if ! git -C {shlex.quote(path)} clean -fdx; then")
    lines.append(
        "    echo 'workspace could not be cleaned (files a previous task left"
        " root-owned?); re-cloning' >&2"
    )
    lines.append(f"    sudo rm -rf {shlex.quote(path)}")
    lines.append(f"    git clone {shlex.quote(remote_url)} {shlex.quote(path)}")
    lines.append("  fi")
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


def _mcp_config_json(target: SandboxTarget, question_path_value: str,
                      comment_path_value: str, review_path_value: str, task_unit: str, *,
                      self_debug: bool = False,
                      self_repair: bool = False) -> str:
    """The `--mcp-config` file `claude -p` loads on the controller —
    `mcp_server.py` is invoked as its own process, per dispatch, with the
    assigned sandbox's connection details baked into its argv here. Nothing
    in a tool call itself can ever change this: the agent only ever sees
    tool names and their declared parameters (`command`, `file_path`, ...),
    never `target` — this JSON is the only place the sandbox gets named.
    `question_path_value` is the same "only place it's named" treatment
    for `ask_question` (docs/roadmap.md item 12): the agent supplies only
    the question text; where it lands is decided here, not by the tool
    call. `comment_path_value` is the identical treatment for
    `comment_on_issue` (bwsalmon/agents#50, bwsalmon/agents#89).
    `review_path_value` is the same again for `add_review_comment`
    (bwsalmon/agents#154).
    `self_debug` (bwsalmon/agents#62) is `dispatch()`'s own record of
    whether this task's issue carried `self_debug_label` -- true only then
    does `mcp_server.py` get `--self-debug`, which is what makes it
    advertise and answer `read_grain_logs` at all. `task_unit`
    (bwsalmon/agents#97) is `_start_task`'s own `unit_name(sandbox)` --
    always passed, not gated on `self_debug` like the flag above, since
    it's just a systemd unit name (no more sensitive than `--workspace`)
    and `read_grain_logs`'s `grain-task` case is what actually gates on
    self-debug, the same way `--workspace` is always passed despite
    `read_file`/`write_file` having nothing to do with self-debug either.
    """
    args = [
        "-m", "grain.automation.mcp_server",
        "--address", target.address,
        "--user", target.ssh_user,
        "--key-path", target.ssh_key_path,
        "--workspace", target.workspace,
        "--question-path", question_path_value,
        "--comment-path", comment_path_value,
        "--review-path", review_path_value,
        "--task-unit", task_unit,
    ]
    if self_debug:
        args.append("--self-debug")
    if self_repair:
        args.append("--self-repair")
    return json.dumps({
        "mcpServers": {
            "grain-sandbox": {
                "command": "python3",
                "args": args,
            },
        },
    })


# The only native (non-MCP) tool kept enabled -- confirmed safe live
# (docs/roadmap.md item 8's "Update"): a Task-spawned subagent inherits
# this same restricted roster, not a bypass to it (an explicit system
# denial confirmed this, not just self-report). Named on `--tools` itself,
# not just `--allowedTools` -- found live: `--tools ''` excludes every
# native tool from the registry outright, and naming one in
# `--allowedTools` alone does not add it back (that flag only pre-approves
# tools already in the registry). `--tools 'Task'` is what actually admits
# it alongside whatever `--mcp-config` supplies.
#
# `TodoWrite` was tried alongside it and dropped: confirmed live (twice --
# a real dispatch and an isolated local check) that `--tools` cannot admit
# it in `-p`/headless mode at all, regardless of syntax. Most likely
# excluded from `-p` mode as a product decision -- a todo list is a visible,
# ongoing tracker meant for an interactive session, which a one-shot
# headless dispatch never has.
_NATIVE_TOOLS = "Task"

_ALLOWED_TOOLS = (
    "mcp__grain-sandbox__run_command,mcp__grain-sandbox__read_file,"
    "mcp__grain-sandbox__edit_file,mcp__grain-sandbox__write_file,"
    "mcp__grain-sandbox__ask_question,mcp__grain-sandbox__comment_on_issue,"
    "mcp__grain-sandbox__add_review_comment,"
    # bwsalmon/agents#62/#86: pre-approved unconditionally, same as every
    # other tool name here -- harmless on a task that never turns any of
    # these on, since `mcp_server.py` only advertises (and answers) this
    # whole self-debug roster when started with `--self-debug`, which
    # `_mcp_config_json` only ever adds when the task issue actually
    # carried `self_debug_label`.
    "mcp__grain-sandbox__read_grain_logs,"
    "mcp__grain-sandbox__check_grain_health,"
    "mcp__grain-sandbox__read_grain_config,"
    "mcp__grain-sandbox__read_automation_audit_log,"
    # bwsalmon/agents#99: same unconditional pre-approval as the four
    # self-debug tools above, for the same reason -- `mcp_server.py` only
    # advertises (and answers) this roster when started with
    # `--self-repair`, which `_mcp_config_json` only adds for a task whose
    # issue carried `self_repair_label`.
    "mcp__grain-sandbox__restart_grain_service,"
    "mcp__grain-sandbox__reboot_sandbox,"
    "mcp__grain-sandbox__reformat_sandbox,"
    "mcp__grain-sandbox__reboot_controller,"
    f"{_NATIVE_TOOLS}"
)


def _start_task(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
                 target: SandboxTarget, prompt: str, *, remote_url: str, token: str,
                 branch: str | None = None, gemini_key: str | None = None,
                 gcp_key: str | None = None,
                 self_debug: bool = False, self_repair: bool = False) -> str:
    """The common tail of both `dispatch()` and `dispatch_pr()`. Two
    runners now, not one (docs/roadmap.md item 8's "Update"): `sandbox_runner`
    still prepares the workspace and credentials on the sandbox itself,
    exactly as before; `controller_runner` is new and does everything that
    now happens on the controller instead — writing the prompt and MCP
    config, and starting the unit. `branch` unset gets the fresh-issue
    workspace (default branch); `branch` given gets `ensure_workspace`'s
    branch-reset path (docs/roadmap.md item 9) instead — either
    `dispatch_pr`'s PR-continuation branch, or `dispatch`'s own resolved
    `/base` — see its docstring.

    `gemini_key` (bwsalmon/agents#47) is the raw key string `core.py`'s
    `_dispatch` already minted via `gemini_keys.create_key`, or `None` for
    every task that didn't ask for one — this function never mints or
    revokes a key itself, only places one that already exists. `gcp_key`
    (bwsalmon/agents#126) is the identical shape for a GCP service-account
    key, the raw JSON key file content `core.py`'s `_dispatch` already
    minted via `gcp_keys.create_key`, or `None` for a deployment that
    never configured `Orchestrator.gcp_key_config`. `self_debug`
    (bwsalmon/agents#62) is `dispatch()`'s own record of whether the task
    issue carried `self_debug_label` — threaded straight through to
    `_mcp_config_json`, the only place it has any effect.
    """
    configure_git_credentials(sandbox_runner, remote_url, token)
    ensure_workspace(sandbox_runner, remote_url, target.workspace, branch=branch)
    if gemini_key is not None:
        configure_gemini_key(sandbox_runner, gemini_key)
    if gcp_key is not None:
        configure_gcp_key(sandbox_runner, gcp_key)

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
    q_path = question_path(unit)
    # A fixed path, reused across every dispatch to this sandbox
    # (docs/roadmap.md item 12) -- reset unconditionally so a question from
    # an earlier, unrelated task can never be misread as belonging to this
    # one. `mcp_server.py`'s `ask_question` tool (running as
    # CONTROLLER_AGENT_USER, same as the rest of this unit) can create it
    # fresh on its own; nothing here needs to pre-create or chown it.
    controller_runner.run(["sudo", "rm", "-f", q_path])
    c_path = comment_path(unit)
    # Same reset discipline as q_path above, for the same reason
    # (bwsalmon/agents#50): a leftover comment from an earlier dispatch to
    # this sandbox must never be misread as belonging to this one.
    controller_runner.run(["sudo", "rm", "-f", c_path])
    r_path = review_path(unit)
    # Same reset discipline again, for the same reason (bwsalmon/agents#154):
    # leftover review comments from an earlier dispatch to this sandbox must
    # never be posted as part of this one's review.
    controller_runner.run(["sudo", "rm", "-f", r_path])
    m_path = _mcp_config_path(unit)
    controller_runner.run(
        ["sudo", "dd", f"of={m_path}", "status=none"],
        stdin=_mcp_config_json(target, q_path, c_path, r_path, unit,
                                self_debug=self_debug,
                                self_repair=self_repair),
    )
    out_path = transcript_path(unit)
    start_unit(
        controller_runner, unit,
        # The token is read from CONTROLLER_AGENT_TOKEN_PATH into this
        # process's own environment at runtime, not passed as a systemd-run
        # argument -- see that constant's own docstring for why (argv would
        # land in `ps` output).
        f"export CLAUDE_CODE_OAUTH_TOKEN=\"$(cat {shlex.quote(CONTROLLER_AGENT_TOKEN_PATH)})\" && "
        f"cd /opt/grain && claude -p "
        f"--tools {shlex.quote(_NATIVE_TOOLS)} "
        f"--mcp-config {shlex.quote(m_path)} --strict-mcp-config "
        f"--allowedTools {shlex.quote(_ALLOWED_TOOLS)} "
        f"--no-session-persistence "
        f"--output-format stream-json --verbose "
        f"< {shlex.quote(p_path)} > {shlex.quote(out_path)}",
        uid=CONTROLLER_AGENT_USER,
    )
    return unit


def dispatch(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
             target: SandboxTarget, issue: Issue, *, remote_url: str, token: str,
             base: str | None = None, comments: list[Comment] = (), task_repo: str = "",
             target_repo: str = "", gemini_key: str | None = None,
             gcp_key: str | None = None,
             self_debug: bool = False, self_repair: bool = False) -> str:
    """Starts an issue-triggered task. `sandbox_runner` prepares the
    workspace on the sandbox `target` describes; `controller_runner` starts
    `claude -p` on the controller, pointed at that same sandbox via
    `target`. Returns the unit name — the caller records it in
    `AutomationState` to poll later.

    `remote_url` is the git-proxy URL for the *target* repo — the one the
    task's `/repo` directive named (`directives.py`), not the task repo the
    issue itself lives in (`http://<controller>:<port>/<owner>/<repo>.git`) and `token` is this
    sandbox's own git-proxy bearer token (`grain/proxy/tokens.py`'s
    `SandboxTokenStore.ensure_token` mints it on first use) — both supplied
    by `core.py`, which is the only layer that knows the controller's
    address and holds the token store. `comments` (docs/roadmap.md item 12)
    is the issue's top-level conversation, from `GitHubClient.list_comments`
    — how a redispatch after a prior `ask_question` call sees the human's
    reply, since `AutomationState` itself carries no memory of that round
    trip once the assignment is released.

    `base` is the resolved PR base `core.py`'s `_resolve_target` already
    pinned onto this task (a `/base` directive, or the target repo's own
    default branch) — passed straight through to `ensure_workspace`'s own
    `branch` parameter so the agent's new branch is actually *built on top
    of* that base, not just opened against it later. Without this, a `/base`
    that differs from the target repo's real default branch only affected
    where `create_pull_request` (`core.py`'s `_finish_succeeded_issue`)
    opened the PR — the workspace itself always reset to `origin/HEAD`, so
    the agent's branch carried every commit the real default branch had
    that `base` didn't, polluting the PR's diff. `None` (a test-only shape;
    production always resolves and passes a real value) falls back to
    `ensure_workspace`'s plain `origin/HEAD` default-branch reset, exactly
    as before this parameter existed.

    `gemini_key` (bwsalmon/agents#47) is the raw key string `core.py`'s
    `_dispatch` minted for this task, or `None` for the common case of no
    `gemini_key_label` on the task issue — threaded through to both
    `_start_task` (which places it in the sandbox) and the prompt (which
    tells the agent it's there, only when it actually is). `gcp_key`
    (bwsalmon/agents#126) is the identical shape for a GCP service-account
    key, minted unconditionally (no task label involved) whenever
    `core.py`'s `Orchestrator.gcp_key_config` is set — `None` for a
    deployment that never configured it. `self_debug` (bwsalmon/agents#62)
    is `core.py`'s own record of whether the task issue carried
    `self_debug_label` — threaded through to `_start_task` (which turns on
    `mcp_server.py`'s `read_grain_logs` tool) and the prompt (which tells
    the agent about it, only when it's on).
    `self_repair` (bwsalmon/agents#99) is the identical treatment for
    `self_repair_label` — a separate label/flag from `self_debug`, turning
    on the mutating `restart_grain_service`/`reboot_sandbox`/
    `reformat_sandbox`/`reboot_controller` roster instead of the read-only
    one.
    """
    push_branch = branch_name(issue.number)
    return _start_task(
        sandbox_runner, controller_runner, sandbox, target,
        _prompt(issue, push_branch, target.workspace, comments,
                task_repo=task_repo, target_repo=target_repo,
                agent_id_value=agent_id(), gemini_key=gemini_key is not None,
                gcp_key=gcp_key is not None,
                self_debug=self_debug, self_repair=self_repair),
        remote_url=remote_url, token=token, branch=base, gemini_key=gemini_key,
        gcp_key=gcp_key, self_debug=self_debug, self_repair=self_repair,
    )


def dispatch_pr(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
                 target: SandboxTarget, pr: PullRequestDetail,
                 comments: list[ReviewComment], *, remote_url: str, token: str,
                 thread_comments: list[Comment] = (), task_repo: str = "",
                 target_repo: str = "", task_issue: int | None = None,
                 gemini_key: str | None = None, gcp_key: str | None = None,
                 self_debug: bool = False, self_repair: bool = False) -> str:
    """Starts a PR-triggered task (docs/roadmap.md item 9): same mechanism as
    `dispatch()`, but the workspace lands on the PR's *own* existing branch
    (`pr.head_ref`) instead of the default branch, and the prompt carries the
    PR's title/body/review-comments instead of an issue's title/body — see
    `_pr_prompt` and `ensure_workspace`'s `branch` parameter.

    `pr` and `comments` come from `GitHubClient.get_pull_request` and
    `list_review_comments` respectively, both read against the *target*
    repo — `core.py` reads both before calling this, same division of labour as `dispatch()`
    (all GitHub API work stays on the controller; the sandbox only ever
    sees git, and only through the tools `mcp_server.py` exposes).
    `thread_comments` (docs/roadmap.md item 12) is the *task issue's*
    top-level conversation, from `GitHubClient.list_comments` — distinct
    from `comments`'s inline review comments on the PR itself, and where a
    human's reply to a prior `ask_question` call actually lands, since the
    task repo is the only repo this deployment ever comments on.

    `gemini_key` (bwsalmon/agents#47) is `dispatch()`'s own parameter of the
    same name — see its docstring; `gemini_key_label` on the task issue
    works identically for a PR-continuation dispatch. `gcp_key`
    (bwsalmon/agents#126) is likewise `dispatch()`'s own parameter of the
    same name, unchanged. `self_debug` (bwsalmon/agents#62) is likewise
    `dispatch()`'s own parameter of the same name, unchanged. `self_repair`
    (bwsalmon/agents#99) is likewise `dispatch()`'s own parameter of the
    same name, unchanged.
    """
    return _start_task(
        sandbox_runner, controller_runner, sandbox, target,
        _pr_prompt(pr, comments, target.workspace, thread_comments,
                   task_repo=task_repo, target_repo=target_repo,
                   task_issue=task_issue, agent_id_value=agent_id(),
                   gemini_key=gemini_key is not None, gcp_key=gcp_key is not None,
                   self_debug=self_debug, self_repair=self_repair),
        remote_url=remote_url, token=token, branch=pr.head_ref, gemini_key=gemini_key,
        gcp_key=gcp_key, self_debug=self_debug, self_repair=self_repair,
    )


def dispatch_review(sandbox_runner: Runner, controller_runner: Runner, sandbox: str,
                     target: SandboxTarget, pr: PullRequestDetail, *, remote_url: str,
                     token: str, task_repo: str = "", target_repo: str = "",
                     task_issue: int | None = None, gemini_key: str | None = None,
                     gcp_key: str | None = None, self_debug: bool = False,
                     self_repair: bool = False) -> str:
    """Starts a `/review`-directed dispatch (bwsalmon/agents#154): the same
    mechanism `dispatch_pr()` is, on the same branch (`pr.head_ref`), but
    with `_review_prompt` instead of `_pr_prompt` -- the agent is asked to
    read the PR and leave feedback via `add_review_comment`, never to push
    to it. Unlike `dispatch_pr`, this takes no inline review comments or
    thread comments to render into the prompt: a review dispatch reads the
    PR fresh rather than responding to feedback already on it, so there is
    no prior conversation to show it here (`task_repo`/`task_issue` are
    still passed through, for the one line of context naming which task
    asked for this review).

    Every other parameter is `dispatch_pr()`'s own parameter of the same
    name, unchanged.
    """
    return _start_task(
        sandbox_runner, controller_runner, sandbox, target,
        _review_prompt(pr, target.workspace, task_repo=task_repo, target_repo=target_repo,
                        task_issue=task_issue, agent_id_value=agent_id(),
                        gemini_key=gemini_key is not None, gcp_key=gcp_key is not None,
                        self_debug=self_debug, self_repair=self_repair),
        remote_url=remote_url, token=token, branch=pr.head_ref, gemini_key=gemini_key,
        gcp_key=gcp_key, self_debug=self_debug, self_repair=self_repair,
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
