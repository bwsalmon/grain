import json

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
    assert out.count("tcp dport 8080 accept") == 4


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


def test_dry_run_egress_applies_the_named_policy(capsys):
    out = run(["--dry-run", "host", "egress", "allowlist"], capsys)
    assert "+ nft -f -" in out
    assert "masquerade" not in out


def test_dry_run_egress_open_keeps_masquerade(capsys):
    out = run(["--dry-run", "host", "egress", "open"], capsys)
    assert "+ nft -f -" in out
    assert "masquerade" in out


def test_dry_run_stop_probes_every_targets_state_first(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "host", "stop", "sandboxes"], capsys)
    assert out.count("virsh -c qemu:///system list --all") == 2


def test_dry_run_destroy_probes_every_targets_state_first(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "host", "destroy", "sandboxes"], capsys)
    assert out.count("virsh -c qemu:///system list --all") == 2


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


def test_build_cluster_applies_an_image_override(tmp_path):
    import argparse

    from grain.cli import build_cluster

    args = argparse.Namespace(
        cluster_file=str(tmp_path / "does-not-exist.json"),
        sandboxes=None, image="/custom/base.qcow2",
    )
    cluster = build_cluster(args)
    assert cluster.image == "/custom/base.qcow2"


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


def test_admin_ssh_public_key_flows_into_created_vms(tmp_path, capsys):
    pubkey = tmp_path / "admin-ssh.pub"
    pubkey.write_text("ssh-ed25519 AAAAtest admin\n")
    config_dir = tmp_path / "instances"
    run(
        ["--dry-run", "--config-dir", str(config_dir),
         "--admin-ssh-public-key", str(pubkey), "host", "create", "sandbox-0"],
        capsys,
    )
    meta_data = (config_dir / "sandbox-0-meta-data").read_text()
    assert "ssh-ed25519 AAAAtest admin" in meta_data


def test_controller_ssh_public_key_flows_into_created_sandboxes_only(tmp_path, capsys):
    pubkey = tmp_path / "controller-ssh.pub"
    pubkey.write_text("ssh-ed25519 BBBBtest controller\n")
    config_dir = tmp_path / "instances"
    run(
        ["--dry-run", "--config-dir", str(config_dir),
         "--controller-ssh-public-key", str(pubkey), "host", "create", "sandbox-0"],
        capsys,
    )
    meta_data = (config_dir / "sandbox-0-meta-data").read_text()
    assert "ssh-ed25519 BBBBtest controller" in meta_data

    run(
        ["--dry-run", "--config-dir", str(config_dir),
         "--controller-ssh-public-key", str(pubkey), "host", "create", "controller"],
        capsys,
    )
    controller_meta = (config_dir / "controller-meta-data").read_text()
    assert "ssh-ed25519 BBBBtest controller" not in controller_meta


def test_ssh_public_keys_default_to_host_local_paths():
    """Not /data/... — /data lives on the controller, a different machine
    from the host this command runs on (docs/design.md's host/controller
    split). See grain/adapter/libvirt.py's LibvirtAdapter defaults.
    """
    args = build_parser().parse_args(["--dry-run", "host", "status"])
    assert args.admin_ssh_public_key == "/var/lib/grain/admin-ssh.pub"
    assert args.controller_ssh_public_key == "/var/lib/grain/controller-ssh.pub"
    assert not args.admin_ssh_public_key.startswith("/data")
    assert not args.controller_ssh_public_key.startswith("/data")


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


# --- grain sessions (docs/roadmap.md item 10) -------------------------------

def test_sessions_list_reports_nothing_recorded_yet(tmp_path, capsys):
    out = run(["--data-dir", str(tmp_path), "sessions", "list"], capsys)
    assert "no sessions recorded yet" in out


def test_sessions_list_prints_a_row_per_recorded_session(tmp_path, capsys):
    from datetime import datetime, timezone

    from grain.automation.history import FileSessionHistory
    from grain.automation.state import TriggerKind

    history = FileSessionHistory(tmp_path / "state" / "automation" / "sessions")
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    history.record(issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0",
                    unit="grain-task-sandbox-0", started_at=now, finished_at=now,
                    outcome="succeeded", transcript_text="hi\n")
    history.record(issue=8, kind=TriggerKind.PR, sandbox="sandbox-1",
                    unit="grain-task-sandbox-1", started_at=now, finished_at=now,
                    outcome="failed", transcript_text=None)

    out = run(["--data-dir", str(tmp_path), "sessions", "list"], capsys)
    assert "issue#7" in out
    assert "pr#8" in out


def test_sessions_list_filters_by_kind(tmp_path, capsys):
    from datetime import datetime, timezone

    from grain.automation.history import FileSessionHistory
    from grain.automation.state import TriggerKind

    history = FileSessionHistory(tmp_path / "state" / "automation" / "sessions")
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    history.record(issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=now, finished_at=now, outcome="succeeded",
                    transcript_text=None)
    history.record(issue=8, kind=TriggerKind.PR, sandbox="sandbox-1", unit="u1",
                    started_at=now, finished_at=now, outcome="succeeded",
                    transcript_text=None)

    out = run(["--data-dir", str(tmp_path), "sessions", "list", "--kind", "pr"], capsys)
    assert "issue#7" not in out
    assert "pr#8" in out


def test_sessions_list_filters_by_outcome(tmp_path, capsys):
    from datetime import datetime, timezone

    from grain.automation.history import FileSessionHistory
    from grain.automation.state import TriggerKind

    history = FileSessionHistory(tmp_path / "state" / "automation" / "sessions")
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    history.record(issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=now, finished_at=now, outcome="succeeded",
                    transcript_text=None)
    history.record(issue=8, kind=TriggerKind.ISSUE, sandbox="sandbox-1", unit="u1",
                    started_at=now, finished_at=now, outcome="failed",
                    transcript_text=None)

    out = run(
        ["--data-dir", str(tmp_path), "sessions", "list", "--outcome", "failed"], capsys,
    )
    assert "issue#7" not in out
    assert "issue#8" in out


def test_sessions_browse_builds_history_from_the_data_dir_and_hands_it_to_the_tui(
    tmp_path, monkeypatch,
):
    """`cmd_sessions_browse`'s own two lines -- the curses event loop
    (`sessions_tui.run`) is deliberately not unit tested, per
    `grain/automation/tui.py`'s own module docstring, so it's stubbed out
    here to check only the wiring: the right, data-dir-rooted
    `FileSessionHistory` reaches it.
    """
    from grain.automation.history import FileSessionHistory

    captured = {}
    monkeypatch.setattr(
        "grain.cli.sessions_tui.run", lambda history: captured.setdefault("history", history)
    )
    assert main(["--data-dir", str(tmp_path), "sessions", "browse"]) == 0
    assert isinstance(captured["history"], FileSessionHistory)
    assert captured["history"].root == tmp_path / "state" / "automation" / "sessions"


def test_sessions_list_filters_by_trigger_number(tmp_path, capsys):
    from datetime import datetime, timezone

    from grain.automation.history import FileSessionHistory
    from grain.automation.state import TriggerKind

    history = FileSessionHistory(tmp_path / "state" / "automation" / "sessions")
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    history.record(issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=now, finished_at=now, outcome="succeeded",
                    transcript_text=None)
    history.record(issue=8, kind=TriggerKind.ISSUE, sandbox="sandbox-1", unit="u1",
                    started_at=now, finished_at=now, outcome="succeeded",
                    transcript_text=None)

    out = run(["--data-dir", str(tmp_path), "sessions", "list", "--trigger", "7"], capsys)
    assert "issue#7" in out
    assert "issue#8" not in out


def test_sessions_browse_is_wired_to_the_tui_entry_point():
    args = build_parser().parse_args(["sessions", "browse"])
    from grain.cli import cmd_sessions_browse
    assert args.func is cmd_sessions_browse


# --- grain host wait/deploy/bootstrap, grain controller configure, grain
# --- sandbox login (docs/bootstrap.md) --------------------------------------

def test_dry_run_wait_probes_ssh_and_cloud_init_for_every_target(capsys):
    out = run(["--dry-run", "--sandboxes", "1", "host", "wait"], capsys)
    assert out.count("+ ssh") == 4  # (probe + cloud-init) x (controller + sandbox-0)
    assert "controller" in out and "ready" in out
    assert "sandbox-0" in out


def test_a_host_wait_that_times_out_prints_diagnostics(capsys, monkeypatch):
    """`host wait` is the manual half of `host bootstrap`'s stage 5, and a
    bare TimeoutError is as unreadable run by hand as it was in a deploy --
    see grain/adapter/diagnostics.py.
    """
    def never(ssh, timeout):
        raise TimeoutError("10.100.0.2 never became reachable over SSH")

    monkeypatch.setattr("grain.cli.wait_for_ssh", never)
    with pytest.raises(TimeoutError):
        main(["--dry-run", "host", "wait", "controller"])
    out = capsys.readouterr().out
    assert "dominfo controller" in out
    assert "serial console" in out


def test_a_host_wait_whose_cloud_init_never_finishes_prints_guest_diagnostics(capsys, monkeypatch):
    """The other half of `host wait`'s two diagnostics dumps: a VM that
    answers SSH but never finishes provisioning gets the guest-side dump
    (`dump_guest_diagnostics`), not the host-side one `wait_for_ssh`'s own
    timeout gets.
    """
    def never(ssh):
        raise TimeoutError("cloud-init never reported done")

    monkeypatch.setattr("grain.cli.wait_for_provisioning", never)
    with pytest.raises(TimeoutError):
        main(["--dry-run", "host", "wait", "controller"])
    out = capsys.readouterr().out
    assert "provisioning did not finish" in out
    assert "cloud-init status --long" in out


def test_dry_run_deploy_prints_the_tar_over_ssh_pipeline(capsys):
    out = run(["--dry-run", "host", "deploy"], capsys)
    assert "tar -czf -" in out
    assert "/opt/grain" in out


def test_deploy_rejects_a_sandbox_target():
    with pytest.raises(SystemExit, match="controller"):
        main(["--dry-run", "host", "deploy", "sandbox-0"])


def test_dry_run_controller_configure_writes_automation_json_over_ssh(capsys, tmp_path):
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets"], capsys,
    )
    assert "dd of=/data/config/automation.json" in out
    assert "dd of=/data/config/repo-allowlist.json" in out
    assert "credentials.json" not in out  # no --github-token-file given


def test_controller_configure_restarts_the_git_proxy_so_it_picks_up_the_new_config(capsys):
    """`build_proxy` reads automation.json once, at startup -- a live proxy
    would otherwise keep forwarding to whatever host it started with after
    reconfiguring to a new repo/host.
    """
    out = run(["--dry-run", "controller", "configure", "--repo", "acme/widgets"], capsys)
    assert "systemctl restart grain-git-proxy.service" in out
    # ...and after the config is written, not before.
    assert out.index("dd of=/data/config/automation.json") < out.index("systemctl restart")


def test_dry_run_controller_configure_with_a_github_token_file(capsys, tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("ghp_dryruntoken\n")
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--github-token-file", str(token_file)],
        capsys,
    )
    assert "dd of=/data/secrets/github/bot.token" in out
    assert "dd of=/data/secrets/github/credentials.json" in out
    # The token itself never appears in the printed command line -- it
    # travels as the (also printed, but separately, after <<'EOF') stdin
    # heredoc, not as an argv element.
    for line in out.splitlines():
        if line.startswith("+ ssh") and "dd of=" in line:
            assert "ghp_dryruntoken" not in line


def test_dry_run_controller_configure_with_a_claude_token_file(capsys, tmp_path):
    token_file = tmp_path / "claude-token"
    token_file.write_text("sk-ant-oat01-dryruntoken\n")
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--claude-token-file", str(token_file)],
        capsys,
    )
    assert "dd of=/data/secrets/claude-oauth-token" in out


def test_controller_configure_restarts_the_proxy_after_the_credential_write_too(capsys, tmp_path):
    """Found live: an earlier version restarted the proxy right after
    automation.json, before configure_github_credential got to write
    credentials.json -- so a newly added target repo's credential never
    reached the running proxy, and the very first dispatch against it
    failed with a proxied 500 ("no credential configured"). The restart
    has to come after *both* writes, not just the first.
    """
    token_file = tmp_path / "token"
    token_file.write_text("ghp_dryruntoken\n")
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--github-token-file", str(token_file)],
        capsys,
    )
    assert out.index("dd of=/data/secrets/github/credentials.json") < out.index(
        "systemctl restart"
    )


def test_dry_run_controller_configure_with_a_github_key(capsys, tmp_path):
    key_file = tmp_path / "workflow-token"
    key_file.write_text("ghp_workflowtoken\n")
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--github-key", f"workflow={key_file}"],
        capsys,
    )
    assert "dd of=/data/secrets/github/workflow.token" in out
    # bwsalmon/agents#52: never becomes any repo's default -- no
    # credentials.json write for it (no --github-token-file given either).
    assert "credentials.json" not in out
    for line in out.splitlines():
        if line.startswith("+ ssh") and "dd of=" in line:
            assert "ghp_workflowtoken" not in line


def test_dry_run_controller_configure_with_multiple_github_keys(capsys, tmp_path):
    workflow_file = tmp_path / "workflow-token"
    workflow_file.write_text("ghp_workflow\n")
    release_file = tmp_path / "release-token"
    release_file.write_text("ghp_release\n")
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--github-key", f"workflow={workflow_file}",
         "--github-key", f"release={release_file}"],
        capsys,
    )
    assert "dd of=/data/secrets/github/workflow.token" in out
    assert "dd of=/data/secrets/github/release.token" in out


def test_controller_configure_rejects_a_malformed_github_key():
    with pytest.raises(SystemExit, match="NAME=FILE"):
        main(["--dry-run", "controller", "configure", "--repo", "acme/widgets",
              "--github-key", "not-a-valid-entry"])


def test_controller_configure_rejects_the_reserved_anonymous_github_key_name(tmp_path):
    key_file = tmp_path / "token"
    key_file.write_text("tok")
    with pytest.raises(SystemExit, match="reserved"):
        main(["--dry-run", "controller", "configure", "--repo", "acme/widgets",
              "--github-key", f"anonymous={key_file}"])


def test_dry_run_controller_configure_with_a_gcp_service_account_key_file(capsys, tmp_path):
    key_file = tmp_path / "gcp-key.json"
    key_file.write_text('{"type": "service_account"}\n')
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--gcp-service-account-key-file", str(key_file),
         "--gcp-agent-service-account-email", "grain-agent@acme.iam.gserviceaccount.com",
         "--gcp-project-id", "acme"],
        capsys,
    )
    assert "dd of=/data/secrets/gcp-service-account.json" in out
    assert "dd of=/data/config/gcp-key.json" in out
    for line in out.splitlines():
        if line.startswith("+ ssh") and "dd of=" in line:
            assert "service_account" not in line


def test_controller_configure_gcp_agent_key_alone_needs_no_service_account_key_file(capsys, tmp_path):
    """bwsalmon/agents#126: unlike before, --gcp-agent-service-account-email
    and --gcp-project-id are plain, non-secret config -- naming them
    without --gcp-service-account-key-file at all is not an error, and
    still writes gcp-key.json."""
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--gcp-agent-service-account-email", "grain-agent@acme.iam.gserviceaccount.com",
         "--gcp-project-id", "acme"],
        capsys,
    )
    assert "dd of=/data/config/gcp-key.json" in out
    assert "gcp-service-account.json" not in out


def test_controller_configure_gcp_agent_email_without_project_id_writes_no_gcp_key_config(capsys):
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--gcp-agent-service-account-email", "grain-agent@acme.iam.gserviceaccount.com"],
        capsys,
    )
    assert "gcp-key.json" not in out


def test_dry_run_controller_configure_with_a_gemini_project_id(capsys):
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets",
         "--gemini-project-id", "acme"],
        capsys,
    )
    assert "dd of=/data/config/gemini-key.json" in out


def test_controller_configure_without_gemini_project_id_writes_no_gemini_config(capsys):
    out = run(
        ["--dry-run", "controller", "configure", "--repo", "acme/widgets"],
        capsys,
    )
    assert "gemini-key.json" not in out


def test_repo_without_a_slash_is_rejected():
    with pytest.raises(SystemExit, match="owner/name"):
        main(["--dry-run", "controller", "configure", "--repo", "not-a-repo-slug"])


def test_configure_allowlists_the_target_repos_not_the_task_repo(capsys):
    """The allowlist gates git transport, and no sandbox ever clones the
    task repo -- it is read over the API, which credentials.json covers.
    """
    written = _configure_with(
        ["--task-repo", "acme/tasks",
         "--target-repo", "acme/widgets", "--target-repo", "acme/gadgets"],
        capsys,
    )
    assert json.loads(written["/data/config/repo-allowlist.json"]) == [
        "acme/widgets", "acme/gadgets",
    ]
    automation = json.loads(written["/data/config/automation.json"])
    assert (automation["task_owner"], automation["task_repo"]) == ("acme", "tasks")
    # No default: with several targets named, a task that names none is
    # parked rather than guessed at.
    assert automation["default_target_repo"] is None


def test_configure_with_no_target_repo_keeps_the_single_repo_shape(capsys):
    """The migration path: a deployment whose task repo *is* its code gets
    that repo as its only target and as the default, so no issue needs a
    `/repo` line.
    """
    written = _configure_with(["--task-repo", "acme/widgets"], capsys)
    assert json.loads(written["/data/config/repo-allowlist.json"]) == ["acme/widgets"]
    automation = json.loads(written["/data/config/automation.json"])
    assert automation["default_target_repo"] == "acme/widgets"


def test_a_default_target_outside_the_target_list_is_rejected():
    with pytest.raises(SystemExit, match="not one of"):
        main(["--dry-run", "controller", "configure", "--task-repo", "acme/tasks",
              "--target-repo", "acme/widgets",
              "--default-target-repo", "acme/elsewhere"])


def test_the_credential_mapping_covers_the_task_repo_and_every_target(tmp_path, capsys):
    token_file = tmp_path / "token"
    token_file.write_text("ghp_x\n")
    written = _configure_with(
        ["--task-repo", "acme/tasks", "--target-repo", "acme/widgets",
         "--github-token-file", str(token_file)],
        capsys,
    )
    assert json.loads(written["/data/secrets/github/credentials.json"]) == {
        "acme/tasks": "bot", "acme/widgets": "bot",
    }


def _configure_with(extra_args: list[str], capsys) -> dict[str, str]:
    """Runs `controller configure --dry-run` and returns `{remote path:
    content}` for every file it would write -- `DryRunRunner` echoes each
    command plus its stdin heredoc, and every remote file this command
    writes goes through `dd of=<path>` fed over stdin.
    """
    out = run(["--dry-run", "controller", "configure", *extra_args], capsys)
    written: dict[str, str] = {}
    path: str | None = None
    lines = out.splitlines()
    for index, line in enumerate(lines):
        if line.startswith("+ ") and "dd of=" in line and line.endswith("<<'EOF'"):
            path = line.split("dd of=", 1)[1].split()[0]
            body: list[str] = []
            for content in lines[index + 1:]:
                if content == "EOF":
                    break
                body.append(content)
            written[path] = "\n".join(body)
    return written



def test_dry_run_bootstrap_runs_every_stage_without_touching_a_real_vm(tmp_path, capsys):
    out = run(
        ["--dry-run", "--sandboxes", "1", "--config-dir", str(tmp_path / "instances"),
         "--admin-ssh-public-key", str(tmp_path / "admin-ssh.pub"),
         "--controller-ssh-public-key", str(tmp_path / "controller-ssh.pub"),
         "host", "bootstrap", "--repo", "acme/widgets",
         "--admin-ssh-private-key", str(tmp_path / "admin-ssh")],
        capsys,
    )
    assert "+ ssh-keygen -t ed25519" in out
    assert "+ virsh -c qemu:///system define" in out
    assert "grain-git-proxy.service" in out
    assert "grain-automation.timer" in out


def test_dry_run_bootstrap_with_a_gcp_service_account_key_and_agent_email(
    tmp_path, capsys,
):
    key_file = tmp_path / "gcp-key.json"
    key_file.write_text('{"type": "service_account"}\n')
    out = run(
        ["--dry-run", "--sandboxes", "1", "--config-dir", str(tmp_path / "instances"),
         "--admin-ssh-public-key", str(tmp_path / "admin-ssh.pub"),
         "--controller-ssh-public-key", str(tmp_path / "controller-ssh.pub"),
         "host", "bootstrap", "--repo", "acme/widgets",
         "--admin-ssh-private-key", str(tmp_path / "admin-ssh"),
         "--gcp-service-account-key-file", str(key_file),
         "--gcp-agent-service-account-email", "grain-agent@acme.iam.gserviceaccount.com",
         "--gcp-project-id", "acme"],
        capsys,
    )
    assert "dd of=/data/secrets/gcp-service-account.json" in out
    assert "dd of=/data/config/gcp-key.json" in out
    # bwsalmon/agents#126: no per-sandbox metadata server to start anymore.
    assert "metadata start" not in out


def test_sandbox_login_dry_run_prints_the_ssh_command_and_does_not_exec(capsys):
    out = run(["--dry-run", "--sandboxes", "2", "sandbox", "login", "sandbox-0"], capsys)
    assert out.startswith("+ ssh")
    assert "10.100.0.10" in out


def test_sandbox_login_rejects_an_unknown_name():
    with pytest.raises(SystemExit, match="unknown VM"):
        main(["--dry-run", "sandbox", "login", "not-a-real-vm"])


def test_sandbox_login_can_reach_the_controller_too(capsys):
    out = run(["--dry-run", "sandbox", "login", "controller"], capsys)
    assert "10.100.0.2" in out


def test_sandbox_login_without_dry_run_execs_ssh_replacing_this_process(monkeypatch):
    """Without `--dry-run`, `cmd_sandbox_login` never returns -- it replaces
    the current process with an interactive `ssh` session
    (`os.execvp`). `os.execvp` itself is stubbed out here so the test
    process survives; what's under test is that the right argv reaches it.
    """
    captured = {}

    def fake_execvp(file, argv):
        captured["file"] = file
        captured["argv"] = argv

    monkeypatch.setattr("os.execvp", fake_execvp)
    assert main(["--sandboxes", "2", "sandbox", "login", "sandbox-0"]) is None
    assert captured["file"] == "ssh"
    assert captured["argv"][0] == "ssh"
    assert captured["argv"][-1] == "debian@10.100.0.10"


def test_missing_tool_raises_a_legible_error_when_check_is_on():
    from grain.run import CommandError, RealRunner

    try:
        RealRunner().run(["definitely-not-a-real-binary"])
    except CommandError as exc:
        assert "not found on PATH" in str(exc)
        assert exc.returncode == 127
    else:
        raise AssertionError("expected CommandError")


def test_recreate_controller_is_refused_without_the_data_loss_flag():
    """/data has no disk of its own yet, so recreating the controller
    silently destroys every credential and all automation state. This
    should be blocked before the adapter is even built.
    """
    with pytest.raises(SystemExit, match="destroys /data"):
        main(["--dry-run", "host", "recreate", "controller"])


def test_recreate_all_is_refused_too_since_it_includes_the_controller():
    with pytest.raises(SystemExit, match="destroys /data"):
        main(["--dry-run", "host", "recreate", "all"])


def test_recreate_sandboxes_needs_no_flag(tmp_path, capsys):
    out = run(
        ["--dry-run", "--config-dir", str(tmp_path / "instances"),
         "host", "recreate", "sandboxes"],
        capsys,
    )
    assert "sandbox-0" in out and "sandbox-1" in out


def test_recreate_controller_proceeds_with_the_flag(tmp_path, capsys):
    out = run(
        ["--dry-run", "--config-dir", str(tmp_path / "instances"),
         "host", "recreate", "controller", "--i-know-this-deletes-data"],
        capsys,
    )
    assert "controller" in out
