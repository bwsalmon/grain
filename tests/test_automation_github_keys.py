import base64
import json
import subprocess
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from grain.automation.github import ApiResponse, FakeTransport, GitHubError
from grain.automation.github_keys import (
    FallbackTokenSource, GitHubKey, GitHubKeyConfig, InstallationTokenSource,
    create_installation_token, repo_for_sandbox,
)
from grain.run import CommandError, FakeRunner, RealRunner

NOW = datetime(2026, 8, 27, 12, 0, 0, tzinfo=timezone.utc)


def config(**overrides) -> GitHubKeyConfig:
    fields = {"app_id": "123", "installation_id": "456", "owner": "acme"}
    fields.update(overrides)
    return GitHubKeyConfig(**fields)


def token_response(*, token="ghs_secret", expires_at="2026-08-27T13:00:00Z", status=201):
    return FakeTransport(responses=[
        ApiResponse(status, {}, json.dumps({"token": token, "expires_at": expires_at}).encode())
    ])


def _hex_sign_runner() -> FakeRunner:
    """A FakeRunner scripting `openssl dgst -sha256 -hex -sign` the way it
    actually answers -- see test_signs_a_real_jwt_verifiable_signature
    below for proof this shape is real, not assumed.
    """
    runner = FakeRunner()
    runner.expect(
        "openssl dgst -sha256 -hex -sign",
        stdout="SHA2-256(stdin)= " + ("ab" * 256) + "\n",
    )
    return runner


def test_repo_for_sandbox_names_the_dedicated_repo():
    assert repo_for_sandbox(config(), "sandbox-0") == "grain-scratch-sandbox-0"


def test_repo_for_sandbox_honours_a_custom_prefix():
    assert repo_for_sandbox(config(repo_prefix="test-repo"), "sandbox-1") == "test-repo-sandbox-1"


def test_config_load_reads_the_private_key_path_as_a_path(tmp_path):
    path = tmp_path / "github-key.json"
    path.write_text(json.dumps({
        "app_id": "1", "installation_id": "2", "owner": "acme",
        "private_key_path": "/custom/key.pem",
    }))
    loaded = GitHubKeyConfig.load(path)
    assert loaded.private_key_path == Path("/custom/key.pem")


def test_create_installation_token_scopes_to_exactly_one_repo():
    runner = _hex_sign_runner()
    transport = token_response()
    create_installation_token(runner, config(), "grain-scratch-sandbox-0",
                                transport=transport, now=NOW)
    call = transport.calls[0]
    assert json.loads(call["body"]) == {"repositories": ["grain-scratch-sandbox-0"]}


def test_create_installation_token_hits_the_right_installation():
    runner = _hex_sign_runner()
    transport = token_response()
    create_installation_token(runner, config(installation_id="789"), "r",
                                transport=transport, now=NOW)
    assert transport.calls[0]["path"] == "/app/installations/789/access_tokens"


def test_create_installation_token_authenticates_with_a_bearer_jwt():
    runner = _hex_sign_runner()
    transport = token_response()
    create_installation_token(runner, config(), "r", transport=transport, now=NOW)
    auth = transport.calls[0]["headers"]["Authorization"]
    assert auth.startswith("Bearer ")
    assert auth.count(".") == 2  # header.payload.signature


def test_create_installation_token_signs_with_the_configured_key_path():
    runner = _hex_sign_runner()
    create_installation_token(
        runner, config(private_key_path=Path("/custom/key.pem")), "r",
        transport=token_response(), now=NOW,
    )
    sign_call = next(c for c in runner.commands if "dgst" in c)
    assert "/custom/key.pem" in sign_call


def test_create_installation_token_returns_the_token_and_expiry():
    runner = _hex_sign_runner()
    key = create_installation_token(
        runner, config(), "r",
        transport=token_response(token="ghs_abc", expires_at="2026-08-27T13:00:00Z"),
        now=NOW,
    )
    assert key == GitHubKey(
        token="ghs_abc",
        expires_at=datetime(2026, 8, 27, 13, 0, 0, tzinfo=timezone.utc),
    )


def test_create_installation_token_raises_on_a_non_201():
    runner = _hex_sign_runner()
    transport = FakeTransport(responses=[ApiResponse(403, {}, b"not installed on that repo")])
    with pytest.raises(GitHubError):
        create_installation_token(runner, config(), "r", transport=transport, now=NOW)


def test_jwt_claims_the_configured_app_id():
    runner = _hex_sign_runner()
    create_installation_token(runner, config(app_id="my-app-id"), "r",
                                transport=token_response(), now=NOW)
    jwt = runner.calls[0][1]  # the signing input passed as stdin
    payload = jwt.split(".")[1]
    padded = payload + "=" * (-len(payload) % 4)
    claims = json.loads(base64.urlsafe_b64decode(padded))
    assert claims["iss"] == "my-app-id"


def test_jwt_expires_within_ten_minutes_as_github_requires():
    runner = _hex_sign_runner()
    create_installation_token(runner, config(), "r", transport=token_response(), now=NOW)
    jwt = runner.calls[0][1]
    payload = jwt.split(".")[1]
    padded = payload + "=" * (-len(payload) % 4)
    claims = json.loads(base64.urlsafe_b64decode(padded))
    assert claims["exp"] - claims["iat"] <= 600


@pytest.mark.skipif(
    subprocess.run(["which", "openssl"], capture_output=True).returncode != 0,
    reason="openssl not on PATH",
)
def test_signs_a_real_jwt_verifiable_signature():
    """FakeRunner tests above assert this module *asks* openssl to sign
    correctly, but the shape of `openssl dgst -sha256 -hex -sign`'s real
    output is itself an assumption -- exactly the kind gcp_keys.py's own
    docstring warns a FakeRunner-only test can't catch (bwsalmon/agents#100,
    #104). This runs the real binary and verifies the signature it
    produces actually validates against the matching public key.
    """
    with tempfile.TemporaryDirectory() as tmp:
        key_path = Path(tmp) / "key.pem"
        pub_path = Path(tmp) / "key.pub"
        subprocess.run(["openssl", "genrsa", "-out", str(key_path), "2048"],
                        check=True, capture_output=True)
        subprocess.run(["openssl", "rsa", "-in", str(key_path), "-pubout", "-out", str(pub_path)],
                        check=True, capture_output=True)

        runner = RealRunner()
        transport = token_response()
        create_installation_token(
            runner, config(private_key_path=key_path), "r", transport=transport, now=NOW,
        )
        jwt = transport.calls[0]["headers"]["Authorization"].removeprefix("Bearer ")
        signing_input, signature_segment = jwt.rsplit(".", 1)
        padded = signature_segment + "=" * (-len(signature_segment) % 4)
        signature = base64.urlsafe_b64decode(padded)

        sig_path = Path(tmp) / "sig.bin"
        sig_path.write_bytes(signature)
        verify = subprocess.run(
            ["openssl", "dgst", "-sha256", "-verify", str(pub_path),
             "-signature", str(sig_path)],
            input=signing_input.encode(), capture_output=True,
        )
        assert verify.returncode == 0, verify.stderr


# --- InstallationTokenSource ------------------------------------------------

def test_token_source_returns_none_for_a_non_scratch_repo():
    source = InstallationTokenSource(_hex_sign_runner(), config(), token_response())
    assert source.token_for("acme", "some-other-repo") is None


def test_token_source_returns_none_for_a_different_owner():
    source = InstallationTokenSource(_hex_sign_runner(), config(), token_response())
    assert source.token_for("someone-else", "grain-scratch-sandbox-0") is None


def test_token_source_mints_for_a_matching_scratch_repo():
    source = InstallationTokenSource(_hex_sign_runner(), config(),
                                      token_response(token="ghs_minted"))
    assert source.token_for("acme", "grain-scratch-sandbox-0") == "ghs_minted"


def test_token_source_caches_within_the_tokens_lifetime():
    transport = token_response(
        token="ghs_first",
        expires_at=(NOW + timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ"),
    )
    source = InstallationTokenSource(_hex_sign_runner(), config(), transport)
    first = source.token_for("acme", "grain-scratch-sandbox-0")
    second = source.token_for("acme", "grain-scratch-sandbox-0")
    assert first == second == "ghs_first"
    assert len(transport.calls) == 1


def test_token_source_re_mints_a_different_repo_independently():
    transport = FakeTransport(responses=[
        ApiResponse(201, {}, json.dumps({
            "token": "ghs_zero", "expires_at": "2026-08-27T13:00:00Z",
        }).encode()),
        ApiResponse(201, {}, json.dumps({
            "token": "ghs_one", "expires_at": "2026-08-27T13:00:00Z",
        }).encode()),
    ])
    source = InstallationTokenSource(_hex_sign_runner(), config(), transport)
    assert source.token_for("acme", "grain-scratch-sandbox-0") == "ghs_zero"
    assert source.token_for("acme", "grain-scratch-sandbox-1") == "ghs_one"


# --- FallbackTokenSource -----------------------------------------------------

class _StaticSource:
    def __init__(self, token):
        self._token = token

    def token_for(self, owner, repo):
        return self._token


def test_fallback_prefers_the_primary_when_it_has_an_answer():
    source = FallbackTokenSource(_StaticSource("primary"), _StaticSource("secondary"))
    assert source.token_for("o", "r") == "primary"


def test_fallback_falls_through_when_the_primary_has_none():
    source = FallbackTokenSource(_StaticSource(None), _StaticSource("secondary"))
    assert source.token_for("o", "r") == "secondary"
