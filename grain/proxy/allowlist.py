"""The repo allowlist: default-deny, hot-reloaded, no restart required.

docs/design.md is specific about the shape: "an admin edits a file with no
restart." The file is small, so it is simply re-read on every check rather
than cached against an mtime — that avoids a whole class of staleness bugs
(mtime resolution on some filesystems is coarse enough that two edits within
the same second are indistinguishable) for a negligible cost per git
operation, and needs no filesystem watcher dependency either.
"""

from __future__ import annotations

import json
from pathlib import Path


def canonicalize(owner: str, repo: str) -> tuple[str, str]:
    """Case-insensitive, `.git`-suffix-insensitive identity for a repo.

    GitHub itself treats owner/repo case-insensitively, and a client may or
    may not include the `.git` suffix — the allowlist must agree with both,
    or a real allowed repo can appear to be denied.
    """
    if repo.endswith(".git"):
        repo = repo[: -len(".git")]
    return owner.lower(), repo.lower()


class Allowlist:
    """Wraps a JSON file: a list of `"owner/repo"` strings, default-deny."""

    def __init__(self, path: Path) -> None:
        self._path = path

    def _entries(self) -> frozenset[tuple[str, str]]:
        try:
            raw = json.loads(self._path.read_text())
        except FileNotFoundError:
            return frozenset()
        return frozenset(canonicalize(*item.split("/", 1)) for item in raw)

    def allows(self, owner: str, repo: str) -> bool:
        return canonicalize(owner, repo) in self._entries()
