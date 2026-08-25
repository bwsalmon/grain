import ipaddress

import pytest

from grain.inventory import Cluster, Role


def test_names_and_addresses_are_derived_from_one_list():
    c = Cluster(sandbox_count=3)
    assert c.names == ["controller", "sandbox-0", "sandbox-1", "sandbox-2"]
    assert c.host_ip == ipaddress.IPv4Address("10.100.0.1")
    assert c.controller_ip == ipaddress.IPv4Address("10.100.0.2")
    assert c.address_of("sandbox-0") == ipaddress.IPv4Address("10.100.0.10")
    assert c.address_of("sandbox-2") == ipaddress.IPv4Address("10.100.0.12")


def test_addresses_are_unique_across_the_cluster():
    c = Cluster(sandbox_count=8)
    addresses = [c.address_of(n) for n in c.names] + [c.host_ip]
    assert len(set(addresses)) == len(addresses)


def test_interface_names_are_unique_and_within_the_kernel_limit():
    c = Cluster(sandbox_count=8)
    ifaces = [c.interface_of(n) for n in c.names]
    assert len(set(ifaces)) == len(ifaces)
    # Linux caps interface names at 15 characters.
    assert all(len(i) <= 15 for i in ifaces)


def test_metadata_ports_are_unique_per_sandbox():
    c = Cluster(sandbox_count=4)
    ports = [c.metadata_port(n) for n in c.sandbox_names]
    assert len(set(ports)) == len(ports)


def test_controller_has_no_metadata_server_of_its_own():
    c = Cluster()
    with pytest.raises(ValueError):
        c.metadata_port("controller")


def test_specs_carry_the_right_role_and_sizing():
    c = Cluster()
    controller = c.spec_of("controller")
    sandbox = c.spec_of("sandbox-0")
    assert controller.role is Role.CONTROLLER
    assert sandbox.role is Role.SANDBOX
    # A sandbox has to hold a kind control plane plus a build.
    assert sandbox.mem_mb > controller.mem_mb


def test_specs_lists_one_spec_per_name_in_the_same_order():
    c = Cluster(sandbox_count=2)
    assert [s.name for s in c.specs] == c.names


def test_unknown_names_are_rejected_rather_than_guessed():
    c = Cluster(sandbox_count=2)
    for bad in ["sandbox-2", "sandbox-x", "nope", "sandbox-", ""]:
        with pytest.raises(ValueError):
            c.address_of(bad)


def test_cluster_rejects_a_count_that_does_not_fit_the_subnet():
    with pytest.raises(ValueError):
        Cluster(sandbox_count=1, subnet=ipaddress.IPv4Network("10.0.0.0/30"))
    with pytest.raises(ValueError):
        Cluster(sandbox_count=0)


# --- Cluster.load (docs/bootstrap.md Phase 2) -------------------------------

def test_load_falls_back_to_defaults_when_the_file_is_absent(tmp_path):
    c = Cluster.load(tmp_path / "does-not-exist.toml")
    assert c == Cluster()


def test_load_overrides_only_the_keys_present(tmp_path):
    path = tmp_path / "cluster.toml"
    path.write_text('sandbox_count = 3\nimage = "/var/lib/grain/images/debian-12.qcow2"\n')
    c = Cluster.load(path)
    assert c.sandbox_count == 3
    assert c.image == "/var/lib/grain/images/debian-12.qcow2"
    # Everything else keeps the dataclass default.
    assert c.bridge == Cluster().bridge
    assert c.sandbox_mem_mb == Cluster().sandbox_mem_mb


def test_load_parses_the_subnet_as_a_network(tmp_path):
    path = tmp_path / "cluster.toml"
    path.write_text('subnet = "10.200.0.0/24"\n')
    c = Cluster.load(path)
    assert c.subnet == ipaddress.IPv4Network("10.200.0.0/24")
    assert c.host_ip == ipaddress.IPv4Address("10.200.0.1")


def test_load_rejects_an_unknown_key(tmp_path):
    path = tmp_path / "cluster.toml"
    path.write_text('not_a_real_field = 1\n')
    with pytest.raises(TypeError):
        Cluster.load(path)
