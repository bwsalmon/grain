import json
import shlex
import tempfile
from pathlib import Path

import pytest

from grain.automation import gemini_keys as gemini_keys_module
from grain.automation.gemini_keys import (
    GeminiKey, GeminiKeyConfig, create_key, delete_key,
)
from grain.run import CommandError, FakeRunner, Result

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


def test_create_key_resolves_an_operation_returned_by_create():
    # bwsalmon/agents#100: `api-keys create` can hand back the id of its
    # own long-running operation instead of the created key's resource
    # name; `get-key-string` 404s unconditionally if called with that.
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect(
        f"gcloud services api-keys operations describe {OPERATION_NAME}",
        stdout=json.dumps({"done": True, "response": {"name": KEY_NAME}}),
    )
    runner.expect("gcloud services api-keys get-key-string", stdout="secret-value\n")
    key = create_key(runner, config(), display_name="d")
    assert key == GeminiKey(name=KEY_NAME, key_string="secret-value")
    get_call = next(c for c in runner.commands if "get-key-string" in c)
    assert KEY_NAME in get_call
    assert OPERATION_NAME not in get_call


def test_create_key_raises_when_the_operation_reports_an_error():
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect(
        f"gcloud services api-keys operations describe {OPERATION_NAME}",
        stdout=json.dumps({"done": True, "error": {"message": "quota exceeded"}}),
    )
    with pytest.raises(CommandError):
        create_key(runner, config(), display_name="d")


class _SequencedDescribeRunner:
    """A minimal `Runner` that reports an operation as not-yet-done on its
    first `operations describe` call and done on its second, to exercise
    `_await_operation`'s polling loop -- `FakeRunner` scripts one fixed
    response per command prefix, so it can't express a response that
    changes across repeated calls to the same command.
    """

    def __init__(self) -> None:
        self.commands: list[str] = []
        self._describe_calls = 0

    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result:
        rendered = shlex.join(argv)
        self.commands.append(rendered)
        if "api-keys create" in rendered:
            return Result(argv, 0, f"{OPERATION_NAME}\n", "")
        if "operations describe" in rendered:
            self._describe_calls += 1
            done = self._describe_calls >= 2
            body = {"done": done}
            if done:
                body["response"] = {"name": KEY_NAME}
            return Result(argv, 0, json.dumps(body), "")
        if "get-key-string" in rendered:
            return Result(argv, 0, "secret-value\n", "")
        return Result(argv, 0, "", "")


def test_create_key_polls_the_operation_until_it_finishes(monkeypatch):
    slept = []
    monkeypatch.setattr(gemini_keys_module.time, "sleep", slept.append)
    runner = _SequencedDescribeRunner()
    key = create_key(runner, config(), display_name="d")
    assert key.name == KEY_NAME
    assert slept  # polled at least once before the operation was done
    describe_calls = [c for c in runner.commands if "operations describe" in c]
    assert len(describe_calls) == 2


def test_create_key_gives_up_on_an_operation_that_never_finishes(monkeypatch):
    monkeypatch.setattr(gemini_keys_module.time, "sleep", lambda seconds: None)
    ticks = iter([0, 1000])  # first check < deadline, second is past it
    monkeypatch.setattr(gemini_keys_module.time, "monotonic", lambda: next(ticks))
    runner = FakeRunner()
    runner.expect("gcloud services api-keys create", stdout=f"{OPERATION_NAME}\n")
    runner.expect(
        f"gcloud services api-keys operations describe {OPERATION_NAME}",
        stdout=json.dumps({"done": False}),
    )
    with pytest.raises(CommandError):
        create_key(runner, config(), display_name="d")


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
    # second credential just to enable the grain-gemini-key label.
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
