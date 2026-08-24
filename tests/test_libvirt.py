import shlex
from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.libvirt import (
    LibvirtAdapter, mac_for, render_domain_xml, render_meta_data,
)
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
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    assert a.state("sandbox-0") is VmState.ABSENT


def test_state_maps_virsh_status(adapter):
    a, runner = adapter
    runner.expect(
        "virsh -c qemu:///system list --all",
        stdout=virsh_list(("sandbox-0", "running"), ("sandbox-1", "shut off")),
    )
    assert a.state("sandbox-0") is VmState.RUNNING
    assert a.state("sandbox-1") is VmState.STOPPED


def test_foreign_domains_are_ignored(adapter):
    a, runner = adapter
    runner.expect(
        "virsh -c qemu:///system list --all",
        stdout=virsh_list(("someone-elses-vm", "running"), ("sandbox-0", "running")),
    )
    assert [i.name for i in a.list_vms()] == ["sandbox-0"]


def test_create_refuses_to_adopt_an_existing_vm(adapter, cluster):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    with pytest.raises(RuntimeError, match="already exists"):
        a.create(cluster.spec_of("sandbox-0"))
    assert not runner.ran("virsh -c qemu:///system define")


def test_create_writes_seed_and_domain_xml_and_defines_it(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    xml = (tmp_path / "sandbox-0.xml").read_text()
    assert "<vcpu>2</vcpu>" in xml
    assert "<memory unit='MiB'>8192</memory>" in xml
    assert cluster.interface_of("sandbox-0") in xml
    network_config = (tmp_path / "sandbox-0-network-config").read_text()
    assert str(cluster.address_of("sandbox-0")) in network_config
    assert "nameservers" in network_config
    assert runner.ran("qemu-img create")
    assert runner.ran("cloud-localds")
    assert runner.ran(f"virsh -c qemu:///system define {tmp_path}/sandbox-0.xml")


def test_destroy_is_idempotent(adapter):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.destroy("sandbox-0")
    assert not runner.ran("virsh -c qemu:///system destroy")
    assert not runner.ran("virsh -c qemu:///system undefine")


def test_destroy_removes_the_files_it_created(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    written = list(tmp_path.glob("sandbox-0*"))
    assert written, "create() should have written per-instance files"

    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    a.destroy("sandbox-0")
    # virsh --remove-all-storage refuses plain (non-pool) files, so the
    # adapter must remove them itself rather than relying on that flag.
    assert list(tmp_path.glob("sandbox-0*")) == []


def test_stop_only_stops_a_running_vm(adapter):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list(("sandbox-0", "shut off")))
    a.stop("sandbox-0")
    assert not runner.ran("virsh -c qemu:///system shutdown")


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


def test_meta_data_omits_public_keys_by_default():
    assert "public-keys" not in render_meta_data("sandbox-0")


def test_meta_data_embeds_one_key():
    out = render_meta_data("sandbox-0", ["ssh-ed25519 AAAAtest controller\n"])
    assert "public-keys:\n  - ssh-ed25519 AAAAtest controller\n" in out


def test_meta_data_embeds_multiple_keys_one_entry_each():
    out = render_meta_data("sandbox-0", ["ssh-ed25519 AAAAtest admin", "ssh-ed25519 BBBBtest controller"])
    assert out.count("  - ") == 2
    assert "  - ssh-ed25519 AAAAtest admin\n" in out
    assert "  - ssh-ed25519 BBBBtest controller\n" in out


def test_meta_data_skips_blank_keys():
    out = render_meta_data("sandbox-0", ["", "ssh-ed25519 AAAAtest admin"])
    assert out.count("  - ") == 1


def test_create_embeds_the_admin_key_on_the_controller_only(cluster, tmp_path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    admin_key = tmp_path / "admin-ssh.pub"
    admin_key.write_text("ssh-ed25519 AAAAtest admin\n")
    controller_key = tmp_path / "controller-ssh.pub"
    controller_key.write_text("ssh-ed25519 BBBBtest controller\n")
    a = LibvirtAdapter(cluster, runner, network, config_dir=tmp_path / "instances",
                        admin_public_key_path=admin_key,
                        controller_public_key_path=controller_key)
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.create(cluster.spec_of("controller"))
    meta_data = (tmp_path / "instances" / "controller-meta-data").read_text()
    assert "ssh-ed25519 AAAAtest admin" in meta_data
    assert "ssh-ed25519 BBBBtest controller" not in meta_data


def test_create_embeds_both_keys_on_a_sandbox(cluster, tmp_path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    admin_key = tmp_path / "admin-ssh.pub"
    admin_key.write_text("ssh-ed25519 AAAAtest admin\n")
    controller_key = tmp_path / "controller-ssh.pub"
    controller_key.write_text("ssh-ed25519 BBBBtest controller\n")
    a = LibvirtAdapter(cluster, runner, network, config_dir=tmp_path / "instances",
                        admin_public_key_path=admin_key,
                        controller_public_key_path=controller_key)
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    meta_data = (tmp_path / "instances" / "sandbox-0-meta-data").read_text()
    assert "ssh-ed25519 AAAAtest admin" in meta_data
    assert "ssh-ed25519 BBBBtest controller" in meta_data


def test_create_omits_public_keys_when_no_key_file_present(adapter, cluster, tmp_path):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.create(cluster.spec_of("sandbox-0"))
    meta_data = (tmp_path / "sandbox-0-meta-data").read_text()
    assert "public-keys" not in meta_data


def test_key_paths_default_host_local_not_data(cluster):
    """/data lives on the controller; this adapter runs on the host, a
    different machine (docs/design.md's host/controller split) -- so the
    default must not assume a shared /data.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    a = LibvirtAdapter(cluster, runner, network)
    assert str(a.admin_public_key_path) == "/var/lib/grain/admin-ssh.pub"
    assert str(a.controller_public_key_path) == "/var/lib/grain/controller-ssh.pub"


def test_recreate_destroys_then_creates_then_starts(adapter, cluster):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    a.recreate("sandbox-0")
    assert runner.ran("virsh -c qemu:///system define")
    assert runner.ran("virsh -c qemu:///system start sandbox-0")


def _tee_stdin(runner: FakeRunner, prefix: str) -> str:
    for argv, stdin in runner.calls:
        if shlex.join(argv).startswith(prefix):
            return stdin or ""
    raise AssertionError(f"no call starting with {prefix!r}; calls were {runner.commands}")


def test_create_allowlists_config_dir_and_image_dir_in_apparmor(cluster, tmp_path):
    """Found live: qemu failed to start a VM whose disk lived at
    config_dir's own default (a custom path, not the
    /var/lib/libvirt/images/ Debian's libvirt AppArmor profile allows by
    default) with "Cannot access storage file ... Permission denied" --
    not an ownership problem, AppArmor denies the open() before file
    permissions are ever checked. FakeRunner only records the `tee` call
    rather than really executing it, so the write is checked via its
    recorded stdin, not by reading the (never actually written) file back.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    image_path = tmp_path / "images" / "debian-12.qcow2"
    sized_cluster = Cluster(sandbox_count=2, image=str(image_path))
    rules = tmp_path / "apparmor" / "local" / "abstractions" / "libvirt-qemu"
    rules.parent.mkdir(parents=True)
    a = LibvirtAdapter(sized_cluster, runner, network,
                        config_dir=tmp_path / "instances",
                        apparmor_rules_path=rules)
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())

    a.create(sized_cluster.spec_of("sandbox-0"))

    written = _tee_stdin(runner, f"tee -a {rules}")
    assert f"{tmp_path}/instances/*.qcow2 rwk," in written
    assert f"{tmp_path}/images/*.qcow2 rwk," in written
    assert runner.ran("systemctl reload apparmor")


def test_create_is_a_noop_when_apparmor_isnt_installed(cluster, tmp_path):
    """Not every host runs AppArmor at all -- a tmp_path-based
    apparmor_rules_path whose parent was never created stands in for that,
    regardless of whether *this* machine happens to have AppArmor
    installed (the shared `adapter` fixture uses the real system path, so
    it can't be relied on to test the absent case portably).
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    a = LibvirtAdapter(cluster, runner, network, config_dir=tmp_path / "instances",
                        apparmor_rules_path=tmp_path / "no-apparmor" / "libvirt-qemu")
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    assert not a.apparmor_rules_path.parent.exists()

    a.create(cluster.spec_of("sandbox-0"))  # must not raise

    assert not runner.ran("tee")
    assert not runner.ran("systemctl reload apparmor")


def test_create_skips_an_already_allowlisted_directory(cluster, tmp_path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    rules = tmp_path / "apparmor" / "libvirt-qemu"
    rules.parent.mkdir(parents=True)
    config_dir = tmp_path / "instances"
    rules.write_text(f"  {config_dir}/*.qcow2 rwk,\n  {config_dir}/*.iso rk,\n")
    a = LibvirtAdapter(cluster, runner, network, config_dir=config_dir,
                        apparmor_rules_path=rules)
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())

    a.create(cluster.spec_of("sandbox-0"))

    # debian-12 (the default bare image string) resolves to a relative
    # ".", which is deliberately never allowlisted (not a real path) --
    # config_dir is the only real candidate here, and it's already
    # present, so no write happens at all.
    assert not runner.ran("tee")
    assert not runner.ran("systemctl reload apparmor")
