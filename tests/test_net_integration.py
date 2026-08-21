"""Integration checks against a real nftables.

Skipped unless the kernel's netfilter is reachable and we are root, so the
suite still runs on a developer machine. Where it *can* run, it is worth a
lot: the ruleset is the security model, and a rule that parses is not
necessarily a rule the kernel accepts.

The rules touch only a forwarding path for a subnet that does not exist on
the test machine, and everything is torn down in a finally block.
"""

from __future__ import annotations

import os
import subprocess

import pytest

from grain.adapter.base import EgressMode
from grain.adapter.net_linux import NAT_TABLE, TABLE, render_ruleset
from grain.inventory import Cluster


def _netfilter_available() -> bool:
    if os.geteuid() != 0:
        return False
    try:
        return subprocess.run(
            ["nft", "list", "ruleset"], capture_output=True
        ).returncode == 0
    except FileNotFoundError:
        return False


pytestmark = pytest.mark.skipif(
    not _netfilter_available(), reason="needs root and a reachable netfilter"
)


def _nft(args: list[str], stdin: str | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["nft", *args], input=stdin, capture_output=True, text=True
    )


def _destroy() -> None:
    _nft(["delete", "table", "inet", TABLE])
    _nft(["delete", "table", "ip", NAT_TABLE])


@pytest.fixture
def applied():
    cluster = Cluster(sandbox_count=2)
    ruleset = render_ruleset(cluster, EgressMode.OPEN)
    # Same sequence LinuxNetwork.apply_rules uses: pre-create so the
    # ruleset's own `delete table` is unconditional and the swap is atomic.
    _nft(["add", "table", "inet", TABLE])
    _nft(["add", "table", "ip", NAT_TABLE])
    try:
        result = _nft(["-f", "-"], stdin=ruleset)
        assert result.returncode == 0, result.stderr
        yield cluster
    finally:
        _destroy()


def test_kernel_accepts_the_generated_ruleset(applied):
    """Parsing is not the same as the kernel accepting it."""
    listed = _nft(["list", "table", "inet", TABLE]).stdout
    assert "policy drop" in listed


def test_applying_twice_is_idempotent(applied):
    cluster = applied
    ruleset = render_ruleset(cluster, EgressMode.OPEN)
    second = _nft(["-f", "-"], stdin=ruleset)
    assert second.returncode == 0, second.stderr
    listed = _nft(["list", "table", "inet", TABLE]).stdout
    # A re-apply must replace, not append: counting a distinctive rule guards
    # against a ruleset that grows every time the network comes up.
    assert listed.count("169.254.169.254") == 0  # that rule lives in the nat table
    nat = _nft(["list", "table", "ip", NAT_TABLE]).stdout
    assert nat.count("169.254.169.254") == cluster.sandbox_count


def test_switching_egress_mode_removes_the_masquerade(applied):
    cluster = applied
    locked = render_ruleset(cluster, EgressMode.ALLOWLIST)
    result = _nft(["-f", "-"], stdin=locked)
    assert result.returncode == 0, result.stderr
    nat = _nft(["list", "table", "ip", NAT_TABLE]).stdout
    assert "masquerade" not in nat
    # ...but each sandbox keeps its own metadata route.
    assert nat.count("dnat to") == cluster.sandbox_count
