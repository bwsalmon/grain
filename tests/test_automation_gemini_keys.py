import json
import tempfile
from pathlib import Path

import pytest

from grain.automation.gemini_keys import (
    GeminiKey, GeminiKeyConfig, create_key, delete_key,
)
from grain.run import CommandError, FakeRunner

KEY_NAME = "projects/123/locations/global/keys/abc-def"
OPERATION_NAME = "operations/akmf.p7-123-abc"


def config(**overrides) -> GeminiKeyConfig:
    fields = {"project_id": "proj"}
    fields.update(overrides)
    return GeminiKeyConfig(**fields)


def test_create_key_activates_the_service_account_first():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    create_key(runner, config(), display_name="grain-sandbox-0-issue-1")
    commands = runner.commands
    activate_index = next(i for i, c in enumerate(commands) if "activate-service-account" in c)
    create_index = next(i for i, c in enumerate(commands) if "api-keys create" in c)
    assert activate_index < create_index


def test_create_key_authenticates_from_the_configured_key_file():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    create_key(runner, config(key_path=Path("/custom/key.json")), display_name="d")
    assert runner.ran("gcloud auth activate-service-account --key-file=/custom/key.json")


def test_create_key_restricts_the_api_target():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    create_key(runner, config(), display_name="d")
    create_call = next(c for c in runner.commands if "api-keys create" in c)
    assert "--api-target=service=generativelanguage.googleapis.com" in create_call


def test_create_key_scopes_to_a_custom_api_target():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    create_key(runner, config(api_target_service="example.googleapis.com"), display_name="d")
    create_call = next(c for c in runner.commands if "api-keys create" in c)
    assert "--api-target=service=example.googleapis.com" in create_call


def test_create_key_returns_the_name_and_key_string():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="AIzaSecretValue\n")
    key = create_key(runner, config(), display_name="d")
    assert key == GeminiKey(name=KEY_NAME, key_string="AIzaSecretValue")


def test_create_key_looks_up_the_key_string_by_the_created_resource_name():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="v\n")
    create_key(runner, config(), display_name="d")
    get_call = next(c for c in runner.commands if "get-key-string" in c)
    assert KEY_NAME in get_call


def test_create_key_scopes_the_project():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{KEY_NAME}\n")
    runner.expect("gcloud services api-keys get-key-string", stdout="v\n")
    create_key(runner, config(project_id="my-proj"), display_name="d")
    for call in runner.commands:
        if "api-keys" in call:
            assert "--project=my-proj" in call


def _listing(*entries) -> str:
    """An `api-keys list --format=json` payload: an array of objects with
    the fields real gcloud returns (displayName, uid, createTime,
    updateTime, name, restrictions)."""
    return json.dumps([
        {"displayName": display_name, "name": name, "createTime": created,
         "uid": name.rsplit("/", 1)[-1], "updateTime": created, "restrictions": {}}
        for display_name, name, created in entries
    ])


def test_create_key_looks_the_key_up_when_create_returns_an_operation():
    # bwsalmon/agents#100: `api-keys create` can hand back the id of its
    # own long-running operation instead of the created key's resource
    # name; `get-key-string` 404s unconditionally if called with that.
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list",
                  stdout=_listing(("d", KEY_NAME, "2026-08-26T00:00:00Z")))
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    key = create_key(runner, config(), display_name="d")
    assert key == GeminiKey(name=KEY_NAME, key_string="secret-value")
    get_call = next(c for c in runner.commands if "get-key-string" in c)
    assert KEY_NAME in get_call
    assert OPERATION_NAME not in get_call


def test_create_key_never_describes_an_operation():
    """bwsalmon/agents#104 twice: `services api-keys operations describe`
    does not exist, and `services operations describe` returns an array
    this code then crashed on. The operation is not consulted at all now --
    only create/list/get-key-string, which are exercised for real."""
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list",
                  stdout=_listing(("d", KEY_NAME, "2026-08-26T00:00:00Z")))
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    create_key(runner, config(), display_name="d")
    assert not [c for c in runner.commands if "operations" in c]


def test_create_key_takes_the_newest_key_sharing_a_display_name():
    """A task whose earlier attempt died between create and lookup leaves a
    key behind under the same deterministic display name, so a match is not
    necessarily unique -- the one just minted is the newest."""
    older = "projects/123/locations/global/keys/older"
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list", stdout=_listing(
        ("d", older, "2026-08-25T00:00:00Z"),
        ("d", KEY_NAME, "2026-08-26T00:00:00Z"),
        ("other-task", "projects/123/locations/global/keys/nope", "2026-08-27T00:00:00Z"),
    ))
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    key = create_key(runner, config(), display_name="d")
    assert key.name == KEY_NAME


@pytest.mark.parametrize("payload", [
    "[]",                      # the shape that actually crashed production
    '{"keys": []}',            # an object where an array was expected
    "not json at all",
    '[{"displayName": "d"}]',  # a match carrying no resource name
])
def test_create_key_raises_command_error_on_an_unexpected_listing(payload):
    """The crash that took the whole dispatch pass down: `_await_operation`
    called `.get` on a list and raised `AttributeError`, which is not the
    `CommandError` core.py's `_dispatch` catches -- so one task's failure
    ended the cycle before any later issue was reached. Whatever gcloud
    hands back, this has to stay a CommandError about one task.
    """
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list", stdout=payload)
    runner.expect("gcloud services api-keys delete", stdout="")
    with pytest.raises(CommandError):
        create_key(runner, config(), display_name="d")


def test_create_key_revokes_the_key_it_made_when_the_read_back_fails():
    """bwsalmon/agents#104 leaked one live Generative Language API key per
    retry: the key existed the moment `create` returned, but the caller
    only records a name to revoke later if `create_key` *returns*, so an
    exception on the way out stranded it with nothing holding its name.
    """
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list",
                  stdout=_listing(("d", KEY_NAME, "2026-08-26T00:00:00Z")))
    runner.expect("gcloud services api-keys get-key-string", returncode=1,
                  stderr="PERMISSION_DENIED")
    with pytest.raises(CommandError):
        create_key(runner, config(), display_name="d")
    deletes = [c for c in runner.commands if "api-keys delete" in c]
    assert deletes, "the key create made was left live in the project"
    assert KEY_NAME in deletes[0]


def test_a_cleanup_failure_does_not_replace_the_original_error():
    """The caller is already re-raising the failure that got us here; a
    cleanup error must not mask it with a less informative one."""
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect("gcloud services api-keys list",
                  stdout=_listing(("d", KEY_NAME, "2026-08-26T00:00:00Z")))
    runner.expect("gcloud services api-keys get-key-string", returncode=1,
                  stderr="the original failure")
    runner.expect("gcloud services api-keys delete", returncode=1, stderr="cleanup also failed")
    with pytest.raises(CommandError) as raised:
        create_key(runner, config(), display_name="d")
    assert "the original failure" in str(raised.value)


def test_delete_key_activates_then_deletes_by_resource_name():
    runner = FakeRunner()
    delete_key(runner, config(), KEY_NAME)
    commands = runner.commands
    activate_index = next(i for i, c in enumerate(commands) if "activate-service-account" in c)
    delete_index = next(i for i, c in enumerate(commands) if "api-keys delete" in c)
    assert activate_index < delete_index
    assert KEY_NAME in commands[delete_index]


def test_delete_key_is_quiet_and_scoped_to_the_project():
    runner = FakeRunner()
    delete_key(runner, config(project_id="my-proj"), KEY_NAME)
    delete_call = next(c for c in runner.commands if "api-keys delete" in c)
    assert "--project=my-proj" in delete_call
    assert "--quiet" in delete_call


def test_config_defaults_to_the_controllers_one_gcp_credential():
    # Same default configure_gcp_service_account already writes to --
    # a deployment that set that up for the metadata broker needs no
    # second credential just to enable the grain-gemini-key label.
    assert str(config().key_path) == "/data/secrets/gcp-key-minter.json"


def test_config_loads_from_json():
    path = Path(tempfile.mkdtemp()) / "gemini-key.json"
    path.write_text(json.dumps({"project_id": "proj", "key_path": "/custom/key.json"}))
    loaded = GeminiKeyConfig.load(path)
    assert loaded.project_id == "proj"
    assert loaded.key_path == Path("/custom/key.json")


def test_config_load_honours_a_custom_api_target():
    path = Path(tempfile.mkdtemp()) / "gemini-key.json"
    path.write_text(json.dumps({"project_id": "proj", "api_target_service": "x.googleapis.com"}))
    assert GeminiKeyConfig.load(path).api_target_service == "x.googleapis.com"
