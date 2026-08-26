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

import re
import subprocess
from pathlib import Path

from grain.automation.config import AutomationConfig

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
                 "/data/state/automation", "/data/state/git-proxy"):
        assert path in text, f"{path} not created by provision/controller.sh"


def test_no_longer_installs_the_metadata_broker():
    """bwsalmon/agents#126 removed the per-sandbox gce_metadata_server
    broker entirely -- GCP access now arrives as a real key minted and
    pushed per dispatch (grain/automation/gcp_keys.py), needing nothing
    installed on the controller beyond gcloud itself."""
    text = read()
    assert "gce_metadata_server" not in text
    assert "grain-metadata" not in text
    assert "/data/state/metadata-server" not in text


def test_installs_gcloud_for_gemini_and_gcp_key_support():
    # bwsalmon/agents#47/#126: grain/automation/gemini_keys.py and
    # grain/automation/gcp_keys.py both shell out to gcloud on the
    # controller -- must actually be on the box.
    text = read()
    assert "google-cloud-cli" in text
    assert "gnupg" in text
    assert "apt-get install" in text


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
    # On the verb, not on substrings: `systemctl restart systemd-journald`
    # (the journal-forwarding block below) contains "start" but enables
    # nothing of grain's -- which is what this is actually guarding.
    assert all(
        not re.search(r"systemctl\s+(--\S+\s+)*(enable|start)\b", line)
        for line in executable_lines
    ), executable_lines
    assert "systemctl daemon-reload" in text


def test_git_proxy_unit_binds_the_controllers_private_address_not_0000():
    text = read()
    assert "--host ${CONTROLLER_IP}" in text
    assert "--host 0.0.0.0" not in text


def test_controller_ip_is_the_substitutable_placeholder_not_the_default_subnet():
    """The adapter fills this in per-deployment (grain/inventory.py's
    CONTROLLER_IP_PLACEHOLDER) -- a literal 10.100.0.2 here would only be
    right for the default subnet and silently wrong on any other one.
    """
    from grain.inventory import CONTROLLER_IP_PLACEHOLDER

    text = read()
    assert f'CONTROLLER_IP="{CONTROLLER_IP_PLACEHOLDER}"' in text
    assert "10.100.0.2" not in text


def test_creates_opt_grain_for_the_manual_code_deploy_step():
    assert "/opt/grain" in read()


def test_journal_forwarding_is_applied_with_a_job_type_journald_supports():
    """Found live, and it took down the whole deploy: `systemctl reload
    systemd-journald` fails with "Job type reload is not applicable for unit
    systemd-journald.service" -- journald reads journald.conf at start, so
    `restart` is what applies the drop-in.
    """
    text = read()
    assert "/etc/systemd/journald.conf.d/forward-to-console.conf" in text
    assert "ForwardToConsole=yes" in text
    assert "systemctl reload systemd-journald" not in text
    assert "systemctl restart systemd-journald" in text


def test_journal_forwarding_cannot_abort_provisioning():
    """The block is a diagnostic convenience; under `set -eux` its one
    failing line aborted the entire script, cloud-init recorded the run as
    failed, and `grain host bootstrap` stopped at stage 5 on a controller
    that was in fact fully built. Logging must not be able to do that.
    """
    text = read()
    line = next(l for l in text.splitlines()
                if l.startswith("systemctl restart systemd-journald"))
    assert line.rstrip().endswith("||") or "||" in line, line


def test_grants_grain_agent_a_narrow_sudoers_rule_for_self_repair():
    """bwsalmon/agents#99: the mutating counterpart to the read-only
    systemd-journal group grant -- exactly the three command lines
    `restart_grain_service`/`reboot_controller` (`mcp_server.py`) need,
    matched verbatim by sudoers, never a wildcard or NOPASSWD:ALL."""
    text = read()
    assert "/etc/sudoers.d/grain-agent-self-repair" in text
    assert "grain-agent ALL=(root) NOPASSWD: /usr/bin/systemctl restart grain-automation.service" in text
    assert "grain-agent ALL=(root) NOPASSWD: /usr/bin/systemctl restart grain-git-proxy.service" in text
    assert "grain-agent ALL=(root) NOPASSWD: /usr/bin/systemctl reboot" in text
    assert "NOPASSWD: ALL" not in text
    assert "NOPASSWD:ALL" not in text


def test_the_sudoers_rule_is_syntax_checked_before_it_can_be_loaded():
    text = read()
    assert "visudo -cf /etc/sudoers.d/grain-agent-self-repair" in text


def test_no_command_reloads_a_unit_that_cannot_be_reloaded():
    """`systemctl daemon-reload` (the manager) is always valid; a per-unit
    `systemctl reload` is only valid for a unit that declares ExecReload,
    which is exactly the assumption that failed here.
    """
    reloads = [
        line for line in read().splitlines()
        if not line.strip().startswith("#")
        and "systemctl reload" in line
        and "daemon-reload" not in line
    ]
    assert not reloads, reloads
