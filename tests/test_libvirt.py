from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.libvirt import LibvirtAdapter, mac_for, render_domain_xml
from grain.adapter.net_linux import LinuxNetwork
from grain.inventory import Cluster
from grain.run import FakeRunner


def virsh_list(*rows: tuple[str, str]) -> str:
    """Rows are (name, state). Mirrors `virsh list --all`'s table output."""
    lines = [" Id   Name       State", "----------------------------"]
    for i, (name, state) in enumerate(rows):
        lines.append(f" {i:<4} {name:<10} {state}")
    return "\n".join(lines) + "\n"


@pytest.fixture
def cluster() -> Cluster:
    return Cluster(sandbox_count=2)


@pytest.fixture
def adapter(cluster: Cluster, tmp_path: Path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    return LibvirtAdapter(cluster, runner, network, config_dir=tmp_path), runner


def test_address_comes_from_the_inventory_not_the_hypervisor(adapter, cluster):
    a, runner = adapter
    assert a.address("sandbox-1") == cluster.address_of("sandbox-1")
    assert runner.calls == []


def test_state_reports_absent_for_a_vm_libvirt_does_not_know(adapter):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list())
    assert a.state("sandbox-0") is VmState.ABSENT


def test_state_maps_virsh_status(adapter):
    a, runner = adapter
    runner.expect(
        "virsh list --all",
        stdout=virsh_list(("sandbox-0", "running"), ("sandbox-1", "shut off")),
    )
    assert a.state("sandbox-0") is VmState.RUNNING
    assert a.state("sandbox-1") is VmState.STOPPED


def test_foreign_domains_are_ignored(adapter):
    a, runner = adapter
    runner.expect(
        "virsh list --all",
        stdout=virsh_list(("someone-elses-vm", "running"), ("sandbox-0", "running")),
    )
    assert [i.name for i in a.list_vms()] == ["sandbox-0"]


def test_create_refuses_to_adopt_an_existing_vm(adapter, cluster):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    with pytest.raises(RuntimeError, match="already exists"):
        a.create(cluster.spec_of("sandbox-0"))
    assert not runner.ran("virsh define")


def test_create_writes_seed_and_domain_xml_and_defines_it(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    xml = (tmp_path / "sandbox-0.xml").read_text()
    assert "<vcpu>2</vcpu>" in xml
    assert "<memory unit='MiB'>8192</memory>" in xml
    assert cluster.interface_of("sandbox-0") in xml
    network_config = (tmp_path / "sandbox-0-network-config").read_text()
    assert str(cluster.address_of("sandbox-0")) in network_config
    assert runner.ran("qemu-img create")
    assert runner.ran("cloud-localds")
    assert runner.ran(f"virsh define {tmp_path}/sandbox-0.xml")


def test_destroy_is_idempotent(adapter):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list())
    a.destroy("sandbox-0")
    assert not runner.ran("virsh destroy")
    assert not runner.ran("virsh undefine")


def test_destroy_removes_the_files_it_created(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    written = list(tmp_path.glob("sandbox-0*"))
    assert written, "create() should have written per-instance files"

    runner.expect("virsh list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    a.destroy("sandbox-0")
    # virsh --remove-all-storage refuses plain (non-pool) files, so the
    # adapter must remove them itself rather than relying on that flag.
    assert list(tmp_path.glob("sandbox-0*")) == []


def test_stop_only_stops_a_running_vm(adapter):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    a.stop("sandbox-0")
    assert not runner.ran("virsh shutdown")


def test_domain_xml_pins_the_assigned_address_via_mac(cluster):
    spec = cluster.spec_of("sandbox-1")
    out = render_domain_xml(cluster, spec, Path("/x/sandbox-1.qcow2"), Path("/x/sandbox-1-seed.iso"))
    assert cluster.interface_of("sandbox-1") in out
    assert mac_for(cluster.address_of("sandbox-1")) in out


def test_mac_is_deterministic_and_distinct_per_vm(cluster):
    m0 = mac_for(cluster.address_of("sandbox-0"))
    m1 = mac_for(cluster.address_of("sandbox-1"))
    assert m0 != m1
    assert mac_for(cluster.address_of("sandbox-0")) == m0


def test_recreate_destroys_then_creates_then_starts(adapter, cluster):
    a, runner = adapter
    runner.expect("virsh list --all", stdout=virsh_list())
    a.recreate("sandbox-0")
    assert runner.ran("virsh define")
    assert runner.ran("virsh start sandbox-0")
