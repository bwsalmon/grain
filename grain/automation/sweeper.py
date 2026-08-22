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
injected per-sandbox `Runner` and mutates `AutomationState`. Label moves and
PR creation are `core.py`'s job, which is the only piece holding a
`GitHubClient`.

**Every release also runs the between-task cleanup hook and a health check**
(docs/roadmap.md item 5, docs/design.md step 8) — the moment a sandbox's
slot is freed for reuse is exactly the point `cleanup.py`'s own docstring
argues for: guaranteed-clean before the next dispatch, not "clean once some
other job gets to it." This runs for all four release paths uniformly,
`UnitState.ABSENT` included — a "never started" dispatch can still have left
a partial workspace or a stray container behind, and a sandbox that really
was recreated out from under this state just gets a harmless no-op (nothing
to clean, everything healthy). Both `cleanup()` and `check_health()` already
tolerate an unreachable sandbox without raising (see their own docstrings),
so this never turns "couldn't reach it" into a crashed sweep.

Health results are collected as `SweepResult.health_warnings` rather than
acted on here: this module's whole job is deciding "is this sandbox free
now," and a health problem doesn't change that answer — quarantining an
unhealthy sandbox out of the dispatch pool would be a real change to
`AutomationState`'s pool-assignment shape, out of scope for what this item
is (visibility, per docs/design.md step 8's own "log line... just
visibility" bar, not new lifecycle machinery). `core.py` turns each warning
into an audit-log line an operator can see; `grain host health` is what
finds the same thing on demand, for a sandbox that isn't mid-sweep at all.

**docs/roadmap.md item 10: every release also captures the session's
trajectory, before the slot is freed.** This is the same "guaranteed before
slot reuse" argument that already governs cleanup/health here: a browse
attempted later would otherwise find nothing (a reused sandbox's next task
overwrites the same fixed transcript path — see `dispatch.py`'s
`transcript_path`, keyed only by sandbox, not by task) or, worse, another
task's content passed off as this one's. `capture.py`'s `capture_trajectory`
never raises (same discipline `cleanup()`/`check_health()` already hold to
for this call site), so a capture problem — nothing written, a `cat`
failure — never blocks a sandbox's slot from freeing; it just means this
session's history entry records no transcript. Recorded into an injected
`SessionHistory` (`history.py`) — `NullSessionHistory` by default, so every
existing caller of `sweep()` that doesn't care about history keeps working
unchanged; `core.py` wires in a real `FileSessionHistory` in production.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Callable

from .capture import capture_trajectory
from .cleanup import cleanup
from .config import AutomationConfig
from .dispatch import UnitState, reap, unit_name, unit_status
from .health import check_health
from .history import NullSessionHistory, SessionHistory
from .state import Assignment, AutomationState, TriggerKind
from ..run import Runner


@dataclass(frozen=True)
class Outcome:
    sandbox: str
    issue: int
    # Carried straight from the freed Assignment (docs/roadmap.md item 9) —
    # this module still knows nothing about GitHub, but core.py's
    # `_finish_succeeded` needs to know which kind of trigger this was and,
    # for a PR, which branch to verify, and the assignment is gone the
    # instant `_release` below calls `state.release`.
    kind: TriggerKind = TriggerKind.ISSUE
    branch: str | None = None


@dataclass(frozen=True)
class HealthWarning:
    sandbox: str
    detail: str


@dataclass(frozen=True)
class SweepResult:
    succeeded: list[Outcome] = field(default_factory=list)
    failed: list[Outcome] = field(default_factory=list)
    stranded: list[Outcome] = field(default_factory=list)
    health_warnings: list[HealthWarning] = field(default_factory=list)


def _release(state: AutomationState, runner: Runner, sandbox: str, *,
             history: SessionHistory, assignment: Assignment, unit: str,
             outcome_label: str, now: datetime) -> HealthWarning | None:
    """Captures the session's trajectory, runs the between-task hook and a
    post-cleanup health check, then frees the slot. Called from every
    branch below that frees a sandbox, so the capture/cleanup/health
    behaviour is identical regardless of why the slot is being freed.

    Capture runs first, before cleanup or the state release — a transcript
    is what `claude -p` already wrote under `/tmp` (docs/roadmap.md item
    10), and neither `cleanup()` nor freeing the slot touches that path, but
    reading it before either still keeps "capture happens before a sandbox
    could possibly be reused" true by construction rather than by the
    accident of what cleanup happens not to touch today.
    """
    transcript_text = capture_trajectory(runner, unit)
    history.record(
        issue=assignment.issue, kind=assignment.kind, sandbox=sandbox, unit=unit,
        started_at=assignment.started_at, finished_at=now, outcome=outcome_label,
        transcript_text=transcript_text,
    )
    cleanup(runner)
    report = check_health(runner)
    state.release(sandbox)
    if not report.ok:
        return HealthWarning(sandbox, f"{report.status.value}: {report.summary()}")
    return None


def sweep(state: AutomationState, ssh_runner_for: Callable[[str], Runner],
          config: AutomationConfig, now: datetime,
          history: SessionHistory | None = None) -> SweepResult:
    if history is None:
        history = NullSessionHistory()
    result = SweepResult()
    max_runtime = timedelta(minutes=config.max_runtime_minutes)
    for sandbox, assignment in list(state.assignments.items()):
        runner = ssh_runner_for(sandbox)
        unit = unit_name(sandbox)
        status = unit_status(runner, unit)
        outcome = Outcome(sandbox=sandbox, issue=assignment.issue,
                           kind=assignment.kind, branch=assignment.branch)

        if status is UnitState.DONE_SUCCESS:
            reap(runner, unit)
            warning = _release(state, runner, sandbox, history=history,
                                assignment=assignment, unit=unit,
                                outcome_label="succeeded", now=now)
            result.succeeded.append(outcome)
        elif status is UnitState.DONE_FAILED:
            reap(runner, unit)
            warning = _release(state, runner, sandbox, history=history,
                                assignment=assignment, unit=unit,
                                outcome_label="failed", now=now)
            result.failed.append(outcome)
        elif status is UnitState.ABSENT:
            # Assigned in our state but the unit isn't there — never
            # started (a dispatch that failed partway) or the sandbox was
            # recreated out from under it. Nothing to reap.
            warning = _release(state, runner, sandbox, history=history,
                                assignment=assignment, unit=unit,
                                outcome_label="stranded", now=now)
            result.stranded.append(outcome)
        elif now - assignment.started_at > max_runtime:
            reap(runner, unit)
            warning = _release(state, runner, sandbox, history=history,
                                assignment=assignment, unit=unit,
                                outcome_label="stranded", now=now)
            result.stranded.append(outcome)
        else:
            # ACTIVE and within budget — leave it running, and leave its
            # slot alone: cleanup/health only run on a sandbox that's
            # actually being freed.
            continue
        if warning is not None:
            result.health_warnings.append(warning)
    return result
