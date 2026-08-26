"""Tests for the filtering policy.

The design says a silently-permissive firewall is the failure that
invalidates the whole security model, and that the negative cases must be
verified explicitly. So these tests assert what is *forbidden* at least as
hard as what is allowed, and they check rule *ordering*, since an accept
placed after a drop — or a drop placed after a broad accept — is exactly how
a ruleset ends up meaning something other than it reads.
"""

from __future__ import annotations

import pytest

from pathlib import Path

from grain.adapter.base import EgressMode
from grain.adapter.net_linux import (
    BOOT_UNIT_NAME,
    BOOT_UNIT_PATH,
    LinuxNetwork,
    render_boot_unit,
    render_host_input_rules,
    render_ruleset,
)
from grain.inventory import GIT_PROXY_PORT, Cluster
from grain.run import FakeRunner


def line_index(ruleset: str, needle: str) -> int:
    for i, line in enumerate(ruleset.splitlines()):
        if needle in line:
            return i
    raise AssertionError(f"not found in ruleset: {needle!r}")


@pytest.fixture
def cluster() -> Cluster:
    return Cluster(sandbox_count=2)


@pytest.fixture
def ruleset(cluster: Cluster) -> str:
    return render_ruleset(cluster, EgressMode.OPEN)


# --- the default must be deny ---------------------------------------------

def test_forward_chain_defaults_to_drop(ruleset: str):
    assert "type filter hook forward priority filter; policy drop;" in ruleset


# --- anti-spoofing: what makes the source address a fact -------------------

def test_every_vm_gets_an_anti_spoof_rule(cluster: Cluster, ruleset: str):
    for name in cluster.names:
        iface = cluster.interface_of(name)
        addr = cluster.address_of(name)
        assert f'iifname "{iface}" ip saddr != {addr} drop' in ruleset


def test_anti_spoof_precedes_every_accept(cluster: Cluster, ruleset: str):
    last_spoof = max(
        line_index(ruleset, f'iifname "{cluster.interface_of(n)}"')
        for n in cluster.names
    )
    first_accept = min(
        i
        for i, line in enumerate(ruleset.splitlines())
        if line.strip().endswith("accept") and "ct state" not in line
    )
    assert last_spoof < first_accept, (
        "an accept before the anti-spoof rules would let a sandbox "
        "impersonate another for that traffic"
    )


# --- sandbox reachability -------------------------------------------------

def test_sandbox_may_reach_the_git_proxy(cluster: Cluster, ruleset: str):
    for name in cluster.sandbox_names:
        addr = cluster.address_of(name)
        assert (
            f"ip saddr {addr} ip daddr {cluster.controller_ip} "
            f"tcp dport {GIT_PROXY_PORT} accept" in ruleset
        )


def test_sandbox_to_sandbox_is_dropped(cluster: Cluster, ruleset: str):
    assert f"ip saddr {cluster.subnet} ip daddr {cluster.subnet} drop" in ruleset


def test_intra_subnet_drop_comes_after_the_specific_accepts(cluster: Cluster, ruleset: str):
    drop = line_index(ruleset, f"ip saddr {cluster.subnet} ip daddr {cluster.subnet} drop")
    proxy_accept = line_index(ruleset, f"tcp dport {GIT_PROXY_PORT} accept")
    controller = line_index(ruleset, f"ip saddr {cluster.controller_ip} ip daddr {cluster.subnet} accept")
    assert proxy_accept < drop
    assert controller < drop


def test_no_rule_permits_a_sandbox_to_reach_another_sandbox(cluster: Cluster, ruleset: str):
    """Belt and braces: scan every accept for a sandbox-to-sandbox pair."""
    sandbox_addrs = {str(cluster.address_of(n)) for n in cluster.sandbox_names}
    for line in ruleset.splitlines():
        stripped = line.strip()
        if not stripped.endswith("accept") or stripped.startswith("#"):
            continue
        tokens = stripped.split()
        if "saddr" not in tokens or "daddr" not in tokens:
            continue
        src = tokens[tokens.index("saddr") + 1]
        dst = tokens[tokens.index("daddr") + 1]
        assert not (src in sandbox_addrs and dst in sandbox_addrs), (
            f"rule allows sandbox-to-sandbox traffic: {stripped}"
        )


# --- no metadata DNAT (bwsalmon/agents#126) --------------------------------
#
# The per-sandbox gce_metadata_server broker this ruleset used to DNAT a
# sandbox's metadata-anycast traffic into is gone -- GCP access now arrives
# as a real key pushed directly into the sandbox (grain/automation/
# gcp_keys.py), which needs no DNAT rule at all.

def test_the_nat_table_carries_no_dnat_rule(ruleset: str):
    assert "dnat to" not in ruleset


def test_the_nat_table_has_no_prerouting_chain(ruleset: str):
    assert "chain prerouting" not in ruleset


# --- egress ---------------------------------------------------------------

def test_open_egress_masquerades(cluster: Cluster):
    out = render_ruleset(cluster, EgressMode.OPEN)
    assert "masquerade" in out
    assert f'ip saddr {cluster.subnet} oifname != "{cluster.bridge}" accept' in out


def test_allowlist_egress_has_no_masquerade_and_no_blanket_accept(cluster: Cluster):
    out = render_ruleset(cluster, EgressMode.ALLOWLIST)
    assert "masquerade" not in out
    assert f'oifname != "{cluster.bridge}" accept' not in out


# --- scaling --------------------------------------------------------------

def test_rules_scale_with_sandbox_count():
    for count in (1, 2, 5):
        c = Cluster(sandbox_count=count)
        out = render_ruleset(c, EgressMode.OPEN)
        # one anti-spoof rule per VM, controller included
        assert out.count("ip saddr != ") == count + 1


# --- the host's own input chain -------------------------------------------

def test_host_input_rules_keep_ssh_and_are_not_applied_automatically(cluster: Cluster):
    out = render_host_input_rules(cluster, ssh_port=22)
    assert "tcp dport 22 accept" in out
    assert "policy drop" in out
    runner = FakeRunner()
    LinuxNetwork(cluster, runner).up()
    # network_up must never install an INPUT policy: doing so on a remote
    # host without console access is how machines become unreachable.
    assert not any("grain_input" in (stdin or "") for _, stdin in runner.calls)


# --- applying -------------------------------------------------------------

def test_up_creates_the_bridge_and_feeds_nft_on_stdin(cluster: Cluster):
    runner = FakeRunner()
    runner.expect("ip -j link show br-grain", returncode=1)  # absent
    LinuxNetwork(cluster, runner).up()
    assert runner.ran("ip link add br-grain type bridge")
    assert runner.ran("ip link set br-grain up")
    assert runner.ran("sysctl -w net.ipv4.ip_forward=1")
    nft_calls = [stdin for argv, stdin in runner.calls if argv[:2] == ["nft", "-f"]]
    assert len(nft_calls) == 1
    assert "policy drop" in nft_calls[0]


def test_up_does_not_recreate_an_existing_bridge(cluster: Cluster):
    runner = FakeRunner()
    runner.expect("ip -j link show br-grain", stdout="[{}]", returncode=0)
    LinuxNetwork(cluster, runner).up()
    assert not runner.ran("ip link add")


# --- surviving a host reboot (bwsalmon/agents#111) -------------------------
#
# The bridge and the nftables policy `up()` just applied live only in the
# running kernel; nothing before this reapplied either across a reboot,
# while libvirt's own autostart brings the sandboxes back regardless --
# found live as sandbox-to-sandbox isolation silently gone while everything
# else (SSH, a direct connection to the controller) kept working, with
# nothing anywhere flagging it.

def test_up_does_not_install_a_boot_unit_by_default(cluster: Cluster):
    runner = FakeRunner()
    LinuxNetwork(cluster, runner).up()
    assert not any(BOOT_UNIT_NAME in argv for argv, _ in runner.calls)
    assert not any("systemctl" in argv for argv, _ in runner.calls)


def test_render_boot_unit_reruns_host_up_with_the_given_egress_mode():
    unit = render_boot_unit(Path("/opt/grain"), EgressMode.ALLOWLIST)
    assert "WorkingDirectory=/opt/grain" in unit
    assert "ExecStart=/usr/bin/python3 -m grain.cli host up --egress allowlist" in unit
    # Ordering: the policy must exist before libvirt starts routing traffic
    # for the sandboxes it autostarts, not at some unordered later point.
    assert "Before=libvirtd.service" in unit


def test_install_boot_unit_writes_and_enables_it(cluster: Cluster):
    runner = FakeRunner()
    LinuxNetwork(cluster, runner).install_boot_unit(Path("/opt/grain"), EgressMode.OPEN)
    tee_calls = [
        (argv, stdin) for argv, stdin in runner.calls if argv[:1] == ["tee"]
    ]
    assert len(tee_calls) == 1
    argv, stdin = tee_calls[0]
    assert argv == ["tee", BOOT_UNIT_PATH]
    assert stdin == render_boot_unit(Path("/opt/grain"), EgressMode.OPEN)
    assert runner.ran("systemctl daemon-reload")
    assert runner.ran(f"systemctl enable {BOOT_UNIT_NAME}")
