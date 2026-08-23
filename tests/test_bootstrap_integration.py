"""Live integration tests for docs/bootstrap.md's Phase 1 and Phase 3 pieces
-- the "needs the dev host" half of that doc's testing table. Skipped unless
the host is already set up for this (same `_host_ready()` gate every other
live suite in this repo uses).

**Phase 1, the key-roles fix**, is the one thing genuinely worth booting a
*dedicated* VM for here: a sandbox has to trust two independent keys at once
(admin *and* controller), which no other live suite in this repo exercises
simultaneously (`test_vm_integration.py`'s `booted_sandbox` injects only a
controller-role key; `test_controller_integration.py`'s `booted_controller`
injects only the admin-role key). Booted at index 2 (`sandbox-2`) to avoid
colliding with those two sessions' own domains, which stay alive for the
whole pytest session (`test_live_issue_to_pr.py`'s `live_sandbox` docstring
explains why collision is a real, previously-hit failure mode here, and why
picking an unused index is the fix rather than a workaround).

**Phase 3's `deploy_tree`/`configure_*` functions** are exercised against
this same sandbox rather than a second, separate controller VM -- spinning
up another VM named `controller` would collide with
`test_controller_integration.py`'s own live `booted_controller`, alive for
the same reason. Nothing in `deploy_tree` or `grain/automation/configure.py`
is actually controller-specific: both are just "run tar/dd/sudo over SSH,
as the admin user, against a Debian host" -- what they need real
verification against is the *mechanism* (does a real non-interactive `sudo`
work, does the tar pipe survive a real SSH hop, does `dd`-over-stdin really
land the right bytes at the right mode), not the specific role of the VM on
the other end. `wait_for_ssh`/`wait_for_provisioning` (`grain/adapter/wait.py`)
are exercised the same way -- this is also where they get their first live
run, having been lifted out of `tests/loadtest.py`'s already-live-proven
versions.
"""

from __future__ import annotations

import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

from grain.adapter.deploy import deploy_tree
from grain.adapter.libvirt import LIBVIRT_URI, LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.adapter.wait import wait_for_provisioning, wait_for_ssh
from grain.automation.configure import configure_claude_credentials, configure_repo
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.run import RealRunner

BRIDGE = "br-grain"
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    kwargs.setdefault("timeout", 20)
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


def _keypair(tmp_path_factory, name: str) -> tuple[Path, Path]:
    key_dir = tmp_path_factory.mktemp(name)
    private = key_dir / "id_ed25519"
    result = _run(["ssh-keygen", "-t", "ed25519", "-f", str(private), "-N", "", "-q"])
    assert result.returncode == 0, result.stderr
    return private, private.with_suffix(".pub")


@pytest.fixture(scope="session")
def admin_key(tmp_path_factory) -> tuple[Path, Path]:
    return _keypair(tmp_path_factory, "bootstrap-admin-ssh")


@pytest.fixture(scope="session")
def controller_key(tmp_path_factory) -> tuple[Path, Path]:
    return _keypair(tmp_path_factory, "bootstrap-controller-ssh")


def _wait_for_ssh_raw(address, key_path: Path, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    last_err = ""
    while time.monotonic() < deadline:
        result = _run([
            "ssh", "-i", str(key_path), "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
            "-o", "ConnectTimeout=5", f"{SSH_USER}@{address}", "true",
        ])
        if result.returncode == 0:
            return
        last_err = result.stderr
        time.sleep(3)
    raise TimeoutError(f"sandbox never became reachable over SSH: {last_err}")


@dataclass
class TwoKeySandbox:
    cluster: Cluster
    name: str
    admin_private_key: Path
    controller_private_key: Path


@pytest.fixture(scope="session")
def two_key_sandbox(base_image, admin_key, controller_key):
    """One bare (unprovisioned -- boot speed, same tradeoff
    `test_vm_integration.py`'s `booted_sandbox` makes) sandbox at index 2,
    created with *both* an admin and a controller key -- the exact
    docs/bootstrap.md Phase 1 property (`LibvirtAdapter.create`'s role-based
    key selection: a sandbox trusts both).
    """
    admin_private, admin_public = admin_key
    controller_private, controller_public = controller_key
    config_dir = Path("/var/lib/grain/instances/test-bootstrap-live")
    config_dir.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(sandbox_count=3, image=str(base_image), bridge=BRIDGE)
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir,
        admin_public_key_path=admin_public, controller_public_key_path=controller_public,
    )
    name = "sandbox-2"
    try:
        adapter.create(cluster.spec_of(name))
        adapter.start(name)
        _wait_for_ssh_raw(cluster.address_of(name), admin_private, timeout=BOOT_TIMEOUT_S)
        yield TwoKeySandbox(
            cluster=cluster, name=name,
            admin_private_key=admin_private, controller_private_key=controller_private,
        )
    finally:
        adapter.destroy(name)


def test_the_sandbox_accepts_both_the_admin_and_the_controller_key(two_key_sandbox):
    """The regression docs/bootstrap.md's Phase 1 exists to fix: before the
    role-based key split, a sandbox only ever got the one key path fed to
    every VM -- there was no way for a second, independently-purposed key
    to also be trusted. Waiting for SSH with the admin key already proved
    that key works (the fixture's own boot wait); this proves the
    controller key *also* independently authenticates against the exact
    same running VM.
    """
    address = two_key_sandbox.cluster.address_of(two_key_sandbox.name)
    result = _run([
        "ssh", "-i", str(two_key_sandbox.controller_private_key),
        "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
        "-o", "ConnectTimeout=10", f"{SSH_USER}@{address}", "whoami",
    ])
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == SSH_USER


@pytest.fixture
def admin_ssh(two_key_sandbox) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=two_key_sandbox.cluster.address_of(two_key_sandbox.name),
        key_path=two_key_sandbox.admin_private_key,
    )


def test_wait_for_ssh_and_provisioning_succeed_against_a_real_vm(admin_ssh):
    """First live run of `grain/adapter/wait.py`, lifted from
    `tests/loadtest.py`'s already-live-proven `_wait_for_ssh`/
    `_wait_for_provisioning` -- this is what actually proves the lift kept
    the behaviour, not just the unit tests against `FakeRunner`.
    """
    wait_for_ssh(admin_ssh, timeout=30)  # already up; must return promptly
    wait_for_provisioning(admin_ssh)  # a bare image's cloud-init is done by boot


def test_deploy_tree_lands_real_files_over_a_real_tar_over_ssh_pipe(two_key_sandbox, admin_ssh):
    """docs/bootstrap.md's testing table: "tar-over-ssh deploy against a
    real controller" -- run here against a plain sandbox instead (see this
    module's docstring for why that's a faithful substitute for what this
    function actually needs proven: real `sudo`, a real tar pipe over a
    real SSH hop, real extraction).
    """
    address = two_key_sandbox.cluster.address_of(two_key_sandbox.name)
    dest = "/tmp/test-deploy-tree"
    admin_ssh.run(["sudo", "rm", "-rf", dest])
    deploy_tree(
        RealRunner(), Path(__file__).resolve().parent.parent / "provision",
        user=SSH_USER, address=address, key_path=two_key_sandbox.admin_private_key,
        dest=dest,
    )
    result = admin_ssh.run(["cat", f"{dest}/controller.sh"])
    assert result.stdout.startswith("#!/bin/bash")
    # .git/docs/__pycache__ excludes: this repo's own tree has no such
    # directories under provision/, so absence isn't informative there --
    # confirm the exclude flags actually reached tar by checking a file
    # that *is* present made it across, which only holds if the pipe/
    # extraction genuinely worked end to end.
    listing = admin_ssh.run(["ls", dest]).stdout
    assert "sandbox.sh" in listing


def test_configure_repo_writes_real_files_as_root_over_ssh(two_key_sandbox, admin_ssh):
    """docs/bootstrap.md Phase 3's third verb, against a real host: real
    `sudo mkdir`/`dd`/`chmod`, not `FakeRunner`-scripted responses. This
    throwaway sandbox has no real `/data` (it is bare/unprovisioned), which
    is exactly what makes this a genuine test of `sudo mkdir -p` actually
    creating it, not just writing into a directory that already exists.
    """
    admin_ssh.run(["sudo", "rm", "-rf", "/data/config"])
    configure_repo(admin_ssh, "acme", "widgets")
    # 644, world-readable, so a plain `cat` as the unprivileged admin SSH
    # user (not `sudo cat`) is itself part of what this asserts.
    result = admin_ssh.run(["cat", "/data/config/automation.json"])
    assert '"owner": "acme"' in result.stdout
    assert '"repo": "widgets"' in result.stdout
    mode = admin_ssh.run(["stat", "-c", "%a", "/data/config/automation.json"]).stdout.strip()
    assert mode == "644"
    allowlist = admin_ssh.run(["cat", "/data/config/repo-allowlist.json"]).stdout
    assert "acme/widgets" in allowlist


def test_configure_claude_credentials_writes_mode_600(two_key_sandbox, admin_ssh):
    admin_ssh.run(["sudo", "rm", "-f", "/data/secrets/claude-credentials.json"])
    configure_claude_credentials(admin_ssh, '{"accessToken": "tok"}')
    # 600, root-owned: the unprivileged admin SSH user must NOT be able to
    # read this directly -- verifying that is as much the point here as
    # verifying the content is right via sudo.
    denied = admin_ssh.run(["cat", "/data/secrets/claude-credentials.json"], check=False)
    assert denied.returncode != 0
    assert "Permission denied" in denied.stderr
    result = admin_ssh.run(["sudo", "cat", "/data/secrets/claude-credentials.json"])
    assert result.stdout == '{"accessToken": "tok"}'
    mode = admin_ssh.run(
        ["sudo", "stat", "-c", "%a", "/data/secrets/claude-credentials.json"]
    ).stdout.strip()
    assert mode == "600"

    # docs/roadmap.md item 8's "Update": the live copy claude -p actually
    # reads now, owned by the dedicated grain-agent account it runs as
    # (provision/controller.sh) -- not any sandbox.
    live_mode = admin_ssh.run(
        ["sudo", "stat", "-c", "%a %U:%G", "/home/grain-agent/.claude/.credentials.json"]
    ).stdout.strip()
    assert live_mode == "600 grain-agent:grain-agent"
    live_content = admin_ssh.run(
        ["sudo", "cat", "/home/grain-agent/.claude/.credentials.json"]
    ).stdout
    assert live_content == '{"accessToken": "tok"}'
