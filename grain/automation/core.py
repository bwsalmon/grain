"""The orchestrator's decision logic for one `run-once` invocation.

Order, mirroring `grain/proxy/core.py`'s own "order matters and mirrors
docs/design.md" convention:

    sweep first, so a sandbox a finished or stranded run just freed is
    available to the same cycle's dispatch pass rather than sitting idle
    for one more `run-once` interval — a *successful* sweep also verifies
    the pushed branch and opens the PR, since that is the other half of
    "this run is really done", not a separate pass,
    list open trigger-labelled issues not already tracked as in-progress,
    oldest first, so a backlog drains in the order it was filed,
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

docs/roadmap.md item 9 adds a second intake path: dispatching to an
*existing* PR instead of a labelled issue, to address review feedback, fix
CI, or continue work. It reuses this same pool and rate limit rather than
adding a second budget — a PR-triggered run occupies a sandbox exactly like
an issue-triggered one, and the whole point of `runs_per_hour` is capping
total dispatch volume regardless of what triggered it. What's new:

- **`_dispatch` polls both `list_issues` and `list_pull_requests`** (same
  `trigger_label`, since the human-gate reasoning is identical either way —
  a label a person applied), merges the two candidate lists, and sorts the
  merge by number alone. That sort is sound with no separate interleaving
  policy because issues and PRs share one number sequence per GitHub repo (a
  PR is a special kind of issue in GitHub's own data model) — a PR and an
  issue can never tie, so "process the oldest-numbered trigger first"
  already means "process whichever was filed/opened first," across both
  kinds at once.
- **`AutomationState.Assignment` carries a `kind`.** An issue assignment's
  branch is always recomputable from `branch_name(issue)`; a PR assignment's
  branch is whatever the PR's author already called it, so it's recorded at
  dispatch time instead — see `state.py`'s `Assignment` docstring.
- **`_finish_succeeded` branches on `outcome.kind`.** An issue success still
  means "open a new PR"; a PR success means the PR already exists and the
  agent just pushed more commits to it — verified the same way (does the
  branch it was told to push to still exist?), but with no PR to create and
  no new label story: only the in-progress label comes off. Both branches
  route through the same `_requeue` on failure/no-branch, unchanged — a
  requeue only ever needs the trigger's own number, which is `outcome.issue`
  regardless of kind.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Callable

from . import ratelimit
from .audit import AuditLog, NullAuditLog
from .config import AutomationConfig
from .dispatch import (
    CONTROLLER_AGENT_SSH_KEY_PATH, SandboxTarget, branch_name, dispatch, dispatch_pr,
    question_path, unit_name,
)
from .github import GitHubClient, GitHubError, Issue, PullRequestDetail
from .history import NullSessionHistory, SessionHistory
from .ssh import SshRunner
from .state import AutomationState, TriggerKind
from .sweeper import Outcome, sweep
from ..inventory import GIT_PROXY_PORT, Cluster
from ..proxy.tokens import SandboxTokenStore
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

    def _remote_url(self) -> str:
        # Always through the git proxy, on the controller's own address —
        # never GitHub directly (docs/design.md, "GitHub access").
        return (
            f"http://{self.cluster.controller_ip}:{GIT_PROXY_PORT}/"
            f"{self.config.owner}/{self.config.repo}.git"
        )

    def run_once(self, now: datetime) -> None:
        self._sweep(now)
        self._promote_answered_questions(now)
        self._dispatch(now)

    # --- sweep --------------------------------------------------------
    def _sweep(self, now: datetime) -> None:
        result = sweep(self.state, self._ssh_runner_for, self.base_runner,
                        self.config, now, history=self.history)
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
        branch = branch_name(outcome.issue)
        if not self.github.branch_exists(self.config.owner, self.config.repo, branch):
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
        pr = self.github.create_pull_request(
            self.config.owner, self.config.repo,
            head=branch, base=self.config.base_branch,
            title=f"🤖 grain: fix #{outcome.issue}",
            body=f"Closes #{outcome.issue}.\n\nOpened automatically after "
                 f"issue #{outcome.issue} finished on {outcome.sandbox}."
                 f"\n\n---\n{_AUTOMATION_SIGNATURE}",
        )
        self.github.remove_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"opened PR #{pr.number}: {pr.html_url}")

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
        if not self.github.branch_exists(self.config.owner, self.config.repo, branch):
            # Same "verify, don't trust" bar as the issue path (docs/roadmap.md
            # item 2): the unit exiting zero isn't proof the agent pushed
            # anything, or pushed to the branch it was told to.
            self._requeue(outcome, f"succeeded but branch {branch!r} does not exist")
            return

        # No PR to open — it already exists, which is the whole premise of
        # this trigger — so a successful finish is just "stop marking it
        # in-progress." Unlike the issue path there is no trigger label to
        # withhold either: the PR is simply no longer being worked, and a
        # human can label it again for another round if more feedback comes
        # in, the same way they would for a fresh issue.
        self.github.remove_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.audit.record(
            sandbox=outcome.sandbox, issue=outcome.issue,
            outcome=f"pushed additional commits to PR #{outcome.issue} ({branch!r})",
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
                self.config.owner, self.config.repo, outcome.issue,
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
                        f"{self.config.owner}/{self.config.repo} -- stale assignment? "
                        f"question was: {question[:200]!r}",
            )
            return
        self.github.remove_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.add_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.awaiting_reply_label,
        )
        self.state.record_pending_question(
            outcome.issue, comment_id, kind=outcome.kind, branch=outcome.branch,
        )
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                           outcome=f"asked a question, awaiting reply: {question[:200]!r}")

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
                    self.config.owner, self.config.repo, pending.issue
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
                            f"{self.config.owner}/{self.config.repo} while "
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
                self.config.owner, self.config.repo,
                pending.issue, self.config.awaiting_reply_label,
            )
            self.github.add_label(
                self.config.owner, self.config.repo,
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
                self.config.owner, self.config.repo,
                outcome.issue, self.config.in_progress_label,
            )
            self.github.add_label(
                self.config.owner, self.config.repo,
                outcome.issue, self.config.trigger_label,
            )
        except GitHubError as exc:
            if exc.status != 404:
                raise
            self.audit.record(
                sandbox=outcome.sandbox, issue=outcome.issue,
                outcome=f"{reason} (requeue skipped: issue #{outcome.issue} not found in "
                        f"{self.config.owner}/{self.config.repo} -- stale assignment?)",
            )
            return
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue, outcome=reason)

    # --- dispatch -------------------------------------------------------
    def _dispatch(self, now: datetime) -> None:
        issue_candidates = self.github.list_issues(
            self.config.owner, self.config.repo, self.config.trigger_label
        )
        pr_candidates = self.github.list_pull_requests(
            self.config.owner, self.config.repo, self.config.trigger_label
        )
        in_progress = self.state.in_progress_issues()
        # Issues and PRs share one number sequence per GitHub repo (a PR is
        # a special kind of issue in GitHub's own data model), so merging
        # both lists and sorting by number alone already processes the
        # combined backlog oldest-first — no separate interleaving policy
        # needed between the two trigger kinds.
        queue: list[tuple[int, Issue | PullRequestDetail]] = sorted(
            [(i.number, i) for i in issue_candidates if i.number not in in_progress]
            + [(p.number, p) for p in pr_candidates if p.number not in in_progress],
            key=lambda t: t[0],
        )

        for number, item in queue:
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
            target = SandboxTarget(
                address=str(self.cluster.address_of(sandbox)),
                ssh_user=self.config.ssh_user,
                ssh_key_path=CONTROLLER_AGENT_SSH_KEY_PATH,
            )
            token = self.token_store.ensure_token(sandbox)
            # Fetched for every dispatch, issue or PR alike, whether or not
            # a prior attempt ever asked a question (docs/roadmap.md item
            # 12) -- `AutomationState` carries no memory of that once an
            # assignment is released, so this is the only way a redispatch
            # sees a human's reply. Empty for the (common) case of no
            # conversation yet; `_prompt`/`_pr_prompt` render that plainly
            # rather than omitting the section, matching the existing
            # blank-state convention for review comments.
            thread_comments = self.github.list_comments(
                self.config.owner, self.config.repo, number
            )
            # A `CommandError` here (an SSH/command failure anywhere in
            # dispatch()/dispatch_pr()'s path -- ensure_workspace,
            # configure_git_credentials, starting the unit) must not take
            # down every other candidate still queued this cycle. Found
            # live (docs/next-session.md): a proxy-auth failure on one
            # sandbox crashed `run_once` before it ever reached the next
            # candidate. Neither the sandbox nor the issue's labels are
            # touched below on failure -- the sandbox stays free and the
            # issue keeps its trigger label, so both are simply retried on
            # a later cycle, same "log and move on" discipline
            # `_requeue`/`_finish_question` already apply to a GitHub-side
            # 404. Only `CommandError` specifically: anything else is a
            # real bug, not an expected failure mode, and should still
            # surface immediately.
            try:
                if isinstance(item, PullRequestDetail):
                    comments = self.github.list_review_comments(
                        self.config.owner, self.config.repo, number
                    )
                    unit = dispatch_pr(sandbox_runner, self.base_runner, sandbox, target,
                                        item, comments, remote_url=self._remote_url(), token=token,
                                        thread_comments=thread_comments)
                else:
                    unit = dispatch(sandbox_runner, self.base_runner, sandbox, target, item,
                                     remote_url=self._remote_url(), token=token,
                                     comments=thread_comments)
            except CommandError as exc:
                self.audit.record(sandbox=sandbox, issue=number,
                                   outcome=f"dispatch failed: {exc}")
                continue

            if isinstance(item, PullRequestDetail):
                self.state.assign(sandbox, number, unit, now,
                                   kind=TriggerKind.PR, branch=item.head_ref)
            else:
                self.state.assign(sandbox, number, unit, now)
            self.state.record_run(now)
            self.github.remove_label(
                self.config.owner, self.config.repo,
                number, self.config.trigger_label,
            )
            self.github.add_label(
                self.config.owner, self.config.repo,
                number, self.config.in_progress_label,
            )
            self.audit.record(sandbox=sandbox, issue=number,
                               outcome="dispatched")
