"""The orchestrator's decision logic for one `run-once` invocation.

Order, mirroring `grain/proxy/core.py`'s own "order matters and mirrors
docs/design.md" convention:

    sweep first, so a sandbox a finished or stranded run just freed is
    available to the same cycle's dispatch pass rather than sitting idle
    for one more `run-once` interval — a *successful* sweep also verifies
    the pushed branch and opens the PR, since that is the other half of
    "this run is really done", not a separate pass; the same sweep also
    stops any in-progress unit whose task issue was closed on GitHub since
    dispatch (bwsalmon/agents#82), reported back as "cancelled" rather than
    requeued,
    requeue any issue still carrying the in-progress label that this
    process's own state has no assignment for (bwsalmon/agents#139) — the
    fallback for a restart that lost the state file that named it, not
    just the crash-mid-run case a *surviving* state file already covers,
    poll every task issue with a PR still open for it (bwsalmon/agents#54)
    and close the ones whose PR has itself closed since,
    list open trigger-labelled issues in the *task repo* not already
    tracked as in-progress, oldest first, so a backlog drains in the order
    it was filed, resolving each one's target repo from its own text,
    while a free sandbox exists and the rate limit allows it, mint the
    sandbox's git-proxy token if it doesn't have one yet, dispatch, move the
    label, and record the assignment,
    stop — cron will call again.

Cron, not a loop: docs/design.md's issue-intake section is explicit that
polling (not webhooks) is what keeps the host closed to inbound traffic.
`Orchestrator.run_once` is meant to be invoked by a systemd timer, once per
call, not run as a daemon.

PR creation (docs/roadmap.md item 2) is a `_sweep`-side concern, not a new
pass: a sweep only calls a run "succeeded" once the unit exited zero, and
whether that success produced a real, mergeable branch is the next question
about the exact same event, checked with `GitHubClient.branch_exists` before
`create_pull_request` — never the agent's own claim about what it pushed,
since the prompt it received came from untrusted issue content
(docs/design.md's split surface). A success with no branch is requeued
through the same `_requeue` path as a failed or stranded run: `sweeper.py`
still knows nothing about GitHub, and a run that produced nothing usable is
not meaningfully different from one that failed outright.

Closing the task issue is *not* a `_sweep`-side concern, though (bwsalmon/agents#54):
opening a PR only proves the agent's own part is done, not that the task
itself is — that is true once a human has reviewed and merged (or decided to
close without merging) the PR it produced. `_finish_succeeded_issue` records
that PR against the issue (`state.py`'s `OpenPullRequest`) instead of closing
anything itself, and a dedicated poll, `_close_finished_prs`, checks every
such record each `run_once` and closes the ones whose PR itself now reads
`state == "closed"`. `completed_label` goes on immediately either way — it
marks the agent's own contribution as finished, independent of whether the
issue itself ever auto-closes (a no-branch finish, `_finish_no_changes`,
gets the same label but is never auto-closed at all: bwsalmon/agents#54
asked for that explicitly, since it has no PR whose merge/close is a
natural "done" signal to wait on).

**One task repo, many target repos.** The repo polled above is the *task
repo* — a queue of issues for the agent set, not the code being changed.
Each task names its own target repo (and optionally a PR to continue, and a
PR base) with a `/repo`-style directive in its text; `directives.py` is the
parser and the full rationale. What follows from that split:

- **Only the task repo is ever polled, labelled, or commented on.** Labels
  and the whole question/reply cycle (docs/roadmap.md items 12–13) live on
  the task issue; target repos see git pushes, a `branch_exists` check and a
  `create_pull_request` call, and nothing else.
- **A target repo must be on the same allowlist the git proxy enforces**
  (`/data/config/repo-allowlist.json`, `grain/proxy/allowlist.py`). One
  operator-owned list, checked here at dispatch and again at every git
  operation, rather than two that can disagree. A task naming anything else
  never dispatches.
- **An unusable directive parks the task instead of failing it.** No
  `/repo` and no configured `default_target_repo`, a malformed one, a
  non-allow-listed or nonexistent target: `_park` posts a comment saying
  exactly what was wrong and swaps the trigger label for
  `awaiting_reply_label`, which is precisely the state item 13 already
  knows how to promote out of — a maintainer replies `/repo owner/name` and
  the next cycle picks it up. Leaving the trigger label on instead would
  redispatch the identical failure every two minutes with nothing new to
  act on, the same argument `_finish_question` makes for a question.
- **The target is recorded on the assignment, not re-derived at sweep
  time** (`state.py`'s `Assignment`): an issue body can be edited mid-run,
  and an edit must not be able to redirect where the finished work's PR is
  opened.
- **`/base` shapes the workspace itself, not just where the PR opens.**
  `_resolve_target` resolves `base` once (a `/base` directive, or else the
  target repo's own default branch) and `_dispatch` passes it straight into
  `dispatch()`'s own `base` parameter, which threads it through to
  `ensure_workspace`'s `branch` — the fresh sandbox checkout (and the
  agent's new branch) is built on top of the resolved base, the same way a
  PR-continuation dispatch already builds on `pr.head_ref`. Without this, a
  `/base` that differs from the real default branch only redirected
  `create_pull_request` at finish time while the agent still worked from
  the default branch the whole run, producing a PR diff polluted with
  every commit the default branch had that `base` didn't.

docs/roadmap.md item 9's second intake path — continue an *existing* PR
rather than start a fresh branch — survives the split as a `/pr N`
directive on a task issue, rather than a labelled PR in a second polled
repo (with a task repo, the PRs are in target repos, where no label of ours
lives). It reuses this same pool and rate limit rather than adding a second
budget — a PR-continuation run occupies a sandbox exactly like a fresh one,
and the whole point of `runs_per_hour` is capping total dispatch volume
regardless of what triggered it. What that leaves:

- **`AutomationState.Assignment` carries a `kind`.** An issue assignment's
  branch is always recomputable from `branch_name(issue)`; a PR assignment's
  branch is whatever the PR's author already called it, so it's recorded at
  dispatch time instead — see `state.py`'s `Assignment` docstring.
- **`_finish_succeeded` branches on `outcome.kind`.** A fresh-branch success
  means "open a new PR" in the target repo; a PR-continuation success means
  the PR already exists and the agent just pushed more commits to it —
  verified the same way (does the branch it was told to push to still
  exist?), but with no PR to create and no new label story: only the
  in-progress label comes off the task issue. Both branches route through
  the same `_requeue` on failure/no-branch, unchanged — a requeue only ever
  needs the trigger's own number, which is `outcome.issue` regardless of
  kind.
"""

from __future__ import annotations

import dataclasses
import json
import re
from dataclasses import dataclass
from datetime import datetime, timedelta
from pathlib import Path
from typing import Callable

from . import ratelimit
from .audit import AuditLog, NullAuditLog
from .config import AutomationConfig
from .directives import DirectiveError, RepoRef, parse_directives, strip_directives
from .dispatch import (
    CONTROLLER_AGENT_SSH_KEY_PATH, SandboxTarget, UnitState, branch_name, comment_path,
    dispatch, dispatch_pr, dispatch_review, proposed_tasks_path, question_path,
    review_path, unit_name,
)
from .gcp_keys import GcpKeyConfig
from .gcp_keys import create_key as create_gcp_key
from .gcp_keys import delete_expired_keys as delete_expired_gcp_keys
from .gcp_keys import delete_key as delete_gcp_key
from .gemini_keys import GeminiKeyConfig
from .gemini_keys import create_key as create_gemini_key
from .gemini_keys import delete_expired_keys as delete_expired_gemini_keys
from .gemini_keys import delete_key as delete_gemini_key
from .github import (
    Comment, GitHubClient, GitHubError, Issue, NewReviewComment, PullRequestDetail,
)
from .scratch_repo import ScratchRepoConfig, repo_for_sandbox
from .history import NullSessionHistory, SessionHistory
from .janitor import JanitorConfig, run_janitor
from .labels import agent_label
from .scheduled_jobs import ScheduledJobsConfig
from .ssh import SshRunner
from .state import AutomationState, OpenPullRequest, TriggerKind
from .sweeper import Outcome, sweep
from ..inventory import GIT_PROXY_PORT, Cluster
from ..proxy.allowlist import Allowlist
from ..proxy.credentials import CredentialSet
from ..proxy.tokens import SandboxCredentialStore, SandboxTokenStore
from ..run import CommandError, Runner

# The same trust tier "can apply a label" implies -- write access to the
# repo, in one of GitHub's own shapes for it (docs/roadmap.md item 13). A
# random public commenter (author_association "NONE"/"CONTRIBUTOR"/
# "FIRST_TIME_CONTRIBUTOR"/...) must not be able to redispatch the agent
# with content of their choosing just by replying to a question thread --
# that would reopen the exact prompt-injection gate the trigger label
# exists to close (docs/design.md's split surface).
_TRUSTED_REPLY_ASSOCIATIONS = frozenset({"OWNER", "MEMBER", "COLLABORATOR"})

# `/lgtm` on its own line approves a `needs_approval_label` issue
# (bwsalmon/agents#136) -- the comment-triggered equivalent of applying
# `trigger_label` by hand. Line-anchored like `directives.py`'s own
# `_DIRECTIVE_RE`, so a prose sentence that merely mentions "lgtm" doesn't
# count, but this isn't a directive itself: it carries no value, and it
# acts on a label rather than becoming part of a task's own configuration.
_LGTM_RE = re.compile(r"^\s*/lgtm\s*$", re.MULTILINE | re.IGNORECASE)

# A consistent, visible marker on everything grain-agent itself posts to
# GitHub (docs/roadmap.md item 14) -- so a comment or PR is immediately
# recognizable as automation output at a glance, regardless of which
# credential actually posted it (a personal PAT and a dedicated machine
# account otherwise look identical in a thread).
_AUTOMATION_SIGNATURE = "🤖 _Posted automatically by grain-agent — not a human._"


def _is_automation_comment(body: str | None) -> bool:
    """Whether `body` is something grain-agent posted itself, by the
    `_AUTOMATION_SIGNATURE` marker it stamps on every comment it writes.

    grain posts to the task thread as the same credential a maintainer
    would (that is the whole point of `_AUTOMATION_SIGNATURE`'s own
    "regardless of which credential actually posted it"), so GitHub
    reports its comments with `author_association` "OWNER" -- squarely
    inside `_TRUSTED_REPLY_ASSOCIATIONS`. Every place that reads "a
    trusted human said something new" therefore has to subtract grain's
    own voice first, or the automation answers its own questions and
    restarts its own finished tasks. Found live: `_suggest_fix` comments
    on a *completed* issue to announce the follow-up task it just filed,
    and `_restart_commented_completions` read that as a maintainer's
    follow-up and put `trigger_label` back on -- a task reopening itself
    with nobody having asked.

    The marker is checked per line, and lines quoted with ">" do not
    count: GitHub's own "Quote reply" button copies the comment being
    replied to, signature and all, into the new comment's body, and a
    maintainer using it is exactly the human reply these callers exist to
    notice. grain always writes the signature as a line of its own, so an
    exact match on a stripped, unquoted line is both sufficient and the
    tightest test available.
    """
    return any(
        stripped == _AUTOMATION_SIGNATURE
        for stripped in (line.strip() for line in (body or "").splitlines())
        if not stripped.startswith(">")
    )

# bwsalmon/agents#52: a task labelled `grain-github-<name>` uses the named
# credential `<name>` (`CredentialSet.get`) for every git push, in place of
# the owner/repo default -- for a task that needs a scope the default
# deliberately withholds (docs/design.md, "Scopes to withhold", e.g.
# `workflow`). A real GitHub label, not a body directive like `/gemini-key`:
# applying one already requires the same "can apply a label" trust tier the
# trigger gate itself relies on, so this opens no new gate.
_GITHUB_KEY_LABEL_RE = re.compile(r"^grain-github-(.+)$")

# GitHub's own `CheckRun.conclusion` values that `_pr_health` (bwsalmon/agents#83)
# reads as "this check is broken," not merely incomplete -- "cancelled",
# "skipped" and "neutral" all mean the check has nothing to say about
# whether the code is good, which isn't the same claim.
_FAILING_CHECK_CONCLUSIONS = frozenset({"failure", "timed_out", "action_required"})


@dataclass(frozen=True)
class _PrHealth:
    """What `_close_finished_prs` (bwsalmon/agents#83) needs to decide
    whether an open PR is in trouble (conflicts with its base, or a failing
    check) worth suggesting a fix for, or clean enough to auto-merge.
    `mergeable` is `PullRequestDetail.mergeable` straight through -- `None`
    while GitHub is still computing it, read as "don't know yet" by every
    caller here, never guessed at either way.
    """
    mergeable: bool | None
    checks_pending: bool
    failing_checks: tuple[str, ...]

    @property
    def has_conflict(self) -> bool:
        return self.mergeable is False

    @property
    def is_broken(self) -> bool:
        """A definite "something is wrong" signal -- what `_close_finished_prs`
        suggests a fix for. Pending checks don't count: a check still
        running may yet pass, and filing a fix for a task that was never
        actually broken would be pure noise.
        """
        return self.has_conflict or bool(self.failing_checks)

    @property
    def is_clean(self) -> bool:
        """A definite "safe to merge" signal -- what `_close_finished_prs`
        auto-merges on. Pending checks don't count here either: merging
        before a check has even finished running is the one thing
        `/auto-merge` must never do.
        """
        return self.mergeable is True and not self.checks_pending and not self.failing_checks


def _pending_question(sandbox: str) -> str | None:
    """Reads back an `ask_question` call, if the agent made one this run
    (docs/roadmap.md item 12) — a plain local file, same discipline as
    `capture.py`'s `capture_trajectory`: `claude -p` (and the MCP server
    child that wrote this) run on the controller now, so this is a direct
    read, no `Runner`/SSH involved, and absence just means "no question,"
    never an exception. `unit_name(sandbox)` is recomputed rather than
    threaded through `Outcome` — it's a pure function of the sandbox, the
    same shape `branch_name(issue)` already gets recomputed instead of
    carried around for an issue-kind assignment.
    """
    try:
        text = Path(question_path(unit_name(sandbox))).read_text().strip()
    except OSError:
        return None
    return text or None


def _pending_comment(sandbox: str) -> str | None:
    """Reads back a `comment_on_issue` call, if the agent made one this run
    (bwsalmon/agents#50) -- the same file-based handoff `_pending_question`
    already uses for `ask_question`: `mcp_server.py`'s tool and this read
    both run on the controller, so no `Runner`/SSH is involved, and absence
    just means "no comment to post," never an exception.

    bwsalmon/agents#89: this alone no longer decides whether a PR opens --
    see `_finish_succeeded_issue`, which now always checks the branch
    first and only falls back to this comment once that branch turns out
    to have nothing on it.
    """
    try:
        text = Path(comment_path(unit_name(sandbox))).read_text().strip()
    except OSError:
        return None
    return text or None


def _pending_review_comments(sandbox: str) -> list[dict]:
    """Reads back every `add_review_comment` call the agent made this run
    (bwsalmon/agents#154) -- the same file-based handoff `_pending_question`/
    `_pending_comment` already use, but a list rather than a single value:
    `mcp_server.py`'s tool appends, this reads the whole thing back at once.
    Absence (never written, or an empty/corrupt file) reads as "no
    comments," never an exception -- a review dispatch that genuinely found
    nothing to say is a normal outcome, not a broken one.
    """
    try:
        return json.loads(Path(review_path(unit_name(sandbox))).read_text())
    except (OSError, json.JSONDecodeError):
        return []


def _pending_proposed_tasks(sandbox: str) -> list[dict]:
    """Reads back every `propose_task` call the agent made this run
    (bwsalmon/agents#175) -- the same file-based handoff `_pending_review_comments`
    already uses: `mcp_server.py`'s tool appends, this reads the whole list
    back at once. Absence (never written, or an empty/corrupt file) reads
    as "nothing proposed," never an exception.
    """
    try:
        return json.loads(Path(proposed_tasks_path(unit_name(sandbox))).read_text())
    except (OSError, json.JSONDecodeError):
        return []


@dataclass(frozen=True)
class ResolvedTask:
    """What one task issue resolved to: the target repo, the PR being
    continued (None for a fresh branch), and the base a new PR should
    target. Produced by `_resolve_target` from the task's own directives
    and consumed once, at dispatch — everything here is then pinned onto
    the `Assignment`, so the sweep that finishes this run never re-reads
    the issue body to find out where the work went.
    """

    repo: RepoRef
    pr: PullRequestDetail | None
    base: str
    # Whether the task issue carried `config.gemini_key_label`
    # (bwsalmon/agents#47, #49) -- `_resolve_target` already refuses one
    # this deployment can't honour (no `GeminiKeyConfig` configured), so by
    # the time this reaches `_dispatch` it is always safe to act on.
    gemini_key: bool = False
    # bwsalmon/agents#62: whether the task issue carried
    # `config.self_debug_label`. Unlike `gemini_key` above this never needs
    # refusing -- no per-deployment config gates it -- so `_resolve_target`
    # sets it straight from `issue.labels`, unconditionally.
    self_debug: bool = False
    # bwsalmon/agents#99: whether the task issue carried
    # `config.self_repair_label` — the mutating counterpart to `self_debug`
    # above, and set the same unconditional way (no deployment config can
    # refuse it; the sudo grant it relies on, `provision/controller.sh`, is
    # unconditional on every deployment).
    self_repair: bool = False
    # The named credential a `grain-github-<name>` label asked for
    # (bwsalmon/agents#52), or `None` for the overwhelming common case of no
    # such label. `_resolve_target` already refuses a name this deployment
    # has no `<name>.token` for, so by the time this reaches `_dispatch` it
    # is always safe to act on.
    github_key: str | None = None
    # bwsalmon/agents#83: whether the task issue carried `/auto-merge`
    # (`directives.py`). Read straight off `Directives.auto_merge` -- there
    # is nothing to refuse the way `gemini_key`/`github_key` sometimes are,
    # since honouring it costs this deployment nothing it doesn't already
    # have (the same GitHub credential that opens a PR can merge one).
    auto_merge: bool = False
    # bwsalmon/agents#154: whether the task issue carried `/review`. Unlike
    # `auto_merge`, `_resolve_target` *does* refuse this one -- without a
    # `/pr` alongside it there is no branch to check out or PR to post the
    # draft review against, so `pr` above is always set whenever this is
    # true.
    review: bool = False
    # bwsalmon/agents#164: the issue numbers a `/depends` line named (in
    # the *task* repo), read straight off `Directives.depends`. Unlike
    # every other field here, `_dispatch` -- not `_resolve_target` -- is
    # what acts on this: whether the named issues are still open can
    # change cycle to cycle with nothing about the task itself changing,
    # so it isn't the kind of thing `_resolve_target` can resolve once and
    # be done with the way a repo or PR number is.
    depends: tuple[int, ...] = ()


@dataclass
class Orchestrator:
    cluster: Cluster
    github: GitHubClient
    config: AutomationConfig
    state: AutomationState
    base_runner: Runner
    # Where sandbox git-proxy tokens are minted and recorded — the same file
    # `grain/proxy/tokens.py`'s `SandboxTokens` reads on the proxy side. See
    # `_dispatch`'s use of `ensure_token`.
    token_store: SandboxTokenStore
    # Which target repos this deployment may dispatch into: the same
    # `/data/config/repo-allowlist.json` the git proxy enforces on every
    # fetch and push (`grain/proxy/allowlist.py`), read here so a task
    # naming an off-list repo is refused with an explanation instead of
    # dispatching and failing later as an opaque clone error. Required, not
    # optional with a permissive default -- a gate whose absence means
    # "allow everything" is the wrong shape for the one control standing
    # between an issue body and which repos this agent set can write to.
    allowlist: Allowlist
    audit: AuditLog | None = None
    # docs/roadmap.md item 10: where a finished sandbox's captured
    # trajectory gets recorded, keyed by trigger. None (production's
    # default when nothing is wired in) falls back to a no-op, same shape
    # as `audit` above -- `cli.py`'s `build_orchestrator` supplies a real
    # `FileSessionHistory` under /data/state/automation/sessions.
    history: SessionHistory | None = None
    # Overridable seam for tests: production leaves this None and gets a
    # real `SshRunner` per sandbox; a test can inject a lookup straight to
    # per-sandbox fakes without needing to match SshRunner's exact argv.
    ssh_runner_factory: Callable[[str], Runner] | None = None
    # bwsalmon/agents#47: the on/off switch for `gemini_key_label`. `None`
    # (production's default for a deployment that never ran `grain
    # controller configure --gemini-project-id ...`) makes `_resolve_target`
    # refuse the label with an explanation, the same "unusable request
    # parks the task" shape an unlisted `/repo` already gets — see
    # `gemini_keys.py`'s own docstring for why this lives on the controller
    # account, never a sandbox's.
    gemini_key_config: GeminiKeyConfig | None = None
    # bwsalmon/agents#52: the same `CredentialSet` `cli.py`'s
    # `build_orchestrator` already builds for `self.github`'s own
    # `TokenSource` -- reused here (not reloaded) so `_resolve_target` can
    # check whether a `grain-github-<name>` label names a credential this
    # deployment actually has, before ever dispatching. `None` (a test that
    # doesn't wire one) makes any such label refused outright, the same
    # "unusable directive parks the task" shape an unconfigured
    # `/gemini-key` already gets.
    credentials: CredentialSet | None = None
    # Where `_dispatch` records which sandbox should use which named
    # credential, and `sweeper.py`'s `_release` clears it once the task
    # ends -- see `SandboxCredentialStore`'s own docstring for the full
    # lifecycle. `None` alongside `credentials=None` above for the same
    # "feature not wired" reason.
    credential_store: SandboxCredentialStore | None = None
    # bwsalmon/agents#126: the on/off switch for minting a GCP service-
    # account key on every dispatch, replacing the old per-sandbox
    # `gce_metadata_server` broker (`metadata_launcher`, removed by this
    # same change) -- see `gcp_keys.py`'s own docstring for the full
    # design. `None` (production's default for a deployment that never ran
    # `grain controller configure --gcp-agent-service-account-email ...`,
    # or simply has no `/data/config/gcp-key.json` yet) makes `_dispatch`
    # skip minting one entirely, the same "feature not configured" shape
    # `gemini_key_config`/`credentials` above already have -- a deployment
    # with no GCP key config gets no GCP access in its sandboxes, not an
    # error.
    gcp_key_config: GcpKeyConfig | None = None
    # bwsalmon/agents#113: the on/off switch for the GCP janitor. `None`
    # (production's default for a deployment that never ran `grain
    # controller configure --janitor-ttl-hours ...`, or a test that doesn't
    # care) makes `_janitor` a no-op, the same "feature not configured"
    # shape `gemini_key_config`/`gcp_key_config` above already have. See
    # `janitor.py`'s own docstring for what it deletes and how it avoids
    # grain's own core infrastructure.
    janitor_config: JanitorConfig | None = None
    # bwsalmon/agents#163: the on/off switch for scheduled jobs -- `None`
    # (production's default for a deployment with no
    # `/data/config/scheduled-jobs/` directory) makes `_scheduled_jobs` a
    # no-op, the same "feature not configured" shape `janitor_config`
    # above already has. See `scheduled_jobs.py`'s own docstring for how a
    # job's own template file is authored and what it fires.
    scheduled_jobs_config: ScheduledJobsConfig | None = None
    # bwsalmon/agents#159: the on/off switch for `config.scratch_repo_label`.
    # `None` (production's default for a deployment that never ran `grain
    # controller configure --scratch-repo-owner ...`) makes `_resolve_target`
    # refuse the label with an explanation, the same "unusable request
    # parks the task" shape `gemini_key_config` already has. Carries no
    # credential of any kind -- bwsalmon/agents#186 moved scratch-repo auth
    # onto the same `credentials`/`CredentialSet` ladder every other repo
    # already uses, so there is nothing for `_dispatch` to mint here; see
    # `scratch_repo.py`'s own docstring for the full reasoning.
    scratch_repo_config: ScratchRepoConfig | None = None
    # bwsalmon/agents#51: where to persist `AutomationState` immediately
    # after each mutation, not just once at the very end of `run_once`
    # (`cli.py`'s `cmd_automation_run_once`, still done there too as a
    # final, redundant safety net). `None` is what every existing test
    # helper leaves this at -- those only ever assert against
    # `orchestrator.state` in memory, never against a file on disk, so
    # `_save_state` below is a no-op for them. Production (`cli.py`'s
    # `build_orchestrator`) always sets this to the real state path.
    #
    # Why this matters: a controller VM can be restarted (or recreated)
    # at any moment, including mid-`run_once` -- that is the whole premise
    # of the stranded-work sweeper (docs/design.md: "if the host is
    # stopped, or a run dies mid-flight"). Before this field existed, the
    # *only* place `AutomationState` hit disk was one `state.save()` call
    # after `run_once` returned in full. A crash between an in-memory
    # `state.assign()` in `_dispatch` and that final save was invisible to
    # every recovery path: the real GitHub side effect that follows it
    # (`remove_label` off the trigger label) had already landed, so the
    # issue would never again show up in `_dispatch`'s own poll (it no
    # longer carries the trigger label) *and* the freshly-dispatched
    # assignment naming it was never written to disk, so the sweeper could
    # never find it stranded either -- a task genuinely lost forever, with
    # no audit trail and no automatic recovery. Persisting right after each
    # `state.assign()`/`sweep()` call, before the GitHub side effect that
    # depends on it, closes that gap: whichever side of the crash the
    # process lands on, the *persisted* state is never behind a
    # already-committed GitHub side effect, so the next `run_once` either
    # sees the assignment (and the sweep's `UnitState.ABSENT` case reclaims
    # it as stranded) or never removed the trigger label to begin with.
    state_path: Path | None = None

    def __post_init__(self) -> None:
        if self.audit is None:
            self.audit = NullAuditLog()
        if self.history is None:
            self.history = NullSessionHistory()

    def _save_state(self) -> None:
        if self.state_path is not None:
            self.state.save(self.state_path)

    def _ssh_runner_for(self, sandbox: str) -> Runner:
        if self.ssh_runner_factory is not None:
            return self.ssh_runner_factory(sandbox)
        return SshRunner(
            inner=self.base_runner,
            user=self.config.ssh_user,
            address=self.cluster.address_of(sandbox),
            key_path=self.config.ssh_key_path,
        )

    def _remote_url(self, target: RepoRef) -> str:
        # Always through the git proxy, on the controller's own address —
        # never GitHub directly (docs/design.md, "GitHub access"). The proxy
        # routes on the path, so pointing a sandbox at a different target
        # repo needs nothing but a different URL here; what it may actually
        # reach is decided by the allowlist, on both sides.
        return (
            f"http://{self.cluster.controller_ip}:{GIT_PROXY_PORT}/"
            f"{target.owner}/{target.name}.git"
        )

    @property
    def _task(self) -> RepoRef:
        """The task repo — every label, comment and issue read in this
        module is against this one repo, and nothing else ever is.
        """
        return RepoRef(self.config.task_owner, self.config.task_repo)

    def _target_of(self, outcome: Outcome) -> RepoRef:
        """The target repo a finished run was working in. Falls back to the
        task repo for an assignment written before the task/target split,
        when a deployment had exactly one repo and that is precisely what
        this meant (see `state.py`'s `Assignment.target_owner`).
        """
        if outcome.target_owner and outcome.target_repo:
            return RepoRef(outcome.target_owner, outcome.target_repo)
        return self._task

    def run_once(self, now: datetime) -> None:
        self._sweep(now)
        self._janitor(now)
        self._refresh_agent_labels()
        self._restart_orphaned_in_progress()
        self._promote_answered_questions(now)
        self._restart_commented_completions()
        self._promote_lgtm_comments()
        self._close_finished_prs()
        self._scheduled_jobs(now)
        self._dispatch(now)

    # --- sweep --------------------------------------------------------
    def _is_issue_closed(self, number: int) -> bool:
        """`sweeper.py`'s hook for "has this task issue been closed on
        GitHub" (bwsalmon/agents#82) — called only for a unit `sweep()`
        would otherwise leave running untouched, so this fires at most
        once per still-active assignment per cycle, not once per
        in-progress issue regardless of status. Checked fresh every call
        rather than cached, the same "poll, don't trust a stale copy" bar
        `_close_finished_prs`'s own per-cycle PR-state check already holds
        to — there is no webhook to react to a close with (docs/design.md's
        cron-not-webhooks stance).

        A 404 means the task issue named by this assignment is gone from
        the currently configured repo entirely — read as "not closed"
        (there is nothing this method can act on), the same tolerance
        `_requeue` already extends to that exact stale-assignment case.
        """
        try:
            issue = self.github.get_issue(
                self.config.task_owner, self.config.task_repo, number
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            return False
        return issue.state == "closed"

    def _sweep(self, now: datetime) -> None:
        result = sweep(self.state, self._ssh_runner_for, self.base_runner,
                        self.config, now, history=self.history,
                        gemini_key_config=self.gemini_key_config,
                        gcp_key_config=self.gcp_key_config,
                        credential_store=self.credential_store,
                        is_issue_closed=self._is_issue_closed)
        # `sweep()` already called `state.release()` in memory for every
        # outcome above -- persist that now, before any of the GitHub calls
        # below (a PR, a label move) make this run's outcome irreversible.
        # See `state_path`'s own docstring (bwsalmon/agents#51) for why this
        # ordering, not just the fact of saving, is what actually closes the
        # crash gap.
        self._save_state()
        for outcome in result.succeeded:
            self._finish_succeeded(outcome)
        for outcome in (*result.failed, *result.stranded):
            reason = "failed" if outcome in result.failed else "stranded"
            self._requeue(outcome, reason)
        for outcome in result.cancelled:
            self._finish_cancelled(outcome)
        for warning in result.health_warnings:
            # Visibility only (docs/roadmap.md item 5) — a health problem
            # doesn't change the sandbox's dispatch eligibility, see
            # sweeper.py's own docstring for why not. This is the same
            # audit trail an operator already checks for dispatch/sweep
            # outcomes, so a sandbox quietly degrading shows up in the one
            # place already worth reading after an unexpected run.
            self.audit.record(sandbox=warning.sandbox, issue=None,
                               outcome=f"health warning: {warning.detail}")
        for warning in result.credential_warnings:
            # Same visibility-only treatment (bwsalmon/agents#47): a failed
            # Gemini key revocation doesn't gate the sandbox's slot from
            # freeing (sweeper.py's `_release` already logged it and moved
            # on) — this just makes sure an operator can see a key that may
            # still be live and needs revoking by hand.
            self.audit.record(sandbox=warning.sandbox, issue=None,
                               outcome=f"credential warning: {warning.detail}")
        self._reap_expired_gcp_keys(now)
        self._reap_expired_gemini_keys(now)

    def _reap_expired_gcp_keys(self, now: datetime) -> None:
        """The safety-net half of bwsalmon/agents#126's "24-hour expiry":
        `gcp_keys.py`'s own docstring explains why GCP itself enforces no
        such thing for a user-managed service-account key, so this deletes
        any key under `gcp_key_config.service_account_email` older than
        `gcp_key_config.max_key_age_hours`, once per `run_once` cycle,
        independent of `AutomationState` entirely -- it still catches an
        orphaned key even if the assignment that minted it was lost (a
        controller crash between mint and `state.assign`, most notably).

        A no-op when `gcp_key_config` is unset, the same "feature not
        configured" shape every other call site here already has.
        Best-effort: a `CommandError` (Google's API unreachable) is a
        visibility-only audit line, the same "surface it, don't gate on
        it" treatment `_sweep`'s own health/credential warnings already
        get -- there is no sandbox slot for this to block from freeing.
        """
        if self.gcp_key_config is None:
            return
        try:
            deleted = delete_expired_gcp_keys(self.base_runner, self.gcp_key_config, now=now)
        except CommandError as exc:
            self.audit.record(
                sandbox=None, issue=None,
                outcome=f"GCP service-account key reap failed: {exc}",
            )
            return
        for key_id in deleted:
            self.audit.record(
                sandbox=None, issue=None,
                outcome=f"reaped expired GCP service-account key {key_id}",
            )

    def _reap_expired_gemini_keys(self, now: datetime) -> None:
        """The same safety net `_reap_expired_gcp_keys` above is, for the
        Gemini API keys `gemini_keys.py` mints (bwsalmon/agents#131).

        These used to have no reap of their own: a stranded key was only
        ever cleaned up by the janitor, a separate opt-in feature, so a
        deployment running `enable_gemini_key` without `enable_janitor`
        leaked them indefinitely -- while an agent key in the same
        position was always reaped. Expiry now belongs to the feature
        that mints the key, for both kinds.

        Same no-op-when-unconfigured and visibility-only error handling as
        its agent-key counterpart; `delete_expired_keys` itself is what
        makes sure only grain-minted keys are ever in scope.
        """
        if self.gemini_key_config is None:
            return
        try:
            deleted = delete_expired_gemini_keys(
                self.base_runner, self.gemini_key_config, now=now,
            )
        except CommandError as exc:
            self.audit.record(
                sandbox=None, issue=None,
                outcome=f"Gemini API key reap failed: {exc}",
            )
            return
        for name in deleted:
            self.audit.record(
                sandbox=None, issue=None,
                outcome=f"reaped expired Gemini API key {name}",
            )

    # --- janitor (bwsalmon/agents#113) ---------------------------------
    def _janitor(self, now: datetime) -> None:
        """No-op when `janitor_config` is unset (production's default for
        a deployment that never enabled it). Run after `_sweep` so a key
        `_sweep` just revoked for a freed sandbox is already gone from
        `self.state.assignments` by the time `protected_gemini_key_names`
        below is built — not that it would matter either way, since a
        revoked key is no longer live in the project regardless of what
        this set contains.
        """
        if self.janitor_config is None:
            return
        protected_gemini_key_names = frozenset(
            assignment.gemini_key_name
            for assignment in self.state.assignments.values()
            if assignment.gemini_key_name is not None
        )
        result = run_janitor(
            self.base_runner, self.janitor_config, now,
            protected_gemini_key_names=protected_gemini_key_names,
        )
        for deleted in result.deleted:
            self.audit.record(sandbox=None, issue=None,
                               outcome=f"janitor deleted {deleted.kind} {deleted.name}")
        for warning in result.warnings:
            # Visibility only, same treatment as the health/credential
            # warnings above -- a listing or deletion failure this cycle
            # doesn't stop the janitor from trying again next cycle.
            self.audit.record(
                sandbox=None, issue=None,
                outcome=f"janitor warning ({warning.kind} {warning.name}): {warning.detail}",
            )

    # --- scheduled jobs (bwsalmon/agents#163) ---------------------------
    def _scheduled_jobs(self, now: datetime) -> None:
        """No-op when `scheduled_jobs_config` is unset. Run last in
        `run_once`, right before `_dispatch`, so an issue this fires is
        picked up the very same cycle rather than sitting idle for one
        extra `run_once` tick -- the same "sweep before dispatch" ordering
        every other phase above already follows.

        For each job: skip it outright if `interval_hours` hasn't elapsed
        since it last fired (`AutomationState.scheduled_job_last_fired`),
        cheapest check first so a job nowhere near due costs nothing but a
        dict lookup. Only once a job is otherwise due does this check
        GitHub for an issue still carrying `job.marker_label` without
        `completed_label` -- an earlier firing whose work isn't finished
        yet -- and skips filing a second one if it finds one. That gate is
        deliberately independent of `interval_hours`: a job whose issue
        took longer than one interval to finish must not get a duplicate
        the moment the interval elapses, and a job whose issue finished
        early is still held to its own cadence rather than refiring
        immediately.
        """
        if self.scheduled_jobs_config is None:
            return
        for job in self.scheduled_jobs_config.jobs:
            last_fired = self.state.scheduled_job_last_fired.get(job.name)
            if last_fired is not None and now - last_fired < timedelta(hours=job.interval_hours):
                continue
            open_issues = self.github.list_issues(
                self.config.task_owner, self.config.task_repo, job.marker_label,
            )
            if any(
                self.config.completed_label not in issue.labels
                for issue in open_issues
            ):
                continue
            label = (
                self.config.needs_approval_label if job.needs_approval
                else self.config.trigger_label
            )
            issue = self.github.create_issue(
                self.config.task_owner, self.config.task_repo,
                title=job.render_title(now), body=job.render_body(now),
                labels=[label, job.marker_label],
            )
            self.state.record_scheduled_job_fired(job.name, now)
            self._save_state()
            self.audit.record(
                sandbox=None, issue=issue.number,
                outcome=f"scheduled job {job.name!r} filed issue #{issue.number}",
            )

    def _refresh_agent_labels(self) -> None:
        """Re-applies `labels.agent_label` for every assignment `_sweep`
        just above left standing (bwsalmon/agents#95) -- run every cycle,
        not just at dispatch, so a task that sits through many `run_once`
        calls keeps saying *which* sandbox is doing the work rather than
        that label only ever being right the moment it was first applied.
        `GitHubClient.add_label` is a no-op against a label the issue
        already carries, so this costs nothing on the common case where
        nothing has changed since last cycle; it only actually does
        something the rare time a label was knocked off by hand or never
        landed because an earlier cycle's call failed partway.

        Deliberately reads `self.state.assignments` *after* `_sweep` has
        already released anything that finished this cycle -- an
        assignment `_sweep`'s own finish handling just freed has already
        had its label removed by that same handling (`_finish_succeeded_issue`
        et al.), and re-adding it here a moment later would undo that.

        A 404 means the same "stale assignment" story every other
        GitHub-facing call in this module already tolerates: the task issue
        this assignment names is gone from the currently configured repo.
        Logged and skipped rather than raised, so one stale assignment
        can't stop every other still-live one from being refreshed.
        """
        for sandbox, assignment in self.state.assignments.items():
            try:
                self.github.add_label(
                    self.config.task_owner, self.config.task_repo,
                    assignment.issue, agent_label(sandbox),
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.audit.record(
                    sandbox=sandbox, issue=assignment.issue,
                    outcome=f"could not refresh agent label: issue #{assignment.issue} "
                            f"not found in {self.config.task_owner}/{self.config.task_repo} "
                            "-- stale assignment?",
                )

    def _restart_orphaned_in_progress(self) -> None:
        """bwsalmon/agents#139: the other half of the "controller can be
        restarted or recreated at any moment" story bwsalmon/agents#51
        already covers for a *state file that survives* the restart. That
        fix made sure a crash between an in-memory `state.assign()` and
        the next save can never lose the one on-disk record of an
        in-progress assignment -- but it still assumes there is a state
        file to reload. A restart that also loses `/data` (a fresh
        volume, a wiped or corrupted `state.json`, a from-scratch
        redeploy -- "reformatted" in the sense the issue title uses, not
        `mcp_server.py`'s per-sandbox `reformat_sandbox`, which never
        touches this file) comes back with `AutomationState.assignments`
        empty, and every fallback the sweeper gives a *known* stranded
        assignment (`sweeper.py`'s own docstring) has nothing left to act
        on: an issue that still carries `in_progress_label` out on GitHub
        is no longer `trigger_label`-ed, so `_dispatch`'s own poll never
        sees it either. Without this, such an issue would sit
        `in_progress` forever with no agent actually working it --
        indistinguishable, from the queue's point of view, from a task
        that's simply taking a long time.

        So this treats GitHub's own labels as the fallback source of
        truth, the same "poll, don't trust a cache" bar every other
        reconciliation pass here already holds to: any issue carrying
        `in_progress_label` that this process's own state has no
        assignment for gets exactly the treatment `_requeue` gives a
        stranded one -- `in_progress_label` off, `trigger_label` back on
        -- so the next `_dispatch` picks it up fresh. Run after
        `_sweep`/`_refresh_agent_labels`, so a *tracked* assignment
        `_sweep` just finished this very cycle has already had its own
        labels updated by the time this runs, and can never be
        double-counted as orphaned here.

        Every sandbox's `agent_label` is stripped, not just whichever one
        was actually working the issue -- the `Assignment` that would
        have named it is exactly what's gone, so there is no way left to
        know which sandbox that was. Harmless either way:
        `GitHubClient.remove_label` already treats a label the issue
        never carried as success (see its own docstring), so stripping
        every sandbox's label costs nothing beyond the extra calls.

        Deliberately scoped to `in_progress_label` alone, not
        `awaiting_reply_label`/`completed_label` too -- bwsalmon/agents#139
        asks specifically about restarting "in progress" work, which is
        the one state where a lost record means a live task's outcome
        would never be heard from again. The other two are already
        resting states with a human reply or a later poll expected to
        move them along, not silently orphaned work in the sense this is
        closing the gap on.

        A 404 from `add_label` here means the same "stale listing" story
        every other GitHub-facing call in this module already tolerates:
        the issue this listing just returned closed or vanished in the
        instant between that read and this write. `remove_label` never
        raises on a 404 either way (its own docstring), so only
        `add_label` needs the guard.
        """
        known = self.state.in_progress_issues()
        for issue in self.github.list_issues(
            self.config.task_owner, self.config.task_repo,
            self.config.in_progress_label,
        ):
            if issue.number in known:
                continue
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                issue.number, self.config.in_progress_label,
            )
            for sandbox in self.cluster.sandbox_names:
                self.github.remove_label(
                    self.config.task_owner, self.config.task_repo,
                    issue.number, agent_label(sandbox),
                )
            try:
                self.github.add_label(
                    self.config.task_owner, self.config.task_repo,
                    issue.number, self.config.trigger_label,
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.audit.record(
                    sandbox=None, issue=issue.number,
                    outcome=f"in progress with no known assignment but issue "
                            f"#{issue.number} not found in {self.config.task_owner}/"
                            f"{self.config.task_repo} -- stale listing?",
                )
                continue
            self.audit.record(
                sandbox=None, issue=issue.number,
                outcome="in progress with no known assignment -- requeued "
                        f"({self.config.in_progress_label!r} -> "
                        f"{self.config.trigger_label!r})",
            )

    def _finish_succeeded(self, outcome: Outcome) -> None:
        # bwsalmon/agents#175: filed first, unconditionally, regardless of
        # `outcome.kind` or whether the run below also asks a question --
        # proposing follow-up work is orthogonal to how the rest of this
        # outcome resolves, and every kind-specific path (including the
        # question one) returns early in a way that would otherwise skip it.
        self._file_proposed_tasks(outcome)
        if outcome.kind is TriggerKind.REVIEW:
            self._finish_succeeded_review(outcome)
        elif outcome.kind is TriggerKind.PR:
            self._finish_succeeded_pr(outcome)
        else:
            self._finish_succeeded_issue(outcome)

    def _finish_succeeded_issue(self, outcome: Outcome) -> None:
        """bwsalmon/agents#89: the branch is always checked first now, and
        it alone decides whether a PR gets opened -- `comment_on_issue`
        (bwsalmon/agents#50) used to be checked *before* the branch and,
        if the agent had called it, short-circuited the branch check
        entirely. That made the two signals disagreeable: an agent that
        pushed real commits and then also (mistakenly, the common failure
        mode this issue was filed over) called `comment_on_issue` at the
        end had its PR silently dropped, since the file-based comment won
        regardless of what was actually on the branch. Checking the branch
        first and treating a pending comment only as a fallback for when
        that branch turns out empty means a comment can never suppress a
        PR that real commits earned.
        """
        question = _pending_question(outcome.sandbox)
        if question is not None:
            self._finish_question(outcome, question)
            return
        target = self._target_of(outcome)
        branch = branch_name(outcome.issue)
        head = self.github.get_branch_head(target.owner, target.name, branch)
        if head is None:
            # The unit exited zero, but that is not the same claim as "a PR
            # can be opened" — the agent may never have pushed, or pushed
            # somewhere other than the branch dispatch() told it to. Verify,
            # don't trust (docs/roadmap.md item 2), and make the gap visible
            # rather than treating a branchless "success" as done -- unless
            # the agent left a comment explaining why there's nothing to
            # push, in which case an empty branch is the expected shape of
            # a research-only task, not a failure to retry.
            comment = _pending_comment(outcome.sandbox)
            if comment is None:
                self._requeue(outcome, f"succeeded but branch {branch!r} does not exist")
            else:
                self._finish_no_changes(outcome, comment)
            return

        # PR first, in-progress label off second: if the PR step fails
        # partway (a transient 5xx, a 422 this can't account for), the
        # issue stays visibly in-progress rather than looking finished with
        # nothing to show for it.
        # `Closes <task repo>#<n>`, fully qualified: still worth the
        # cross-repo link/mention on the issue even though (bwsalmon/agents#23)
        # a qualified `Closes` reference never auto-closes across repos —
        # GitHub only auto-closes within the same repo the PR is opened in.
        # The task issue is closed once that PR itself closes instead
        # (bwsalmon/agents#54, `_close_finished_prs`), not the moment it's
        # opened -- opening a PR is not the same claim as "this task is
        # done," only "a human can review it now."
        task = self._task
        # The issue's title isn't on hand here — `Outcome` only carries the
        # number (see sweeper.py's `Outcome` docstring on what does and
        # doesn't survive to finish time) — so it's read fresh rather than
        # threaded through, the same "one more call, no stale copy to keep
        # in sync" trade-off `get_pull_request` already makes elsewhere.
        issue = self.github.get_issue(task.owner, task.name, outcome.issue)
        base = outcome.base or self.github.default_branch(target.owner, target.name)
        # bwsalmon/agents#79: the body used to be built entirely from
        # metadata (which task, which sandbox) and never said anything
        # about what the change actually did, so a number of these PRs read
        # as description-free. `head.message` is the pushed branch's own
        # tip commit message -- `dispatch.py`'s `_prompt` now tells the
        # agent that message becomes the PR body verbatim, so it's the one
        # place an agent can put a real account of its own change.
        try:
            pr = self.github.create_pull_request(
                target.owner, target.name,
                head=branch, base=base,
                title=f"🤖 grain: {task}#{outcome.issue}: {issue.title}",
                body=f"{head.message.strip()}\n\n---\n"
                     f"Closes {task}#{outcome.issue}.\n\n{_AUTOMATION_SIGNATURE}",
            )
            reused = False
        except GitHubError as exc:
            # A second run of the same task is routine -- a human
            # re-applies `trigger_label` to ask for another round, or
            # `_restart_commented_completions` does it for them -- and
            # `branch_name` is a pure function of the issue number, so that
            # run pushes to the very branch the first run already opened a
            # PR from. GitHub allows one open PR per head branch and
            # answers the second `create_pull_request` with a 422.
            #
            # That 422 is not a failure of this task: a PR tracks its
            # branch, so the commits this run just pushed are already in
            # the existing PR. Treating it as an error left the issue
            # stuck in-progress with its work sitting in a PR nobody was
            # told about -- and, because `_requeue` never ran either, it
            # stayed that way.
            #
            # Only when an open PR for this head actually exists, though.
            # 422 is also what GitHub returns for "No commits between
            # `base` and `head`", which is a real problem with a message
            # worth keeping, so anything this lookup can't explain is
            # re-raised untouched.
            if exc.status != 422:
                raise
            existing = self.github.find_open_pull_request_for_branch(
                target.owner, target.name, branch
            )
            if existing is None:
                raise
            pr = existing
            reused = True
        # The task issue itself isn't closed here -- see `_close_finished_prs`
        # (bwsalmon/agents#54) for why that waits on the PR's own state
        # instead. `completed_label` goes on now regardless: it marks the
        # agent's own part as done, which is true the moment a real PR
        # exists, whether or not a human has reviewed or merged it yet.
        self.github.add_label(
            task.owner, task.name,
            outcome.issue, self.config.completed_label,
        )
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, agent_label(outcome.sandbox),
        )
        self.state.record_open_pr(outcome.issue, target.owner, target.name, pr.number,
                                   auto_merge=outcome.auto_merge)
        # bwsalmon/agents#135: tracked the same "poll and diff a baseline"
        # way as a pending question, so a later human comment on this now-
        # completed issue can restart it -- see
        # `_restart_commented_completions`/`CompletedIssue`.
        self.state.record_completed_issue(outcome.issue)
        # bwsalmon/agents#51: persist the open-PR record right after the
        # label moves that go with it, before anything later in this cycle
        # can crash on top of it -- the same ordering `_finish_question`
        # already uses for its own pending-state record.
        self._save_state()
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome=(f"pushed to the branch of existing PR {target}#{pr.number}: "
                     f"{pr.html_url}") if reused else
                    f"opened PR {target}#{pr.number}: {pr.html_url}",
        )

    def _finish_succeeded_pr(self, outcome: Outcome) -> None:
        question = _pending_question(outcome.sandbox)
        if question is not None:
            self._finish_question(outcome, question)
            return
        # A PR assignment always carries its branch (recorded at dispatch
        # time — see state.py's Assignment docstring for why it can't be
        # recomputed the way an issue's can); sweeper.py always fills it in
        # from the assignment it just freed, so this is never actually None
        # in practice.
        branch = outcome.branch
        assert branch is not None, "a PR outcome must carry the branch it was assigned"
        target = self._target_of(outcome)
        if not self.github.branch_exists(target.owner, target.name, branch):
            # Same "verify, don't trust" bar as the issue path (docs/roadmap.md
            # item 2): the unit exiting zero isn't proof the agent pushed
            # anything, or pushed to the branch it was told to.
            self._requeue(outcome, f"succeeded but branch {branch!r} does not exist")
            return

        # No PR to open — it already exists, which is the whole premise of
        # a `/pr` task — so a successful finish is just "stop marking the
        # task in-progress." Unlike the fresh-branch path there is no PR to
        # announce either: the task issue is simply no longer being worked,
        # and a human can label it again for another round if more feedback
        # comes in, the same way they would for a fresh task. The task issue
        # was never closed on this path even before bwsalmon/agents#54 (the
        # PR predates the task and is a human's to manage), so there is no
        # open-PR record to track here -- only `completed_label`, the same
        # "agent's own part is done" marker the fresh-branch path applies.
        self.github.add_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.completed_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, agent_label(outcome.sandbox),
        )
        # bwsalmon/agents#135: same restart-on-comment tracking the fresh-
        # branch path records -- see `_finish_succeeded_issue`.
        self.state.record_completed_issue(outcome.issue)
        self._save_state()
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome=f"pushed additional commits to {target} ({branch!r})",
        )

    def _finish_succeeded_review(self, outcome: Outcome) -> None:
        """A `/review`-directed dispatch (bwsalmon/agents#154): posts
        whatever the agent left via `add_review_comment` as one draft
        review on the PR it was pointed at, then finishes the task the
        same "just stop marking it in-progress" way `_finish_succeeded_pr`
        does -- there is no PR of this task's own to open or track, the
        target PR already existed before this task ever ran.
        """
        question = _pending_question(outcome.sandbox)
        if question is not None:
            self._finish_question(outcome, question)
            return
        # A REVIEW assignment always carries its branch and PR number
        # (recorded at dispatch time -- see state.py's Assignment
        # docstring); sweeper.py always fills both in from the assignment
        # it just freed, so neither is ever actually None in practice.
        branch = outcome.branch
        assert branch is not None, "a REVIEW outcome must carry the branch it was assigned"
        assert outcome.pr_number is not None, \
            "a REVIEW outcome must carry the PR it was assigned to review"
        target = self._target_of(outcome)
        if not self.github.branch_exists(target.owner, target.name, branch):
            # Same "verify, don't trust" bar every other finish path here
            # already holds to: the unit exiting zero isn't proof the
            # branch it was asked to read is still there to have read.
            self._requeue(outcome, f"succeeded but branch {branch!r} does not exist")
            return

        comments = _pending_review_comments(outcome.sandbox)
        general = [c["body"] for c in comments if c.get("path") is None]
        inline = [
            NewReviewComment(path=c["path"], line=c["line"], body=c["body"])
            for c in comments if c.get("path") is not None
        ]
        if not comments:
            # The agent looked and genuinely had nothing to say -- a normal
            # outcome for a review (`_review_prompt` explicitly allows it),
            # not a reason to post an empty draft review nobody asked for.
            outcome_text = "left no review comments"
        else:
            body = "\n\n".join(general + [_AUTOMATION_SIGNATURE])
            self.github.create_review(
                target.owner, target.name, outcome.pr_number,
                body=body, comments=inline,
            )
            outcome_text = (f"posted a draft review on {target}#{outcome.pr_number} "
                             f"({len(inline)} inline comment(s))")

        self.github.add_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.completed_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, agent_label(outcome.sandbox),
        )
        self.state.record_completed_issue(outcome.issue)
        self._save_state()
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue, outcome=outcome_text)

    def _finish_question(self, outcome: Outcome, question: str) -> None:
        """The agent called `ask_question` instead of finishing the task
        (docs/roadmap.md item 12) — post it to a human and take the issue
        out of the queue, rather than treating this like any other
        "succeeded but nothing usable" case. `_requeue` re-adds the trigger
        label immediately, which is right for a failed/branchless run (retry
        with no new information is still a reasonable default) but wrong
        here: it would redispatch on the very next `run_once` and most
        likely ask the identical question again, looping at real cost with
        nothing to act on in between.

        `awaiting_reply_label` replaces the in-progress label rather than
        just removing it (docs/roadmap.md item 13) — visible on GitHub, the
        same way the other two labels already are, so an operator can see
        which issues are genuinely idle waiting on a human versus untouched.
        The comment's own id is recorded (`state.record_pending_question`)
        as the baseline `_promote_answered_questions` checks future replies
        against; that pass is what re-adds the trigger label once a trusted
        reply shows up, not this method.

        A 404 from `create_comment` here means the same thing it means in
        `_requeue`: a stale assignment against a repo that's since changed
        out from under it, not a reason to crash the sweep. Best-effort
        only for the comment in that case — there's no human to post to
        either way, so the label swap and pending-question bookkeeping are
        skipped too; the assignment simply lapses.
        """
        try:
            comment_id = self.github.create_comment(
                self.config.task_owner, self.config.task_repo, outcome.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                f"I have a question before I can continue:\n\n{question}\n\n"
                "Reply here to continue -- a reply from a maintainer picks "
                f"this back up automatically. If that doesn't happen, "
                f"re-applying the {self.config.trigger_label!r} label works too.",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=outcome.sandbox, issue=outcome.issue,
                outcome=f"asked a question but issue #{outcome.issue} not found in "
                        f"{self.config.task_owner}/{self.config.task_repo} -- stale assignment? "
                        f"question was: {question[:200]!r}",
            )
            return
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.add_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.awaiting_reply_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, agent_label(outcome.sandbox),
        )
        self.state.record_pending_question(
            outcome.issue, comment_id, kind=outcome.kind, branch=outcome.branch,
        )
        # bwsalmon/agents#51: the GitHub-side label swap above already
        # landed by the time this runs; persist the pending-question record
        # that goes with it so a crash right after doesn't leave a
        # `_promote_answered_questions` with nothing to check a later reply
        # against (the fallback -- re-applying the trigger label by hand --
        # still works either way, but this keeps the automatic path intact).
        self._save_state()
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"asked a question, awaiting reply: {question[:200]!r}")

    def _finish_no_changes(self, outcome: Outcome, comment: str) -> None:
        """The agent finished without ever pushing a branch, and left a
        `comment_on_issue` comment behind explaining why (bwsalmon/agents#50,
        reworked by bwsalmon/agents#89) -- some tasks only ever needed an
        answer, an investigation, or a recommendation, not a code change,
        and forcing those through the branch/PR path is a poor fit. Only
        ever called from `_finish_succeeded_issue` once its own branch
        check has already come back empty, so there is nothing left to
        verify here -- unlike the old `complete_analysis` tool this
        replaced, a comment on its own never gets this far if the branch
        actually had commits on it.

        Unlike `_finish_question`, this is a genuine finish, not a park:
        the comment is posted, the same "post first, then update state"
        order `_finish_succeeded_issue`'s own PR path already uses -- if
        `create_comment` fails partway there is nothing yet to roll back.

        The issue itself is *not* closed (bwsalmon/agents#54): this path
        only ever produced an answer, not a change for anyone to review or
        merge, so there is no later event to wait on the way a PR's own
        close is for the fresh-branch path -- closing it outright here would
        make it easy to miss before anyone's actually read the comment. Only
        `completed_label` marks it as agent-done; a human closes it by hand
        once they're satisfied.

        A 404 here means the same thing it means in `_finish_question`/
        `_requeue`: a stale assignment against an issue that's since
        changed out from under it. Best-effort only in that case -- there
        is no issue left to label either.
        """
        task = self._task
        try:
            self.github.create_comment(
                task.owner, task.name, outcome.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                "This task has been completed with no code change -- "
                f"nothing was pushed:\n\n{comment}",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=outcome.sandbox, issue=outcome.issue,
                outcome=f"finished with no changes but issue #{outcome.issue} not found in "
                        f"{task.owner}/{task.name} -- stale assignment? "
                        f"comment was: {comment[:200]!r}",
            )
            return
        self.github.add_label(
            task.owner, task.name,
            outcome.issue, self.config.completed_label,
        )
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, agent_label(outcome.sandbox),
        )
        # bwsalmon/agents#135: same restart-on-comment tracking the other
        # two finish paths record -- see `_finish_succeeded_issue`.
        self.state.record_completed_issue(outcome.issue)
        self._save_state()
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"finished with no changes: {comment[:200]!r}")

    # --- pending questions (docs/roadmap.md item 13) ---------------------

    def _promote_answered_questions(self, now: datetime) -> None:
        """For every issue/PR still waiting on a question, checks whether a
        trusted reply has landed since the question was posted and, if so,
        re-adds the trigger label so `_dispatch`'s own polling picks it up
        in this same `run_once` call -- no new hydration path needed, since
        `_dispatch` already fetches full `Issue`/`PullRequestDetail` objects
        for anything carrying the trigger label.

        A reply "since the question" means a comment with a higher id than
        the question comment's own (`state.pending_questions`'s recorded
        baseline) -- not a count or a timestamp, both of which a deleted or
        backdated comment could make lie.
        """
        for pending in list(self.state.pending_questions.values()):
            try:
                comments = self.github.list_comments(
                    self.config.task_owner, self.config.task_repo, pending.issue
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                # Same "stale assignment" story as _requeue/_finish_question
                # -- the issue is gone from the currently configured repo.
                self.state.clear_pending_question(pending.issue)
                self._save_state()
                self.audit.record(
                    sandbox=None, issue=pending.issue,
                    outcome=f"issue #{pending.issue} not found in "
                            f"{self.config.task_owner}/{self.config.task_repo} while "
                            "checking for a reply -- stale assignment?",
                )
                continue
            reply = next(
                (c for c in comments
                 if c.id > pending.question_comment_id
                 and c.author_association in _TRUSTED_REPLY_ASSOCIATIONS
                 and not _is_automation_comment(c.body)),
                None,
            )
            if reply is None:
                continue
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                pending.issue, self.config.awaiting_reply_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                pending.issue, self.config.trigger_label,
            )
            self.state.clear_pending_question(pending.issue)
            # bwsalmon/agents#51: the trigger label is back on by now (the
            # calls above), which is what makes this issue reachable again
            # from `_dispatch`'s own poll -- persist the state that matches
            # before anything else in this cycle can crash on top of it.
            self._save_state()
            self.audit.record(
                sandbox=None, issue=pending.issue,
                outcome=f"{reply.user} ({reply.author_association}) replied -- "
                        "requeued for redispatch",
            )

    # --- restart on comment after completion (bwsalmon/agents#135) -------

    def _restart_commented_completions(self) -> None:
        """A completed task issue (`completed_label`) sits idle until a
        human reviews it. If they come back with a follow-up comment
        instead of relabelling it by hand, this puts `trigger_label` back
        on -- reopening the issue first if `_close_finished_prs` had
        already closed it -- so `_dispatch`'s own poll picks it up again
        in this same `run_once` call, the same "poll, diff a recorded
        baseline" shape `_promote_answered_questions` already uses for a
        reply to a question.

        Comments are read regardless of the issue's own open/closed state
        on GitHub -- commenting on a closed issue is allowed, and is
        exactly the common case this exists for: a human replying about
        work that already merged. Gated to `_TRUSTED_REPLY_ASSOCIATIONS`,
        the same trust tier every other comment-triggered redispatch here
        already requires -- a random public commenter must not be able to
        restart the agent set on a whim, the exact prompt-injection gate
        the trigger label exists to close in the first place.

        Gated equally to comments grain did not write itself
        (`_is_automation_comment`), which the trust tier alone cannot do:
        grain posts as a maintainer's own credential, so its comments come
        back "OWNER" like any other. `_suggest_fix` comments on precisely
        the issues tracked here -- a completed task whose PR went stale --
        to announce the follow-up task it filed, and without this that
        announcement read as a maintainer's follow-up and restarted the
        very task it was reporting on, with nobody having asked. Priming
        the baseline covers only the one comment a finish path posts on
        its way out; anything the automation says *later* needs this.

        A 404 from `list_comments` means the same "stale record" thing it
        means in `_promote_answered_questions`: the issue is gone from the
        currently configured repo. A 404 from `reopen_issue` once a
        restart is actually triggered means the same thing -- the record
        is dropped in both cases rather than raised.
        """
        for completed in list(self.state.completed_issues.values()):
            try:
                comments = self.github.list_comments(
                    self.config.task_owner, self.config.task_repo, completed.issue
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.state.clear_completed_issue(completed.issue)
                self._save_state()
                self.audit.record(
                    sandbox=None, issue=completed.issue,
                    outcome=f"issue #{completed.issue} not found in "
                            f"{self.config.task_owner}/{self.config.task_repo} while "
                            "checking for a restart comment -- stale record?",
                )
                continue
            highest = max((c.id for c in comments), default=0)
            if completed.baseline_comment_id is None:
                # First poll since completion -- nothing to fairly compare
                # a first read against (see `CompletedIssue`'s docstring),
                # so this just primes the baseline rather than restarting.
                self.state.prime_completed_baseline(completed.issue, highest)
                self._save_state()
                continue
            reply = next(
                (c for c in comments
                 if c.id > completed.baseline_comment_id
                 and c.author_association in _TRUSTED_REPLY_ASSOCIATIONS
                 and not _is_automation_comment(c.body)),
                None,
            )
            if reply is None:
                continue
            try:
                self.github.reopen_issue(
                    self.config.task_owner, self.config.task_repo, completed.issue
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.state.clear_completed_issue(completed.issue)
                self.state.clear_open_pr(completed.issue)
                self._save_state()
                self.audit.record(
                    sandbox=None, issue=completed.issue,
                    outcome=f"{reply.user} commented on completed issue "
                            f"#{completed.issue}, but it was not found in "
                            f"{self.config.task_owner}/{self.config.task_repo} while "
                            "restarting it -- stale record?",
                )
                continue
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                completed.issue, self.config.completed_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                completed.issue, self.config.trigger_label,
            )
            self.state.clear_completed_issue(completed.issue)
            # bwsalmon/agents#54's own open-PR record, if this issue still
            # has one, tracks a PR that's now beside the point -- leaving
            # it would let a later `_close_finished_prs` close the issue
            # this restart just reopened the moment that old PR itself
            # closes, with no new work behind it.
            self.state.clear_open_pr(completed.issue)
            # bwsalmon/agents#51: persist right after the GitHub-side
            # change it goes with, before anything later in this cycle can
            # crash on top of it -- same discipline as every other state
            # mutation in this module.
            self._save_state()
            self.audit.record(
                sandbox=None, issue=completed.issue,
                outcome=f"{reply.user} ({reply.author_association}) commented on a "
                        "completed issue -- reopened and requeued",
            )

    # --- lgtm approval on comment (bwsalmon/agents#136) -------------------

    def _promote_lgtm_comments(self) -> None:
        """A `needs_approval_label` issue (`_suggest_fix`, bwsalmon/agents#83)
        sits idle until a human applies `trigger_label` by hand -- what this
        adds is a second way to say the same thing: a trusted `/lgtm`
        comment. Polls every open issue currently carrying
        `needs_approval_label` each cycle and looks for a comment, from
        someone in `_TRUSTED_REPLY_ASSOCIATIONS`, with a line that reads
        `/lgtm` -- the same trust tier every other comment-triggered
        promotion in this module already requires, since approving a task
        is exactly the "a human decided this" act the trigger label itself
        gates.

        No baseline comment id to diff against, unlike
        `_promote_answered_questions`/`_restart_commented_completions`:
        `trigger_label` goes on and `needs_approval_label` comes straight
        off in the same pass, so the issue no longer matches `list_issues`'s
        own label filter on the next cycle -- there is nothing left that
        could re-trigger on the same comment.

        `needs_approval_label` is applied by `_suggest_fix`, which records
        the issue it just filed as `fix_issue` on the *original* PR's own
        `OpenPullRequest` record, and by `_file_proposed_tasks`
        (bwsalmon/agents#175), which records each one in
        `state.proposed_task_issues` instead -- so, the same way
        `_promote_answered_questions`/`_restart_commented_completions` skip
        straight past an empty `pending_questions`/`completed_issues`, this
        skips the `list_issues` call entirely unless `state` says a
        suggested fix or a proposed task is actually outstanding somewhere.
        Without that guard, every single `run_once` on every deployment
        would carry one more GitHub call for a feature the overwhelming
        majority of cycles have nothing to do with.

        Unlike `fix_issue` (never cleared once set -- see
        `OpenPullRequest`'s own docstring), a `proposed_task_issues` entry
        *is* dropped the moment this loop promotes it below, since there is
        no other event downstream that would ever do it for this feature --
        a fix's stale `fix_issue` eventually stops mattering once the
        original PR it was suggested for closes and its whole record is
        dropped with it, but a proposed task has no such original PR to
        wait on. An entry approved by hand instead of `/lgtm` (bypassing
        this loop) lingers here the same way a hand-approved `fix_issue`
        already does -- an accepted, minor extra poll cost, not something
        this method tries to detect.

        A 404 from `list_issues` or `list_comments` means the task repo (or
        one particular issue) is gone from underneath this deployment's own
        config -- read the same as everywhere else in this module: nothing
        to act on this cycle, not a reason to crash it.
        """
        if not any(o.fix_issue is not None
                   for o in self.state.open_pull_requests.values()) \
                and not self.state.proposed_task_issues:
            return
        try:
            issues = self.github.list_issues(
                self.config.task_owner, self.config.task_repo,
                self.config.needs_approval_label,
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            return
        for issue in issues:
            try:
                comments = self.github.list_comments(
                    self.config.task_owner, self.config.task_repo, issue.number
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                continue
            approval = next(
                (c for c in comments
                 if c.author_association in _TRUSTED_REPLY_ASSOCIATIONS
                 and _LGTM_RE.search(c.body)),
                None,
            )
            if approval is None:
                continue
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                issue.number, self.config.needs_approval_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                issue.number, self.config.trigger_label,
            )
            self.state.clear_proposed_task(issue.number)
            self._save_state()
            self.audit.record(
                sandbox=None, issue=issue.number,
                outcome=f"{approval.user} ({approval.author_association}) "
                        "commented /lgtm -- approved and requeued",
            )

    # --- closing on PR close (bwsalmon/agents#54) -------------------------

    def _pr_health(self, owner: str, repo: str, pr: PullRequestDetail) -> _PrHealth:
        """Reads `pr`'s conflict/check status (bwsalmon/agents#83) --
        `pr.mergeable` straight through, plus a check-runs read against its
        own head branch (`GitHubClient.list_check_runs` accepts a branch
        name directly, so no separate commit sha is ever needed here).
        """
        checks = self.github.list_check_runs(owner, repo, pr.head_ref)
        return _PrHealth(
            mergeable=pr.mergeable,
            checks_pending=any(c.status != "completed" for c in checks),
            failing_checks=tuple(
                c.name for c in checks
                if c.status == "completed" and c.conclusion in _FAILING_CHECK_CONCLUSIONS
            ),
        )

    def _file_proposed_tasks(self, outcome: Outcome) -> None:
        """Files whatever the agent proposed via `propose_task` this run
        (bwsalmon/agents#175) as fresh task-repo issues, each carrying
        `needs_approval_label` -- the same "grain suggests, a human
        decides" gate `_suggest_fix` already established
        (bwsalmon/agents#83): an agent proposing a task is not the same as
        a human wanting it attempted, so none of these are ever picked up
        by `_dispatch`'s own `trigger_label` poll on their own.
        `_promote_lgtm_comments` already knows how to bring one into the
        queue, the moment a human applies `trigger_label` by hand or
        comments `/lgtm` -- reusing the label this way, rather than a new
        one of its own, means this feature needed no changes there beyond
        widening its own "is it worth polling" guard.

        `depends_on` entries are resolved here, in the order the agent
        proposed them: a bare issue number names an existing task;
        anything else is looked up against the `id` an earlier proposal
        *in this same batch* gave itself, letting one run propose a small
        dependent chain of tasks without knowing any real issue numbers up
        front. This only ever looks backward through the batch -- a
        `depends_on` naming an `id` not yet filed (a forward reference, a
        typo, or the proposal's own `id`) can't be resolved, and is dropped
        with a note in the filed issue's own body rather than losing the
        whole proposal over one bad reference.

        Best-effort against a stale task issue the same way `_suggest_fix`/
        `_park`/`_finish_question` already are: a 404 from `create_issue`
        means the task repo itself is gone from underneath this
        deployment's own config, not a reason to crash the cycle -- this
        just stops filing the rest of this batch.
        """
        proposals = _pending_proposed_tasks(outcome.sandbox)
        if not proposals:
            return
        task = self._task
        filed_by_id: dict[str, int] = {}
        filed_numbers: list[int] = []
        for proposal in proposals:
            depends: list[int] = []
            notes: list[str] = []
            for ref in proposal.get("depends_on") or []:
                ref = str(ref)
                if ref.isdigit():
                    depends.append(int(ref))
                elif ref in filed_by_id:
                    depends.append(filed_by_id[ref])
                else:
                    notes.append(
                        f"(could not resolve dependency {ref!r} named by this "
                        "proposal -- dropped)"
                    )
            body_parts = [
                _AUTOMATION_SIGNATURE, "",
                f"Proposed by {task}#{outcome.issue} while working on that task.",
                "", proposal["body"],
            ]
            if notes:
                body_parts += [""] + notes
            body_parts.append(
                f"\nApply the `{self.config.trigger_label}` label to this issue, "
                "or comment `/lgtm` on it, to let the agent set attempt it."
            )
            if depends:
                body_parts.append("/depends " + ",".join(str(d) for d in depends))
            try:
                new_issue = self.github.create_issue(
                    task.owner, task.name,
                    title=f"🤖 grain: {proposal['title']}",
                    body="\n".join(body_parts),
                    labels=[self.config.needs_approval_label],
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.audit.record(
                    sandbox=outcome.sandbox, issue=outcome.issue,
                    outcome=f"wanted to file a proposed task ({proposal['title']!r}) "
                            f"but {task} was not found -- stale config?",
                )
                break
            proposal_id = proposal.get("id")
            if proposal_id and str(proposal_id) not in filed_by_id:
                filed_by_id[str(proposal_id)] = new_issue.number
            filed_numbers.append(new_issue.number)
            self.state.record_proposed_task(new_issue.number)

        if not filed_numbers:
            return
        self._save_state()
        named = ", ".join(f"{task}#{n}" for n in filed_numbers)
        try:
            self.github.create_comment(
                task.owner, task.name, outcome.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                f"Proposed {len(filed_numbers)} follow-up task(s) while working on "
                f"this: {named}. Apply the `{self.config.trigger_label}` label (or "
                "comment `/lgtm`) on each one you'd like the agent set to attempt.",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome=f"filed {len(filed_numbers)} proposed task(s): {named}",
        )

    def _suggest_fix(self, pending: OpenPullRequest, pr: PullRequestDetail,
                      health: _PrHealth) -> None:
        """Files a new task to fix `pr` (bwsalmon/agents#83), once
        `_close_finished_prs` has read a definite conflict or failing check
        against it -- the issue this whole feature asked for: a completed
        task's PR going stale (a conflicting rebase, a flaky-turned-broken
        test) previously just sat there until a human noticed by hand.

        The new task carries `/repo`, `/base pr.head_ref` and
        `/auto-merge` -- the same directives `_resolve_target` already
        knows how to honour, so no new dispatch machinery is needed: a
        fresh branch built on top of `pr`'s own branch *is* a stacked PR,
        and `/auto-merge` is what lets `_close_finished_prs` merge that
        stacked PR back into `pr`'s branch itself once it reads clean,
        without a second round of human review. Filed with
        `needs_approval_label`, not `trigger_label` -- the issue this
        deployment asked for is that grain only *suggests* the fix; a human
        still has to apply `trigger_label` (the same action that starts
        every other task) before the agent set attempts it.

        Best-effort against a stale task issue the same way `_park`/
        `_finish_question` already are: a 404 from either call means the
        task repo or task issue #`pending.issue` is gone from underneath a
        record this deployment still holds, not a reason to crash the
        cycle -- the fix simply never gets suggested this cycle, and
        `pending.fix_issue` stays unset so a later cycle tries again.
        """
        target = RepoRef(pending.target_owner, pending.target_repo)
        task = self._task
        reasons = []
        if health.has_conflict:
            reasons.append(f"it has conflicts with `{pr.base_ref}`")
        if health.failing_checks:
            names = ", ".join(f"`{n}`" for n in health.failing_checks)
            reasons.append(f"these checks are failing: {names}")
        reason = " and ".join(reasons)
        try:
            new_issue = self.github.create_issue(
                task.owner, task.name,
                title=f"🤖 grain: fix {target}#{pending.pr_number}",
                body=(
                    f"{_AUTOMATION_SIGNATURE}\n\n"
                    f"Task {task}#{pending.issue} opened {target}#{pending.pr_number} "
                    f"({pr.html_url}), but {reason}.\n\n"
                    f"This task fixes that: it works from `{pr.head_ref}` (the same "
                    "branch) and, once it succeeds, its own pull request is merged "
                    f"back into `{pr.head_ref}` automatically -- no separate review "
                    "needed for the fix itself.\n\n"
                    f"Apply the `{self.config.trigger_label}` label to this issue, or "
                    "comment `/lgtm` on it, to let the agent set attempt it.\n\n"
                    f"/repo {target}\n/base {pr.head_ref}\n/auto-merge true\n"
                ),
                labels=[self.config.needs_approval_label],
            )
            self.github.create_comment(
                task.owner, task.name, pending.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                f"{target}#{pending.pr_number} {reason} -- filed {task}#{new_issue.number} "
                f"to fix it. Apply the `{self.config.trigger_label}` label there, or "
                "comment `/lgtm` on it, once you're happy for the agent to attempt it.",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=None, issue=pending.issue,
                outcome=f"wanted to suggest a fix for {target}#{pending.pr_number} "
                        f"({reason}) but {task}#{pending.issue} was not found -- stale "
                        "record?",
            )
            return
        self.state.mark_fix_suggested(pending.issue, new_issue.number)
        self._save_state()
        self.audit.record(
            sandbox=None, issue=pending.issue,
            outcome=f"suggested fix {task}#{new_issue.number} for {target}#"
                    f"{pending.pr_number}: {reason}",
        )

    def _close_finished_prs(self) -> None:
        """Closes a task issue once the PR `_finish_succeeded_issue` opened
        for it has itself closed — merged or closed without merging both
        read `state == "closed"` from GitHub, and count the same way here:
        either one means nobody is going to push more commits to that PR, so
        the task it was opened for is done. There is no webhook to react to
        this with (docs/design.md's cron-not-webhooks stance, this module's
        own docstring) and no label move to piggyback on either, since the
        PR lives in the *target* repo while every label this deployment
        writes lives on the task issue in the *task* repo — so this polls
        `state.open_pull_requests` (`_finish_succeeded_issue`'s own record)
        instead, the same shape `_promote_answered_questions` already uses
        to poll `state.pending_questions`.

        A 404 from `get_pull_request` means the target repo or PR named in a
        stale record is gone (an operator changed the allowlist, the PR was
        deleted outright) — not a reason to crash the cycle, so the record
        is just dropped. A 404 from `close_issue` means the *task* issue
        itself is gone the same "stale assignment" way `_requeue` and
        `_finish_question` already tolerate; the record is dropped there
        too, since there is nothing left to close.

        **bwsalmon/agents#83: while a PR is still open, this is also where
        it's watched for trouble.** Two things can happen to a still-open
        PR, both read from the same `_pr_health` call so a cycle never pays
        for it twice:

        - A task carrying `/auto-merge` (only ever a fix `_suggest_fix`
          itself filed, though nothing stops a human writing the directive
          by hand) gets merged the moment its PR reads clean --
          `GitHubClient.merge_pull_request`, gated on `health.is_clean` so
          this never merges anything still being computed, still running
          checks, or already known broken. A 405/409 (the PR went stale
          between the read and the merge attempt) is logged and retried
          next cycle rather than raised.
        - Anything else still open gets checked for real trouble
          (`health.is_broken`) and, the first time that's true
          (`pending.fix_issue is None` -- never a second time for the same
          PR), `_suggest_fix` is called. A PR that itself came from
          `/auto-merge` is deliberately excluded from this -- suggesting a
          fix for a fix risks an unbounded chain, and this deployment would
          rather leave a stuck auto-merge PR visibly open (still carrying
          `completed_label`, still open on GitHub) than build one.
        """
        for pending in list(self.state.open_pull_requests.values()):
            try:
                pr = self.github.get_pull_request(
                    pending.target_owner, pending.target_repo, pending.pr_number
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.state.clear_open_pr(pending.issue)
                self._save_state()
                self.audit.record(
                    sandbox=None, issue=pending.issue,
                    outcome=f"PR {pending.target_owner}/{pending.target_repo}#"
                            f"{pending.pr_number} not found while checking for a "
                            "close -- stale record?",
                )
                continue

            merged_now = False
            if pr.state == "open":
                health = self._pr_health(
                    pending.target_owner, pending.target_repo, pr
                )
                if pending.auto_merge:
                    if health.is_clean:
                        try:
                            self.github.merge_pull_request(
                                pending.target_owner, pending.target_repo,
                                pending.pr_number,
                            )
                            merged_now = True
                            self.audit.record(
                                sandbox=None, issue=pending.issue,
                                outcome=f"auto-merged {pending.target_owner}/"
                                        f"{pending.target_repo}#{pending.pr_number}",
                            )
                        except GitHubError as exc:
                            if exc.status not in (404, 405, 409):
                                raise
                            self.audit.record(
                                sandbox=None, issue=pending.issue,
                                outcome=f"auto-merge of {pending.target_owner}/"
                                        f"{pending.target_repo}#{pending.pr_number} "
                                        f"failed ({exc.status}) -- will retry",
                            )
                elif pending.fix_issue is None and health.is_broken:
                    self._suggest_fix(pending, pr, health)

            if pr.state != "closed" and not merged_now:
                continue
            try:
                self.github.close_issue(
                    self.config.task_owner, self.config.task_repo, pending.issue
                )
            except GitHubError as exc:
                if exc.status != 404:
                    raise
                self.state.clear_open_pr(pending.issue)
                self._save_state()
                self.audit.record(
                    sandbox=None, issue=pending.issue,
                    outcome=f"PR {pending.target_owner}/{pending.target_repo}#"
                            f"{pending.pr_number} closed, but issue #{pending.issue} "
                            f"not found in {self.config.task_owner}/"
                            f"{self.config.task_repo} -- stale assignment?",
                )
                continue
            self.state.clear_open_pr(pending.issue)
            # bwsalmon/agents#51: same ordering discipline as every other
            # state-clearing call above -- persist right after the GitHub
            # side change it goes with, before anything later in this cycle
            # can crash on top of it.
            self._save_state()
            self.audit.record(
                sandbox=None, issue=pending.issue,
                outcome=f"closed: PR {pending.target_owner}/{pending.target_repo}#"
                        f"{pending.pr_number} closed",
            )

    def _requeue(self, outcome: Outcome, reason: str) -> None:
        # Back to the trigger label, per docs/design.md: "issues need
        # returning to the queue rather than stalling silently."
        #
        # A 404 here means the issue this assignment names doesn't exist in
        # the *currently configured* repo — not "GitHub rejected the
        # request," but "this assignment is stale," e.g. left over from a
        # repo `controller configure` has since pointed elsewhere (found
        # live: docs/next-session.md). Letting that propagate crashes
        # `run_once` before `_dispatch` ever runs, taking down the whole
        # sweep over one leftover assignment. Log and move on instead; any
        # other status (a real 5xx, an auth failure) still isn't something
        # a stale assignment explains, so it still propagates.
        try:
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                outcome.issue, self.config.in_progress_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                outcome.issue, self.config.trigger_label,
            )
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                outcome.issue, agent_label(outcome.sandbox),
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=outcome.sandbox, issue=outcome.issue,
                outcome=f"{reason} (requeue skipped: issue #{outcome.issue} not found in "
                        f"{self.config.task_owner}/{self.config.task_repo} -- stale assignment?)",
            )
            return
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue, outcome=reason)

    def _finish_cancelled(self, outcome: Outcome) -> None:
        """bwsalmon/agents#82: the task issue was closed on GitHub while its
        agent was still running. Unlike `_requeue`'s failed/stranded
        handling, this must not put the trigger label back on -- a closed
        issue means the work is no longer wanted, not that it should be
        attempted again. `in_progress_label` still comes off, so a closed
        issue doesn't sit looking mid-flight forever; there is no
        `completed_label` to add, since nothing was actually finished.

        No try/except around `remove_label` here, the same as
        `_finish_succeeded_issue`'s own use of it: `GitHubClient.remove_label`
        already treats a 404 as "label already off" internally and never
        raises for it (see its own docstring), so there is no stale-assignment
        case left for this call site to catch.
        """
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, agent_label(outcome.sandbox),
        )
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome="cancelled: issue closed while its agent was still running",
        )

    # --- target resolution ----------------------------------------------

    def _resolve_target(self, issue: Issue, comments: list[Comment], *,
                         sandbox: str) -> "ResolvedTask":
        """What a task's own text says to work on: which repo, optionally
        which PR to continue, and what base a new PR should target.

        `sandbox` is the one `_dispatch` already picked for this candidate
        before ever calling this -- needed only for
        `config.scratch_repo_label` (bwsalmon/agents#159), which repo that
        resolves to name has no coherent meaning independent of which
        sandbox ends up doing the work.

        Reads directives from the issue body plus every *trusted* comment
        on it (`_TRUSTED_REPLY_ASSOCIATIONS`, the same "could have applied
        the label" tier the trigger gate itself relies on) that grain did
        not write itself (`_is_automation_comment`) -- the trust tier
        alone cannot tell those apart, since grain posts as a maintainer's
        own credential and so comes back "OWNER" like any other. A
        directive is an instruction *from* a human, and grain's comments
        quote a task's own text back at it (`_park` names the `/repo` line
        it could not use, `_suggest_fix` describes the follow-up task it
        filed); none of them puts a directive at the start of a line
        today, which is the only shape `_DIRECTIVE_RE` matches, so this is
        a guard against grain instructing itself rather than a fix for a
        failure already seen. `issue.body` is deliberately not filtered:
        grain files tasks itself (`_suggest_fix`), and the directives in
        those bodies are exactly what is meant to apply. Later texts
        override earlier — so repairing a task is a reply, not an edit
        plus a reply. Raises `DirectiveError` for anything unusable; every
        message is written to be posted verbatim by `_park`.
        """
        texts = [issue.body] + [
            c.body for c in comments
            if c.author_association in _TRUSTED_REPLY_ASSOCIATIONS
            and not _is_automation_comment(c.body)
        ]
        directives = parse_directives(texts)
        if issue.number in (directives.depends or ()):
            # A task naming itself in its own `/depends` line could never
            # close the loop -- unlike a dependency on a *different* issue
            # that is merely slow to close, this is never going to resolve
            # on its own, so it is refused outright the same as any other
            # unusable directive, rather than left to block forever with
            # only the audit log to explain why.
            raise DirectiveError(
                f"this task's `/depends` directive names itself (#{issue.number}) "
                "-- an issue can't depend on its own completion. Remove it "
                "from the `/depends` line."
            )
        # bwsalmon/agents#49: a label, not a `/gemini-key` directive --
        # `directives.py`'s own docstring has the reasoning. `issue.labels`
        # is read directly, the same trust tier the trigger label itself
        # already relies on.
        gemini_key = self.config.gemini_key_label in issue.labels
        if gemini_key and self.gemini_key_config is None:
            # Same "unusable request parks the task" shape as an unlisted
            # `/repo` below -- checked before the target/allowlist reads,
            # so a task that can never be honoured is parked without also
            # spending a GitHub call on a repo it will never reach.
            raise DirectiveError(
                f"this task carries the `{self.config.gemini_key_label}` "
                "label, but this deployment has no Gemini key support "
                "configured. An operator enables it with `grain controller "
                "configure --gemini-project-id ...` (see gemini_keys.py)."
            )
        # bwsalmon/agents#62: same label-tier read as gemini_key above, but
        # with nothing to refuse -- the controller-side group grant that
        # makes `read_grain_logs` work (provision/controller.sh) is
        # unconditional, so there is no "not configured" case to park a
        # task for.
        self_debug = self.config.self_debug_label in issue.labels
        # bwsalmon/agents#99: same unconditional label read as self_debug
        # above -- there is nothing to refuse it for, see
        # `ResolvedTask.self_repair`'s own docstring.
        self_repair = self.config.self_repair_label in issue.labels
        github_key = self._resolve_github_key(issue)
        # bwsalmon/agents#159: same label tier as gemini_key above, refused
        # the same way when this deployment has no `scratch_repo_config` to
        # honour it with.
        scratch_repo = self.config.scratch_repo_label in issue.labels
        if scratch_repo and self.scratch_repo_config is None:
            raise DirectiveError(
                f"this task carries the `{self.config.scratch_repo_label}` "
                "label, but this deployment has no scratch-repo support "
                "configured. An operator enables it with `grain controller "
                "configure --scratch-repo-owner ...` (see scratch_repo.py)."
            )
        target = directives.target
        if scratch_repo:
            # One dedicated repo per sandbox, named deterministically off
            # whichever sandbox `_dispatch` already picked for this task
            # (`repo_for_sandbox`) -- which repo that is can't be known
            # until then, so this overrides any `/repo` directive entirely
            # rather than merely defaulting for a task that gave none.
            target = RepoRef(owner=self.scratch_repo_config.owner,
                              name=repo_for_sandbox(self.scratch_repo_config, sandbox))
        elif target is None:
            if not self.config.default_target_repo:
                raise DirectiveError(
                    "this task doesn't say which repository to work in. Add a "
                    "line `/repo owner/name` to the issue body, or reply to "
                    "this issue with one."
                )
            target = RepoRef.parse(
                self.config.default_target_repo, what="`default_target_repo`"
            )
        if not self.allowlist.allows(target.owner, target.name):
            raise DirectiveError(
                f"`{target}` is not on this deployment's repo allowlist, so "
                "nothing here can clone, push to, or open a pull request "
                "against it. An operator adds it to "
                "`/data/config/repo-allowlist.json` on the controller."
            )
        if directives.review and directives.pr is None:
            # bwsalmon/agents#154: checked before the GitHub calls below,
            # same "an unusable request parks the task without spending a
            # call on it" discipline the gemini_key check above already
            # follows -- there is no branch to check out or PR to post a
            # draft review against without one.
            raise DirectiveError(
                "this task carries `/review` but no `/pr` -- a review "
                "needs a pull request number to know which branch to read "
                "and which PR to post its draft review against. Add a "
                "`/pr <number>` line alongside it."
            )
        pr: PullRequestDetail | None = None
        try:
            if directives.pr is not None:
                pr = self.github.get_pull_request(
                    target.owner, target.name, directives.pr
                )
            # Read even when `/base` was given, so a nonexistent target repo
            # is caught here (a clear comment) rather than at clone time (a
            # `CommandError` from inside the sandbox). One extra GET against
            # a repo this deployment is about to clone anyway.
            default_branch = self.github.default_branch(target.owner, target.name)
        except GitHubError as exc:
            if exc.status != 404:
                raise
            raise DirectiveError(
                f"couldn't read `{target}`"
                + (f" pull request #{directives.pr}" if pr is None and directives.pr else "")
                + " -- GitHub returned 404. Either it doesn't exist, or this "
                  "deployment's credential can't see it."
            ) from exc
        base = directives.base or default_branch
        # `default_branch` above is the *configured* default-branch name --
        # GitHub still reports one even for a repo with no commits at all,
        # where no branch of any name actually exists yet. Confirmed here
        # (bwsalmon/agents#224) for the same reason the 404 check above
        # exists: without it, this only surfaces as a `CommandError` from
        # `ensure_workspace`'s `git checkout` deep inside the sandbox --
        # which `_dispatch` merely logs to the audit trail and retries next
        # cycle, forever, with nothing ever posted to the issue. Skipped
        # for a `/pr` dispatch: `dispatch_pr` builds the workspace from the
        # PR's own `head_ref`, never from `ResolvedTask.base`, so a missing
        # `base` branch there is not a reason to refuse the task.
        if pr is None and not self.github.branch_exists(target.owner, target.name, base):
            raise DirectiveError(
                f"the base branch `{base}` doesn't exist in `{target}`"
                + (" -- named by this task's `/base` directive"
                   if directives.base else
                   " -- which is odd, since GitHub reports it as the "
                   "repository's default branch; this usually means the "
                   "repository has no commits yet")
                + f". Push a commit to `{base}` (or correct the `/base` "
                  "line) and reply here to pick this back up."
            )
        return ResolvedTask(repo=target, pr=pr, base=base,
                            gemini_key=gemini_key, self_debug=self_debug,
                            self_repair=self_repair,
                            github_key=github_key, auto_merge=directives.auto_merge,
                            review=directives.review, depends=directives.depends or ())

    def _resolve_github_key(self, issue: Issue) -> str | None:
        """The named credential a `grain-github-<name>` label on `issue`
        asks for (bwsalmon/agents#52), or `None` if no such label is on it.

        A real GitHub label, unlike every other directive `_resolve_target`
        reads: it comes straight off `issue.labels`, never from comment
        text, since applying a label already requires the same trust tier
        `_TRUSTED_REPLY_ASSOCIATIONS` gates comment-borne directives to.

        Raises `DirectiveError` (parking the task, same as every other
        unusable directive here) for two distinct problems: more than one
        `grain-github-*` label on the same issue, which one applies is
        ambiguous; or a name this deployment has no `<name>.token`
        configured for, which would otherwise only surface as an opaque
        500 from the git proxy on the sandbox's first push.
        """
        names = {
            match.group(1) for label in issue.labels
            if (match := _GITHUB_KEY_LABEL_RE.match(label))
        }
        if not names:
            return None
        if len(names) > 1:
            labels = ", ".join(f"`grain-github-{n}`" for n in sorted(names))
            raise DirectiveError(
                f"this issue carries more than one named-GitHub-key label "
                f"({labels}) -- which one applies is ambiguous, so nothing "
                "was dispatched. Remove all but one."
            )
        (name,) = names
        if self.credentials is None or self.credentials.get(name) is None:
            raise DirectiveError(
                f"this issue is labelled `grain-github-{name}`, asking for "
                f"a named GitHub credential this deployment doesn't have. "
                f"An operator adds a `{name}.token` file under "
                "`/data/secrets/github` on the controller (see "
                "`configure_named_github_key` in grain/automation/configure.py)."
            )
        return name

    def _park(self, number: int, reason: str) -> None:
        """Takes a task out of the queue with an explanation, instead of
        retrying an unusable directive every cycle forever.

        Deliberately the same landing state as an unanswered question
        (docs/roadmap.md items 12-13): comment, swap the trigger label for
        `awaiting_reply_label`, record the comment id as the reply baseline.
        `_promote_answered_questions` then does the rest — a maintainer's
        reply (which may itself carry the corrected `/repo` line) puts the
        trigger label back on the next cycle. No sandbox was ever assigned
        here, so there is no in-progress label to remove and nothing to
        release.

        Same 404 tolerance as `_finish_question`: an issue that vanished
        between the listing and this call isn't a reason to crash the
        cycle.
        """
        try:
            comment_id = self.github.create_comment(
                self.config.task_owner, self.config.task_repo, number,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                f"I can't start this task yet: {reason}\n\n"
                "Reply here once it's fixed -- a reply from a maintainer "
                "picks this back up automatically, and a `/repo owner/name` "
                "line in that reply counts.",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=None, issue=number,
                outcome=f"could not park issue #{number}: not found in "
                        f"{self.config.task_owner}/{self.config.task_repo} "
                        "-- deleted mid-cycle?",
            )
            return
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            number, self.config.trigger_label,
        )
        self.github.add_label(
            self.config.task_owner, self.config.task_repo,
            number, self.config.awaiting_reply_label,
        )
        self.state.record_pending_question(number, comment_id)
        # Same reasoning as `_finish_question` (bwsalmon/agents#51): persist
        # the pending-question baseline right after the label swap it
        # accompanies.
        self._save_state()
        self.audit.record(sandbox=None, issue=number,
                           outcome=f"parked, awaiting reply: {reason}")

    # --- dispatch -------------------------------------------------------
    def _dispatch(self, now: datetime) -> None:
        candidates = self.github.list_issues(
            self.config.task_owner, self.config.task_repo, self.config.trigger_label
        )
        in_progress = self.state.in_progress_issues()
        # Oldest number first: one repo's issue numbers are handed out in
        # filing order, so this drains a backlog in the order it arrived.
        # Only issues are polled -- a PR-continuation task is an issue
        # carrying a `/pr` directive, so there is no second listing to merge
        # in and no interleaving policy to decide.
        queue = sorted(
            (i for i in candidates if i.number not in in_progress),
            key=lambda i: i.number,
        )

        for issue in queue:
            number = issue.number
            sandbox = self.state.free_sandbox(self.cluster.sandbox_names)
            if sandbox is None:
                self.audit.record(sandbox=None, issue=number,
                                   outcome="skipped: no free sandbox")
                break
            if not ratelimit.allow(self.state.run_timestamps, now,
                                    self.config.runs_per_hour):
                self.audit.record(sandbox=None, issue=number,
                                   outcome="skipped: rate limit")
                break

            # Fetched for every dispatch, whether or not a prior attempt
            # ever asked a question (docs/roadmap.md item 12) --
            # `AutomationState` carries no memory of that once an assignment
            # is released, so this is the only way a redispatch sees a
            # human's reply. Empty for the (common) case of no conversation
            # yet; `_prompt`/`_pr_prompt` render that plainly rather than
            # omitting the section, matching the existing blank-state
            # convention for review comments. It is also where a trusted
            # `/repo` correction lives, which is why it is read before the
            # task's target is resolved rather than after.
            thread_comments = self.github.list_comments(
                self.config.task_owner, self.config.task_repo, number
            )
            try:
                task = self._resolve_target(issue, thread_comments, sandbox=sandbox)
            except DirectiveError as exc:
                # No sandbox consumed and no rate-limit slot spent: parking
                # is bookkeeping on the task repo, not a run.
                self._park(number, str(exc))
                continue

            # bwsalmon/agents#164: checked fresh every cycle, the same
            # "poll, don't trust a stale copy" discipline `_is_issue_closed`
            # already holds to for the cancel-on-close poll
            # (bwsalmon/agents#82) it's reused from here -- a dependency
            # that was open last cycle may have closed since, with nothing
            # about this task's own text having changed. `()` when
            # `task.depends` is empty, which matters below: it's what makes
            # the label-clearing branch fire equally for "never blocked"
            # and "no longer names any dependency at all".
            blocking = (
                tuple(n for n in task.depends if not self._is_issue_closed(n))
                if task.depends else ()
            )
            if blocking:
                # bwsalmon/agents#194: visible on the issue itself, not just
                # the audit log -- reapplied every cycle the block still
                # holds (a no-op once it's already on, per
                # `GitHubClient.add_label`) rather than tracked in
                # `AutomationState`, so a controller restart mid-block has
                # nothing to lose. Deliberately *not* `_park`: parking swaps
                # the trigger label for the awaiting-reply one and waits on
                # a human reply, but nothing here needs a human -- it needs
                # the dependency issue to close, which happens on its own.
                # A `continue`, not a `break`, since a blocked issue says
                # nothing about whether the next one in the queue is also
                # blocked -- unlike "no free sandbox"/"rate limit" above,
                # which are true for every remaining candidate this cycle.
                if self.config.waiting_on_dependency_label not in issue.labels:
                    self.github.add_label(
                        self.config.task_owner, self.config.task_repo,
                        number, self.config.waiting_on_dependency_label,
                    )
                named = ", ".join(f"#{n}" for n in blocking)
                self.audit.record(sandbox=None, issue=number,
                                   outcome=f"skipped: blocked on {named}")
                continue
            if self.config.waiting_on_dependency_label in issue.labels:
                # The block just cleared (or the `/depends` line that
                # caused it is gone) -- strip the label on this same cycle
                # rather than leaving a stale "blocked" pill on an issue
                # that's about to dispatch.
                self.github.remove_label(
                    self.config.task_owner, self.config.task_repo,
                    number, self.config.waiting_on_dependency_label,
                )

            sandbox_runner = self._ssh_runner_for(sandbox)
            # The same address/user `_ssh_runner_for` just used to build
            # `sandbox_runner` above, passed through as data rather than an
            # object — `mcp_server.py` runs as its own process (a child of
            # the controller-side `claude -p` unit) and builds its own
            # independent `SshRunner`, so it needs this to travel inside the
            # per-dispatch MCP config JSON, not as a live `Runner`. The key
            # path is deliberately CONTROLLER_AGENT_SSH_KEY_PATH, not
            # `self.config.ssh_key_path` (this process's own key) — see
            # that constant's docstring for why they have to be two
            # separate files.
            sandbox_target = SandboxTarget(
                address=str(self.cluster.address_of(sandbox)),
                ssh_user=self.config.ssh_user,
                ssh_key_path=CONTROLLER_AGENT_SSH_KEY_PATH,
            )
            token = self.token_store.ensure_token(sandbox)
            if self.credential_store is not None:
                # Written unconditionally, before the dispatch attempt
                # below can fail partway -- see `SandboxCredentialStore`'s
                # own docstring for why this, plus `_release`'s own clear,
                # is what keeps an override from ever outliving the task
                # that asked for it (or leaking into the next one, if this
                # very attempt never gets far enough to be assigned at
                # all).
                if task.github_key is not None:
                    self.credential_store.set(sandbox, task.github_key)
                else:
                    self.credential_store.clear(sandbox)
            # The prompt never carries the directive lines themselves --
            # they are addressed to this orchestrator, and an agent has no
            # way to act on one anyway (docs/design.md's split surface).
            # Comments get the same treatment as the body: a maintainer's
            # `/repo` correction is a reply, so it would otherwise reach the
            # prompt through the conversation section instead. A comment
            # left empty by that (one that was *only* a directive) is
            # dropped rather than rendered as an author with no message.
            prompt_issue = dataclasses.replace(
                issue, body=strip_directives(issue.body)
            )
            prompt_comments = [
                stripped for stripped in (
                    dataclasses.replace(c, body=strip_directives(c.body))
                    for c in thread_comments
                )
                if stripped.body
            ]
            # A `CommandError` here (an SSH/command failure anywhere in
            # dispatch()/dispatch_pr()'s path -- ensure_workspace,
            # configure_git_credentials, starting the unit; or gemini_keys
            # .create_key's own gcloud calls below (bwsalmon/agents#47))
            # must not take down every other candidate still queued this
            # cycle. Found live (docs/next-session.md): a proxy-auth
            # failure on one sandbox crashed `run_once` before it ever
            # reached the next candidate. Neither the sandbox nor the
            # issue's labels are touched below on failure -- the sandbox
            # stays free and the issue keeps its trigger label, so both are
            # simply retried on a later cycle, same "log and move on"
            # discipline `_requeue`/`_finish_question` already apply to a
            # GitHub-side 404. Only `CommandError` specifically: anything
            # else is a real bug, not an expected failure mode, and should
            # still surface immediately.
            #
            # gcp_keys.create_key's own gcloud calls (bwsalmon/agents#126)
            # are deliberately *not* covered by this catch, unlike the
            # Gemini key -- see the `gcp_key_mint_error` handling just below
            # (bwsalmon/agents#138) for why that failure is degraded instead
            # of treated as this candidate's dispatch failing outright.
            gemini_key_string: str | None = None
            gemini_key_name: str | None = None
            gcp_key_json: str | None = None
            gcp_key_id: str | None = None
            gcp_key_mint_error: str | None = None
            try:
                if task.gemini_key:
                    # `_resolve_target` already refused this task outright
                    # if `self.gemini_key_config` were `None` -- guaranteed
                    # set here.
                    minted = create_gemini_key(
                        self.base_runner, self.gemini_key_config,
                        display_name=f"grain-{sandbox}-issue-{number}",
                    )
                    gemini_key_string, gemini_key_name = minted.key_string, minted.name
                if self.gcp_key_config is not None:
                    # bwsalmon/agents#126: unconditional, unlike the Gemini
                    # key above -- see `gcp_keys.py`'s own docstring for why
                    # this mirrors the old metadata broker's "every sandbox,
                    # every dispatch" behaviour rather than a task label.
                    #
                    # bwsalmon/agents#138: unlike the Gemini key, this one
                    # has no label gate, so a broken minter (bad IAM grant,
                    # a GCP outage, an expired minter key) would otherwise
                    # be a standing veto on *every* dispatch for as long as
                    # it stayed broken, if this were left to the general
                    # `except CommandError` below. Caught locally instead:
                    # fall back to `gcp_key_json = None`, the exact shape
                    # `dispatch()`/`configure_gcp_key` already treat as "no
                    # GCP key configured for this deployment" (see
                    # dispatch.py's own docstring), so the sandbox is
                    # dispatched in degraded mode -- without a key -- rather
                    # than not dispatched at all. The failure is still
                    # surfaced below once the dispatch outcome is known, so
                    # an agent can pick it up from the audit log.
                    try:
                        minted_gcp = create_gcp_key(self.base_runner, self.gcp_key_config)
                        gcp_key_json, gcp_key_id = minted_gcp.key_json, minted_gcp.key_id
                    except CommandError as exc:
                        gcp_key_mint_error = str(exc)
                if task.review:
                    # `_resolve_target` never returns `review=True` without
                    # `pr` also set -- it refuses that combination outright
                    # (bwsalmon/agents#154).
                    unit = dispatch_review(
                        sandbox_runner, self.base_runner, sandbox, sandbox_target,
                        task.pr,
                        remote_url=self._remote_url(task.repo), token=token,
                        task_repo=str(self._task), target_repo=str(task.repo),
                        task_issue=number,
                        gemini_key=gemini_key_string, gcp_key=gcp_key_json,
                        self_debug=task.self_debug, self_repair=task.self_repair,
                    )
                elif task.pr is not None:
                    review_comments = self.github.list_review_comments(
                        task.repo.owner, task.repo.name, task.pr.number
                    )
                    unit = dispatch_pr(
                        sandbox_runner, self.base_runner, sandbox, sandbox_target,
                        task.pr, review_comments,
                        remote_url=self._remote_url(task.repo), token=token,
                        thread_comments=prompt_comments, task_repo=str(self._task),
                        target_repo=str(task.repo), task_issue=number,
                        gemini_key=gemini_key_string, gcp_key=gcp_key_json,
                        self_debug=task.self_debug, self_repair=task.self_repair,
                    )
                else:
                    unit = dispatch(
                        sandbox_runner, self.base_runner, sandbox, sandbox_target,
                        prompt_issue,
                        remote_url=self._remote_url(task.repo), token=token,
                        base=task.base, comments=prompt_comments,
                        task_repo=str(self._task), target_repo=str(task.repo),
                        gemini_key=gemini_key_string, gcp_key=gcp_key_json,
                        self_debug=task.self_debug, self_repair=task.self_repair,
                    )
            except CommandError as exc:
                cleanup_errors: list[str] = []
                if gemini_key_name is not None:
                    # A key was minted but dispatch itself never reached the
                    # point of recording an Assignment for it to ride on --
                    # without this, it would never be revoked at all, since
                    # sweeper.py's `_release` only ever sees keys named on a
                    # real assignment. Best-effort: this cycle already has
                    # one failure to report; a second one revoking the
                    # orphaned key must not mask it.
                    try:
                        delete_gemini_key(self.base_runner, self.gemini_key_config, gemini_key_name)
                    except CommandError as cleanup_exc:
                        cleanup_errors.append(f"gemini API key: {cleanup_exc}")
                if gcp_key_id is not None:
                    # Same orphan-cleanup reasoning as the Gemini key above,
                    # for the GCP key (bwsalmon/agents#126).
                    try:
                        delete_gcp_key(self.base_runner, self.gcp_key_config, gcp_key_id)
                    except CommandError as cleanup_exc:
                        cleanup_errors.append(f"GCP service-account key: {cleanup_exc}")
                if cleanup_errors:
                    self.audit.record(
                        sandbox=sandbox, issue=number,
                        outcome=f"dispatch failed: {exc} (also failed to revoke the "
                                f"{'; '.join(cleanup_errors)})",
                    )
                    continue
                self.audit.record(sandbox=sandbox, issue=number,
                                   outcome=f"dispatch failed: {exc}")
                continue

            if task.review:
                self.state.assign(sandbox, number, unit, now,
                                   kind=TriggerKind.REVIEW, branch=task.pr.head_ref,
                                   pr_number=task.pr.number,
                                   target_owner=task.repo.owner,
                                   target_repo=task.repo.name, base=task.base,
                                   gemini_key_name=gemini_key_name,
                                   gcp_key_id=gcp_key_id,
                                   auto_merge=task.auto_merge)
            elif task.pr is not None:
                self.state.assign(sandbox, number, unit, now,
                                   kind=TriggerKind.PR, branch=task.pr.head_ref,
                                   target_owner=task.repo.owner,
                                   target_repo=task.repo.name, base=task.base,
                                   gemini_key_name=gemini_key_name,
                                   gcp_key_id=gcp_key_id,
                                   auto_merge=task.auto_merge)
            else:
                self.state.assign(sandbox, number, unit, now,
                                   target_owner=task.repo.owner,
                                   target_repo=task.repo.name, base=task.base,
                                   gemini_key_name=gemini_key_name,
                                   gcp_key_id=gcp_key_id,
                                   auto_merge=task.auto_merge)
            self.state.record_run(now)
            # Persist the new assignment *before* the trigger label comes
            # off below (bwsalmon/agents#51) -- removing that label is the
            # step that makes this dispatch irreversible from `_dispatch`'s
            # own polling's point of view (a labelled-issue query will never
            # see this issue again once it's off), so a controller crash or
            # VM restart after this point must find the assignment already
            # on disk, or the sweeper has nothing to reclaim it with. See
            # `state_path`'s own docstring for the full failure mode this
            # closes.
            self._save_state()
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                number, self.config.trigger_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                number, self.config.in_progress_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                number, agent_label(sandbox),
            )
            # bwsalmon/agents#83: harmless (and 404-tolerant, per
            # `remove_label`'s own docstring) for the overwhelming common
            # case of a task that never carried this label at all -- only a
            # `_suggest_fix`-filed task does, and this is what keeps it from
            # still reading "needs approval" once a human's approval (the
            # trigger label above) is exactly what just got it dispatched.
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                number, self.config.needs_approval_label,
            )
            self.audit.record(
                sandbox=sandbox, issue=number,
                outcome=(f"dispatched to {task.repo}"
                          + (f" (review of PR #{task.pr.number})" if task.review
                             else f" (PR #{task.pr.number})" if task.pr else "")
                          + (f" (degraded: GCP service-account key mint "
                             f"failed, dispatched without one: "
                             f"{gcp_key_mint_error})" if gcp_key_mint_error else "")),
            )
