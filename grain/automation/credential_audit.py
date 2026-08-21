"""Audits every GitHub credential under `/data/secrets/github/` for scopes
docs/design.md's "Scopes to withhold" section says must never be granted:
`workflow`, `delete_repo`, `write:org`, and anything `admin:*`.

Same `Transport` seam `github.py` already defines — this makes one read-only
call per credential (`GET /user`) and never mutates anything, so it reuses
`GitHubClient`'s `Transport`/`RealTransport`/`FakeTransport` rather than a
second HTTP client.

Two token families behave differently for this purpose, verified against
GitHub's own docs rather than assumed:

- **Classic PATs and OAuth tokens** return the scopes actually granted on
  *any* authenticated API response, as a comma-separated `X-OAuth-Scopes`
  header — e.g. `X-OAuth-Scopes: repo, user` — per
  https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps.
  This is the only case this module can check by itself.
- **Fine-grained PATs and GitHub App tokens expose no such header.** GitHub's
  own maintainers confirm there is currently no API to introspect a
  fine-grained PAT's granted permissions at all
  (https://github.com/orgs/community/discussions/156115); Apps instead
  return a `permissions` object, but only in the one-time response to
  *minting* an installation token, not on ordinary API calls made with one.
  For these this module reports UNVERIFIABLE rather than faking a check —
  the operator has to read the permission list off GitHub's own settings
  page (or, for an App, its installation permissions) by hand.

Token *kind* is read off GitHub's own identifiable prefix scheme
(`ghp_`, `github_pat_`, `gho_`, `ghu_`, `ghs_`, `ghr_` — see
https://github.blog/changelog/2021-03-31-authentication-token-format-updates-are-generally-available/),
which needs no network call at all. Tokens predating that 2021 rollout are
still legal and carry no prefix; those are treated as classic-PAT-shaped and
introspected the same way, since a 40-hex-char classic token is what they
always were.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass
from pathlib import Path

from .github import ApiResponse, Transport

# docs/design.md, "Scopes to withhold": delete_repo, no admin:*, no
# write:org, no workflow (or workflows: write — the App/fine-grained
# permission-key spelling; not reachable here since those aren't
# introspectable in the first place).
_FORBIDDEN_EXACT = {"workflow", "delete_repo", "write:org"}


def _is_forbidden(scope: str) -> bool:
    return scope in _FORBIDDEN_EXACT or scope.startswith("admin:")


class TokenKind(enum.Enum):
    CLASSIC_PAT = "classic PAT"
    OAUTH = "OAuth token"
    FINE_GRAINED_PAT = "fine-grained PAT"
    APP_USER_TO_SERVER = "GitHub App user-to-server token"
    APP_INSTALLATION = "GitHub App installation token"
    REFRESH = "refresh token"
    LEGACY_OR_UNKNOWN = "legacy-format (pre-2021) or unrecognized token"


# Longest/most specific prefix first: "github_pat_" would also match a
# naive "gh" scan, so order isn't actually load-bearing here (all prefixes
# are mutually exclusive on their own), but checking in this order keeps
# the intent obvious.
_PREFIXES: list[tuple[str, TokenKind]] = [
    ("github_pat_", TokenKind.FINE_GRAINED_PAT),
    ("ghp_", TokenKind.CLASSIC_PAT),
    ("gho_", TokenKind.OAUTH),
    ("ghu_", TokenKind.APP_USER_TO_SERVER),
    ("ghs_", TokenKind.APP_INSTALLATION),
    ("ghr_", TokenKind.REFRESH),
]

# The only kinds GitHub exposes scopes for on an ordinary authenticated
# call. LEGACY_OR_UNKNOWN is included: a pre-2021 classic token has no
# prefix at all but behaves identically to a `ghp_` one.
_INTROSPECTABLE = {TokenKind.CLASSIC_PAT, TokenKind.OAUTH, TokenKind.LEGACY_OR_UNKNOWN}


def classify(token: str) -> TokenKind:
    for prefix, kind in _PREFIXES:
        if token.startswith(prefix):
            return kind
    return TokenKind.LEGACY_OR_UNKNOWN


class Verdict(enum.Enum):
    OK = "ok"
    FLAGGED = "flagged"
    UNVERIFIABLE = "unverifiable"
    ERROR = "error"


@dataclass(frozen=True)
class AuditResult:
    name: str
    kind: TokenKind
    verdict: Verdict
    detail: str


def _get_header(headers: dict[str, str], name: str) -> str | None:
    # HTTP header names are case-insensitive; match tolerantly rather than
    # assume http.client (or a test's FakeTransport) preserves one casing.
    lname = name.lower()
    for k, v in headers.items():
        if k.lower() == lname:
            return v
    return None


def audit_token(transport: Transport, name: str, token: str) -> AuditResult:
    """Audits one credential's token. `name` is only used to label the
    result — usually the file's stem (`bot`, `personal`, ...).
    """
    kind = classify(token)
    if kind not in _INTROSPECTABLE:
        return AuditResult(
            name, kind, Verdict.UNVERIFIABLE,
            f"{kind.value}: GitHub exposes no scopes/permissions header for "
            "this token type via the API; check its permissions by hand at "
            "github.com's token settings (or, for a GitHub App, its "
            "installation permissions page).",
        )

    resp: ApiResponse = transport.request(
        method="GET",
        path="/user",
        headers={
            "Authorization": f"token {token}",
            "User-Agent": "grain-credential-audit",
            "X-GitHub-Api-Version": "2022-11-28",
        },
        body=None,
    )
    if resp.status != 200:
        return AuditResult(
            name, kind, Verdict.ERROR,
            f"GET /user returned {resp.status}; could not read scopes "
            f"(body: {resp.body[:200]!r}).",
        )

    header = _get_header(resp.headers, "X-OAuth-Scopes")
    if header is None:
        return AuditResult(
            name, kind, Verdict.UNVERIFIABLE,
            "GET /user succeeded but returned no X-OAuth-Scopes header, "
            "which is unexpected for this token type; cannot confirm scopes.",
        )

    scopes = [s.strip() for s in header.split(",") if s.strip()]
    bad = sorted(s for s in scopes if _is_forbidden(s))
    granted = ", ".join(scopes) if scopes else "(none)"
    if bad:
        return AuditResult(
            name, kind, Verdict.FLAGGED,
            f"carries withheld scope(s): {', '.join(bad)} (full grant: {granted})",
        )
    return AuditResult(name, kind, Verdict.OK, f"scopes: {granted}")


def audit_secrets_dir(transport: Transport, secrets_dir: Path) -> list[AuditResult]:
    """Audits every `*.token` file directly under `secrets_dir`
    (`/data/secrets/github` in production), alphabetically by file name —
    every credential the proxy or the orchestrator could select, not just
    ones `credentials.json` currently references, so a stale or orphaned
    token left on disk still gets checked.
    """
    results: list[AuditResult] = []
    for path in sorted(secrets_dir.glob("*.token")):
        name = path.stem
        token = path.read_text().strip()
        if not token:
            results.append(AuditResult(
                name, TokenKind.LEGACY_OR_UNKNOWN, Verdict.ERROR,
                "token file is empty",
            ))
            continue
        results.append(audit_token(transport, name, token))
    return results
