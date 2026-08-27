"""The proxy's decision logic — independent of the HTTP transport.

Order matters and mirrors docs/design.md's own spec:

    match the four legal smart-HTTP paths,
    canonicalize and check (owner, repo) against the allowlist, default-deny,
    authenticate the caller by per-sandbox token,
    select the credential for that repo and set Authorization,
    stream the body through, and log the tuple.

Authentication is checked *before* the allowlist so an unauthenticated
caller learns nothing about which repos exist on this proxy — a 401 with no
allowlist information leaked, same as a real git server's own behavior
toward an unauthenticated client.
"""

from __future__ import annotations

from dataclasses import dataclass

from .allowlist import Allowlist
from .audit import AuditLog
from .credentials import Credential, CredentialSet
from .forward import Forwarder
from .protocol import err_pkt, is_valid_git_request, parse_path
from .tokens import SandboxCredentialOverrides, SandboxTokens, extract_basic_auth_token
from ..automation.github import GitHubError, TokenSource
from ..run import CommandError


@dataclass(frozen=True)
class ProxyResponse:
    status: int
    headers: dict[str, str]
    body: bytes


class NullAuditLog:
    def record(self, **_kwargs) -> None:
        pass


class GitProxy:
    def __init__(self, allowlist: Allowlist, credentials: CredentialSet,
                 tokens: SandboxTokens, forwarder: Forwarder,
                 audit: AuditLog | None = None,
                 credential_overrides: SandboxCredentialOverrides | None = None,
                 scratch_source: TokenSource | None = None) -> None:
        self.allowlist = allowlist
        self.credentials = credentials
        self.tokens = tokens
        self.forwarder = forwarder
        self.audit = audit or NullAuditLog()
        # bwsalmon/agents#52: a task's `grain-github-<name>` label, applied
        # per sandbox by `core.py`'s `_dispatch`. `None` for a deployment
        # that never wires one (every existing caller/test) -- every
        # request then falls back to the owner/repo ladder exactly as
        # before, since `for_sandbox` would otherwise always answer None
        # anyway.
        self.credential_overrides = credential_overrides
        # bwsalmon/agents#159: normally an
        # `automation.github_keys.InstallationTokenSource`. Checked before
        # `credential_overrides`/the ordinary ladder below, for the one
        # narrow class of repo (`repo_for_sandbox`'s own naming) that a
        # freshly-minted installation token, not a static file, is the
        # right credential for -- see that module's own docstring for why
        # this can't be a static `CredentialSet` entry the way every other
        # credential here is. `None` for a deployment that never
        # configured `github-key.json` (`build_proxy`), the same
        # "feature not wired" shape `credential_overrides` above already
        # has.
        self.scratch_source = scratch_source

    def handle(self, *, method: str, path: str, query: str,
               headers: dict[str, str], body: bytes | None) -> ProxyResponse:
        req = parse_path(path)
        if req is None:
            return ProxyResponse(404, {}, b"not found")

        if not is_valid_git_request(
            headers.get("User-Agent", ""), headers.get("Accept", ""), req.action
        ):
            return ProxyResponse(404, {}, b"not found")

        token = extract_basic_auth_token(headers.get("Authorization"))
        sandbox = self.tokens.authenticate(token) if token else None
        if sandbox is None:
            return ProxyResponse(
                401,
                {"WWW-Authenticate": 'Basic realm="grain-git-proxy"'},
                err_pkt("authentication required"),
            )

        if not self.allowlist.allows(req.owner, req.repo):
            self.audit.record(
                sandbox=sandbox, owner=req.owner, repo=req.repo,
                action=req.action, credential=None,
                outcome="denied: not allow-listed",
            )
            return ProxyResponse(
                403, {}, err_pkt(f"{req.owner}/{req.repo} is not allow-listed")
            )

        scratch_token = None
        if self.scratch_source is not None:
            try:
                scratch_token = self.scratch_source.token_for(req.owner, req.repo)
            except (GitHubError, CommandError) as exc:
                # Minting happens inline, on this request -- a broken
                # minter (a bad or rotated App key, GitHub unreachable)
                # must come back as a legible 500, the same as any other
                # "cannot resolve a credential" case below, rather than an
                # unhandled exception aborting the connection.
                self.audit.record(
                    sandbox=sandbox, owner=req.owner, repo=req.repo,
                    action=req.action, credential=None,
                    outcome=f"error: could not mint a scratch-repo credential: {exc}",
                )
                return ProxyResponse(
                    500, {}, err_pkt("could not mint a scratch-repo credential")
                )
        override_name = (
            self.credential_overrides.for_sandbox(sandbox)
            if self.credential_overrides is not None else None
        )
        if scratch_token is not None:
            # bwsalmon/agents#159: takes priority over the named-override
            # ladder below -- a repo shaped like `repo_for_sandbox`'s own
            # naming is never meant to be reached any other way, so there
            # is nothing to weigh this against.
            credential = Credential(name="scratch", token=scratch_token)
        elif override_name is not None:
            # This task's `grain-github-<name>` label names a credential
            # explicitly -- it overrides the owner/repo ladder entirely
            # rather than narrowing it, since the whole point is a scope
            # the ladder's own credentials deliberately don't carry.
            credential = self.credentials.get(override_name)
            if credential is None:
                self.audit.record(
                    sandbox=sandbox, owner=req.owner, repo=req.repo,
                    action=req.action, credential=None,
                    outcome=f"error: no credential named {override_name!r} configured",
                )
                return ProxyResponse(
                    500, {},
                    err_pkt(f"no credential named {override_name!r} configured"),
                )
        else:
            credential = self.credentials.select(req.owner, req.repo)
            if credential is None:
                self.audit.record(
                    sandbox=sandbox, owner=req.owner, repo=req.repo,
                    action=req.action, credential=None,
                    outcome="error: no credential configured",
                )
                return ProxyResponse(
                    500, {}, err_pkt("no credential configured for this repository")
                )

        upstream = self.forwarder.forward(
            method=method,
            path=f"/{req.owner}/{req.repo}.git/{req.action}",
            query=query, headers=headers, body=body, token=credential.token,
        )
        self.audit.record(
            sandbox=sandbox, owner=req.owner, repo=req.repo, action=req.action,
            credential=credential.name, outcome=f"forwarded: {upstream.status}",
        )
        return ProxyResponse(upstream.status, upstream.headers, upstream.body)
