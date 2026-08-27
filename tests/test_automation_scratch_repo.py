import json
from pathlib import Path

from grain.automation.scratch_repo import ScratchRepoConfig, repo_for_sandbox


def test_repo_for_sandbox_names_the_dedicated_repo():
    assert repo_for_sandbox(ScratchRepoConfig(owner="acme"), "sandbox-0") == "grain-scratch-sandbox-0"


def test_repo_for_sandbox_honours_a_custom_prefix():
    config = ScratchRepoConfig(owner="acme", repo_prefix="test-repo")
    assert repo_for_sandbox(config, "sandbox-1") == "test-repo-sandbox-1"


def test_load_reads_owner_and_prefix(tmp_path: Path):
    path = tmp_path / "scratch-repo.json"
    path.write_text(json.dumps({"owner": "acme", "repo_prefix": "test-repo"}))
    config = ScratchRepoConfig.load(path)
    assert config == ScratchRepoConfig(owner="acme", repo_prefix="test-repo")


def test_load_defaults_the_prefix_when_absent(tmp_path: Path):
    path = tmp_path / "scratch-repo.json"
    path.write_text(json.dumps({"owner": "acme"}))
    config = ScratchRepoConfig.load(path)
    assert config == ScratchRepoConfig(owner="acme")
