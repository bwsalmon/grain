"""Sandbox identity: a per-sandbox bearer token, consumed via HTTP Basic auth.

docs/design.md is specific that this is "not an SSH key" and that "git
consumes it via a credential helper, so agents never handle it" — a
sandbox's git credential helper supplies the token as the password half of
HTTP Basic auth, the username is irrelevant, and the agent inside the
sandbox never sees the token at all.
"""

from __future__ import annotations

import base64
import binascii
import json
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
