import json
from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.lima import LimaAdapter, render_instance_config
from grain.adapter.net_linux import LinuxNetwork
from grain.inventory import Cluster
from grain.run import FakeRunner


def lima_list(*entries: dict) -> str:
    """Lima emits one JSON object per line, not a JSON array."""
    return "\n".join(json.dumps(e) for e in entries)


@pytest.fixture
def cluster() -> Cluster:
    return Cluster(sandbox_count=2)


@pytest.fixture
def adapter(cluster: Cluster, tmp_path: Path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    return LimaAdapter(cluster, runner, network, config_dir=tmp_path), runner


def test_address_comes_from_the_inventory_not_the_hypervisor(adapter, cluster):
    a, runner = adapter
    assert a.address("sandbox-1") == cluster.address_of("sandbox-1")
    # Crucially, asking for an address must not shell out at all: if it did,
    # the firewall rules and the VMs could disagree about who is who.
    assert runner.calls == []


def test_state_reports_absent_for_a_vm_lima_does_not_know(adapter):
    a, runner = adapter
    runner.expect("limactl list --json", stdout=lima_list())
    assert a.state("sandbox-0") is VmState.ABSENT


def test_state_maps_lima_status(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json",
        stdout=lima_list(
            {"name": "sandbox-0", "status": "Running"},
            {"name": "sandbox-1", "status": "Stopped"},
        ),
    )
    assert a.state("sandbox-0") is VmState.RUNNING
    assert a.state("sandbox-1") is VmState.STOPPED


def test_foreign_lima_instances_are_ignored(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json",
        stdout=lima_list(
            {"name": "someone-elses-vm", "status": "Running"},
            {"name": "sandbox-0", "status": "Running"},
        ),
    )
    assert [i.name for i in a.list_vms()] == ["sandbox-0"]


def test_create_refuses_to_adopt_an_existing_vm(adapter, cluster):
    a, runner = adapter
    runner.expect(
        "limactl list --json", stdout=lima_list({"name": "sandbox-0", "status": "Stopped"})
    )
    with pytest.raises(RuntimeError, match="already exists"):
        a.create(cluster.spec_of("sandbox-0"))
    assert not runner.ran("limactl create")


def test_create_writes_a_config_and_invokes_lima(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("limactl list --json", stdout=lima_list())
    a.create(cluster.spec_of("sandbox-0"))
    written = (tmp_path / "sandbox-0.yaml").read_text()
    assert "cpus: 6" in written
    assert '"15360MiB"' in written
    assert runner.ran("limactl create --name=sandbox-0")


def test_destroy_is_idempotent(adapter):
    a, runner = adapter
    runner.expect("limactl list --json", stdout=lima_list())
    a.destroy("sandbox-0")
    assert not runner.ran("limactl delete")


def test_destroy_deletes_a_vm_that_exists(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json", stdout=lima_list({"name": "sandbox-0", "status": "Stopped"})
    )
    a.destroy("sandbox-0")
    assert runner.ran("limactl delete --force sandbox-0")


def test_stop_only_stops_a_running_vm(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json", stdout=lima_list({"name": "sandbox-0", "status": "Stopped"})
    )
    a.stop("sandbox-0")
    assert not runner.ran("limactl stop")


def test_stop_stops_a_running_vm(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json", stdout=lima_list({"name": "sandbox-0", "status": "Running"})
    )
    a.stop("sandbox-0")
    assert runner.ran("limactl stop sandbox-0")


def test_list_vms_reads_as_empty_when_limactl_is_missing(adapter):
    # A dry run on a host with no limactl installed: absent, not a crash --
    # the same "read-only commands still execute" contract DryRunRunner
    # documents for exactly this case.
    a, runner = adapter
    runner.expect("limactl list --json", returncode=127, stderr="limactl: not found")
    assert a.list_vms() == []


def test_list_vms_skips_blank_lines(adapter):
    a, runner = adapter
    runner.expect(
        "limactl list --json",
        stdout="\n" + json.dumps({"name": "sandbox-0", "status": "Running"}) + "\n\n",
    )
    assert [i.name for i in a.list_vms()] == ["sandbox-0"]


def test_instance_config_pins_the_assigned_address(cluster):
    spec = cluster.spec_of("sandbox-1")
    out = render_instance_config(cluster, spec)
    assert str(cluster.address_of("sandbox-1")) in out
    assert cluster.interface_of("sandbox-1") in out


def test_instance_config_embeds_a_provision_script_indented(cluster):
    spec = cluster.spec_of("sandbox-0")
    out = render_instance_config(cluster, spec, provision_script="set -eux\napt-get update\n")
    assert "provision:" in out
    assert "    set -eux" in out
    assert "    apt-get update" in out


def test_instance_config_substitutes_the_controller_ip_placeholder(cluster):
    spec = cluster.spec_of("controller")
    out = render_instance_config(
        cluster, spec,
        provision_script='CONTROLLER_IP="__GRAIN_CONTROLLER_IP__"\n',
    )
    assert f'CONTROLLER_IP="{cluster.controller_ip}"' in out
    assert "__GRAIN_CONTROLLER_IP__" not in out


def test_recreate_destroys_then_creates_then_starts(adapter, cluster):
    a, runner = adapter
    # absent throughout, so destroy is a no-op and create proceeds
    runner.expect("limactl list --json", stdout=lima_list())
    a.recreate("sandbox-0")
    assert runner.ran("limactl create --name=sandbox-0")
    assert runner.ran("limactl start --tty=false sandbox-0")
