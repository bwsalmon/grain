"""The orchestrator's decision logic for one `run-once` invocation.

Order, mirroring `grain/proxy/core.py`'s own "order matters and mirrors
docs/design.md" convention:

    sweep first, so a sandbox a finished or stranded run just freed is
    available to the same cycle's dispatch pass rather than sitting idle
    for one more `run-once` interval — a *successful* sweep also verifies
    the pushed branch and opens the PR, since that is the other half of
    "this run is really done", not a separate pass,
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
    CONTROLLER_AGENT_SSH_KEY_PATH, SandboxTarget, analysis_path, branch_name, dispatch,
    dispatch_pr, question_path, unit_name,
)
from .gemini_keys import GeminiKeyConfig
from .gemini_keys import create_key as create_gemini_key
from .gemini_keys import delete_key as delete_gemini_key
from .github import Comment, GitHubClient, GitHubError, Issue, PullRequestDetail
from .history import NullSessionHistory, SessionHistory
from .ssh import SshRunner
from .state import AutomationState, TriggerKind
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


def _pending_analysis(sandbox: str) -> str | None:
    """Reads back a `complete_analysis` call, if the agent made one this
    run (bwsalmon/agents#50) -- the same file-based handoff `_pending_question`
    already uses for `ask_question`: `mcp_server.py`'s tool and this read
    both run on the controller, so no `Runner`/SSH is involved, and absence
    just means "no analysis to report," never an exception.
    """
    try:
        text = Path(analysis_path(unit_name(sandbox))).read_text().strip()
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
    # Whether the task's text carried a `/gemini-key` directive
    # (bwsalmon/agents#47) -- `_resolve_target` already refuses one this
    # deployment can't honour (no `GeminiKeyConfig` configured), so by the
    # time this reaches `_dispatch` it is always safe to act on.
    gemini_key: bool = False
    # The named credential a `grain-github-<name>` label asked for
    # (bwsalmon/agents#52), or `None` for the overwhelming common case of no
    # such label. `_resolve_target` already refuses a name this deployment
    # has no `<name>.token` for, so by the time this reaches `_dispatch` it
    # is always safe to act on.
    github_key: str | None = None


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
    # bwsalmon/agents#47: the on/off switch for `/gemini-key`. `None`
    # (production's default for a deployment that never ran `grain
    # controller configure --gemini-project-id ...`) makes `_resolve_target`
    # refuse the directive with an explanation, the same "unusable directive
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

    def __post_init__(self) -> None:
        if self.audit is None:
            self.audit = NullAuditLog()
        if self.history is None:
            self.history = NullSessionHistory()

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
        self._promote_answered_questions(now)
        self._dispatch(now)

    # --- sweep --------------------------------------------------------
    def _sweep(self, now: datetime) -> None:
        result = sweep(self.state, self._ssh_runner_for, self.base_runner,
                        self.config, now, history=self.history,
                        gemini_key_config=self.gemini_key_config,
                        credential_store=self.credential_store)
        for outcome in result.succeeded:
            self._finish_succeeded(outcome)
        for outcome in (*result.failed, *result.stranded):
            reason = "failed" if outcome in result.failed else "stranded"
            self._requeue(outcome, reason)
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

    def _finish_succeeded(self, outcome: Outcome) -> None:
        if outcome.kind is TriggerKind.PR:
            self._finish_succeeded_pr(outcome)
        else:
            self._finish_succeeded_issue(outcome)

    def _finish_succeeded_issue(self, outcome: Outcome) -> None:
        question = _pending_question(outcome.sandbox)
        if question is not None:
            self._finish_question(outcome, question)
            return
        analysis = _pending_analysis(outcome.sandbox)
        if analysis is not None:
            self._finish_analysis(outcome, analysis)
            return
        target = self._target_of(outcome)
        branch = branch_name(outcome.issue)
        if not self.github.branch_exists(target.owner, target.name, branch):
            # The unit exited zero, but that is not the same claim as "a PR
            # can be opened" — the agent may never have pushed, or pushed
            # somewhere other than the branch dispatch() told it to. Verify,
            # don't trust (docs/roadmap.md item 2), and make the gap visible
            # rather than treating a branchless "success" as done.
            self._requeue(outcome, f"succeeded but branch {branch!r} does not exist")
            return

        # PR first, in-progress label off second: if create_pull_request
        # fails partway (a 422 from a stale PR, a transient 5xx), the issue
        # stays visibly in-progress rather than looking finished with
        # nothing to show for it.
        # `Closes <task repo>#<n>`, fully qualified: still worth the
        # cross-repo link/mention on the issue even though (bwsalmon/agents#23)
        # a qualified `Closes` reference never auto-closes across repos —
        # GitHub only auto-closes within the same repo the PR is opened in.
        # The task issue is closed explicitly below instead.
        task = self._task
        # The issue's title isn't on hand here — `Outcome` only carries the
        # number (see sweeper.py's `Outcome` docstring on what does and
        # doesn't survive to finish time) — so it's read fresh rather than
        # threaded through, the same "one more call, no stale copy to keep
        # in sync" trade-off `get_pull_request` already makes elsewhere.
        issue = self.github.get_issue(task.owner, task.name, outcome.issue)
        base = outcome.base or self.github.default_branch(target.owner, target.name)
        pr = self.github.create_pull_request(
            target.owner, target.name,
            head=branch, base=base,
            title=f"🤖 grain: {task}#{outcome.issue}: {issue.title}",
            body=f"Closes {task}#{outcome.issue}.\n\nOpened automatically after "
                 f"task {task}#{outcome.issue} finished on {outcome.sandbox}."
                 f"\n\n---\n{_AUTOMATION_SIGNATURE}",
        )
        # The `Closes` text above never auto-closes across repos
        # (bwsalmon/agents#23), so the task issue is closed explicitly here
        # rather than left to rely on it.
        self.github.close_issue(task.owner, task.name, outcome.issue)
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, self.config.in_progress_label,
        )
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
        # comes in, the same way they would for a fresh task.
        self.github.remove_label(
            self.config.task_owner, self.config.task_repo,
            outcome.issue, self.config.in_progress_label,
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
        self.state.record_pending_question(
            outcome.issue, comment_id, kind=outcome.kind, branch=outcome.branch,
        )
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"asked a question, awaiting reply: {question[:200]!r}")

    def _finish_analysis(self, outcome: Outcome, summary: str) -> None:
        """The agent called `complete_analysis` instead of pushing a branch
        (bwsalmon/agents#50) -- some tasks only ever needed an answer, an
        investigation, or a recommendation, not a code change, and forcing
        those through the branch/PR path is a poor fit. Unlike
        `_finish_question`, this is a genuine finish, not a park: the
        summary is posted as the closing comment and the issue is closed
        outright, the same "post first, then update state" order
        `_finish_succeeded_issue`'s own PR path already uses -- if
        `create_comment` fails partway there is nothing yet to roll back.
        No branch is ever checked and no PR is opened; that is the whole
        point of this path over the default one.

        A 404 here means the same thing it means in `_finish_question`/
        `_requeue`: a stale assignment against an issue that's since
        changed out from under it. Best-effort only in that case -- there
        is no issue left to close or label either.
        """
        task = self._task
        try:
            self.github.create_comment(
                task.owner, task.name, outcome.issue,
                f"{_AUTOMATION_SIGNATURE}\n\n"
                "This task has been completed as an analysis -- no code "
                f"change was needed:\n\n{summary}",
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=outcome.sandbox, issue=outcome.issue,
                outcome=f"completed analysis but issue #{outcome.issue} not found in "
                        f"{task.owner}/{task.name} -- stale assignment? "
                        f"summary was: {summary[:200]!r}",
            )
            return
        self.github.close_issue(task.owner, task.name, outcome.issue)
        self.github.remove_label(
            task.owner, task.name,
            outcome.issue, self.config.in_progress_label,
        )
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"completed analysis: {summary[:200]!r}")

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
            self.audit.record(
                sandbox=None, issue=pending.issue,
                outcome=f"{reply.user} ({reply.author_association}) replied -- "
                        "requeued for redispatch",
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
        if directives.gemini_key and self.gemini_key_config is None:
            # Same "unusable directive parks the task" shape as an unlisted
            # `/repo` below -- checked before the target/allowlist reads,
            # so a task that can never be honoured is parked without also
            # spending a GitHub call on a repo it will never reach.
            raise DirectiveError(
                "this task has a `/gemini-key` directive, but this "
                "deployment has no Gemini key support configured. An "
                "operator enables it with `grain controller configure "
                "--gemini-project-id ...` (see gemini_keys.py)."
            )
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
                            gemini_key=directives.gemini_key, github_key=github_key)

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
            # configure_git_credentials, starting the unit; or, new for
            # bwsalmon/agents#47, gemini_keys.create_key's own gcloud calls
            # below) must not take down every other candidate still queued
            # this cycle. Found live (docs/next-session.md): a proxy-auth
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
                        gemini_key=gemini_key_string,
                    )
                else:
                    unit = dispatch(
                        sandbox_runner, self.base_runner, sandbox, sandbox_target,
                        prompt_issue,
                        remote_url=self._remote_url(task.repo), token=token,
                        base=task.base, comments=prompt_comments,
                        task_repo=str(self._task), target_repo=str(task.repo),
                        gemini_key=gemini_key_string,
                    )
            except CommandError as exc:
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
                        self.audit.record(
                            sandbox=sandbox, issue=number,
                            outcome=f"dispatch failed: {exc} (also failed to revoke the "
                                    f"gemini API key it minted: {cleanup_exc})",
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
                                   gemini_key_name=gemini_key_name)
            else:
                self.state.assign(sandbox, number, unit, now,
                                   target_owner=task.repo.owner,
                                   target_repo=task.repo.name, base=task.base,
                                   gemini_key_name=gemini_key_name)
            self.state.record_run(now)
            self.github.remove_label(
                self.config.task_owner, self.config.task_repo,
                number, self.config.trigger_label,
            )
            self.github.add_label(
                self.config.task_owner, self.config.task_repo,
                number, self.config.in_progress_label,
            )
            self.audit.record(
                sandbox=sandbox, issue=number,
                outcome=(f"dispatched to {task.repo}"
                          + (f" (PR #{task.pr.number})" if task.pr else "")),
            )
