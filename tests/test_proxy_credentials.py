import json

import pytest

from grain.proxy.credentials import CredentialSet


@pytest.fixture
def secrets_dir(tmp_path):
    (tmp_path / "bot.token").write_text("bot-token-value\n")
    (tmp_path / "personal.token").write_text("personal-token-value\n")
    (tmp_path / "credentials.json").write_text(
        json.dumps(
            {
                "bwsalmon/grain": "bot",
                "bwsalmon/*": "bot",
                "*": "personal",
            }
        )
    )
    return tmp_path


def test_exact_match_wins_over_wildcard(secrets_dir):
    creds = CredentialSet(secrets_dir)
    cred = creds.select("bwsalmon", "grain")
    assert cred.name == "bot"
    assert cred.token == "bot-token-value"


def test_owner_wildcard_covers_other_repos(secrets_dir):
    creds = CredentialSet(secrets_dir)
    cred = creds.select("bwsalmon", "some-other-repo")
    assert cred.name == "bot"


def test_global_fallback(secrets_dir):
    creds = CredentialSet(secrets_dir)
    cred = creds.select("someone-else", "their-repo")
    assert cred.name == "personal"
    assert cred.token == "personal-token-value"


def test_no_credential_covers_it_is_distinct_from_not_found(tmp_path):
    (tmp_path / "credentials.json").write_text(json.dumps({"only/this": "bot"}))
    (tmp_path / "bot.token").write_text("x")
    creds = CredentialSet(tmp_path)
    assert creds.select("nobody", "covers-this") is None


def test_anonymous_credential_has_no_token(tmp_path):
    (tmp_path / "credentials.json").write_text(json.dumps({"*": "anonymous"}))
    creds = CredentialSet(tmp_path)
    cred = creds.select("public", "repo")
    assert cred.name == "anonymous"
    assert cred.token is None
