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
                 credential_overrides: SandboxCredentialOverrides | None = None) -> None:
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

        override_name = (
            self.credential_overrides.for_sandbox(sandbox)
            if self.credential_overrides is not None else None
        )
        if override_name is not None:
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
