import pytest

from grain.automation.credential_audit import (
    AuditResult, TokenKind, Verdict, audit_secrets_dir, audit_token, classify,
)
from grain.automation.github import ApiResponse, FakeTransport


# --- classify() -------------------------------------------------------

@pytest.mark.parametrize("token,expected", [
    ("ghp_" + "x" * 36, TokenKind.CLASSIC_PAT),
    ("gho_" + "x" * 36, TokenKind.OAUTH),
    ("github_pat_" + "x" * 22 + "_" + "y" * 59, TokenKind.FINE_GRAINED_PAT),
    ("ghu_" + "x" * 36, TokenKind.APP_USER_TO_SERVER),
    ("ghs_" + "x" * 36, TokenKind.APP_INSTALLATION),
    ("ghr_" + "x" * 36, TokenKind.REFRESH),
    ("a" * 40, TokenKind.LEGACY_OR_UNKNOWN),  # pre-2021 classic-token shape
])
def test_classify_reads_githubs_documented_prefixes(token, expected):
    assert classify(token) == expected


# --- audit_token(): introspectable kinds -------------------------------

def test_clean_classic_pat_is_ok():
    transport = FakeTransport(
        responses=[ApiResponse(200, {"X-OAuth-Scopes": "repo, read:org"}, b"{}")]
    )
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.OK
    assert result.kind is TokenKind.CLASSIC_PAT
    assert "repo" in result.detail


@pytest.mark.parametrize("bad_scope", ["workflow", "delete_repo", "write:org", "admin:org"])
def test_forbidden_scope_is_flagged(bad_scope):
    transport = FakeTransport(
        responses=[ApiResponse(200, {"X-OAuth-Scopes": f"repo, {bad_scope}"}, b"{}")]
    )
    result = audit_token(transport, "personal", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.FLAGGED
    assert bad_scope in result.detail


def test_multiple_forbidden_scopes_all_listed():
    transport = FakeTransport(
        responses=[ApiResponse(200, {"X-OAuth-Scopes": "workflow, delete_repo, repo"}, b"{}")]
    )
    result = audit_token(transport, "personal", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.FLAGGED
    assert "delete_repo" in result.detail and "workflow" in result.detail


def test_admin_wildcard_covers_any_admin_scope():
    transport = FakeTransport(
        responses=[ApiResponse(200, {"X-OAuth-Scopes": "admin:public_key"}, b"{}")]
    )
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.FLAGGED
    assert "admin:public_key" in result.detail


def test_header_lookup_is_case_insensitive():
    transport = FakeTransport(
        responses=[ApiResponse(200, {"x-oauth-scopes": "repo"}, b"{}")]
    )
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.OK


def test_empty_scopes_header_is_ok_not_an_error():
    transport = FakeTransport(responses=[ApiResponse(200, {"X-OAuth-Scopes": ""}, b"{}")])
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.OK
    assert "(none)" in result.detail


def test_legacy_format_token_is_still_introspected():
    transport = FakeTransport(
        responses=[ApiResponse(200, {"X-OAuth-Scopes": "workflow"}, b"{}")]
    )
    result = audit_token(transport, "old", "a" * 40)
    assert result.kind is TokenKind.LEGACY_OR_UNKNOWN
    assert result.verdict is Verdict.FLAGGED


def test_non_200_response_is_an_error_not_a_pass():
    transport = FakeTransport(responses=[ApiResponse(401, {}, b"Bad credentials")])
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.ERROR
    assert "401" in result.detail


def test_missing_scopes_header_on_a_classic_token_is_unverifiable_not_ok():
    transport = FakeTransport(responses=[ApiResponse(200, {}, b"{}")])
    result = audit_token(transport, "bot", "ghp_" + "x" * 36)
    assert result.verdict is Verdict.UNVERIFIABLE


# --- audit_token(): non-introspectable kinds ---------------------------

@pytest.mark.parametrize("token,kind", [
    ("github_pat_" + "x" * 22 + "_" + "y" * 59, TokenKind.FINE_GRAINED_PAT),
    ("ghu_" + "x" * 36, TokenKind.APP_USER_TO_SERVER),
    ("ghs_" + "x" * 36, TokenKind.APP_INSTALLATION),
])
def test_non_introspectable_kinds_are_unverifiable_with_no_network_call(token, kind):
    transport = FakeTransport()
    result = audit_token(transport, "app", token)
    assert result.verdict is Verdict.UNVERIFIABLE
    assert result.kind is kind
    assert transport.calls == []  # never even tries — nothing to read


def test_unverifiable_detail_points_at_a_manual_check():
    transport = FakeTransport()
    result = audit_token(transport, "app", "github_pat_" + "x" * 22 + "_" + "y" * 59)
    assert "github.com" in result.detail


# --- request shape -------------------------------------------------------

def test_request_uses_token_scheme_authorization_and_get_user():
    transport = FakeTransport(responses=[ApiResponse(200, {"X-OAuth-Scopes": "repo"}, b"{}")])
    audit_token(transport, "bot", "ghp_" + "x" * 36)
    call = transport.calls[0]
    assert call["method"] == "GET"
    assert call["path"] == "/user"
    assert call["headers"]["Authorization"] == "token ghp_" + "x" * 36


# --- audit_secrets_dir() --------------------------------------------------

def test_audit_secrets_dir_scans_every_token_file_alphabetically(tmp_path):
    (tmp_path / "bot.token").write_text("ghp_" + "b" * 36 + "\n")
    (tmp_path / "personal.token").write_text("ghp_" + "p" * 36 + "\n")
    (tmp_path / "credentials.json").write_text('{"*": "bot"}')  # not a .token file

    transport = FakeTransport(
        responses=[
            ApiResponse(200, {"X-OAuth-Scopes": "repo"}, b"{}"),
            ApiResponse(200, {"X-OAuth-Scopes": "repo, workflow"}, b"{}"),
        ]
    )
    results = audit_secrets_dir(transport, tmp_path)
    assert [r.name for r in results] == ["bot", "personal"]
    assert results[0].verdict is Verdict.OK
    assert results[1].verdict is Verdict.FLAGGED


def test_audit_secrets_dir_flags_an_empty_token_file_as_an_error(tmp_path):
    (tmp_path / "stale.token").write_text("   \n")
    results = audit_secrets_dir(FakeTransport(), tmp_path)
    assert len(results) == 1
    assert results[0].verdict is Verdict.ERROR
    assert "empty" in results[0].detail


def test_audit_secrets_dir_on_a_missing_directory_layout_is_empty(tmp_path):
    # secrets/github doesn't exist yet on a freshly-checked-out worktree —
    # this environment's actual state, per docs/roadmap.md item 7.
    missing = tmp_path / "does-not-exist"
    assert audit_secrets_dir(FakeTransport(), missing) == []
