"""Pool assignments and rate-limit history, persisted across cron runs.

This is the one file in the package that can't just copy an existing
pattern verbatim: every JSON file elsewhere in the repo
(`repo-allowlist.json`, `credentials.json`, `sandbox-tokens.json`) is
read-only from the consumer's side, written by an operator or a
provisioning step. This one is read-modify-written by `grain automation
run-once` itself, and a cron job can be killed mid-write — so saving goes
through a temp file and an atomic rename rather than a direct
`Path.write_text`, the same guarantee a crashed write must not corrupt the
one record of which sandbox is doing what.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path


@dataclass(frozen=True)
class Assignment:
    issue: int
    unit: str
    started_at: datetime


@dataclass
class AutomationState:
    assignments: dict[str, Assignment] = field(default_factory=dict)
    run_timestamps: list[datetime] = field(default_factory=list)

    @classmethod
    def load(cls, path: Path) -> "AutomationState":
        if not path.exists():
            return cls()
        raw = json.loads(path.read_text())
        assignments = {
            name: Assignment(
                issue=a["issue"], unit=a["unit"],
                started_at=datetime.fromisoformat(a["started_at"]),
            )
            for name, a in raw.get("assignments", {}).items()
        }
        run_timestamps = [
            datetime.fromisoformat(t) for t in raw.get("run_timestamps", [])
        ]
        return cls(assignments=assignments, run_timestamps=run_timestamps)

    def save(self, path: Path) -> None:
        data = {
            "assignments": {
                name: {
                    "issue": a.issue, "unit": a.unit,
                    "started_at": a.started_at.isoformat(),
                }
                for name, a in self.assignments.items()
            },
            "run_timestamps": [t.isoformat() for t in self.run_timestamps],
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(json.dumps(data, indent=2))
        tmp.replace(path)

    # --- pool ---------------------------------------------------------
    def free_sandbox(self, sandbox_names: list[str]) -> str | None:
        for name in sandbox_names:
            if name not in self.assignments:
                return name
        return None

    def assign(self, sandbox: str, issue: int, unit: str, now: datetime) -> None:
        self.assignments[sandbox] = Assignment(issue=issue, unit=unit, started_at=now)

    def release(self, sandbox: str) -> None:
        self.assignments.pop(sandbox, None)

    def in_progress_issues(self) -> set[int]:
        return {a.issue for a in self.assignments.values()}

    # --- rate limit -----------------------------------------------------
    def record_run(self, now: datetime) -> None:
        self.run_timestamps.append(now)
        # Nothing downstream needs more than the last hour; trim on write
        # so the file does not grow without bound over a long deployment.
        cutoff = now.timestamp() - 24 * 3600
        self.run_timestamps = [
            t for t in self.run_timestamps if t.timestamp() >= cutoff
        ]


def utcnow() -> datetime:
    return datetime.now(timezone.utc)
