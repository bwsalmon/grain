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
import re
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Callable

from . import ratelimit
from .audit import AuditLog, NullAuditLog
from .config import AutomationConfig
from .directives import DirectiveError, RepoRef, parse_directives, strip_directives
from .dispatch import (
    CONTROLLER_AGENT_SSH_KEY_PATH, SandboxTarget, UnitState, branch_name, comment_path,
    dispatch, dispatch_pr, question_path, unit_name,
)
from .gcp_keys import GcpKeyConfig
from .gcp_keys import create_key as create_gcp_key
from .gcp_keys import delete_expired_keys as delete_expired_gcp_keys
from .gcp_keys import delete_key as delete_gcp_key
from .gemini_keys import GeminiKeyConfig
from .gemini_keys import create_key as create_gemini_key
from .gemini_keys import delete_key as delete_gemini_key
from .github import Comment, GitHubClient, GitHubError, Issue, PullRequestDetail
from .history import NullSessionHistory, SessionHistory
from .janitor import JanitorConfig, run_janitor
from .labels import agent_label
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

# A consistent, visible marker on everything grain-agent itself posts to
# GitHub (docs/roadmap.md item 14) -- so a comment or PR is immediately
# recognizable as automation output at a glance, regardless of which
# credential actually posted it (a personal PAT and a dedicated machine
# account otherwise look identical in a thread).
_AUTOMATION_SIGNATURE = "🤖 _Posted automatically by grain-agent — not a human._"

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
        self._promote_answered_questions(now)
        self._close_finished_prs()
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

    def _finish_succeeded(self, outcome: Outcome) -> None:
        if outcome.kind is TriggerKind.PR:
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

        # PR first, in-progress label off second: if create_pull_request
        # fails partway (a 422 from a stale PR, a transient 5xx), the issue
        # stays visibly in-progress rather than looking finished with
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
        pr = self.github.create_pull_request(
            target.owner, target.name,
            head=branch, base=base,
            title=f"🤖 grain: {task}#{outcome.issue}: {issue.title}",
            body=f"{head.message.strip()}\n\n---\n"
                 f"Closes {task}#{outcome.issue}.\n\n{_AUTOMATION_SIGNATURE}",
        )
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
        # bwsalmon/agents#51: persist the open-PR record right after the
        # label moves that go with it, before anything later in this cycle
        # can crash on top of it -- the same ordering `_finish_question`
        # already uses for its own pending-state record.
        self._save_state()
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"opened PR {target}#{pr.number}: {pr.html_url}")

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
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome=f"pushed additional commits to {target} ({branch!r})",
        )

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
                 and c.author_association in _TRUSTED_REPLY_ASSOCIATIONS),
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
                    f"Apply the `{self.config.trigger_label}` label to this issue to "
                    "let the agent set attempt it.\n\n"
                    f"/repo {target}\n/base {pr.head_ref}\n/auto-merge true\n"
                ),
                labels=[self.config.needs_approval_label],
            )
            self.github.create_comment(
                task.owner, task.name, pending.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                f"{target}#{pending.pr_number} {reason} -- filed {task}#{new_issue.number} "
                f"to fix it. Apply the `{self.config.trigger_label}` label there once "
                "you're happy for the agent to attempt it.",
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

    def _resolve_target(self, issue: Issue, comments: list[Comment]) -> "ResolvedTask":
        """What a task's own text says to work on: which repo, optionally
        which PR to continue, and what base a new PR should target.

        Reads directives from the issue body plus every *trusted* comment
        on it (`_TRUSTED_REPLY_ASSOCIATIONS`, the same "could have applied
        the label" tier the trigger gate itself relies on), later texts
        overriding earlier — so repairing a task is a reply, not an edit
        plus a reply. Raises `DirectiveError` for anything unusable; every
        message is written to be posted verbatim by `_park`.
        """
        texts = [issue.body] + [
            c.body for c in comments
            if c.author_association in _TRUSTED_REPLY_ASSOCIATIONS
        ]
        directives = parse_directives(texts)
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
        target = directives.target
        if target is None:
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
        return ResolvedTask(repo=target, pr=pr, base=directives.base or default_branch,
                            gemini_key=gemini_key, self_debug=self_debug,
                            self_repair=self_repair,
                            github_key=github_key, auto_merge=directives.auto_merge)

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
                task = self._resolve_target(issue, thread_comments)
            except DirectiveError as exc:
                # No sandbox consumed and no rate-limit slot spent: parking
                # is bookkeeping on the task repo, not a run.
                self._park(number, str(exc))
                continue

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
            # configure_git_credentials, starting the unit; gemini_keys
            # .create_key's own gcloud calls below (bwsalmon/agents#47); or
            # gcp_keys.create_key's own gcloud calls (bwsalmon/agents#126))
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
            gemini_key_string: str | None = None
            gemini_key_name: str | None = None
            gcp_key_json: str | None = None
            gcp_key_id: str | None = None
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
                    minted_gcp = create_gcp_key(self.base_runner, self.gcp_key_config)
                    gcp_key_json, gcp_key_id = minted_gcp.key_json, minted_gcp.key_id
                if task.pr is not None:
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

            if task.pr is not None:
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
                          + (f" (PR #{task.pr.number})" if task.pr else "")),
            )
