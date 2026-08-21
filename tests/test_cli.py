import pytest

from grain.cli import build_parser, main


def run(argv, capsys) -> str:
    assert main(argv) == 0
    return capsys.readouterr().out


def test_rules_prints_without_applying(capsys):
    out = run(["host", "rules"], capsys)
    assert "policy drop" in out
    assert "masquerade" in out
    # Nothing was executed: no command echo, no side effects.
    assert "+ nft" not in out


def test_rules_reflects_sandbox_count(capsys):
    out = run(["--sandboxes", "4", "host", "rules"], capsys)
    assert out.count("dnat to") == 4


def test_rules_allowlist_mode_drops_egress(capsys):
    out = run(["host", "rules", "--egress", "allowlist"], capsys)
    assert "masquerade" not in out


def test_input_chain_is_opt_in_and_keeps_ssh(capsys):
    out = run(["host", "rules", "--input-chain", "--ssh-port", "2222"], capsys)
    assert "tcp dport 2222 accept" in out
    assert "grain_input" in out


def test_dry_run_up_executes_nothing_but_shows_everything(capsys):
    out = run(["--dry-run", "host", "up"], capsys)
    assert "+ ip link add br-grain type bridge" in out or "+ ip addr replace" in out
    assert "+ nft -f -" in out
    assert "policy drop" in out


def test_status_lists_every_vm_with_its_assigned_address(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "host", "status"], capsys)
    assert "controller" in out and "10.100.0.2" in out
    assert "sandbox-0" in out and "10.100.0.10" in out
    assert "sandbox-1" in out and "10.100.0.11" in out


def test_unknown_vm_name_is_rejected():
    with pytest.raises(SystemExit):
        main(["--dry-run", "host", "start", "sandbox-99"])


def test_targets_accept_group_aliases(capsys):
    out = run(["--dry-run", "host", "start", "sandboxes"], capsys)
    assert "sandbox-0" in out and "sandbox-1" in out
    assert "start --tty=false controller" not in out


def test_a_subcommand_is_required():
    with pytest.raises(SystemExit):
        build_parser().parse_args([])


def test_missing_tooling_is_reported_not_crashed(capsys):
    """A dry run is most useful on a host that isn't set up yet."""
    out = run(["--dry-run", "host", "status"], capsys)
    # limactl is absent here; every VM should read as absent, not explode.
    assert out.count("absent") == 3


def test_provision_with_all_target_is_rejected():
    """The controller and the sandboxes are provisioned by different
    scripts now (provision/controller.sh vs. provision/sandbox.sh) — one
    --provision applied to 'all' would be wrong for one role or the other.
    """
    with pytest.raises(SystemExit, match="controller.*sandboxes|sandboxes.*controller"):
        main(["--dry-run", "host", "create", "all", "--provision", "some/script.sh"])


def test_provision_with_a_specific_target_is_still_allowed(tmp_path, capsys):
    script = tmp_path / "script.sh"
    script.write_text("#!/bin/bash\necho hi\n")
    out = run(
        ["--dry-run", "--config-dir", str(tmp_path / "instances"),
         "host", "create", "controller", "--provision", str(script)],
        capsys,
    )
    assert "+ virsh -c qemu:///system define" in out


def test_ssh_public_key_flows_into_created_vms(tmp_path, capsys):
    pubkey = tmp_path / "controller-ssh.pub"
    pubkey.write_text("ssh-ed25519 AAAAtest controller\n")
    config_dir = tmp_path / "instances"
    run(
        ["--dry-run", "--config-dir", str(config_dir),
         "--ssh-public-key", str(pubkey), "host", "create", "sandbox-0"],
        capsys,
    )
    meta_data = (config_dir / "sandbox-0-meta-data").read_text()
    assert "ssh-ed25519 AAAAtest controller" in meta_data


def test_ssh_public_key_defaults_to_a_host_local_path():
    """Not /data/... — /data lives on the controller, a different machine
    from the host this command runs on (docs/design.md's host/controller
    split). See grain/adapter/libvirt.py's LibvirtAdapter default.
    """
    args = build_parser().parse_args(["--dry-run", "host", "status"])
    assert args.ssh_public_key == "/var/lib/grain/controller-ssh.pub"
    assert not args.ssh_public_key.startswith("/data")


def test_missing_tool_raises_a_legible_error_when_check_is_on():
    from grain.run import CommandError, RealRunner

    try:
        RealRunner().run(["definitely-not-a-real-binary"])
    except CommandError as exc:
        assert "not found on PATH" in str(exc)
        assert exc.returncode == 127
    else:
        raise AssertionError("expected CommandError")
