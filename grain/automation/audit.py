"""One line per dispatch/sweep decision. Same shape as
`grain/proxy/audit.py`'s `FileAuditLog` — an append-only JSONL record of
every decision, so which sandbox is running which issue (and why one was
skipped: no free sandbox, rate limit) is something an operator can read
back rather than infer from GitHub's label history alone.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Protocol


class AuditLog(Protocol):
    def record(self, *, sandbox: str | None, issue: int | None,
               outcome: str) -> None: ...


class FileAuditLog:
    def __init__(self, path: Path) -> None:
        self._path = path
        self._path.parent.mkdir(parents=True, exist_ok=True)

    def record(self, *, sandbox: str | None, issue: int | None,
               outcome: str) -> None:
        entry = {
            "time": datetime.now(timezone.utc).isoformat(),
            "sandbox": sandbox,
            "issue": issue,
            "outcome": outcome,
        }
        with self._path.open("a") as f:
            f.write(json.dumps(entry) + "\n")


class NullAuditLog:
    def record(self, **_kwargs) -> None:
        pass


@dataclass
class RecordingAuditLog:
    """Collects entries in memory. For tests."""

    entries: list[dict] = field(default_factory=list)

    def record(self, *, sandbox: str | None, issue: int | None,
               outcome: str) -> None:
        self.entries.append({"sandbox": sandbox, "issue": issue, "outcome": outcome})
