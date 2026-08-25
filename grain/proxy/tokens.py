"""Sandbox identity: a per-sandbox bearer token, consumed via HTTP Basic auth.

docs/design.md is specific that this is "not an SSH key" and that "git
consumes it via a credential helper, so agents never handle it" — a
sandbox's git credential helper supplies the token as the password half of
HTTP Basic auth, the username is irrelevant, and the agent inside the
sandbox never sees the token at all.

Two classes, one file, because they read and write the same
`sandbox-tokens.json` from different sides: `SandboxTokens` is the proxy's
read-only lookup, loaded once at process start (`server.py`'s
`build_proxy`); `SandboxTokenStore` is the controller's write side, called
from `grain/automation/dispatch.py` to mint a sandbox's token the first time
it's dispatched to. Keeping both here, rather than reimplementing minting in
`automation/`, is what docs/roadmap.md item 2 asked for: the credential
lives in one place.
"""

from __future__ import annotations

import base64
import binascii
import json
import secrets
from pathlib import Path


class SandboxTokens:
    """Maps a bearer token to the sandbox it identifies.

    File format: `{"sandbox-0": "<token>", "sandbox-1": "<token>", ...}`,
    generated and injected at provisioning time, replaced at recreate — see
    docs/design.md, "sandbox identity."
    """

    def __init__(self, path: Path) -> None:
        raw = json.loads(path.read_text()) if path.exists() else {}
        self._by_token: dict[str, str] = {token: name for name, token in raw.items()}

    def authenticate(self, token: str) -> str | None:
        """The sandbox name owning this token, or None if it is unknown."""
        return self._by_token.get(token)


class SandboxTokenStore:
    """The write side of the same `sandbox-tokens.json` file `SandboxTokens`
    reads. Split into its own class rather than added to `SandboxTokens`
    because the two run in different processes with different lifecycles:
    the proxy loads the token map once at startup and only ever looks
    tokens up, while minting one is a controller-side, read-modify-write
    operation against the file the proxy trusts — done here, in `proxy/`,
    per docs/roadmap.md item 2's instruction to keep credential-selection
    logic where it already lives rather than duplicating it in
    `automation/`. `dispatch()` calls `ensure_token` on every dispatch; it
    is a no-op read after the first sandbox recreate.

    Same atomic-write discipline as `AutomationState.save`: a temp file plus
    `rename`, since a killed write here would corrupt the proxy's only
    record of sandbox identity, not just one orchestrator's own state.
    """

    def __init__(self, path: Path) -> None:
        self._path = path

    def _load(self) -> dict[str, str]:
        return json.loads(self._path.read_text()) if self._path.exists() else {}

    def _save(self, tokens: dict[str, str]) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self._path.with_suffix(self._path.suffix + ".tmp")
        tmp.write_text(json.dumps(tokens, indent=2))
        tmp.replace(self._path)

    def ensure_token(self, sandbox: str) -> str:
        """The sandbox's existing token, or a freshly minted one recorded to
        disk. Idempotent per sandbox name — safe to call unconditionally
        before every dispatch.
        """
        tokens = self._load()
        if sandbox in tokens:
            return tokens[sandbox]
        token = secrets.token_hex(32)
        tokens[sandbox] = token
        self._save(tokens)
        return token

    def rotate(self, sandbox: str) -> str:
        """Mints and records a fresh token unconditionally, replacing any
        existing one. docs/design.md: "Rotation is explicit, folded into
        recreate." Not wired into a CLI path yet — sandbox recreate itself
        is docs/roadmap.md item 3 — exposed now as the mechanism that step
        will need, so `ensure_token`'s "mint once, reuse forever" shape
        isn't the only option on record.
        """
        tokens = self._load()
        token = secrets.token_hex(32)
        tokens[sandbox] = token
        self._save(tokens)
        return token


class SandboxCredentialOverrides:
    """Maps a sandbox to the named `CredentialSet` credential its current
    task must use in place of the owner/repo default -- bwsalmon/agents#52's
    `grain-github-<name>` label, for a task that needs a scope the default
    credential deliberately withholds (docs/design.md, "Scopes to
    withhold" -- `workflow`, most notably).

    File format: `{"sandbox-0": "workflow", ...}` -- absent entries mean
    "no override, use the normal per-repo selection."

    Deliberately re-read on every lookup, `Allowlist`'s "small file, no
    staleness window" shape rather than `SandboxTokens`' load-once-at-
    startup one just above: sandbox *identity* only changes at sandbox
    recreate, but which credential a sandbox's current task should use
    changes on every dispatch and every release, and nothing here
    restarts the proxy that often. That is also why this file lives under
    `/data/config`, not `/data/secrets` alongside the credentials it
    merely *names* -- `/data/secrets` is the "replace and restart"
    tier (`CredentialSet`'s own docstring), and this is not.
    """

    def __init__(self, path: Path) -> None:
        self._path = path

    def for_sandbox(self, sandbox: str) -> str | None:
        try:
            raw = json.loads(self._path.read_text())
        except FileNotFoundError:
            return None
        return raw.get(sandbox)


class SandboxCredentialStore:
    """The controller's write side of the same file
    `SandboxCredentialOverrides` reads. `grain/automation/core.py`'s
    `_dispatch` calls `set`/`clear` for every dispatch attempt (whether or
    not it goes on to succeed), and `sweeper.py`'s `_release` calls `clear`
    unconditionally when a sandbox's slot frees -- between the two, an
    override never outlives the task that asked for it: `set`/`clear` at
    the top of `_dispatch` closes the gap a `CommandError` partway through
    dispatch would otherwise leave (no `Assignment` ever recorded for
    `_release` to find), and `_release`'s own `clear` is what actually
    ends an override the moment a *successful* dispatch's task finishes.

    Same atomic-write discipline as `SandboxTokenStore`: a temp file plus
    `rename`, so a killed write can't corrupt the file the proxy trusts on
    its very next (unrestarted) request.
    """

    def __init__(self, path: Path) -> None:
        self._path = path

    def _load(self) -> dict[str, str]:
        return json.loads(self._path.read_text()) if self._path.exists() else {}

    def _save(self, overrides: dict[str, str]) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self._path.with_suffix(self._path.suffix + ".tmp")
        tmp.write_text(json.dumps(overrides, indent=2))
        tmp.replace(self._path)

    def set(self, sandbox: str, name: str) -> None:
        overrides = self._load()
        overrides[sandbox] = name
        self._save(overrides)

    def clear(self, sandbox: str) -> None:
        overrides = self._load()
        if sandbox in overrides:
            del overrides[sandbox]
            self._save(overrides)


def extract_basic_auth_token(header: str | None) -> str | None:
    """Pull the token out of `Authorization: Basic base64(user:token)`.

    The username is ignored — a credential helper is configured with an
    arbitrary username and the token as the password, so the token is what
    identifies the caller.
    """
    if not header or not header.startswith("Basic "):
        return None
    try:
        decoded = base64.b64decode(header[len("Basic ") :], validate=True).decode()
    except (binascii.Error, ValueError, UnicodeDecodeError):
        return None
    if ":" not in decoded:
        return None
    _, _, token = decoded.partition(":")
    return token
