import json
from pathlib import Path

from grain.automation.config import AutomationConfig


def write(tmp_path: Path, body: dict) -> Path:
    path = tmp_path / "automation.json"
    path.write_text(json.dumps(body))
    return path


def test_load_reads_every_field_from_the_file(tmp_path):
    path = write(tmp_path, {
        "task_owner": "acme", "task_repo": "widgets",
        "default_target_repo": "acme/widgets", "runs_per_hour": 30,
    })
    config = AutomationConfig.load(path)
    assert config.task_owner == "acme"
    assert config.task_repo == "widgets"
    assert config.default_target_repo == "acme/widgets"
    assert config.runs_per_hour == 30


def test_load_translates_legacy_owner_repo_keys(tmp_path):
    # Predates the task/target split -- see config.py's own docstring.
    path = write(tmp_path, {"owner": "acme", "repo": "widgets"})
    config = AutomationConfig.load(path)
    assert config.task_owner == "acme"
    assert config.task_repo == "widgets"


def test_load_prefers_the_new_key_when_both_old_and_new_are_present(tmp_path):
    path = write(tmp_path, {
        "owner": "legacy-owner", "task_owner": "acme",
        "repo": "legacy-repo", "task_repo": "widgets",
    })
    config = AutomationConfig.load(path)
    assert config.task_owner == "acme"
    assert config.task_repo == "widgets"


def test_load_drops_the_legacy_base_branch_key(tmp_path):
    # No longer has a coherent meaning across many target repos -- accepted
    # and discarded rather than rejected, so an already-deployed controller
    # doesn't need /data edited before a version bump.
    path = write(tmp_path, {
        "task_owner": "acme", "task_repo": "widgets", "base_branch": "main",
    })
    config = AutomationConfig.load(path)  # must not raise on the unknown field
    assert config.task_owner == "acme"


def test_load_converts_a_custom_ssh_key_path_to_a_path_object(tmp_path):
    path = write(tmp_path, {
        "task_owner": "acme", "task_repo": "widgets",
        "ssh_key_path": "/custom/controller-ssh",
    })
    config = AutomationConfig.load(path)
    assert config.ssh_key_path == Path("/custom/controller-ssh")


def test_load_defaults_ssh_key_path_when_not_given(tmp_path):
    path = write(tmp_path, {"task_owner": "acme", "task_repo": "widgets"})
    config = AutomationConfig.load(path)
    assert config.ssh_key_path == Path("/data/secrets/controller-ssh")
