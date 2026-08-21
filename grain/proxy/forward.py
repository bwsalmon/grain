"""Forwards a validated request to the real GitHub smart-HTTP endpoint.

Behind a `Forwarder` interface for the same reason `run.py` puts command
execution behind a `Runner`: the proxy's decision logic (core.py) is worth
testing without a real network call to github.com on every test run.
"""

from __future__ import annotations

import base64
import http.client
from dataclasses import dataclass, field
from typing import Protocol


@dataclass(frozen=True)
class UpstreamResponse:
    status: int
    headers: dict[str, str]
    body: bytes


class Forwarder(Protocol):
    def forward(self, *, method: str, path: str, query: str,
                headers: dict[str, str], body: bytes | None,
                token: str | None) -> UpstreamResponse: ...


class RealForwarder:
    """Talks to the real GitHub over HTTPS."""

    def __init__(self, host: str = "github.com") -> None:
        self.host = host

    def forward(self, *, method: str, path: str, query: str,
                headers: dict[str, str], body: bytes | None,
                token: str | None) -> UpstreamResponse:
        out_headers = {
            "Content-Type": headers.get("Content-Type", ""),
            "Accept": headers.get("Accept", "*/*"),
            "User-Agent": headers.get("User-Agent", "git/grain-proxy"),
        }
        if token:
            # GitHub's HTTPS PAT convention: the token as the Basic auth
            # username, empty password.
            out_headers["Authorization"] = (
                "Basic " + base64.b64encode(f"{token}:".encode()).decode()
            )
        out_headers = {k: v for k, v in out_headers.items() if v}

        conn = http.client.HTTPSConnection(self.host, timeout=30)
        try:
            full_path = f"{path}?{query}" if query else path
            conn.request(method, full_path, body=body, headers=out_headers)
            resp = conn.getresponse()
            data = resp.read()
            return UpstreamResponse(resp.status, dict(resp.getheaders()), data)
        finally:
            conn.close()


@dataclass
class FakeForwarder:
    """Records calls and replays a scripted response. For tests."""

    response: UpstreamResponse = field(
        default_factory=lambda: UpstreamResponse(200, {}, b"")
    )
    calls: list[dict] = field(default_factory=list)

    def forward(self, *, method: str, path: str, query: str,
                headers: dict[str, str], body: bytes | None,
                token: str | None) -> UpstreamResponse:
        self.calls.append(
            {"method": method, "path": path, "query": query,
             "headers": dict(headers), "body": body, "token": token}
        )
        return self.response
