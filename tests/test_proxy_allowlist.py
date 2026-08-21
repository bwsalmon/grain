import json

import pytest

from grain.proxy.allowlist import Allowlist, canonicalize


def test_canonicalize_lowercases_and_strips_dot_git():
    assert canonicalize("Owner", "Repo.git") == ("owner", "repo")
    assert canonicalize("owner", "repo") == ("owner", "repo")


@pytest.fixture
def allowlist_path(tmp_path):
    return tmp_path / "repo-allowlist.json"


def test_default_deny_when_file_is_absent(allowlist_path):
    a = Allowlist(allowlist_path)
    assert not a.allows("owner", "repo")


def test_allows_a_listed_repo(allowlist_path):
    allowlist_path.write_text(json.dumps(["owner/repo"]))
    a = Allowlist(allowlist_path)
    assert a.allows("owner", "repo")
    assert a.allows("Owner", "Repo.git")  # case- and suffix-insensitive
    assert not a.allows("owner", "other-repo")


def test_picks_up_edits_without_restart(allowlist_path):
    allowlist_path.write_text(json.dumps(["owner/repo"]))
    a = Allowlist(allowlist_path)
    assert a.allows("owner", "repo")
    assert not a.allows("owner", "second")

    allowlist_path.write_text(json.dumps(["owner/repo", "owner/second"]))
    assert a.allows("owner", "second")
