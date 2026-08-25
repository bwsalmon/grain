import json
import tempfile
from pathlib import Path

from grain.automation.gemini_keys import (
    GeminiKey, GeminiKeyConfig, create_key, delete_key,
)
from grain.run import FakeRunner

KEY_NAME = "projects/123/locations/global/keys/abc-def"


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


def test_config_defaults_to_the_shared_gcp_service_account_key_path():
    # Same default configure_gcp_service_account already writes to --
    # a deployment that set that up for the metadata broker needs no
    # second credential just to enable /gemini-key.
    assert str(config().key_path) == "/data/secrets/gcp-service-account.json"


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
