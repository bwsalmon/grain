import base64
import json

import pytest

from grain.proxy.tokens import SandboxTokenStore, SandboxTokens, extract_basic_auth_token


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


# --- SandboxTokenStore ------------------------------------------------------

def test_ensure_token_mints_a_new_token_for_an_unknown_sandbox(tmp_path):
    store = SandboxTokenStore(tmp_path / "sandbox-tokens.json")
    token = store.ensure_token("sandbox-0")
    assert token
    assert json.loads((tmp_path / "sandbox-tokens.json").read_text()) == {"sandbox-0": token}


def test_ensure_token_is_idempotent(tmp_path):
    store = SandboxTokenStore(tmp_path / "sandbox-tokens.json")
    first = store.ensure_token("sandbox-0")
    second = store.ensure_token("sandbox-0")
    assert first == second


def test_ensure_token_mints_distinct_tokens_per_sandbox(tmp_path):
    store = SandboxTokenStore(tmp_path / "sandbox-tokens.json")
    t0 = store.ensure_token("sandbox-0")
    t1 = store.ensure_token("sandbox-1")
    assert t0 != t1


def test_ensure_token_preserves_other_sandboxes_already_on_file(tmp_path):
    path = tmp_path / "sandbox-tokens.json"
    path.write_text(json.dumps({"sandbox-0": "existing-token"}))
    store = SandboxTokenStore(path)
    store.ensure_token("sandbox-1")
    data = json.loads(path.read_text())
    assert data["sandbox-0"] == "existing-token"
    assert "sandbox-1" in data


def test_a_token_minted_by_the_store_authenticates_via_sandbox_tokens(tmp_path):
    path = tmp_path / "sandbox-tokens.json"
    token = SandboxTokenStore(path).ensure_token("sandbox-0")
    assert SandboxTokens(path).authenticate(token) == "sandbox-0"


def test_rotate_replaces_an_existing_token(tmp_path):
    path = tmp_path / "sandbox-tokens.json"
    store = SandboxTokenStore(path)
    original = store.ensure_token("sandbox-0")
    rotated = store.rotate("sandbox-0")
    assert rotated != original
    assert SandboxTokens(path).authenticate(original) is None
    assert SandboxTokens(path).authenticate(rotated) == "sandbox-0"


def test_save_is_atomic_no_partial_file_left_behind(tmp_path):
    path = tmp_path / "sandbox-tokens.json"
    SandboxTokenStore(path).ensure_token("sandbox-0")
    assert path.exists()
    assert not (tmp_path / "sandbox-tokens.json.tmp").exists()
