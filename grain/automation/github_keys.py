"""Mints short-lived GitHub App installation tokens, each scoped to exactly
one repo -- bwsalmon/agents#159's scratch-repo-for-testing-grain feature:
one dedicated repo per sandbox slot (`repo_for_sandbox`), so a task that
opts in (`config.scratch_repo_label`, `core.py`'s `_resolve_target`) always
lands in a repo nothing else ever touches.

**Minted on demand, not once per dispatch.** `gcp_keys.py`/`gemini_keys.py`
both mint once, at dispatch, record an identifier on the `Assignment`, and
revoke it when the sandbox's slot frees. That shape does not fit here: a
task's target repo is read from GitHub *twice* on two different clocks --
once at dispatch (`_resolve_target`'s own `default_branch`/`branch_exists`
calls) and again whenever the sweep that finishes it happens to run
(`core.py`'s PR-creation step, which can land well over an hour later) --
and an installation token's lifetime is capped at one hour by GitHub
itself, not by this project. A single per-dispatch token cannot be
guaranteed to still be valid by the time the second read happens.

So nothing here is minted once and stored. `InstallationTokenSource`
(a `TokenSource`, `github.py`'s own protocol) mints a fresh token the
moment something actually needs to talk to a scratch repo, caches it
in memory only for as long as it stays valid, and mints again once it's
close to expiring. The same object is usable from both sides that need
one: `cli.py` layers it in front of the orchestrator's own `GitHubClient`
for the API calls it makes (PR creation, branch reads), and
`grain/proxy/server.py` layers an identical one in front of the git
proxy's own `CredentialSet` for the git push a sandbox makes through it
(docs/design.md's "GitHub access": sandboxes never talk to GitHub
directly, so the proxy is the one place a scratch repo's git traffic is
actually authenticated). Neither side ever needs to revoke anything or
coordinate with the other -- each mints its own token, on its own clock,
and an unused one simply expires.

**Why a GitHub App, not a fine-shared PAT.** A fine-grained personal
access token can be scoped to a handful of repos, but GitHub gives no API
to mint one programmatically -- rotating it means a human clicking
"regenerate" on a calendar reminder, and it is a known blind spot that
`grain github audit` cannot even verify a fine-grained PAT's scopes via
the API. An installation access token natively expires in one hour
(GitHub enforces the TTL, not this project) and can be scoped, per mint,
to exactly one repo via the `repositories` parameter below -- tighter
than a PAT, and with no rotation step for a human to forget.

**Authentication: a JWT signed with `openssl`.** Minting an installation
token is a two-step exchange: sign a JWT with the App's own RS256 private
key, then trade it for a token via `POST
/app/installations/{id}/access_tokens`. Python's stdlib has no RSA
implementation, so the signature is produced by shelling out to
`openssl` -- the same "an existing, correct CLI rather than hand-rolled
crypto" call `gemini_keys.py`'s own docstring already made for `gcloud`.
`-hex` rather than `openssl dgst -sign`'s own binary default: `Runner.run`
captures stdout as text (`RealRunner`'s `subprocess.run(..., text=True)`),
which would corrupt a raw binary signature -- hex is ASCII-safe, and
`bytes.fromhex` recovers the real bytes from it below. The token exchange
itself is a plain HTTPS call, made with `github.py`'s own `Transport`
seam (`RealTransport` by default) rather than a second `openssl`/`curl`
shell-out -- no crypto is needed for it, just an HTTP POST, which this
project's other GitHub API code already does directly.
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path

from .github import GitHubError, RealTransport, TokenSource, Transport
from ..run import Runner

DEFAULT_REPO_PREFIX = "grain-scratch"
_DEFAULT_PRIVATE_KEY_PATH = Path("/data/secrets/github-key-minter.pem")

# GitHub caps an App-level JWT's own validity at ten minutes; nine leaves
# a margin without needing to mint one per token exchange.
_JWT_TTL = timedelta(minutes=9)
# GitHub also caps an installation token's own lifetime at one hour,
# non-negotiable -- this module never asks for a different one. A cached
# token is treated as due for renewal a little before that expiry, not
# exactly at it, so a request that starts just before the deadline never
# races GitHub's own clock.
_TOKEN_RENEW_MARGIN = timedelta(minutes=2)


@dataclass(frozen=True)
class GitHubKeyConfig:
    """Operator-set tunables -- the same JSON-file-under-`/data/config`
    shape as `GcpKeyConfig`/`GeminiKeyConfig`. Its presence (or absence)
    at `/data/config/github-key.json` is the on/off switch for
    `config.scratch_repo_label`: `cli.py`'s `build_orchestrator` leaves
    `Orchestrator.github_key_config` `None` when the file is absent, which
    makes `core.py`'s `_resolve_target` refuse the label with an
    explanation, the same "feature not configured" shape
    `gemini_key_config` already has.
    """

    app_id: str
    installation_id: str
    # The account the scratch repos live under -- never guessed from a
    # task's own directives (a scratch-repo task carries no `/repo` line
    # at all; see `core.py`'s `_resolve_target`).
    owner: str
    repo_prefix: str = DEFAULT_REPO_PREFIX
    private_key_path: Path = _DEFAULT_PRIVATE_KEY_PATH

    @classmethod
    def load(cls, path: Path) -> "GitHubKeyConfig":
        raw = json.loads(path.read_text())
        if "private_key_path" in raw:
            raw["private_key_path"] = Path(raw["private_key_path"])
        return cls(**raw)


@dataclass(frozen=True)
class GitHubKey:
    token: str
    expires_at: datetime


def repo_for_sandbox(config: GitHubKeyConfig, sandbox: str) -> str:
    """The one repo dedicated to `sandbox` -- deterministic, so `core.py`
    can pick a scratch task's target the moment a sandbox is assigned,
    before anything is ever minted.
    """
    return f"{config.repo_prefix}-{sandbox}"


def _b64url(data: bytes) -> str:
    # JWT segments are unpadded base64url (RFC 7519) -- `=` is stripped,
    # not just swapped for `-`/`_`.
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _sign_rs256(runner: Runner, key_path: Path, signing_input: str) -> bytes:
    """RS256-signs `signing_input` with the private key at `key_path`,
    returning the raw signature bytes -- see this module's docstring for
    why `-hex` rather than `dgst -sign`'s own binary default.
    """
    result = runner.run(
        ["openssl", "dgst", "-sha256", "-hex", "-sign", str(key_path)],
        stdin=signing_input,
    )
    # openssl's own output is `"<digest-name>(stdin)= <hex>"`; only the
    # hex half is ever needed, and the digest-name prefix is not this
    # module's to depend on (it names the hash, not the signature).
    hex_signature = result.stdout.strip().rsplit(" ", 1)[-1]
    return bytes.fromhex(hex_signature)


def _build_jwt(runner: Runner, config: GitHubKeyConfig, now: datetime) -> str:
    header = _b64url(json.dumps({"alg": "RS256", "typ": "JWT"}).encode())
    payload = _b64url(json.dumps({
        # Issued 60s in the past -- the margin GitHub's own docs
        # recommend, so a controller clock a little ahead of GitHub's own
        # doesn't get this rejected as "not yet valid."
        "iat": int(now.timestamp()) - 60,
        "exp": int((now + _JWT_TTL).timestamp()),
        "iss": config.app_id,
    }).encode())
    signing_input = f"{header}.{payload}"
    signature = _sign_rs256(runner, config.private_key_path, signing_input)
    return f"{signing_input}.{_b64url(signature)}"


def create_installation_token(runner: Runner, config: GitHubKeyConfig, repo: str, *,
                                transport: Transport | None = None,
                                now: datetime | None = None) -> GitHubKey:
    """Mints an installation access token scoped to exactly `repo` --
    never every repo the App's installation covers -- via the
    `repositories` parameter on GitHub's own token-exchange endpoint.
    `runner` is always the controller's own account, and is used only to
    shell out to `openssl` for the JWT signature above; the token
    exchange itself talks to GitHub directly over `transport` (defaulting
    to `RealTransport` -- the same `Transport` seam `github.py`'s own
    `GitHubClient` uses, reused rather than duplicated, so this has the
    identical fake-for-tests story).
    """
    now = now or datetime.now(timezone.utc)
    jwt = _build_jwt(runner, config, now)
    transport = transport or RealTransport()
    resp = transport.request(
        method="POST",
        path=f"/app/installations/{config.installation_id}/access_tokens",
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "grain-automation",
            "X-GitHub-Api-Version": "2022-11-28",
            "Authorization": f"Bearer {jwt}",
            "Content-Type": "application/json",
        },
        body=json.dumps({"repositories": [repo]}).encode(),
    )
    if resp.status != 201:
        raise GitHubError(resp.status, resp.body)
    data = json.loads(resp.body)
    return GitHubKey(
        token=data["token"],
        expires_at=datetime.fromisoformat(data["expires_at"].replace("Z", "+00:00")),
    )


@dataclass
class InstallationTokenSource:
    """A `TokenSource` (`github.py`'s protocol) that mints a fresh
    installation token on demand for any repo shaped like
    `repo_for_sandbox`'s own output, under `config.owner` -- `None` for
    anything else, so this composes with an ordinary `CredentialSet`
    ladder (`cli.py`'s `build_orchestrator`, `grain/proxy/server.py`'s
    `build_proxy`) rather than replacing it: every non-scratch repo falls
    straight through to whatever `TokenSource` this one is layered in
    front of.

    Caches in memory, per repo, until a token is within
    `_TOKEN_RENEW_MARGIN` of expiring -- see this module's own docstring
    for why nothing here is minted once and stored past that.
    """

    runner: Runner
    config: GitHubKeyConfig
    transport: Transport | None = None
    _cache: dict[str, GitHubKey] = field(default_factory=dict)

    def token_for(self, owner: str, repo: str, *, now: datetime | None = None) -> str | None:
        if owner != self.config.owner or not repo.startswith(f"{self.config.repo_prefix}-"):
            return None
        now = now or datetime.now(timezone.utc)
        cached = self._cache.get(repo)
        if cached is None or cached.expires_at - now < _TOKEN_RENEW_MARGIN:
            cached = create_installation_token(
                self.runner, self.config, repo, transport=self.transport, now=now,
            )
            self._cache[repo] = cached
        return cached.token


@dataclass(frozen=True)
class FallbackTokenSource:
    """Tries `primary` first, falling back to `secondary` when it answers
    `None` -- how `cli.py`'s `build_orchestrator` and
    `grain/proxy/server.py`'s `build_proxy` each layer an
    `InstallationTokenSource` in front of their existing `CredentialSet`,
    without either needing to know the other exists.
    """

    primary: TokenSource
    secondary: TokenSource

    def token_for(self, owner: str, repo: str) -> str | None:
        token = self.primary.token_for(owner, repo)
        return token if token is not None else self.secondary.token_for(owner, repo)
