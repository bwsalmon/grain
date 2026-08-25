import ipaddress
import shlex
from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.libvirt import (
    LibvirtAdapter, mac_for, render_domain_xml, render_meta_data, render_user_data,
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
    assert f"<log file='{tmp_path}/sandbox-0-console.log' append='on'/>" in xml
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


def test_stop_shuts_down_a_running_vm(adapter):
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list(("sandbox-0", "running")))
    a.stop("sandbox-0")
    assert runner.ran("virsh -c qemu:///system shutdown sandbox-0")


def test_list_vms_ignores_a_domain_not_in_this_cluster(adapter):
    """`virsh list --all` reports every domain on the host, not just this
    deployment's own -- a stray domain from something else entirely must
    not be treated as one of this cluster's VMs.
    """
    a, runner = adapter
    runner.expect(
        "virsh -c qemu:///system list --all",
        stdout=virsh_list(("some-unrelated-vm", "running"), ("sandbox-0", "running")),
    )
    names = [info.name for info in a.list_vms()]
    assert "some-unrelated-vm" not in names
    assert "sandbox-0" in names


def test_list_vms_tolerates_a_line_with_no_state_column(adapter):
    a, runner = adapter
    header = " Id   Name       State\n----------------------------\n"
    runner.expect("virsh -c qemu:///system list --all", stdout=header + " 0    sandbox-0\n")
    infos = a.list_vms()
    assert len(infos) == 1
    assert infos[0].name == "sandbox-0"
    assert infos[0].state is VmState.UNKNOWN


def test_list_vms_reads_as_empty_when_virsh_is_missing(adapter):
    # A dry run on a host with no virsh installed: absent, not a crash --
    # the same "read-only commands still execute" contract DryRunRunner
    # documents for exactly this case.
    a, runner = adapter
    runner.expect("virsh -c qemu:///system list --all", returncode=127, stderr="virsh: not found")
    assert a.list_vms() == []


def test_list_vms_skips_a_line_with_only_one_field(adapter):
    """Defensive: a line that doesn't even carry a name is not parseable
    at all, so it's skipped outright rather than raised on.
    """
    a, runner = adapter
    header = " Id   Name       State\n----------------------------\n"
    runner.expect(
        "virsh -c qemu:///system list --all",
        stdout=header + " 0\n 1    sandbox-0   running\n",
    )
    names = [info.name for info in a.list_vms()]
    assert names == ["sandbox-0"]


def test_domain_xml_pins_the_assigned_address_via_mac(cluster):
    spec = cluster.spec_of("sandbox-1")
    out = render_domain_xml(cluster, spec, Path("/x/sandbox-1.qcow2"), Path("/x/sandbox-1-seed.iso"),
                             Path("/x/sandbox-1-console.log"))
    assert cluster.interface_of("sandbox-1") in out
    assert mac_for(cluster.address_of("sandbox-1")) in out


def test_domain_xml_logs_the_serial_console_to_the_host(cluster):
    spec = cluster.spec_of("sandbox-1")
    out = render_domain_xml(cluster, spec, Path("/x/sandbox-1.qcow2"), Path("/x/sandbox-1-seed.iso"),
                             Path("/x/sandbox-1-console.log"))
    assert "<log file='/x/sandbox-1-console.log' append='on'/>" in out


def test_user_data_substitutes_the_controller_ip_placeholder(cluster):
    out = render_user_data('CONTROLLER_IP="__GRAIN_CONTROLLER_IP__"\n', cluster)
    assert f'CONTROLLER_IP="{cluster.controller_ip}"' in out
    assert "__GRAIN_CONTROLLER_IP__" not in out


def test_user_data_substitution_follows_a_non_default_subnet(cluster):
    other = Cluster(subnet=ipaddress.IPv4Network("10.200.0.0/24"))
    out = render_user_data('CONTROLLER_IP="__GRAIN_CONTROLLER_IP__"\n', other)
    assert 'CONTROLLER_IP="10.200.0.2"' in out


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


def test_start_also_allowlists_apparmor_for_a_vm_that_skipped_create(cluster, tmp_path):
    """Found live: create()'s own allowlisting wasn't enough on its own --
    bootstrap.py's _ensure_started() calls start() directly, with no
    create() at all, for a VM already defined from an earlier attempt
    (predating this fix, or from a previous deploy generation). The exact
    failure this reproduces: 'Cannot access storage file ... Permission
    denied' on a real redeploy, because the domain existed already and
    start() alone never ran the allowlist check.
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

    a.start("sandbox-0")  # no create() call at all

    written = _tee_stdin(runner, f"tee -a {rules}")
    assert f"{tmp_path}/instances/*.qcow2 rwk," in written
    assert f"{tmp_path}/images/*.qcow2 rwk," in written
    assert runner.ran("systemctl reload apparmor")
    assert runner.ran("virsh -c qemu:///system start sandbox-0")


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


def test_create_labels_config_dir_and_image_dir_for_selinux(cluster, tmp_path):
    """Found live: AppArmor wasn't the actual confining layer on a real
    deployment -- SELinux was (confirmed by _SELINUX_CONTEXT: "libvirtd
    (enforce)" in the journal, and separately by the per-VM AppArmor
    profile itself being reported as profile="unconfined", i.e. genuinely
    inactive). sVirt only auto-relabels a file's dynamic MCS category at
    start time, not its base type -- that has to already be virt_image_t,
    which the packaged policy only pre-labels for the default
    /var/lib/libvirt/images.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    image_path = tmp_path / "images" / "debian-12.qcow2"
    sized_cluster = Cluster(sandbox_count=2, image=str(image_path))
    a = LibvirtAdapter(sized_cluster, runner, network,
                        config_dir=tmp_path / "instances",
                        selinux_marker_path=tmp_path / "selinux")
    (tmp_path / "selinux").mkdir()
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())

    a.create(sized_cluster.spec_of("sandbox-0"))

    assert runner.ran(f"chcon -R -t virt_image_t {tmp_path}/instances")
    assert runner.ran(f"chcon -R -t virt_image_t {tmp_path}/images")


def test_create_is_a_noop_for_selinux_when_not_active(cluster, tmp_path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    a = LibvirtAdapter(cluster, runner, network, config_dir=tmp_path / "instances",
                        selinux_marker_path=tmp_path / "no-selinux")
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())
    assert not a.selinux_marker_path.is_dir()

    a.create(cluster.spec_of("sandbox-0"))  # must not raise

    assert not runner.ran("chcon")


def test_start_also_labels_selinux_for_a_vm_that_skipped_create(cluster, tmp_path):
    """The exact failure this reproduces: start() called with no
    preceding create() at all, for a VM already defined from an earlier
    attempt -- create()'s own labelling never ran for it either.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    image_path = tmp_path / "images" / "debian-12.qcow2"
    sized_cluster = Cluster(sandbox_count=2, image=str(image_path))
    a = LibvirtAdapter(sized_cluster, runner, network,
                        config_dir=tmp_path / "instances",
                        selinux_marker_path=tmp_path / "selinux")
    (tmp_path / "selinux").mkdir()

    a.start("sandbox-0")  # no create() call at all

    assert runner.ran(f"chcon -R -t virt_image_t {tmp_path}/instances")
    assert runner.ran(f"chcon -R -t virt_image_t {tmp_path}/images")
    assert runner.ran("virsh -c qemu:///system start sandbox-0")


def test_create_grants_world_access_to_config_dir_and_image_dir(cluster, tmp_path):
    """Found live: neither AppArmor nor SELinux was ever the actual
    confining layer -- real diagnostics from a failing deploy showed
    config_dir still root:root 0700 and every file in it still root:root
    0600 right through a failed start, meaning libvirt's own
    dynamic_ownership never chowned them to the unprivileged uid:gid qemu
    actually runs as. Unlike the two MAC calls, this one has no "is the
    subsystem present" guard -- plain Unix permissions always apply.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    image_path = tmp_path / "images" / "debian-12.qcow2"
    sized_cluster = Cluster(sandbox_count=2, image=str(image_path))
    a = LibvirtAdapter(sized_cluster, runner, network, config_dir=tmp_path / "instances")
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list())

    a.create(sized_cluster.spec_of("sandbox-0"))

    assert runner.ran(f"chmod -R o+rwX {tmp_path}/instances")
    assert runner.ran(f"chmod -R o+rwX {tmp_path}/images")


def test_start_also_grants_dac_access_for_a_vm_that_skipped_create(cluster, tmp_path):
    """The exact failure this reproduces: start() called with no
    preceding create() at all, for a VM already defined from an earlier
    attempt -- create()'s own chmod never ran for it either.
    """
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    image_path = tmp_path / "images" / "debian-12.qcow2"
    sized_cluster = Cluster(sandbox_count=2, image=str(image_path))
    a = LibvirtAdapter(sized_cluster, runner, network, config_dir=tmp_path / "instances")

    a.start("sandbox-0")  # no create() call at all

    assert runner.ran(f"chmod -R o+rwX {tmp_path}/instances")
    assert runner.ran(f"chmod -R o+rwX {tmp_path}/images")
    assert runner.ran("virsh -c qemu:///system start sandbox-0")
