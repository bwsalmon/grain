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


def test_dry_run_cleanup_prints_commands_for_every_sandbox(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "host", "cleanup"], capsys)
    assert out.count("+ ssh") == 4  # kind + docker prune, per sandbox
    assert "kind delete clusters --all" in out
    assert "docker system prune -af --volumes" in out
    assert "sandbox-0" in out and "sandbox-1" in out
    # cleanup never touches the controller's own address.
    assert "10.100.0.2 " not in out and "@10.100.0.2 " not in out


def test_dry_run_cleanup_targets_one_named_sandbox(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "host", "cleanup", "sandbox-1"], capsys)
    assert "sandbox-1" in out
    assert "sandbox-0" not in out


def test_dry_run_health_prints_a_status_line_per_sandbox(capsys):
    # Not the `run()` helper: a dry run has no real df/systemctl/docker
    # output to parse, so a nonzero (unhealthy) exit is the expected
    # outcome here, not an error to assert away.
    main(["--dry-run", "--sandboxes", "2", "host", "health"])
    out = capsys.readouterr().out
    lines = [l for l in out.splitlines() if l.startswith("sandbox-")]
    assert len(lines) == 2
    assert all("ssh=ok" in l for l in lines)


def test_health_exit_code_is_nonzero_when_unhealthy():
    # A dry run never produces real df/systemctl/docker output, so every
    # health check here reports degraded/unparseable — the CLI's own exit
    # code has to reflect that, matching `grain github audit`'s
    # nonzero-on-flagged convention.
    assert main(["--dry-run", "host", "health"]) == 1


def test_cleanup_ssh_user_and_key_are_overridable(capsys):
    out = run(["--dry-run", "host", "cleanup", "sandbox-0",
               "--ssh-user", "op", "--ssh-key", "/tmp/id_ed25519"], capsys)
    assert "op@" in out
    assert "-i /tmp/id_ed25519" in out


def test_missing_tool_raises_a_legible_error_when_check_is_on():
    from grain.run import CommandError, RealRunner

    try:
        RealRunner().run(["definitely-not-a-real-binary"])
    except CommandError as exc:
        assert "not found on PATH" in str(exc)
        assert exc.returncode == 127
    else:
        raise AssertionError("expected CommandError")
