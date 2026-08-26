"""Content checks for provision/sandbox.sh -- the sandbox's first-boot
provisioning script, mirroring tests/test_provision_controller.py's
approach: `bash -n` plus pinned-string assertions rather than an actual
boot (tests/test_vm_integration.py's live suite covers that).
"""

from __future__ import annotations

import subprocess
from pathlib import Path

SCRIPT = Path(__file__).parent.parent / "provision" / "sandbox.sh"


def read() -> str:
    return SCRIPT.read_text()


def test_script_is_syntactically_valid_bash():
    result = subprocess.run(["bash", "-n", str(SCRIPT)], capture_output=True, text=True)
    assert result.returncode == 0, result.stderr


def test_script_is_executable_or_at_least_has_a_shebang():
    assert read().startswith("#!/bin/bash")


def test_installs_gcloud_and_terraform_for_gcp_host_adapter_testing():
    # bwsalmon/agents#117: an agent working in a sandbox needs the CLIs
    # themselves to drive terraform/gcp/ -- the ADC credentials were
    # already reachable with no sandbox-side setup (docs/design.md, "GCP
    # credentials"), but there was nothing installed to use them with.
    text = read()
    assert "google-cloud-cli" in text
    assert " terraform" in text
    assert "apt-get install" in text
    assert "gnupg" in text


def test_gcloud_and_terraform_are_installed_via_their_own_signed_apt_repos():
    text = read()
    assert "packages.cloud.google.com/apt" in text
    assert "apt.releases.hashicorp.com" in text
    # Same keyring pattern the Docker install already uses in this file --
    # no bare `apt-key add`, no unsigned repo.
    assert "signed-by=/usr/share/keyrings/cloud.google.gpg" in text
    assert "signed-by=/usr/share/keyrings/hashicorp.gpg" in text


def test_never_writes_a_secret_value_only_paths():
    """docs/design.md's invariant: "no secret is ever baked into an image
    or a provisioning script." Adding gcloud/terraform must not change
    that -- both authenticate via ADC through the metadata-server proxy,
    never a key baked in here.
    """
    text = read()
    for marker in ("ghp_", "github_pat_", "AIza", "-----BEGIN"):
        assert marker not in text


def test_agent_tools_readme_mentions_the_new_clis():
    text = read()
    assert "gcloud" in text
    assert "terraform" in text
