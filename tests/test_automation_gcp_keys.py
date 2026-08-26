import json
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from grain.automation.gcp_keys import (
    GcpKey, GcpKeyConfig, create_key, delete_expired_keys, delete_key,
)
from grain.run import CommandError, FakeRunner

EMAIL = "grain-agent@my-proj.iam.gserviceaccount.com"
KEY_ID = "abc123def456"
KEY_JSON = json.dumps(
    {"type": "service_account", "private_key": "fake", "private_key_id": KEY_ID})


def config(**overrides) -> GcpKeyConfig:
    fields = {"service_account_email": EMAIL, "project_id": "my-proj"}
    fields.update(overrides)
    return GcpKeyConfig(**fields)


# --- create_key -------------------------------------------------------

def test_create_key_mints_for_the_configured_service_account():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    create_key(runner, config())
    create_call = next(c for c in runner.commands if "keys create" in c)
    assert f"--iam-account={EMAIL}" in create_call


def test_create_key_activates_the_minter_before_minting():
    """bwsalmon/agents#131. This originally asserted the opposite -- that
    nothing here authenticates at all, on the premise that the controller
    runs as the host service account via a native GCE metadata server. The
    controller is a nested libvirt guest, not a GCE VM, so `gcloud` there
    had no account and every mint failed with "You do not currently have
    an active account selected".
    """
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    create_key(runner, config())
    activate = next(i for i, c in enumerate(runner.commands)
                    if "activate-service-account" in c)
    mint = next(i for i, c in enumerate(runner.commands) if "keys create" in c)
    assert activate < mint


def test_the_minter_is_not_the_account_being_minted_for():
    """The separation bwsalmon/agents#126 exists for: a sandbox holding a
    leaked agent key must not be able to mint itself a fresh one. The key
    file activated here is the *minter's*, never the agent account's own
    credential at /data/secrets/gcp-service-account.json.
    """
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    create_key(runner, config())
    activate = next(c for c in runner.commands if "activate-service-account" in c)
    assert "/data/secrets/gcp-key-minter.json" in activate
    assert "gcp-service-account.json" not in activate


def test_create_key_returns_the_bare_id_and_key_json():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    key = create_key(runner, config())
    assert key == GcpKey(key_id=KEY_ID, key_json=KEY_JSON)


def test_create_key_reads_the_key_back_from_the_file_gcloud_wrote():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    create_key(runner, config())
    create_call = next(c for c in runner.commands if "keys create" in c)
    cat_call = next(c for c in runner.commands if c.startswith("cat "))
    # The path passed to `create` is the same one `cat` reads back.
    tmp_path = create_call.split()[5]
    assert tmp_path in cat_call


def test_create_key_removes_its_own_temp_file_afterward():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=KEY_JSON)
    create_key(runner, config())
    create_call = next(c for c in runner.commands if "keys create" in c)
    tmp_path = create_call.split()[5]
    rm_call = next(c for c in runner.commands if c.startswith("rm -f"))
    assert tmp_path in rm_call


def test_create_key_raises_command_error_when_gcloud_prints_no_id():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config())


def test_create_key_raises_command_error_when_the_key_file_is_empty():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout="")
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config())


def test_create_key_revokes_the_key_it_made_when_the_read_back_fails():
    """bwsalmon/agents#104's gemini_keys.py lesson, applied here: the key
    exists in the project the moment `create` returns, so a failure to
    read it back must not leave it live with nothing holding its id."""
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", returncode=1, stderr="permission denied")
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config())
    deletes = [c for c in runner.commands if "keys delete" in c]
    assert deletes, "the key create made was left live in the project"
    assert KEY_ID in deletes[0]


def test_a_cleanup_failure_does_not_replace_the_original_error():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", returncode=1, stderr="the original failure")
    runner.expect("gcloud iam service-accounts keys delete", returncode=1,
                  stderr="cleanup also failed")
    with pytest.raises(CommandError) as raised:
        create_key(runner, config())
    assert "the original failure" in str(raised.value)


def test_create_key_still_removes_the_temp_file_when_read_back_fails():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", returncode=1, stderr="permission denied")
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config())
    assert [c for c in runner.commands if c.startswith("rm -f")]


# --- delete_key ---------------------------------------------------------

def test_delete_key_deletes_by_bare_id_and_iam_account():
    runner = FakeRunner()
    delete_key(runner, config(), KEY_ID)
    delete_call = next(c for c in runner.commands if "keys delete" in c)
    assert KEY_ID in delete_call
    assert f"--iam-account={EMAIL}" in delete_call


def test_delete_key_is_quiet():
    runner = FakeRunner()
    delete_key(runner, config(), KEY_ID)
    delete_call = next(c for c in runner.commands if "keys delete" in c)
    assert "--quiet" in delete_call


def test_delete_key_activates_the_minter_first():
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    delete_key(runner, config(), KEY_ID)
    activate = next(i for i, c in enumerate(runner.commands)
                    if "activate-service-account" in c)
    delete = next(i for i, c in enumerate(runner.commands) if "keys delete" in c)
    assert activate < delete


# --- delete_expired_keys --------------------------------------------------

def _listing(*entries) -> str:
    """A `keys list --format=json` payload: an array of objects with the
    fields real gcloud returns for a service account key."""
    return json.dumps([
        {"name": f"projects/p/serviceAccounts/{EMAIL}/keys/{key_id}",
         "validAfterTime": valid_after, "keyAlgorithm": "KEY_ALG_RSA_2048"}
        for key_id, valid_after in entries
    ])


NOW = datetime(2026, 8, 26, 12, 0, 0, tzinfo=timezone.utc)


def test_delete_expired_keys_deletes_only_keys_older_than_the_max_age():
    runner = FakeRunner()
    fresh_time = (NOW - timedelta(hours=1)).isoformat().replace("+00:00", "Z")
    stale_time = (NOW - timedelta(hours=25)).isoformat().replace("+00:00", "Z")
    runner.expect("gcloud iam service-accounts keys list",
                  stdout=_listing(("fresh", fresh_time), ("stale", stale_time)))
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    deleted = delete_expired_keys(runner, config(), now=NOW)
    assert deleted == ["stale"]
    delete_call = next(c for c in runner.commands if "keys delete" in c)
    assert "stale" in delete_call
    assert "fresh" not in delete_call


def test_delete_expired_keys_honours_a_custom_max_age():
    runner = FakeRunner()
    age_10h = (NOW - timedelta(hours=10)).isoformat().replace("+00:00", "Z")
    runner.expect("gcloud iam service-accounts keys list",
                  stdout=_listing(("k", age_10h)))
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    assert delete_expired_keys(runner, config(max_key_age_hours=24), now=NOW) == []
    assert delete_expired_keys(runner, config(max_key_age_hours=1), now=NOW) == ["k"]


def test_delete_expired_keys_lists_scoped_to_user_managed_keys():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys list", stdout=_listing())
    delete_expired_keys(runner, config(), now=NOW)
    list_call = next(c for c in runner.commands if "keys list" in c)
    assert "--managed-by=user" in list_call
    assert f"--iam-account={EMAIL}" in list_call


def test_delete_expired_keys_skips_a_key_with_no_valid_after_time():
    runner = FakeRunner()
    payload = json.dumps([{"name": f"projects/p/serviceAccounts/{EMAIL}/keys/k"}])
    runner.expect("gcloud iam service-accounts keys list", stdout=payload)
    assert delete_expired_keys(runner, config(), now=NOW) == []


def test_delete_expired_keys_is_best_effort_per_key():
    """One key's delete failing must not stop the rest from being reaped
    this cycle -- the same "surface it, don't gate on it" bar the rest of
    this project's periodic hygiene passes hold to."""
    from grain.run import Result

    stale_time = (NOW - timedelta(hours=48)).isoformat().replace("+00:00", "Z")

    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys list",
                  stdout=_listing(("bad", stale_time), ("good", stale_time)))

    real_run = runner.run

    def run_side_effect(argv, *, stdin=None, check=True):
        if "keys" in argv and "delete" in argv and "bad" in argv:
            runner.calls.append((list(argv), stdin))
            raise CommandError(argv, 1, "not found")
        return real_run(argv, stdin=stdin, check=check)

    runner.run = run_side_effect  # type: ignore[method-assign]
    deleted = delete_expired_keys(runner, config(), now=NOW)
    assert deleted == ["good"]


def test_delete_expired_keys_raises_command_error_on_an_unexpected_listing():
    runner = FakeRunner()
    runner.expect("gcloud iam service-accounts keys list", stdout="not json")
    with pytest.raises(CommandError):
        delete_expired_keys(runner, config(), now=NOW)


# --- GcpKeyConfig ---------------------------------------------------------

def test_config_defaults_to_a_24_hour_max_key_age():
    assert config().max_key_age_hours == 24


def test_config_loads_from_json():
    path = Path(tempfile.mkdtemp()) / "gcp-key.json"
    path.write_text(json.dumps({
        "service_account_email": EMAIL, "project_id": "my-proj",
    }))
    loaded = GcpKeyConfig.load(path)
    assert loaded.service_account_email == EMAIL
    assert loaded.project_id == "my-proj"
    assert loaded.max_key_age_hours == 24


def test_config_load_honours_a_custom_max_key_age():
    path = Path(tempfile.mkdtemp()) / "gcp-key.json"
    path.write_text(json.dumps({
        "service_account_email": EMAIL, "project_id": "my-proj",
        "max_key_age_hours": 6,
    }))
    assert GcpKeyConfig.load(path).max_key_age_hours == 6


# --- where the key id comes from (bwsalmon/agents#140) --------------------

def test_the_key_id_comes_from_the_key_file_when_gcloud_prints_nothing():
    """The bug, reported live: `keys create --format=value(name.basename())`
    prints nothing on the controller's gcloud, so every mint died on
    "gcloud printed no key id". The credentials file gcloud just wrote
    carries private_key_id, which is the same id `keys delete` takes.
    """
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout="")  # prints nothing
    runner.expect("cat", stdout=KEY_JSON)
    key = create_key(runner, config())
    assert key.key_id == KEY_ID
    assert key.key_json == KEY_JSON


def test_the_key_file_wins_over_what_gcloud_printed():
    """The file was written by the same call that made the key, so it
    cannot disagree with what is actually on disk; stdout can."""
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout="something-else\n")
    runner.expect("cat", stdout=KEY_JSON)
    assert create_key(runner, config()).key_id == KEY_ID


def test_a_key_file_with_no_id_falls_back_to_what_gcloud_printed():
    """The fallback exists for a gcloud whose file shape differs -- better
    a working mint than a hard failure, given the file is the new thing."""
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout=f"{KEY_ID}\n")
    runner.expect("cat", stdout=json.dumps({"type": "service_account"}))
    assert create_key(runner, config()).key_id == KEY_ID


def test_no_id_from_either_source_raises_without_a_bogus_delete():
    """A key exists that this call cannot name. Say so rather than calling
    `keys delete ""` -- the periodic reap is the net for it."""
    runner = FakeRunner()
    runner.expect("gcloud auth activate-service-account", stdout="")
    runner.expect("gcloud iam service-accounts keys create", stdout="")
    runner.expect("cat", stdout=json.dumps({"type": "service_account"}))
    runner.expect("gcloud iam service-accounts keys delete", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config())
    assert not [c for c in runner.commands if "keys delete" in c]
