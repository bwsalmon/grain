"""Git smart-HTTP protocol bits: path validation and pkt-line encoding.

The proxy's whole job is deciding *whether* a request may proceed and *which*
credential serves it — never parsing the pack itself. docs/design.md is
explicit about why: getting a hand-rolled parser subtly wrong fails open,
so ref-level policy (no push to main, no force-push) is left to GitHub
rulesets, and this module stays confined to the four legal smart-HTTP paths
and the error encoding a git client actually understands.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

# The four legal smart-HTTP paths, e.g. /owner/repo.git/info/refs
_PATH_RE = re.compile(
    r"^/(?P<owner>[^/]+)/(?P<repo>[^/]+?)(?:\.git)?/"
    r"(?P<action>info/refs|git-upload-pack|git-receive-pack)$"
)


@dataclass(frozen=True)
class GitRequest:
    owner: str
    repo: str
    action: str  # "info/refs", "git-upload-pack", or "git-receive-pack"


def parse_path(path: str) -> GitRequest | None:
    """Match one of the four legal smart-HTTP paths. None if it isn't one."""
    match = _PATH_RE.match(path)
    if not match:
        return None
    return GitRequest(
        owner=match.group("owner"), repo=match.group("repo"),
        action=match.group("action"),
    )


def is_valid_git_request(user_agent: str, accept: str, action: str) -> bool:
    """A tight allowlist of the client shapes a real git client produces.

    Mirrors what FINOS Git Proxy's `validGitRequest()` checks (worth
    stealing, per docs/design.md): a `git/` user agent, and — for the pack
    endpoints — an Accept header naming the matching `x-git-*` result type.
    Rejecting anything else means a browser poking at these paths gets a
    plain 404 rather than a confusing partial response.
    """
    if not user_agent.startswith("git/"):
        return False
    if action == "info/refs":
        return True  # git sends `Accept: */*` here; nothing more to check
    expected = f"application/x-{action}-result"
    return expected in accept


def pkt_line(data: bytes) -> bytes:
    """Encode one pkt-line: a 4-hex-digit length prefix, then the data."""
    length = len(data) + 4
    return f"{length:04x}".encode("ascii") + data


FLUSH_PKT = b"0000"


def err_pkt(message: str) -> bytes:
    """A single ERR pkt-line — the encoding every git client already knows
    how to surface as an abort message, instead of the opaque "the remote
    end hung up unexpectedly" a plain connection drop produces.
    """
    return pkt_line(f"ERR {message}\n".encode())
