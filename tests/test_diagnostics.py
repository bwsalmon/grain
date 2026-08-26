"""Unit tests for grain/adapter/diagnostics.py -- the "why did the wait
fail" dumps. Same shape as tests/test_bootstrap.py: a real `LibvirtAdapter`
over a `FakeRunner`, no hypervisor.
"""

from __future__ import annotations

import ipaddress
from pathlib import Path

import pytest

from grain.adapter.diagnostics import (
    CONSOLE_LOG_LINES, dump_guest_diagnostics, dump_host_diagnostics,
)
from grain.adapter.libvirt import LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.run import CommandError, FakeRunner


@pytest.fixture
def env(tmp_path: Path):
    cluster = Cluster(sandbox_count=1)
    runner = FakeRunner()
    adapter = LibvirtAdapter(
        cluster, runner, LinuxNetwork(cluster, runner),
        config_dir=tmp_path / "instances",
    )
    lines: list[str] = []
    return adapter, runner, lines


def test_host_diagnostics_report_the_domain_and_the_path_to_it(env):
    adapter, runner, lines = env
    dump_host_diagnostics(adapter, "controller", lines.append)
    text = "\n".join(lines)
    assert runner.ran("virsh -c qemu:///system dominfo controller")
    assert runner.ran("ip -br addr show br-grain")
    assert runner.ran("ip neigh show 10.100.0.2")
    assert runner.ran("ping -c 2 -W 2 10.100.0.2")
    assert "domain state" in text


def test_host_diagnostics_tail_the_serial_console_log(env):
    """The console log is the guest's own account of a boot that never got
    as far as answering SSH -- the whole reason `render_domain_xml` points
    the console at a file on the host.
    """
    adapter, runner, lines = env
    console = adapter.config_dir / "controller-console.log"
    console.parent.mkdir(parents=True, exist_ok=True)
    console.write_text("[   0.0] Linux version ...\ncloud-init: failed\n")
    dump_host_diagnostics(adapter, "controller", lines.append)
    assert runner.ran(f"tail -n {CONSOLE_LOG_LINES} {console}")


def test_an_empty_console_log_is_reported_as_never_booted(env):
    adapter, runner, lines = env
    console = adapter.config_dir / "controller-console.log"
    console.parent.mkdir(parents=True, exist_ok=True)
    console.write_text("")
    dump_host_diagnostics(adapter, "controller", lines.append)
    text = "\n".join(lines)
    assert "did not get as far as a kernel boot" in text
    assert not runner.ran("tail")


def test_a_missing_console_log_says_so_rather_than_failing(env):
    adapter, runner, lines = env
    dump_host_diagnostics(adapter, "controller", lines.append)
    assert "missing" in "\n".join(lines)


def test_host_diagnostics_never_raise(env):
    """A diagnostic that fails must not replace the error it was called to
    explain -- the caller is already unwinding a TimeoutError.
    """
    adapter, _runner, lines = env

    class Exploding:
        def run(self, argv, *, stdin=None, check=True):
            raise CommandError(argv, 1, "boom")

    adapter.runner = Exploding()
    dump_host_diagnostics(adapter, "controller", lines.append)
    assert any("could not be collected" in line for line in lines)


def test_guest_diagnostics_read_cloud_inits_own_account_of_the_failure():
    inner = FakeRunner()
    ssh = SshRunner(
        inner=inner, user="debian", address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    lines: list[str] = []
    dump_guest_diagnostics(ssh, lines.append)
    commands = "\n".join(inner.commands)
    assert "cloud-init status --long" in commands
    assert "/var/log/cloud-init-output.log" in commands
    assert "systemctl list-units --failed" in commands


def test_guest_diagnostics_never_raise():
    class Exploding:
        def run(self, argv, *, stdin=None, check=True):
            raise CommandError(argv, 255, "connection closed")

    ssh = SshRunner(
        inner=Exploding(), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    lines: list[str] = []
    dump_guest_diagnostics(ssh, lines.append)
    assert any("could not be collected" in line for line in lines)
