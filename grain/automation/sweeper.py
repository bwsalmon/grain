"""The stranded-work sweeper.

docs/design.md: "if the host is stopped, or a run dies mid-flight, issues
need returning to the queue rather than stalling silently." Two distinct
ways that happens, both handled the same way here — release the sandbox and
report the issue back to `core.py` for requeuing:

- The unit finished (success or failure) — the ordinary case, checked every
  run since there is no long-lived process to notify the orchestrator when
  a job completes between cron invocations.
- The unit is unreachable (`UnitState.ABSENT` on a sandbox this state
  believes is assigned — the sandbox never got the job, or died with it) or
  still running past `max_runtime_minutes` — the actual "stranded" case.

This module knows nothing about GitHub; it only reads `unit_status` over an
injected per-sandbox `Runner` and mutates `AutomationState`. Label moves are
`core.py`'s job, which is the only piece holding a `GitHubClient`.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Callable

from .config import AutomationConfig
from .dispatch import UnitState, reap, unit_name, unit_status
from .state import AutomationState
from ..run import Runner


@dataclass(frozen=True)
class Outcome:
    sandbox: str
    issue: int


@dataclass(frozen=True)
class SweepResult:
    succeeded: list[Outcome] = field(default_factory=list)
    failed: list[Outcome] = field(default_factory=list)
    stranded: list[Outcome] = field(default_factory=list)


def sweep(state: AutomationState, ssh_runner_for: Callable[[str], Runner],
          config: AutomationConfig, now: datetime) -> SweepResult:
    result = SweepResult()
    max_runtime = timedelta(minutes=config.max_runtime_minutes)
    for sandbox, assignment in list(state.assignments.items()):
        runner = ssh_runner_for(sandbox)
        unit = unit_name(sandbox)
        status = unit_status(runner, unit)
        outcome = Outcome(sandbox=sandbox, issue=assignment.issue)

        if status is UnitState.DONE_SUCCESS:
            reap(runner, unit)
            state.release(sandbox)
            result.succeeded.append(outcome)
        elif status is UnitState.DONE_FAILED:
            reap(runner, unit)
            state.release(sandbox)
            result.failed.append(outcome)
        elif status is UnitState.ABSENT:
            # Assigned in our state but the unit isn't there — never
            # started (a dispatch that failed partway) or the sandbox was
            # recreated out from under it. Nothing to reap.
            state.release(sandbox)
            result.stranded.append(outcome)
        elif now - assignment.started_at > max_runtime:
            reap(runner, unit)
            state.release(sandbox)
            result.stranded.append(outcome)
        # else: ACTIVE and within budget — leave it running.
    return result
