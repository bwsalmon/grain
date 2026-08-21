"""The orchestrator's decision logic for one `run-once` invocation.

Order, mirroring `grain/proxy/core.py`'s own "order matters and mirrors
docs/design.md" convention:

    sweep first, so a sandbox a finished or stranded run just freed is
    available to the same cycle's dispatch pass rather than sitting idle
    for one more `run-once` interval,
    list open trigger-labelled issues not already tracked as in-progress,
    oldest first, so a backlog drains in the order it was filed,
    while a free sandbox exists and the rate limit allows it, dispatch,
    move the label, and record the assignment,
    stop — cron will call again.

Cron, not a loop: docs/design.md's issue-intake section is explicit that
polling (not webhooks) is what keeps the host closed to inbound traffic.
`Orchestrator.run_once` is meant to be invoked by a systemd timer, once per
call, not run as a daemon.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Callable

from . import ratelimit
from .audit import AuditLog, NullAuditLog
from .config import AutomationConfig
from .dispatch import dispatch
from .github import GitHubClient
from .ssh import SshRunner
from .state import AutomationState
from .sweeper import Outcome, sweep
from ..inventory import Cluster
from ..run import Runner


@dataclass
class Orchestrator:
    cluster: Cluster
    github: GitHubClient
    config: AutomationConfig
    state: AutomationState
    base_runner: Runner
    audit: AuditLog | None = None
    # Overridable seam for tests: production leaves this None and gets a
    # real `SshRunner` per sandbox; a test can inject a lookup straight to
    # per-sandbox fakes without needing to match SshRunner's exact argv.
    ssh_runner_factory: Callable[[str], Runner] | None = None

    def __post_init__(self) -> None:
        if self.audit is None:
            self.audit = NullAuditLog()

    def _ssh_runner_for(self, sandbox: str) -> Runner:
        if self.ssh_runner_factory is not None:
            return self.ssh_runner_factory(sandbox)
        return SshRunner(
            inner=self.base_runner,
            user=self.config.ssh_user,
            address=self.cluster.address_of(sandbox),
            key_path=self.config.ssh_key_path,
        )

    def run_once(self, now: datetime) -> None:
        self._sweep(now)
        self._dispatch(now)

    # --- sweep --------------------------------------------------------
    def _sweep(self, now: datetime) -> None:
        result = sweep(self.state, self._ssh_runner_for, self.config, now)
        for outcome in result.succeeded:
            self.github.remove_label(
                self.config.owner, self.config.repo,
                outcome.issue, self.config.in_progress_label,
            )
            self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue,
                               outcome="succeeded")
        for outcome in (*result.failed, *result.stranded):
            reason = "failed" if outcome in result.failed else "stranded"
            self._requeue(outcome, reason)

    def _requeue(self, outcome: Outcome, reason: str) -> None:
        # Back to the trigger label, per docs/design.md: "issues need
        # returning to the queue rather than stalling silently."
        self.github.remove_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.in_progress_label,
        )
        self.github.add_label(
            self.config.owner, self.config.repo,
            outcome.issue, self.config.trigger_label,
        )
        self.audit.record(sandbox=outcome.sandbox, issue=outcome.issue, outcome=reason)

    # --- dispatch -------------------------------------------------------
    def _dispatch(self, now: datetime) -> None:
        candidates = self.github.list_issues(
            self.config.owner, self.config.repo, self.config.trigger_label
        )
        in_progress = self.state.in_progress_issues()
        queue = sorted(
            (i for i in candidates if i.number not in in_progress),
            key=lambda i: i.number,
        )

        for issue in queue:
            sandbox = self.state.free_sandbox(self.cluster.sandbox_names)
            if sandbox is None:
                self.audit.record(sandbox=None, issue=issue.number,
                                   outcome="skipped: no free sandbox")
                break
            if not ratelimit.allow(self.state.run_timestamps, now,
                                    self.config.runs_per_hour):
                self.audit.record(sandbox=None, issue=issue.number,
                                   outcome="skipped: rate limit")
                break

            runner = self._ssh_runner_for(sandbox)
            unit = dispatch(runner, sandbox, issue)
            self.state.assign(sandbox, issue.number, unit, now)
            self.state.record_run(now)
            self.github.remove_label(
                self.config.owner, self.config.repo,
                issue.number, self.config.trigger_label,
            )
            self.github.add_label(
                self.config.owner, self.config.repo,
                issue.number, self.config.in_progress_label,
            )
            self.audit.record(sandbox=sandbox, issue=issue.number,
                               outcome="dispatched")
