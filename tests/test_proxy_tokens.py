import base64
import json

import pytest

from grain.proxy.tokens import SandboxTokens, extract_basic_auth_token


@pytest.fixture
def tokens_path(tmp_path):
    path = tmp_path / "sandbox-tokens.json"
    path.write_text(json.dumps({"sandbox-0": "tok0", "sandbox-1": "tok1"}))
    return path


def test_authenticate_known_token(tokens_path):
    tokens = SandboxTokens(tokens_path)
    assert tokens.authenticate("tok0") == "sandbox-0"
    assert tokens.authenticate("tok1") == "sandbox-1"


def test_authenticate_unknown_token(tokens_path):
    tokens = SandboxTokens(tokens_path)
    assert tokens.authenticate("not-a-real-token") is None


def test_missing_file_authenticates_nothing(tmp_path):
    tokens = SandboxTokens(tmp_path / "does-not-exist.json")
    assert tokens.authenticate("anything") is None


def _basic(username: str, password: str) -> str:
    return "Basic " + base64.b64encode(f"{username}:{password}".encode()).decode()


def test_extract_basic_auth_token_ignores_username():
    assert extract_basic_auth_token(_basic("whatever", "the-token")) == "the-token"
    assert extract_basic_auth_token(_basic("", "the-token")) == "the-token"


def test_extract_basic_auth_token_rejects_malformed_header():
    assert extract_basic_auth_token(None) is None
    assert extract_basic_auth_token("Bearer sometoken") is None
    assert extract_basic_auth_token("Basic not-valid-base64!!!") is None
    assert extract_basic_auth_token("Basic " + base64.b64encode(b"no-colon-here").decode()) is None
