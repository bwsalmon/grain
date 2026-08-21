"""Live integration test: boot a real controller VM via libvirt with
`provision/controller.sh` as its cloud-init user-data, and check what it
actually did on the guest -- not just that the script parses.

Skipped under the same conditions as `test_vm_integration.py` (KVM,
`qemu:///system`, `br-grain` already up) -- see that file's docstring for
why this suite is gated rather than run unconditionally. This is the
"boot a real controller VM live to verify provisioning actually runs" half
of docs/roadmap.md item 3's verification; `test_provision_controller.py`
covers the script's content and syntax without paying a VM's boot cost.

What this does NOT verify: the running services actually working
end-to-end (the git proxy serving traffic, `gce_metadata_server` minting a
real token, `grain automation run-once` against a real GitHub repo) -- all
of that needs credentials this environment doesn't have, the same caveat
`test_vm_integration.py` and docs/runbook.md's "Gaps" section already carry
for the sandbox side. This checks that the *provisioning* left the guest in
the state the rest of docs/runbook.md's first-time setup checklist assumes:
the right packages, users, directory layout, and unit files.

The SSH key injected here via `ssh_public_key_path` is a throwaway *test*
identity used only so this suite can log in and inspect the guest -- it is
not standing in for the controller's own dispatch keypair (which the
script generates *on* the controller, at `/data/secrets/controller-ssh`,
and which this test also checks for).
"""

from __future__ import annotations

import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.libvirt import LIBVIRT_URI, LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.config import AutomationConfig
from grain.inventory import Cluster
from grain.metadata.config import MetadataConfig
from grain.run import RealRunner

BRIDGE = "br-grain"
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180
PROVISION_TIMEOUT_S = 600
SCRIPT = Path(__file__).parent.parent / "provision" / "controller.sh"


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    kwargs.setdefault("timeout", 30)
    try:
        return subprocess.run(argv, capture_output=True, text=True, **kwargs)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(
            argv, returncode=124, stdout=exc.stdout or "",
            stderr=(exc.stderr or "") + f"\n[timed out after {kwargs['timeout']}s]",
        )


def _host_ready() -> bool:
    if not Path("/dev/kvm").exists():
        return False
    if _run(["virsh", "-c", LIBVIRT_URI, "list"]).returncode != 0:
        return False
    if _run(["ip", "link", "show", BRIDGE]).returncode != 0:
        return False
    for tool in ("qemu-img", "cloud-localds", "ssh", "ssh-keygen"):
        if _run(["which", tool]).returncode != 0:
            return False
    return True


pytestmark = pytest.mark.skipif(
    not _host_ready(), reason="needs /dev/kvm, qemu:///system, and br-grain already up",
)


@pytest.fixture(scope="session")
def base_image() -> Path:
    if not BASE_IMAGE.exists():
        pytest.skip(f"base image not cached at {BASE_IMAGE}")
    return BASE_IMAGE


@pytest.fixture(scope="session")
def admin_key(tmp_path_factory) -> tuple[Path, Path]:
    """A throwaway keypair used only so this suite can log in -- see the
    module docstring. Not the controller's own dispatch key.
    """
    key_dir = tmp_path_factory.mktemp("admin-ssh")
    private = key_dir / "id_ed25519"
    result = _run(["ssh-keygen", "-t", "ed25519", "-f", str(private), "-N", "", "-q"])
    assert result.returncode == 0, result.stderr
    return private, private.with_suffix(".pub")


def _ssh(address, key_path: Path, *cmd: str, timeout: float = 30) -> subprocess.CompletedProcess:
    return _run(
        ["ssh", "-i", str(key_path), "-o", "BatchMode=yes",
         "-o", "StrictHostKeyChecking=accept-new",
         "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
         "-o", "ConnectTimeout=5", f"{SSH_USER}@{address}", *cmd],
        timeout=timeout,
    )


def _wait_for_ssh(address, key_path: Path, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    last_err = ""
    while time.monotonic() < deadline:
        result = _ssh(address, key_path, "true")
        if result.returncode == 0:
            return
        last_err = result.stderr
        time.sleep(3)
    raise TimeoutError(f"controller never became reachable over SSH: {last_err}")


@dataclass
class Controller:
    adapter: LibvirtAdapter
    cluster: Cluster
    address: object
    private_key: Path


@pytest.fixture(scope="session")
def booted_controller(base_image, admin_key):
    private_key, public_key = admin_key
    config_dir = Path("/var/lib/grain/instances/test-controller-live")
    config_dir.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(sandbox_count=1, image=str(base_image), bridge=BRIDGE)
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir,
        ssh_public_key_path=public_key,
    )
    name = cluster.controller_name
    address = cluster.address_of(name)
    try:
        adapter.create(cluster.spec_of(name), SCRIPT.read_text())
        adapter.start(name)
        _wait_for_ssh(address, private_key, timeout=BOOT_TIMEOUT_S)
        # Block until cloud-init's scripts-user stage (which runs
        # provision/controller.sh) has actually finished -- SSH usually
        # comes up before that stage completes.
        result = _ssh(
            address, private_key, "sudo", "cloud-init", "status", "--wait",
            timeout=PROVISION_TIMEOUT_S,
        )
        assert result.returncode == 0, (
            f"cloud-init did not finish cleanly: {result.stdout}\n{result.stderr}"
        )
        yield Controller(adapter=adapter, cluster=cluster, address=address, private_key=private_key)
    finally:
        adapter.destroy(name)


def _sh(controller: Controller, *cmd: str) -> subprocess.CompletedProcess:
    return _ssh(controller.address, controller.private_key, *cmd)


def test_the_controller_reaches_running_state(booted_controller: Controller):
    assert booted_controller.adapter.state(booted_controller.cluster.controller_name) is VmState.RUNNING


def test_python_3_11_or_newer_is_present(booted_controller: Controller):
    # A single argv element, not several joined with spaces -- SSH has no
    # real argv (grain/automation/ssh.py's own docstring), so a command
    # built from multiple tokens containing punctuation gets word-split
    # again by the remote shell rather than round-tripping.
    result = _sh(booted_controller, "python3 --version")
    assert result.returncode == 0, result.stdout + result.stderr
    # "Python 3.11.2" -> (3, 11)
    _, version = result.stdout.strip().split()
    major, minor, *_ = version.split(".")
    assert (int(major), int(minor)) >= (3, 11), result.stdout


def test_the_controller_ssh_keypair_was_generated_on_the_guest(booted_controller: Controller):
    key_path = AutomationConfig(owner="x", repo="y").ssh_key_path
    result = _sh(booted_controller, "sudo", "test", "-f", str(key_path))
    assert result.returncode == 0
    result = _sh(booted_controller, "sudo", "test", "-f", f"{key_path}.pub")
    assert result.returncode == 0
    # 0600 on the private half.
    result = _sh(booted_controller, "sudo", "stat", "-c", "%a", str(key_path))
    assert result.stdout.strip() == "600", result.stdout


def test_the_data_layout_exists_with_the_paths_automation_expects(booted_controller: Controller):
    for path in ("/data/secrets", "/data/secrets/github", "/data/config",
                 "/data/state/automation", "/data/state/git-proxy",
                 "/data/state/metadata-server"):
        result = _sh(booted_controller, "sudo", "test", "-d", path)
        assert result.returncode == 0, f"{path} missing on the guest"


def test_gce_metadata_server_binary_runs(booted_controller: Controller):
    result = _sh(booted_controller, "/usr/local/bin/gce_metadata_server", "-h")
    # The binary prints usage and exits non-zero for -h, but must actually
    # execute (not "not found" / permission denied) -- confirmed by stderr
    # containing its own usage banner rather than a shell error.
    assert "gce_metadata_server" in (result.stdout + result.stderr)


def test_grain_metadata_system_user_exists(booted_controller: Controller):
    config = MetadataConfig(service_account_email="x@y.iam.gserviceaccount.com",
                             project_id="proj")
    result = _sh(booted_controller, "id", "-u", config.metadata_user)
    assert result.returncode == 0, result.stderr
    # No login shell for this account.
    result = _sh(booted_controller, "getent", "passwd", config.metadata_user)
    assert "nologin" in result.stdout or "false" in result.stdout


def test_opt_grain_exists_for_the_manual_deploy_step(booted_controller: Controller):
    result = _sh(booted_controller, "sudo", "test", "-d", "/opt/grain")
    assert result.returncode == 0


def test_systemd_units_are_installed_but_not_enabled(booted_controller: Controller):
    for unit in ("grain-automation.service", "grain-automation.timer",
                 "grain-git-proxy.service"):
        result = _sh(booted_controller, "sudo", "test", "-f", f"/etc/systemd/system/{unit}")
        assert result.returncode == 0, f"{unit} not installed"
        enabled = _sh(booted_controller, "systemctl", "is-enabled", unit)
        assert enabled.stdout.strip() != "enabled", f"{unit} should not be enabled yet"
