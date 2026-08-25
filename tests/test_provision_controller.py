"""Content checks for provision/controller.sh -- the controller's first-boot
provisioning script (docs/roadmap.md item 3), mirroring the checks that
would matter most: it parses as valid bash, and the paths/values it writes
actually match what the Python modules that read them expect by default.
`bash -n` plus these pinned-path assertions is the "reasonable bar" the
task set for a script whose logic is mostly file content, not control flow
-- see tests/test_vm_integration.py's `TestControllerProvisioning` for the
live-boot half of this verification.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

from grain.automation.config import AutomationConfig
from grain.metadata.config import MetadataConfig

SCRIPT = Path(__file__).parent.parent / "provision" / "controller.sh"


def read() -> str:
    return SCRIPT.read_text()


def test_script_is_syntactically_valid_bash():
    result = subprocess.run(["bash", "-n", str(SCRIPT)], capture_output=True, text=True)
    assert result.returncode == 0, result.stderr


def test_script_is_executable_or_at_least_has_a_shebang():
    assert read().startswith("#!/bin/bash")


def test_installs_python_and_ssh_and_git():
    text = read()
    assert "python3" in text
    assert "openssh-client" in text
    assert "git " in text or "git\n" in text


def test_generates_the_controller_ssh_key_idempotently_at_the_configured_path():
    text = read()
    key_path = AutomationConfig(task_owner="x", task_repo="y").ssh_key_path
    assert str(key_path) in text
    assert f"{key_path}.pub" in text or f"{key_path}" in text
    # Idempotent: must not clobber an existing identity on a re-run.
    assert f"if [ ! -f {key_path} ]" in text
    assert "ssh-keygen -t ed25519" in text


def test_creates_the_data_layout_automation_expects():
    text = read()
    for path in ("/data/secrets", "/data/secrets/github", "/data/config",
                 "/data/state/automation", "/data/state/git-proxy",
                 "/data/state/metadata-server"):
        assert path in text, f"{path} not created by provision/controller.sh"


def test_metadata_server_state_dir_is_writable_by_the_metadata_user():
    """Unlike git-proxy and automation next to it, this directory is
    written by `gce_metadata_server` itself, running as the unprivileged
    `grain-metadata` user (grain/metadata/launcher.py) -- root:root 0755
    alone would leave every instance unable to create its own log file.
    """
    text = read()
    config = MetadataConfig(service_account_email="x@y.iam.gserviceaccount.com",
                             project_id="proj")
    assert f"chown {config.metadata_user}:{config.metadata_user} /data/state/metadata-server" in text


def test_metadata_server_default_key_path_and_user_are_consistent():
    text = read()
    config = MetadataConfig(service_account_email="x@y.iam.gserviceaccount.com",
                             project_id="proj")
    assert str(config.key_path) in text
    assert config.metadata_user in text
    assert config.binary in text


def test_installs_gce_metadata_server_binary():
    text = read()
    assert "gce_metadata_server" in text
    assert "/usr/local/bin/gce_metadata_server" in text
    assert "chmod +x /usr/local/bin/gce_metadata_server" in text


def test_never_writes_a_secret_value_only_paths():
    """The invariant docs/design.md states outright: "no secret is ever
    baked into an image or a provisioning script." A generated *keypair* is
    not a baked-in secret (it's fresh, unique per controller) but this
    script must never embed a GitHub token, API key, or GCP credential
    value.
    """
    text = read()
    for marker in ("ghp_", "github_pat_", "AIza", "-----BEGIN"):
        assert marker not in text


def test_installs_systemd_units_but_does_not_enable_them():
    text = read()
    assert "grain-automation.service" in text
    assert "grain-automation.timer" in text
    assert "grain-git-proxy.service" in text
    executable_lines = [
        line for line in text.splitlines()
        if not line.strip().startswith("#") and "systemctl" in line
    ]
    assert all("enable" not in line and "start" not in line for line in executable_lines), (
        executable_lines
    )
    assert "systemctl daemon-reload" in text


def test_git_proxy_unit_binds_the_controllers_private_address_not_0000():
    text = read()
    assert "--host ${CONTROLLER_IP}" in text
    assert "--host 0.0.0.0" not in text


def test_creates_opt_grain_for_the_manual_code_deploy_step():
    assert "/opt/grain" in read()
