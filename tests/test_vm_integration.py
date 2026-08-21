"""Live integration tests: boot a real sandbox VM via libvirt and exercise
SSH plus the automation dispatch mechanism against it.

Skipped unless the host is already set up for this — KVM, `qemu:///system`
reachable, and `br-grain` already up. Bringing the host network up is
`grain host up`'s job, not a test fixture's — this suite only checks the
precondition holds, the same split `test_net_integration.py` makes for
nftables. Where it *can* run, this is the one place that verifies what the
rest of the suite fakes: that a real cloud-init seed actually lands the
controller's SSH key on the guest (`libvirt.py`'s `public-keys` support),
and that SSH from the controller genuinely reaches a sandbox — net_linux.py
says "controller may reach any sandbox"; this is what turns that rule into
a fact rather than something that merely parses.

One VM, session-scoped: boot is the slow part (cloud-init + sshd coming up,
tens of seconds), so every test in this file shares one sandbox rather than
paying that cost per test.

`claude -p` itself is not exercised: it needs a real login credential this
environment doesn't have, and docs/design.md is explicit that logging in is
"a manual first-setup step, not something [the provisioning script] can do
unattended." `dispatch()`'s systemd-run/unit_status/reap mechanism is
verified with `start_unit` and stand-in commands instead — see
`grain/automation/dispatch.py`'s `start_unit`.
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
from grain.automation.dispatch import UnitState, dispatch, reap, start_unit, unit_status
from grain.automation.github import Issue
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.run import RealRunner

BRIDGE = "br-grain"
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
BASE_IMAGE_URL = (
    "https://cloud.debian.org/images/cloud/bookworm/latest/"
    "debian-12-genericcloud-amd64.qcow2"
)
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    # A default, overridable timeout: an ssh call whose connection hangs
    # (e.g. a stale SSH_AUTH_SOCK — see grain/automation/ssh.py's
    # IdentityAgent=none docstring, reproduced live in this very
    # environment) must not wedge the whole suite indefinitely. Reported as
    # an ordinary failed CompletedProcess, not an exception, so retry loops
    # like `_wait_for_ssh` keep working without a special case.
    kwargs.setdefault("timeout", 20)
    try:
        return subprocess.run(argv, capture_output=True, text=True, **kwargs)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(
            argv, returncode=124, stdout=exc.stdout or "",
            stderr=(exc.stderr or "") + f"\n[timed out after {kwargs['timeout']}s]",
        )


def _host_ready() -> bool:
    """Cheap, local, no network — safe to call at collection time on every
    `pytest` run. The base image (a one-time ~350MB fetch) is checked and,
    if needed, fetched lazily inside the `base_image` fixture instead, so
    an ordinary test run on a host without KVM never touches the network.
    """
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
    not _host_ready(),
    reason="needs /dev/kvm, qemu:///system, and br-grain already up",
)


@pytest.fixture(scope="session")
def base_image() -> Path:
    if not BASE_IMAGE.exists():
        BASE_IMAGE.parent.mkdir(parents=True, exist_ok=True)
        result = _run(
            ["curl", "-fsSL", "-o", str(BASE_IMAGE), BASE_IMAGE_URL], timeout=600
        )
        if result.returncode != 0:
            BASE_IMAGE.unlink(missing_ok=True)
            pytest.skip(f"could not fetch base image: {result.stderr}")
    return BASE_IMAGE


@pytest.fixture(scope="session")
def controller_key(tmp_path_factory) -> tuple[Path, Path]:
    key_dir = tmp_path_factory.mktemp("controller-ssh")
    private = key_dir / "id_ed25519"
    result = _run(
        ["ssh-keygen", "-t", "ed25519", "-f", str(private), "-N", "", "-q"]
    )
    assert result.returncode == 0, result.stderr
    return private, private.with_suffix(".pub")


def _wait_for_ssh(address, key_path: Path, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    last_err = ""
    while time.monotonic() < deadline:
        result = _run([
            "ssh", "-i", str(key_path), "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
            "-o", "ConnectTimeout=5",
            f"{SSH_USER}@{address}", "true",
        ])
        if result.returncode == 0:
            return
        last_err = result.stderr
        time.sleep(3)
    raise TimeoutError(f"sandbox never became reachable over SSH: {last_err}")


@dataclass
class Sandbox:
    adapter: LibvirtAdapter
    cluster: Cluster
    name: str
    private_key: Path


@pytest.fixture(scope="session")
def booted_sandbox(base_image, controller_key):
    private_key, public_key = controller_key
    # Not a pytest tmp_path: those land under /tmp mode 0700, which blocks
    # the qemu process (uid libvirt-qemu, verified via `id libvirt-qemu`)
    # from opening the disk even after dynamic_ownership chowns the file
    # itself — verified live, "Cannot access storage file ... Permission
    # denied" — dynamic_ownership relabels the leaf file, not the parent
    # directory chain. `/var/lib/grain/instances` is the real config_dir
    # default and already 0755, so it's traversable the same way a real
    # deployment's would be.
    config_dir = Path("/var/lib/grain/instances/test-live")
    config_dir.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(sandbox_count=1, image=str(base_image), bridge=BRIDGE)
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir,
        ssh_public_key_path=public_key,
    )
    name = "sandbox-0"
    try:
        adapter.create(cluster.spec_of(name))
        adapter.start(name)
        _wait_for_ssh(cluster.address_of(name), private_key, timeout=BOOT_TIMEOUT_S)
        yield Sandbox(adapter=adapter, cluster=cluster, name=name, private_key=private_key)
    finally:
        adapter.destroy(name)


@pytest.fixture
def ssh_runner(booted_sandbox: Sandbox) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=booted_sandbox.cluster.address_of(booted_sandbox.name),
        key_path=booted_sandbox.private_key,
    )


# --- boot + the new libvirt.py SSH key plumbing ---------------------------

def test_the_sandbox_reaches_running_state(booted_sandbox: Sandbox):
    assert booted_sandbox.adapter.state(booted_sandbox.name) is VmState.RUNNING


def test_the_controller_key_authenticates_at_the_assigned_address(booted_sandbox: Sandbox):
    address = booted_sandbox.cluster.address_of(booted_sandbox.name)
    result = _run([
        "ssh", "-i", str(booted_sandbox.private_key), "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
        f"{SSH_USER}@{address}", "whoami",
    ])
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == SSH_USER


# --- grain.automation.ssh.SshRunner against a real sandbox ----------------

def test_ssh_runner_executes_a_real_remote_command(ssh_runner: SshRunner):
    result = ssh_runner.run(["echo", "hello-from-sandbox"])
    assert result.returncode == 0
    assert result.stdout.strip() == "hello-from-sandbox"


def test_ssh_runner_stdin_reaches_the_remote_command(ssh_runner: SshRunner):
    result = ssh_runner.run(["cat"], stdin="round-tripped\n")
    assert result.stdout == "round-tripped\n"


# --- grain.automation.dispatch against real systemd -----------------------

def test_a_real_systemd_unit_goes_active_then_done_success(ssh_runner: SshRunner):
    unit = "grain-test-success"
    reap(ssh_runner, unit)  # in case a previous run left it behind
    start_unit(ssh_runner, unit, "sleep 5")
    try:
        assert unit_status(ssh_runner, unit) is UnitState.ACTIVE
        time.sleep(8)
        assert unit_status(ssh_runner, unit) is UnitState.DONE_SUCCESS
    finally:
        reap(ssh_runner, unit)


def test_a_real_systemd_unit_reports_failure_then_reaps_to_absent(ssh_runner: SshRunner):
    unit = "grain-test-failure"
    reap(ssh_runner, unit)
    start_unit(ssh_runner, unit, "exit 1")
    time.sleep(2)
    assert unit_status(ssh_runner, unit) is UnitState.DONE_FAILED
    reap(ssh_runner, unit)
    assert unit_status(ssh_runner, unit) is UnitState.ABSENT


def test_dispatch_writes_the_real_prompt_onto_the_guest(ssh_runner: SshRunner):
    issue = Issue(
        number=7, title="a distinctive title marker",
        body="a distinctive body marker",
        html_url="https://github.com/o/r/issues/7", labels=frozenset(),
    )
    unit = dispatch(ssh_runner, "sandbox-0", issue)
    try:
        prompt = ssh_runner.run(["cat", f"/tmp/{unit}.md"]).stdout
        assert "a distinctive title marker" in prompt
        assert "a distinctive body marker" in prompt
        # No `claude` binary on the stock image: this unit is expected to
        # fail (bash: claude: command not found) — that's fine, this test
        # is only about what actually reached the guest's filesystem.
    finally:
        reap(ssh_runner, unit)
