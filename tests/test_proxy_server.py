import json
import threading
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

from grain.proxy.core import ProxyResponse
from grain.proxy.server import build_proxy, main, make_handler, run
from grain.proxy.tokens import SandboxCredentialOverrides


def test_build_proxy_defaults_to_the_real_github_forward_target(tmp_path: Path):
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "github.com"
    assert proxy.forwarder.use_tls is True
    # bwsalmon/agents#52: wired to the same /data/config path
    # grain/cli.py's build_orchestrator writes overrides to.
    assert isinstance(proxy.credential_overrides, SandboxCredentialOverrides)
    assert proxy.credential_overrides._path == tmp_path / "config" / "sandbox-github-key.json"


def test_build_proxy_honours_a_mock_git_forward_host_from_automation_json(tmp_path: Path):
    """The git-transport half of the mock-GitHub live-test seam
    (docs/roadmap.md item 8) -- `automation.json`'s `git_forward_host`/
    `github_use_tls` (written by `configure_repo`) should reach the real
    proxy the same way `github_host` reaches `RealTransport` in
    `grain/cli.py`'s `build_orchestrator`.
    """
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    (tmp_path / "config" / "automation.json").write_text(json.dumps({
        "owner": "acme", "repo": "widgets",
        "github_host": "10.100.0.1:8443", "git_forward_host": "10.100.0.1:8443",
        "github_use_tls": False,
    }))
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "10.100.0.1:8443"
    assert proxy.forwarder.use_tls is False


def test_build_proxy_falls_back_to_real_github_when_automation_json_is_absent(tmp_path: Path):
    """The git-proxy service can be enabled before `controller configure`
    has ever run -- provisioning installs it disabled, and nothing
    sequences configure before enable except `bootstrap()`'s own stage
    order. A missing config file must not crash the proxy.
    """
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "github.com"


# --- make_handler / run (real HTTP transport) -------------------------------

class _StubProxy:
    """Returns a fixed `ProxyResponse` and records what it was called
    with -- `GitProxy`'s own decision logic is covered end-to-end by
    test_proxy_core.py; what's untested here is only the HTTP transport
    `make_handler` wraps around it (path/query split, body read, and which
    response headers survive vs. get overwritten).
    """

    def __init__(self, response: ProxyResponse) -> None:
        self.response = response
        self.calls: list[dict] = []

    def handle(self, *, method, path, query, headers, body):
        self.calls.append(
            {"method": method, "path": path, "query": query, "headers": headers, "body": body}
        )
        return self.response


def _serve(proxy) -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(proxy))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def test_handler_splits_path_and_query_and_streams_the_response_back():
    proxy = _StubProxy(ProxyResponse(200, {"X-Test": "yes"}, b"hello"))
    server = _serve(proxy)
    try:
        port = server.server_address[1]
        url = f"http://127.0.0.1:{port}/acme/widgets.git/info/refs?service=git-upload-pack"
        with urllib.request.urlopen(url) as resp:
            assert resp.status == 200
            assert resp.read() == b"hello"
            assert resp.headers["X-Test"] == "yes"
    finally:
        server.shutdown()
        server.server_close()
    assert proxy.calls[0]["method"] == "GET"
    assert proxy.calls[0]["path"] == "/acme/widgets.git/info/refs"
    assert proxy.calls[0]["query"] == "service=git-upload-pack"


def test_handler_forwards_the_request_body_on_post():
    proxy = _StubProxy(ProxyResponse(200, {}, b"ok"))
    server = _serve(proxy)
    try:
        port = server.server_address[1]
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/acme/widgets.git/git-upload-pack",
            data=b"the request body", method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            assert resp.read() == b"ok"
    finally:
        server.shutdown()
        server.server_close()
    assert proxy.calls[0]["method"] == "POST"
    assert proxy.calls[0]["body"] == b"the request body"


def test_handler_recomputes_content_length_and_drops_hop_by_hop_headers():
    proxy = _StubProxy(ProxyResponse(
        200,
        {"Content-Length": "999", "Connection": "close",
         "Transfer-Encoding": "chunked", "X-Keep": "1"},
        b"body",
    ))
    server = _serve(proxy)
    try:
        port = server.server_address[1]
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/x") as resp:
            assert resp.headers["Content-Length"] == "4"
            assert resp.headers["X-Keep"] == "1"
            assert resp.headers.get("Transfer-Encoding") is None
    finally:
        server.shutdown()
        server.server_close()


def test_handler_surfaces_a_non_200_status_from_the_proxy():
    proxy = _StubProxy(ProxyResponse(403, {}, b"denied"))
    server = _serve(proxy)
    try:
        port = server.server_address[1]
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/x")
            raise AssertionError("expected an HTTPError for a 403 response")
        except urllib.error.HTTPError as exc:
            assert exc.code == 403
            assert exc.read() == b"denied"
    finally:
        server.shutdown()
        server.server_close()


def test_run_binds_the_given_host_and_port_and_serves_forever(monkeypatch):
    captured = {}

    class FakeServer:
        def __init__(self, address, handler_cls):
            captured["address"] = address
            captured["handler_cls"] = handler_cls

        def serve_forever(self):
            captured["served"] = True

    monkeypatch.setattr("grain.proxy.server.ThreadingHTTPServer", FakeServer)
    run(object(), host="10.100.0.1", port=9999)
    assert captured["address"] == ("10.100.0.1", 9999)
    assert captured["served"] is True


# --- main() (CLI wiring) -----------------------------------------------------

def test_main_wires_args_into_build_proxy_and_run(monkeypatch, tmp_path):
    captured = {}

    def fake_build_proxy(data_dir):
        captured["data_dir"] = data_dir
        return "the-proxy"

    def fake_run(proxy, host, port):
        captured["proxy"] = proxy
        captured["host"] = host
        captured["port"] = port

    monkeypatch.setattr("grain.proxy.server.build_proxy", fake_build_proxy)
    monkeypatch.setattr("grain.proxy.server.run", fake_run)
    rc = main(["--data-dir", str(tmp_path), "--host", "127.0.0.1", "--port", "9000"])
    assert rc == 0
    assert captured == {
        "data_dir": tmp_path, "proxy": "the-proxy", "host": "127.0.0.1", "port": 9000,
    }


def test_main_defaults_to_data_dir_and_wildcard_host_and_port_8080(monkeypatch):
    captured = {}
    monkeypatch.setattr(
        "grain.proxy.server.build_proxy",
        lambda data_dir: captured.setdefault("data_dir", data_dir) or "the-proxy",
    )
    monkeypatch.setattr(
        "grain.proxy.server.run",
        lambda proxy, host, port: captured.update(proxy=proxy, host=host, port=port),
    )
    assert main([]) == 0
    assert captured["data_dir"] == Path("/data")
    assert captured["host"] == "0.0.0.0"
    assert captured["port"] == 8080
