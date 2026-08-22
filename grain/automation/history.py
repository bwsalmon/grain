"""Durable history of past dispatch sessions, keyed by trigger (issue/PR
number) — separate from `AutomationState`, which `state.py`'s own module
docstring is explicit is live-pool-only: a released slot's `Assignment` is
simply discarded (`AutomationState.release`), because that module's whole
job is "what is a sandbox doing *right now*," not "what has it ever done."
Browsing past sessions (docs/roadmap.md item 10) needs the opposite shape —
a record that survives long after the slot is freed and the sandbox is
reused for something else entirely.

**Layout**: under `/data/state/automation/sessions/`, as docs/roadmap.md
item 10 names — one `<key>.json` per session (this module's own
`SessionRecord`: sandbox, unit, kind, started/finished times, outcome, and
the captured trajectory's storage location) plus, when a trajectory was
actually captured, a sibling `<key>.jsonl` holding the raw content
`capture.py` pulled off the sandbox (see that module's docstring for what
format that content is and why).

One file per session, not one growing index file: a sweep only ever *adds*
one record at a time, so this mirrors `audit.py`'s FileAuditLog's
append-only shape more than `state.py`'s single-file
read-modify-atomic-rewrite (that file is small and rewritten on every
`run-once` regardless; a session history is meant to accumulate
indefinitely over a long-lived deployment, so a single JSON blob that grows
without bound and gets fully rewritten every sweep is the wrong shape).
Each record's own on-disk write is still atomic (temp file + rename), the
same discipline `state.py`'s `AutomationState.save` uses, for the same
reason: a cron run can be killed mid-write, and a half-written record must
never look like a plausible complete one.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Protocol

from .state import TriggerKind


@dataclass(frozen=True)
class SessionRecord:
    """One finished (or stranded) dispatch, as browsable history. `issue` is
    the trigger's number — an issue or PR number, leaning on the same "one
    shared number sequence per repo" fact `state.py`'s `Assignment.issue`
    already does (docs/roadmap.md item 9) — so a session for issue #5 and
    one for PR #5 can never collide in one repo's history.
    """

    issue: int
    kind: TriggerKind
    sandbox: str
    unit: str
    started_at: datetime
    finished_at: datetime
    # "succeeded" / "failed" / "stranded" -- the same three outcomes
    # sweeper.py's SweepResult already distinguishes, stored as the plain
    # string the sweep already produces rather than a fourth enum that would
    # have to stay in lockstep with sweeper.py's own categories.
    outcome: str
    # Where the captured trajectory lives on this machine, or None if
    # nothing was captured -- never dispatched far enough to write one, or
    # the capture step itself came back empty (see capture.py, which never
    # raises).
    transcript_path: str | None


class SessionHistory(Protocol):
    def record(self, *, issue: int, kind: TriggerKind, sandbox: str, unit: str,
               started_at: datetime, finished_at: datetime, outcome: str,
               transcript_text: str | None) -> SessionRecord | None: ...


def _session_key(started_at: datetime, kind: TriggerKind, issue: int, sandbox: str) -> str:
    # Sortable by filename (timestamp first) and collision-proof across
    # repeated dispatches of the same trigger -- a requeued issue gets
    # redispatched under a fresh key each time (a new started_at), never
    # overwriting an earlier attempt's record.
    stamp = started_at.strftime("%Y%m%dT%H%M%S%f")
    return f"{stamp}-{kind.value}-{issue}-{sandbox}"


def _to_json(record: SessionRecord) -> dict:
    return {
        "issue": record.issue,
        "kind": record.kind.value,
        "sandbox": record.sandbox,
        "unit": record.unit,
        "started_at": record.started_at.isoformat(),
        "finished_at": record.finished_at.isoformat(),
        "outcome": record.outcome,
        "transcript_path": record.transcript_path,
    }


def _from_json(raw: dict) -> SessionRecord:
    return SessionRecord(
        issue=raw["issue"],
        kind=TriggerKind(raw["kind"]),
        sandbox=raw["sandbox"],
        unit=raw["unit"],
        started_at=datetime.fromisoformat(raw["started_at"]),
        finished_at=datetime.fromisoformat(raw["finished_at"]),
        outcome=raw["outcome"],
        transcript_path=raw.get("transcript_path"),
    )


class FileSessionHistory:
    """The real, disk-backed store — `/data/state/automation/sessions/` in
    production (`grain/cli.py`'s `build_orchestrator`); any directory in a
    test.
    """

    def __init__(self, root: Path) -> None:
        self.root = root
        self.root.mkdir(parents=True, exist_ok=True)

    def record(self, *, issue: int, kind: TriggerKind, sandbox: str, unit: str,
               started_at: datetime, finished_at: datetime, outcome: str,
               transcript_text: str | None) -> SessionRecord:
        key = _session_key(started_at, kind, issue, sandbox)
        transcript_path: str | None = None
        if transcript_text:
            transcript_file = self.root / f"{key}.jsonl"
            transcript_file.write_text(transcript_text)
            transcript_path = str(transcript_file)
        record = SessionRecord(
            issue=issue, kind=kind, sandbox=sandbox, unit=unit,
            started_at=started_at, finished_at=finished_at, outcome=outcome,
            transcript_path=transcript_path,
        )
        # Same temp-file-plus-rename discipline as AutomationState.save --
        # this write can be interrupted (a killed cron job) just as easily,
        # and a half-written session record must never look complete.
        path = self.root / f"{key}.json"
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(json.dumps(_to_json(record), indent=2))
        tmp.replace(path)
        return record

    def all(self) -> list[SessionRecord]:
        """Every recorded session, newest first."""
        records = [
            _from_json(json.loads(p.read_text()))
            for p in sorted(self.root.glob("*.json"))
        ]
        records.sort(key=lambda r: r.started_at, reverse=True)
        return records

    def for_trigger(self, issue: int) -> list[SessionRecord]:
        """Every session ever dispatched for trigger `issue`, newest first
        -- the "browse past sessions by their trigger" entry point
        docs/roadmap.md item 10 asks for.
        """
        return [r for r in self.all() if r.issue == issue]

    @staticmethod
    def read_transcript(record: SessionRecord) -> str:
        """The raw captured trajectory content for `record`, or "" if
        nothing was captured for this session.
        """
        if record.transcript_path is None:
            return ""
        return Path(record.transcript_path).read_text()


class NullSessionHistory:
    """No-op, mirroring `audit.py`'s `NullAuditLog` -- what `Orchestrator`
    falls back to when no real history store is wired in. Capture (the SSH
    `cat`) still runs against the sandbox either way; this just means
    nothing is kept afterward.
    """

    def record(self, **_kwargs) -> None:
        return None


@dataclass
class RecordingSessionHistory:
    """Collects every `record()` call in memory, transcript text included --
    the sweeper-capture equivalent of `audit.py`'s `RecordingAuditLog`. For
    tests.
    """

    calls: list[dict] = field(default_factory=list)

    def record(self, *, issue: int, kind: TriggerKind, sandbox: str, unit: str,
               started_at: datetime, finished_at: datetime, outcome: str,
               transcript_text: str | None) -> SessionRecord:
        self.calls.append({
            "issue": issue, "kind": kind, "sandbox": sandbox, "unit": unit,
            "started_at": started_at, "finished_at": finished_at,
            "outcome": outcome, "transcript_text": transcript_text,
        })
        return SessionRecord(
            issue=issue, kind=kind, sandbox=sandbox, unit=unit,
            started_at=started_at, finished_at=finished_at, outcome=outcome,
            transcript_path=None,
        )
