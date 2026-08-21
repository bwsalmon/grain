"""Append-only, one-line-per-request audit log.

docs/design.md: "select the credential for that repo... and log the tuple."
Every decision the proxy makes — allowed, denied, or misconfigured — gets a
line, so the powerful (personal-token) end of the credential ladder staying
mostly unused is something an operator can actually verify rather than
assume.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Protocol


class AuditLog(Protocol):
    def record(self, *, sandbox: str | None, owner: str, repo: str,
               action: str, credential: str | None, outcome: str) -> None: ...


class FileAuditLog:
    def __init__(self, path: Path) -> None:
        self._path = path
        self._path.parent.mkdir(parents=True, exist_ok=True)

    def record(self, *, sandbox: str | None, owner: str, repo: str,
               action: str, credential: str | None, outcome: str) -> None:
        entry = {
            "time": datetime.now(timezone.utc).isoformat(),
            "sandbox": sandbox,
            "repo": f"{owner}/{repo}",
            "action": action,
            "credential": credential,
            "outcome": outcome,
        }
        with self._path.open("a") as f:
            f.write(json.dumps(entry) + "\n")


@dataclass
class RecordingAuditLog:
    """Collects entries in memory. For tests."""

    entries: list[dict] = field(default_factory=list)

    def record(self, *, sandbox: str | None, owner: str, repo: str,
               action: str, credential: str | None, outcome: str) -> None:
        self.entries.append(
            {"sandbox": sandbox, "repo": f"{owner}/{repo}", "action": action,
             "credential": credential, "outcome": outcome}
        )
