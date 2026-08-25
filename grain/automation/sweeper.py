"""The stranded-work sweeper.

docs/design.md: "if the host is stopped, or a run dies mid-flight, issues
need returning to the queue rather than stalling silently." Two distinct
ways that happens, both handled the same way here — release the sandbox and
report the issue back to `core.py` for requeuing:

- The unit finished (success or failure) — the ordinary case, checked every
  run since there is no long-lived process to notify the orchestrator when
  a job completes between cron invocations.
- The unit is unreachable (`UnitState.ABSENT` on a sandbox this state
  believes is assigned — dispatch failed partway before the unit ever
  started, or the controller-side unit was otherwise lost) or still running
  past `max_runtime_minutes` — the actual "stranded" case.

This module knows nothing about GitHub; it only reads `unit_status` over an
injected `Runner` and mutates `AutomationState`. Two different runners are
in play (docs/roadmap.md item 8's "Update": `claude -p` moved from the
sandbox to the controller) — `unit_status`/`reap` act on `controller_runner`
(where the unit actually lives now), while `_release`'s `cleanup()`/
`check_health()` still act on the per-sandbox `Runner` from `ssh_runner_for`
(the sandbox itself is still what needs cleaning/health-checking between
tasks). Label moves and PR creation are `core.py`'s job, which is the only
piece holding a `GitHubClient`.

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

**Every release also revokes the task's Gemini API key, if it minted one**
(bwsalmon/agents#47). The moment a sandbox's slot frees is "the end of the
task" in exactly the sense the issue asked for -- the same checkpoint
`_release` already uses for capture/cleanup/health, and it is reached from
every outcome uniformly (succeeded, failed, stranded, and even a
question-asking "success," since `core.py`'s `_finish_question` only
decides *afterward* how to report an outcome this module already freed).
`assignment.gemini_key_name` (`state.py`) is `None` for the overwhelming
common case of no `/gemini-key` directive, so this is a no-op for every
task that never asked for one. `delete_key` runs against `controller_runner`
(the same account that minted it in `core.py`'s `_dispatch`), never the
per-sandbox `runner` -- a raw GCP credential capable of revoking a key was
never going to be handed to a sandbox to do this itself. Best-effort: a
`CommandError` here (Google's API unreachable, the key already gone) is
turned into a warning rather than left to block the slot from freeing --
the same "surface it, don't gate on it" treatment `check_health`'s own
report already gets.

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
from .gemini_keys import GeminiKeyConfig, delete_key
from .health import check_health
from .history import NullSessionHistory, SessionHistory
from .state import Assignment, AutomationState, TriggerKind
from ..proxy.tokens import SandboxCredentialStore
from ..run import CommandError, Runner


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
    # Which repo the work was actually happening in, and what base a PR
    # from it targets — carried through from the Assignment for the same
    # reason `kind`/`branch` are: `core.py`'s finish handling needs them
    # and the assignment is gone the instant `_release` frees the slot.
    # `None` on an assignment written before the task/target split, which
    # `core.py` reads as the task repo.
    target_owner: str | None = None
    target_repo: str | None = None
    base: str | None = None


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
    # Same `HealthWarning` shape (sandbox + detail), reused rather than a
    # new dataclass for two fields that mean the same thing here: a
    # visibility-only note `core.py` logs, not something that changes
    # whether the sandbox's slot freed (bwsalmon/agents#47) -- see this
    # module's own docstring, "Every release also revokes...".
    credential_warnings: list[HealthWarning] = field(default_factory=list)


def _target_label(assignment: Assignment) -> str | None:
    """`"owner/repo"` for the target repo this session worked in, or None
    for a pre-split assignment that never recorded one — what the session
    browser shows, so "which repo did this task touch" is answerable
    without opening the transcript.
    """
    if assignment.target_owner and assignment.target_repo:
        return f"{assignment.target_owner}/{assignment.target_repo}"
    return None


def _revoke_gemini_key(controller_runner: Runner, sandbox: str, assignment: Assignment,
                        gemini_key_config: GeminiKeyConfig | None) -> HealthWarning | None:
    """Best-effort revocation of `assignment.gemini_key_name`, if this task
    minted one. `None` for the overwhelming common case: no key was ever
    minted, or (an edge case only reachable if a deployment disabled the
    feature mid-flight) one was minted but the config that named its
    project has since been removed -- either way there is nothing this
    process can safely call, so it's left for an operator to clean up by
    hand rather than guessing at a project id.
    """
    if assignment.gemini_key_name is None or gemini_key_config is None:
        return None
    try:
        delete_key(controller_runner, gemini_key_config, assignment.gemini_key_name)
    except CommandError as exc:
        return HealthWarning(sandbox, f"gemini API key revocation failed: {exc}")
    return None


def _release(state: AutomationState, runner: Runner, sandbox: str, *,
             history: SessionHistory, assignment: Assignment, unit: str,
             outcome_label: str, now: datetime, controller_runner: Runner,
             gemini_key_config: GeminiKeyConfig | None,
             credential_store: SandboxCredentialStore | None) -> tuple[HealthWarning | None, HealthWarning | None]:
    """Captures the session's trajectory, runs the between-task hook and a
    post-cleanup health check, revokes the task's Gemini API key if it
    minted one, clears any named-GitHub-key override it set
    (bwsalmon/agents#52), then frees the slot. Called from every branch
    below that frees a sandbox, so this behaviour is identical regardless
    of why the slot is being freed. Returns `(health_warning, credential_warning)`.

    Capture runs first, before cleanup or the state release — `claude -p`
    now runs on the controller (docs/roadmap.md item 8's "Update"), so
    `capture_trajectory` reads a controller-local file with no `runner`
    involved at all; kept first in this sequence purely to preserve
    "capture happens before a sandbox could possibly be reused" by
    construction, not because cleanup or the state release could touch it.

    Key revocation runs before `state.release` too, for the same reason:
    `assignment` (carrying `gemini_key_name`) is gone from `state` the
    instant `release` is called, and this is the last point that still has
    it in hand. The credential-override clear doesn't need `assignment` at
    all (`SandboxCredentialStore.clear` is unconditional, a no-op if this
    sandbox had no override), so its position relative to `state.release`
    doesn't matter the same way -- it runs alongside the key revocation
    purely because both are "end of task" credential cleanup.
    """
    transcript_text = capture_trajectory(unit)
    history.record(
        issue=assignment.issue, kind=assignment.kind, sandbox=sandbox, unit=unit,
        started_at=assignment.started_at, finished_at=now, outcome=outcome_label,
        transcript_text=transcript_text, target=_target_label(assignment),
    )
    cleanup(runner)
    report = check_health(runner)
    credential_warning = _revoke_gemini_key(
        controller_runner, sandbox, assignment, gemini_key_config
    )
    if credential_store is not None:
        credential_store.clear(sandbox)
    state.release(sandbox)
    health_warning = (
        HealthWarning(sandbox, f"{report.status.value}: {report.summary()}")
        if not report.ok else None
    )
    return health_warning, credential_warning


def sweep(state: AutomationState, ssh_runner_for: Callable[[str], Runner],
          controller_runner: Runner, config: AutomationConfig, now: datetime,
          history: SessionHistory | None = None,
          gemini_key_config: GeminiKeyConfig | None = None,
          credential_store: SandboxCredentialStore | None = None) -> SweepResult:
    """`controller_runner` is new (docs/roadmap.md item 8's "Update"):
    `claude -p` now runs on the controller, not the sandbox, so the unit
    `unit_status`/`reap` are checking against lives there too — the
    per-sandbox `runner` from `ssh_runner_for` is still needed for
    everything that genuinely still happens on the sandbox itself
    (`cleanup()`/`check_health()` inside `_release`). It also now doubles
    as the account `_release` revokes a Gemini API key with
    (bwsalmon/agents#47) — the controller's own account, never a
    sandbox's, same as the account that minted it in `core.py`'s
    `_dispatch`. `gemini_key_config` is `None` for a deployment that never
    enabled the feature -- every task's `assignment.gemini_key_name` is
    then also `None` (`core.py`'s `_resolve_target` refuses the directive
    outright when this is unset), so `_revoke_gemini_key` is a no-op
    throughout.
    """
    if history is None:
        history = NullSessionHistory()
    result = SweepResult()
    max_runtime = timedelta(minutes=config.max_runtime_minutes)
    for sandbox, assignment in list(state.assignments.items()):
        runner = ssh_runner_for(sandbox)
        unit = unit_name(sandbox)
        status = unit_status(controller_runner, unit)
        outcome = Outcome(sandbox=sandbox, issue=assignment.issue,
                           kind=assignment.kind, branch=assignment.branch,
                           target_owner=assignment.target_owner,
                           target_repo=assignment.target_repo,
                           base=assignment.base)

        if status is UnitState.DONE_SUCCESS:
            reap(controller_runner, unit)
            warning, credential_warning = _release(
                state, runner, sandbox, history=history, assignment=assignment, unit=unit,
                outcome_label="succeeded", now=now, controller_runner=controller_runner,
                gemini_key_config=gemini_key_config,
                credential_store=credential_store,
            )
            result.succeeded.append(outcome)
        elif status is UnitState.DONE_FAILED:
            reap(controller_runner, unit)
            warning, credential_warning = _release(
                state, runner, sandbox, history=history, assignment=assignment, unit=unit,
                outcome_label="failed", now=now, controller_runner=controller_runner,
                gemini_key_config=gemini_key_config,
                credential_store=credential_store,
            )
            result.failed.append(outcome)
        elif status is UnitState.ABSENT:
            # Assigned in our state but the unit isn't there — never
            # started (a dispatch that failed partway) or the controller
            # unit was otherwise lost. Nothing to reap.
            warning, credential_warning = _release(
                state, runner, sandbox, history=history, assignment=assignment, unit=unit,
                outcome_label="stranded", now=now, controller_runner=controller_runner,
                gemini_key_config=gemini_key_config,
                credential_store=credential_store,
            )
            result.stranded.append(outcome)
        elif now - assignment.started_at > max_runtime:
            reap(controller_runner, unit)
            warning, credential_warning = _release(
                state, runner, sandbox, history=history, assignment=assignment, unit=unit,
                outcome_label="stranded", now=now, controller_runner=controller_runner,
                gemini_key_config=gemini_key_config,
                credential_store=credential_store,
            )
            result.stranded.append(outcome)
        else:
            # ACTIVE and within budget — leave it running, and leave its
            # slot alone: cleanup/health only run on a sandbox that's
            # actually being freed.
            continue
        if warning is not None:
            result.health_warnings.append(warning)
        if credential_warning is not None:
            result.credential_warnings.append(credential_warning)
    return result
