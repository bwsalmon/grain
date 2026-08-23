import ipaddress
from pathlib import Path

import pytest

from grain.adapter.wait import wait_for_provisioning, wait_for_ssh
from grain.automation.ssh import SshRunner
from grain.run import FakeRunner


@pytest.fixture
def ssh():
    inner = FakeRunner()
    runner = SshRunner(
        inner=inner, user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    return runner, inner


def test_wait_for_ssh_returns_once_reachable(ssh):
    runner, inner = ssh
    wait_for_ssh(runner, timeout=5)  # FakeRunner defaults to success
    assert inner.ran("ssh")


def test_wait_for_ssh_raises_after_the_timeout(ssh):
    runner, inner = ssh
    inner.expect("ssh", returncode=255, stderr="Connection refused")
    with pytest.raises(TimeoutError, match="never became reachable"):
        wait_for_ssh(runner, timeout=0, interval=0)


def test_wait_for_provisioning_succeeds_when_cloud_init_reports_done(ssh):
    runner, inner = ssh
    inner.expect("ssh", returncode=0)
    wait_for_provisioning(runner)  # must not raise
    assert inner.ran("ssh")


def test_wait_for_provisioning_raises_on_a_nonzero_exit(ssh):
    runner, inner = ssh
    inner.expect("ssh", returncode=1, stdout="status: error", stderr="")
    with pytest.raises(RuntimeError, match="cloud-init did not finish cleanly"):
        wait_for_provisioning(runner)
