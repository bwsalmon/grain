"""Every mint, audit-logged -- docs/design.md's "GCP credentials" line.

Same shape as `grain/proxy/audit.py` and `grain/automation/audit.py`: an
`AuditLog` Protocol, a `FileAuditLog` that appends one JSON line per
record, and a `RecordingAuditLog` that collects them in memory for tests.
Deliberately not the git proxy's or the orchestrator's exact field set --
those are `AuditLog`s for their own domains (repo/credential, issue/outcome)
-- but the same pattern, so an operator reading `/data/state/` finds three
append-only JSONL logs that all look and behave the same way rather than a
one-off format for this one.

`gce_metadata_server` has no hookable logging or webhook of its own to
call into -- confirmed by reading its README and `cmd/main.go` end to end,
and live: with `-jsonLog -debug` it writes one JSON line per request to
whatever `-logTarget` file points at (`msg="request"`, with `path`), and a
separate `level="ERROR"` line immediately after if minting the token
failed. There's nothing else to subscribe to. So this *is* the thin
wrapper the design doc implies is needed: `iter_mint_events` parses that
log's shape (pure, no I/O, easy to test against captured lines) and `sync`
tails a real instance's log file, forwarding new mint-relevant lines into
an `AuditLog` -- the one place this module touches a filesystem.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable, Iterator, Protocol


class AuditLog(Protocol):
    def record(self, *, sandbox: str, path: str, outcome: str,
               detail: str | None) -> None: ...


class FileAuditLog:
    def __init__(self, path: Path) -> None:
        self._path = path
        self._path.parent.mkdir(parents=True, exist_ok=True)

    def record(self, *, sandbox: str, path: str, outcome: str,
               detail: str | None) -> None:
        entry = {
            "time": datetime.now(timezone.utc).isoformat(),
            "sandbox": sandbox,
            "path": path,
            "outcome": outcome,
            "detail": detail,
        }
        with self._path.open("a") as f:
            f.write(json.dumps(entry) + "\n")


@dataclass
class RecordingAuditLog:
    """Collects entries in memory. For tests."""

    entries: list[dict] = field(default_factory=list)

    def record(self, *, sandbox: str, path: str, outcome: str,
               detail: str | None) -> None:
        self.entries.append(
            {"sandbox": sandbox, "path": path, "outcome": outcome, "detail": detail}
        )


@dataclass(frozen=True)
class MintEvent:
    path: str
    outcome: str  # "minted" or "error"
    detail: str | None


def _is_mint_path(path: str) -> bool:
    # The two endpoints ADC actually calls on the token-minting path --
    # confirmed live against a real binary. Every other request (project
    # id, instance attributes, ...) never touches the impersonated
    # credential, so it isn't a "mint" worth its own audit entry.
    if "/service-accounts/" not in path:
        return False
    leaf = path.rstrip("/").rsplit("/", 1)[-1]
    return leaf in ("token", "identity")


def iter_mint_events(lines: Iterable[str]) -> Iterator[MintEvent]:
    """Parses `gce_metadata_server -jsonLog -debug` output, one JSON object
    per line, and yields a `MintEvent` for each request to a mint-relevant
    path.

    Malformed lines are skipped rather than raising -- a partially-written
    last line (the tailer's own concern, see `sync`) must not poison the
    whole batch. Outcome is read off the lines that follow: this tool logs
    a `msg="request"` line, then (confirmed live, not the single next line
    as might be assumed) a `"using service account context"` line, then --
    only on failure -- a `level="ERROR"` line, before the next request's
    own lines begin. So this scans forward from each mint-relevant request
    up to `_LOOKAHEAD` lines or the next `msg="request"`, whichever comes
    first: an `ERROR` line in that window means the mint failed; reaching
    the next request (or the end of the batch) with no error means it
    succeeded.
    """
    parsed: list[dict] = []
    for raw in lines:
        raw = raw.strip()
        if not raw:
            continue
        try:
            parsed.append(json.loads(raw))
        except json.JSONDecodeError:
            continue

    for i, entry in enumerate(parsed):
        if entry.get("msg") != "request":
            continue
        path = entry.get("path", "")
        if not _is_mint_path(path):
            continue
        outcome, detail = "minted", None
        for j in range(i + 1, min(i + 1 + _LOOKAHEAD, len(parsed))):
            nxt = parsed[j]
            if nxt.get("msg") == "request":
                break
            if nxt.get("level") == "ERROR":
                outcome, detail = "error", nxt.get("raw_error")
                break
        yield MintEvent(path=path, outcome=outcome, detail=detail)


# How many lines past a mint-relevant "request" line to look for its
# ERROR line -- generous headroom over the one intermediate line
# ("using service account context") observed live between the two.
_LOOKAHEAD = 5


@dataclass(frozen=True)
class TailState:
    offset: int = 0

    @classmethod
    def load(cls, path: Path) -> "TailState":
        if not path.exists():
            return cls()
        return cls(offset=json.loads(path.read_text()).get("offset", 0))

    def save(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"offset": self.offset}))


def sync(log_path: Path, state_path: Path, sandbox: str, audit: AuditLog) -> int:
    """Tails `log_path` from where the last call left off, forwarding any
    new mint-relevant lines into `audit`. Returns the number of events
    forwarded.

    Byte-offset resumable, like a log shipper: safe to call from a
    periodic invocation (e.g. alongside `grain automation run-once`'s
    cron timer) without re-forwarding old entries or needing the instance
    to still be running. A log that doesn't exist yet (instance never
    started) is 0 events, not an error -- the same "absent is a valid
    state" stance `unit_status` takes.

    Known gap: a "request" line landing exactly at a batch boundary, with
    its ERROR line not written until after this call already read up to
    that point, is recorded as "minted" rather than "error" -- the next
    call sees the orphaned ERROR line with no preceding request in its
    own batch and drops it (see `iter_mint_events`). Rare in practice
    (the two lines are written back-to-back by the same request), and
    getting it exactly right would mean buffering a trailing unmatched
    request across calls -- not worth the complexity for an audit trail
    whose job is "was this path hit," not billing-grade accounting.
    """
    if not log_path.exists():
        return 0
    state = TailState.load(state_path)
    with log_path.open("rb") as f:
        f.seek(state.offset)
        chunk = f.read()
    if not chunk:
        return 0
    # Only forward complete lines -- a writer mid-append could leave a
    # trailing partial one, and re-reading it whole next time is simpler
    # and safer than trying to resume inside a line. Cut on the raw bytes
    # (not a decoded str) so the new offset stays byte-accurate even with
    # multi-byte UTF-8 in a raw_error message.
    last_newline = chunk.rfind(b"\n")
    if last_newline == -1:
        return 0  # nothing complete yet
    complete = chunk[: last_newline + 1]
    lines = complete.decode(errors="replace").splitlines()
    count = 0
    for event in iter_mint_events(lines):
        audit.record(sandbox=sandbox, path=event.path, outcome=event.outcome,
                      detail=event.detail)
        count += 1
    TailState(offset=state.offset + len(complete)).save(state_path)
    return count
