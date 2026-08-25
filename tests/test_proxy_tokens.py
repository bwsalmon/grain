import base64
import json

import pytest

from grain.proxy.tokens import (
    SandboxCredentialOverrides, SandboxCredentialStore,
    SandboxTokenStore, SandboxTokens, extract_basic_auth_token,
)


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


# --- SandboxCredentialOverrides / SandboxCredentialStore (bwsalmon/agents#52) ----

def test_for_sandbox_with_no_file_is_none(tmp_path):
    overrides = SandboxCredentialOverrides(tmp_path / "sandbox-github-key.json")
    assert overrides.for_sandbox("sandbox-0") is None


def test_for_sandbox_with_no_entry_is_none(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    path.write_text(json.dumps({"sandbox-1": "workflow"}))
    overrides = SandboxCredentialOverrides(path)
    assert overrides.for_sandbox("sandbox-0") is None


def test_set_then_for_sandbox_round_trips(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    SandboxCredentialStore(path).set("sandbox-0", "workflow")
    assert SandboxCredentialOverrides(path).for_sandbox("sandbox-0") == "workflow"


def test_set_is_re_read_with_no_restart_needed(tmp_path):
    # The whole point of this class over SandboxTokens: a single
    # long-lived SandboxCredentialOverrides instance (as the proxy holds)
    # must see a write made after it was constructed.
    path = tmp_path / "sandbox-github-key.json"
    overrides = SandboxCredentialOverrides(path)
    assert overrides.for_sandbox("sandbox-0") is None
    SandboxCredentialStore(path).set("sandbox-0", "workflow")
    assert overrides.for_sandbox("sandbox-0") == "workflow"


def test_set_preserves_other_sandboxes_already_on_file(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    store = SandboxCredentialStore(path)
    store.set("sandbox-0", "workflow")
    store.set("sandbox-1", "release")
    data = json.loads(path.read_text())
    assert data == {"sandbox-0": "workflow", "sandbox-1": "release"}


def test_set_overwrites_a_prior_value_for_the_same_sandbox(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    store = SandboxCredentialStore(path)
    store.set("sandbox-0", "workflow")
    store.set("sandbox-0", "release")
    assert SandboxCredentialOverrides(path).for_sandbox("sandbox-0") == "release"


def test_clear_removes_an_override(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    store = SandboxCredentialStore(path)
    store.set("sandbox-0", "workflow")
    store.clear("sandbox-0")
    assert SandboxCredentialOverrides(path).for_sandbox("sandbox-0") is None


def test_clear_is_a_no_op_with_no_file(tmp_path):
    # Called unconditionally by sweeper.py's _release for every freed
    # sandbox, whether or not that task ever set an override.
    store = SandboxCredentialStore(tmp_path / "sandbox-github-key.json")
    store.clear("sandbox-0")  # must not raise
    assert not (tmp_path / "sandbox-github-key.json").exists()


def test_clear_preserves_other_sandboxes(tmp_path):
    path = tmp_path / "sandbox-github-key.json"
    store = SandboxCredentialStore(path)
    store.set("sandbox-0", "workflow")
    store.set("sandbox-1", "release")
    store.clear("sandbox-0")
    data = json.loads(path.read_text())
    assert data == {"sandbox-1": "release"}
