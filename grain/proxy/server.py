"""Wires `GitProxy`'s decision logic to a real HTTP server.

stdlib only, matching the rest of this project — `http.server` is enough
for a proxy whose whole job is decide-then-stream, no templating or
routing framework needed.
"""

from __future__ import annotations

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

from ..automation.config import AutomationConfig
from .allowlist import Allowlist
from .audit import FileAuditLog
from .core import GitProxy
from .credentials import CredentialSet
from .forward import RealForwarder
from .tokens import SandboxCredentialOverrides, SandboxTokens


def make_handler(proxy: GitProxy) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _handle(self, method: str) -> None:
            split = urlsplit(self.path)
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            headers = {k: v for k, v in self.headers.items()}

            resp = proxy.handle(
                method=method, path=split.path, query=split.query,
                headers=headers, body=body,
            )

            self.send_response(resp.status)
            for k, v in resp.headers.items():
                if k.lower() in ("content-length", "connection", "transfer-encoding"):
                    continue
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(resp.body)))
            self.end_headers()
            self.wfile.write(resp.body)

        def do_GET(self) -> None:
            self._handle("GET")

        def do_POST(self) -> None:
            self._handle("POST")

        def log_message(self, format: str, *args) -> None:
            pass  # the audit log is the record of intent; stderr spam is not

    return Handler


def run(proxy: GitProxy, host: str = "0.0.0.0", port: int = 8080) -> None:
    server = ThreadingHTTPServer((host, port), make_handler(proxy))
    server.serve_forever()


def build_proxy(data_dir: Path) -> GitProxy:
    """Wires the real files under a /data-shaped directory — see
    docs/design.md, "Secrets on /data".

    The forward target defaults to real GitHub; it's overridden only when
    `automation.json` carries a non-default `git_forward_host` — the
    git-transport half of the mock-GitHub live-test seam
    (docs/roadmap.md item 8), the other half being `RealTransport` in
    `grain/cli.py`'s `build_orchestrator`. Missing/unconfigured
    `automation.json` (the proxy can be enabled before `controller
    configure` runs) falls back to the real default rather than failing.
    """
    forwarder = RealForwarder()
    automation_path = data_dir / "config" / "automation.json"
    if automation_path.exists():
        config = AutomationConfig.load(automation_path)
        forwarder = RealForwarder(config.git_forward_host, use_tls=config.github_use_tls)
    return GitProxy(
        allowlist=Allowlist(data_dir / "config" / "repo-allowlist.json"),
        # bwsalmon/agents#159/#186: a scratch repo (`repo_for_sandbox`'s
        # own naming) is authenticated exactly like any other repo here --
        # an `owner/*` (or narrower) pattern an operator adds to
        # credentials.json by hand, resolved by the ordinary ladder below.
        # See `scratch_repo.py`'s own docstring for why no scratch-repo-
        # specific credential logic lives in this module any more.
        credentials=CredentialSet(data_dir / "secrets" / "github"),
        tokens=SandboxTokens(data_dir / "secrets" / "sandbox-tokens.json"),
        forwarder=forwarder,
        audit=FileAuditLog(data_dir / "state" / "git-proxy" / "audit.log"),
        # bwsalmon/agents#52: under /data/config, not /data/secrets --
        # see SandboxCredentialOverrides' own docstring for why it needs
        # the same re-read-every-request treatment as Allowlist above.
        credential_overrides=SandboxCredentialOverrides(
            data_dir / "config" / "sandbox-github-key.json"
        ),
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", default="/data", type=Path)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8080)
    args = parser.parse_args(argv)
    run(build_proxy(args.data_dir), args.host, args.port)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
