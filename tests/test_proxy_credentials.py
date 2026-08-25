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


# --- get() (bwsalmon/agents#52: named-key lookup, bypassing credentials.json) ----

def test_get_reads_a_named_credential_directly(secrets_dir):
    creds = CredentialSet(secrets_dir)
    cred = creds.get("personal")
    assert cred.name == "personal"
    assert cred.token == "personal-token-value"


def test_get_works_for_a_credential_with_no_pattern_mapping(tmp_path):
    # A key meant only for grain-github-<name> selection never needs a
    # credentials.json entry at all (configure_named_github_key never
    # writes one) -- get() must not depend on the pattern file.
    (tmp_path / "workflow.token").write_text("workflow-token-value\n")
    creds = CredentialSet(tmp_path)
    cred = creds.get("workflow")
    assert cred.name == "workflow"
    assert cred.token == "workflow-token-value"


def test_get_returns_none_for_an_unconfigured_name(secrets_dir):
    creds = CredentialSet(secrets_dir)
    assert creds.get("nonexistent") is None


def test_get_anonymous_has_no_token(tmp_path):
    creds = CredentialSet(tmp_path)
    cred = creds.get("anonymous")
    assert cred.name == "anonymous"
    assert cred.token is None


# --- token_for() (the automation.github.TokenSource shape) ------------------

def test_token_for_returns_the_selected_credentials_token(secrets_dir):
    creds = CredentialSet(secrets_dir)
    assert creds.token_for("bwsalmon", "grain") == "bot-token-value"


def test_token_for_is_none_when_nothing_covers_the_repo(tmp_path):
    (tmp_path / "credentials.json").write_text(json.dumps({"only/this": "bot"}))
    (tmp_path / "bot.token").write_text("x")
    creds = CredentialSet(tmp_path)
    assert creds.token_for("nobody", "covers-this") is None


def test_token_for_is_none_for_an_anonymous_credential(tmp_path):
    (tmp_path / "credentials.json").write_text(json.dumps({"*": "anonymous"}))
    creds = CredentialSet(tmp_path)
    assert creds.token_for("public", "repo") is None
