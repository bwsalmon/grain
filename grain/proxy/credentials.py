"""The credential ladder: the narrowest credential that covers a repo.

docs/design.md's shape:

    /data/secrets/github/
      credentials.json     # repo/owner pattern -> credential name
      bot.token            # machine account, most repos
      personal.token       # last resort, only what nothing else reaches

Unlike the allowlist, credentials are not hot-reloaded — docs/design.md
treats rotation as "replace a file under /data/secrets and restart the one
service that reads it," so loading once at construction matches the
intended operational model rather than adding a cache-invalidation path
nothing asks for.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

from .allowlist import canonicalize


@dataclass(frozen=True)
class Credential:
    name: str
    # None means anonymous — no Authorization header at all, which is what
    # a public repo needs and is a real, deliberate credential shape, not
    # an error case.
    token: str | None


class CredentialSet:
    def __init__(self, secrets_dir: Path) -> None:
        self._dir = secrets_dir
        mapping_path = secrets_dir / "credentials.json"
        self._patterns: dict[str, str] = (
            json.loads(mapping_path.read_text()) if mapping_path.exists() else {}
        )
        self._cache: dict[str, Credential] = {}

    def select(self, owner: str, repo: str) -> Credential | None:
        """The narrowest pattern covering (owner, repo): exact, then
        `owner/*`, then the global `*` fallback. None if nothing covers it
        — a distinct, fail-closed condition from "not allow-listed".
        """
        owner, repo = canonicalize(owner, repo)
        for pattern in (f"{owner}/{repo}", f"{owner}/*", "*"):
            name = self._patterns.get(pattern)
            if name:
                return self._load(name)
        return None

    def get(self, name: str) -> Credential | None:
        """A named credential directly, bypassing the owner/repo pattern
        ladder `select` uses -- bwsalmon/agents#52's per-task
        `grain-github-<name>` label, which names a credential explicitly
        rather than letting the target repo pick one (e.g. a `workflow.token`
        carrying the `workflow` scope docs/design.md's default ladder
        deliberately withholds). `None` if no such credential is configured
        (`<name>.token` missing, and `name` isn't the literal `"anonymous"`)
        -- the same fail-closed "not configured" condition `select` already
        returns `None` for, so `GitProxy` can treat both the same way.
        """
        if name != "anonymous" and not (self._dir / f"{name}.token").exists():
            return None
        return self._load(name)

    def token_for(self, owner: str, repo: str) -> str | None:
        """`select`'s token alone — `grain/automation/github.py`'s
        `TokenSource` shape, so the orchestrator's `GitHubClient` can
        resolve a credential per repo instead of holding one fixed at
        construction. Necessary once a deployment talks to more than one
        repo (a task repo plus each target repo it dispatches into), and
        exactly what this class's pattern ladder was already for.

        `None` covers both "nothing covers this repo" and "the covering
        credential is anonymous" — the caller (an unauthenticated request)
        does the same thing either way, and the distinction that matters
        for the *proxy* (fail closed vs. deliberately anonymous) is made by
        `select` itself, which is still what the proxy calls.
        """
        credential = self.select(owner, repo)
        return credential.token if credential else None

    def _load(self, name: str) -> Credential:
        if name not in self._cache:
            if name == "anonymous":
                token = None
            else:
                token = (self._dir / f"{name}.token").read_text().strip()
            self._cache[name] = Credential(name=name, token=token)
        return self._cache[name]
