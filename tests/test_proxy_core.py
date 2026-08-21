import base64
import json

import pytest

from grain.proxy.allowlist import Allowlist
from grain.proxy.audit import RecordingAuditLog
from grain.proxy.core import GitProxy
from grain.proxy.credentials import CredentialSet
from grain.proxy.forward import FakeForwarder, UpstreamResponse
from grain.proxy.tokens import SandboxTokens


def _basic(token: str) -> str:
    return "Basic " + base64.b64encode(f"x:{token}".encode()).decode()


GIT_HEADERS = {"User-Agent": "git/2.39.2", "Accept": "*/*"}


@pytest.fixture
def proxy(tmp_path):
    (tmp_path / "repo-allowlist.json").write_text(json.dumps(["owner/repo"]))
    (tmp_path / "credentials.json").write_text(json.dumps({"*": "bot"}))
    (tmp_path / "bot.token").write_text("bot-token")
    (tmp_path / "sandbox-tokens.json").write_text(json.dumps({"sandbox-0": "tok0"}))

    forwarder = FakeForwarder(response=UpstreamResponse(200, {"Content-Type": "x"}, b"ok"))
    audit = RecordingAuditLog()
    p = GitProxy(
        allowlist=Allowlist(tmp_path / "repo-allowlist.json"),
        credentials=CredentialSet(tmp_path),
        tokens=SandboxTokens(tmp_path / "sandbox-tokens.json"),
        forwarder=forwarder,
        audit=audit,
    )
    return p, forwarder, audit


def test_unknown_path_is_404(proxy):
    p, forwarder, audit = proxy
    resp = p.handle(method="GET", path="/owner/repo.git/HEAD", query="",
                     headers=GIT_HEADERS, body=None)
    assert resp.status == 404
    assert forwarder.calls == []


def test_non_git_user_agent_is_404(proxy):
    p, forwarder, audit = proxy
    resp = p.handle(method="GET", path="/owner/repo.git/info/refs", query="",
                     headers={"User-Agent": "Mozilla/5.0"}, body=None)
    assert resp.status == 404


def test_missing_auth_is_401_before_allowlist_is_consulted(proxy):
    p, forwarder, audit = proxy
    resp = p.handle(method="GET", path="/owner/repo.git/info/refs",
                     query="service=git-upload-pack", headers=GIT_HEADERS, body=None)
    assert resp.status == 401
    assert 'WWW-Authenticate' in resp.headers
    assert forwarder.calls == []
    assert audit.entries == []  # unauthenticated callers leave no trace either way


def test_unknown_token_is_401(proxy):
    p, forwarder, audit = proxy
    headers = {**GIT_HEADERS, "Authorization": _basic("not-a-real-token")}
    resp = p.handle(method="GET", path="/owner/repo.git/info/refs",
                     query="service=git-upload-pack", headers=headers, body=None)
    assert resp.status == 401


def test_unlisted_repo_is_403_with_legible_error(proxy):
    p, forwarder, audit = proxy
    headers = {**GIT_HEADERS, "Authorization": _basic("tok0")}
    resp = p.handle(method="GET", path="/owner/unlisted-repo.git/info/refs",
                     query="service=git-upload-pack", headers=headers, body=None)
    assert resp.status == 403
    assert b"not allow-listed" in resp.body
    assert forwarder.calls == []
    assert audit.entries[0]["outcome"] == "denied: not allow-listed"


def test_allowed_repo_is_forwarded_with_the_selected_credential(proxy):
    p, forwarder, audit = proxy
    headers = {**GIT_HEADERS, "Authorization": _basic("tok0")}
    resp = p.handle(method="GET", path="/owner/repo.git/info/refs",
                     query="service=git-upload-pack", headers=headers, body=None)
    assert resp.status == 200
    assert resp.body == b"ok"
    call = forwarder.calls[0]
    assert call["token"] == "bot-token"
    assert call["path"] == "/owner/repo.git/info/refs"
    assert audit.entries[0]["credential"] == "bot"
    assert audit.entries[0]["sandbox"] == "sandbox-0"


def test_post_body_is_passed_through_untouched(proxy):
    p, forwarder, audit = proxy
    headers = {
        **GIT_HEADERS, "Authorization": _basic("tok0"),
        "Content-Type": "application/x-git-upload-pack-request",
        "Accept": "application/x-git-upload-pack-result",
    }
    resp = p.handle(method="POST", path="/owner/repo.git/git-upload-pack",
                     query="", headers=headers, body=b"some pack request bytes")
    assert resp.status == 200
    assert forwarder.calls[0]["body"] == b"some pack request bytes"


def test_no_configured_credential_is_500_not_forwarded(tmp_path):
    (tmp_path / "repo-allowlist.json").write_text(json.dumps(["owner/repo"]))
    (tmp_path / "credentials.json").write_text(json.dumps({}))  # nothing covers it
    (tmp_path / "sandbox-tokens.json").write_text(json.dumps({"sandbox-0": "tok0"}))
    forwarder = FakeForwarder()
    p = GitProxy(
        allowlist=Allowlist(tmp_path / "repo-allowlist.json"),
        credentials=CredentialSet(tmp_path),
        tokens=SandboxTokens(tmp_path / "sandbox-tokens.json"),
        forwarder=forwarder,
    )
    headers = {**GIT_HEADERS, "Authorization": _basic("tok0")}
    resp = p.handle(method="GET", path="/owner/repo.git/info/refs",
                     query="service=git-upload-pack", headers=headers, body=None)
    assert resp.status == 500
    assert forwarder.calls == []
