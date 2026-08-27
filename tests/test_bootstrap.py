"""Unit tests for grain/bootstrap.py's sequencer -- the "no hypervisor
needed" half of docs/bootstrap.md's testing table. Every VM lifecycle call
goes through a real `LibvirtAdapter` backed by a `FakeRunner`, exactly like
`tests/test_libvirt.py` -- there is no separate fake-adapter class, since
`LibvirtAdapter` itself becomes a fake once its `Runner` is fake.
"""

from __future__ import annotations

import shlex
from pathlib import Path

import pytest

from grain.adapter.libvirt import LibvirtAdapter
from grain.adapter.net_linux import BOOT_UNIT_NAME, BOOT_UNIT_PATH, LinuxNetwork
from grain.bootstrap import BootstrapConfig, bootstrap
from grain.inventory import Cluster
from grain.run import CommandError, FakeRunner


def virsh_list(*rows: tuple[str, str]) -> str:
    lines = [" Id   Name       State", "----------------------------"]
    for i, (name, state) in enumerate(rows):
        lines.append(f" {i:<4} {name:<10} {state}")
    return "\n".join(lines) + "\n"


def ssh_prefix(user: str, address: str, key_path: Path) -> str:
    """The exact prefix `SshRunner` builds up to and including the target
    -- everything before `--`. `FakeRunner.expect` matches the *longest*
    matching prefix, so per-target responses (controller vs. a sandbox)
    don't collide with each other.
    """
    return shlex.join([
        "ssh", "-i", str(key_path),
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "IdentityAgent=none",
        "-o", "ConnectTimeout=10",
        f"{user}@{address}",
    ])


@pytest.fixture
def cluster() -> Cluster:
    return Cluster(sandbox_count=1)


@pytest.fixture
def env(cluster: Cluster, tmp_path: Path):
    runner = FakeRunner()
    network = LinuxNetwork(cluster, runner)
    admin_key = tmp_path / "admin-ssh.pub"
    controller_key = tmp_path / "controller-ssh.pub"
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=tmp_path / "instances",
        admin_public_key_path=admin_key, controller_public_key_path=controller_key,
    )
    admin_private = tmp_path / "admin-ssh"
    config = BootstrapConfig(
        task_repo="acme/widgets",
        admin_private_key_path=admin_private,
    )
    return adapter, runner, cluster, config, admin_private


def prime_happy_path(runner: FakeRunner, cluster: Cluster, admin_private: Path,
                      *, controller_state: tuple[str, str] | None = None,
                      sandbox_states: list[tuple[str, str]] = ()) -> None:
    """Scripts every `virsh`/`cat` response for a clean, fully-converging
    run: every VM absent (created fresh), the controller reports its own
    generated key back, cloud-init and SSH always succeed.
    """
    rows = []
    if controller_state:
        rows.append(controller_state)
    rows.extend(sandbox_states)
    runner.expect("virsh -c qemu:///system list --all", stdout=virsh_list(*rows))
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    runner.expect(
        f"{controller_prefix} -- {shlex.quote('cat /data/secrets/controller-ssh.pub')}",
        stdout="ssh-ed25519 AAAAcontrollerkey\n",
    )


def test_stage_order_controller_key_read_before_any_sandbox_is_created(env):
    """The regression docs/bootstrap.md calls out as the one that matters
    most: creating a sandbox before the controller key is known would embed
    no controller key into it at all.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)

    read_index = next(
        i for i, c in enumerate(runner.commands)
        if c.startswith("ssh") and "cat /data/secrets/controller-ssh.pub" in c
    )
    create_index = next(
        i for i, c in enumerate(runner.commands)
        if c.startswith(f"virsh -c qemu:///system define") and "sandbox-0" in c
    )
    assert read_index < create_index


def test_bootstrap_creates_the_controller_then_every_sandbox(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert runner.ran("virsh -c qemu:///system define")
    defines = [c for c in runner.commands if "virsh -c qemu:///system define" in c]
    assert any("controller.xml" in c for c in defines)
    assert any("sandbox-0.xml" in c for c in defines)


def test_bootstrap_persists_the_network_policy_across_a_reboot(env):
    """Regression test for bwsalmon/agents#119.

    `host up --persist` installs `grain-network.service` so the bridge and
    nftables policy (sandbox-to-sandbox isolation included) survive a host
    reboot -- but until now that persistence only happened on the manual
    CLI path, never on `grain host bootstrap`'s stage 3. A host set up
    purely by `host bootstrap` looked fully healthy (SSH and a direct
    connection to the controller both kept working) right up until it
    rebooted, at which point that isolation silently vanished, with no
    error anywhere to explain why.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert any(argv[:1] == ["tee"] and argv[1] == BOOT_UNIT_PATH for argv, _ in runner.calls), (
        "bootstrap must install grain-network.service, or the network "
        "policy it just applied vanishes on the host's next reboot"
    )
    assert runner.ran(f"systemctl enable {BOOT_UNIT_NAME}")


def test_bootstrap_writes_the_controller_key_read_back_to_the_host_path(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert adapter.controller_public_key_path.read_text() == "ssh-ed25519 AAAAcontrollerkey\n"


def test_a_recreated_controllers_changed_host_key_logs_a_repair_hint(env):
    """A controller key that changed from a *previously recorded* one --
    as opposed to `test_bootstrap_writes_the_controller_key_read_back_...`,
    where there was no previous key to compare against -- means the
    controller was recreated (docs/bootstrap.md, "Repairing a recreated
    controller"): every sandbox still trusts the old key, and stage 9
    fixes that. This only logs the hint; stage 9 itself is out of scope
    here.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    adapter.controller_public_key_path.parent.mkdir(parents=True, exist_ok=True)
    adapter.controller_public_key_path.write_text("ssh-ed25519 AAAAoldkey\n")
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    runner.expect(
        f"{controller_prefix} -- {shlex.quote('cat /data/secrets/controller-ssh.pub')}",
        stdout="ssh-ed25519 AAAAnewkey\n",
    )
    lines: list[str] = []
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner,
               config=config, log=lines.append)
    assert adapter.controller_public_key_path.read_text() == "ssh-ed25519 AAAAnewkey\n"
    assert any("controller key changed" in line for line in lines)


def test_bootstrap_generates_an_admin_key_when_absent(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    assert not adapter.admin_public_key_path.exists()
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert runner.ran(f"ssh-keygen -t ed25519 -f {admin_private}")


def test_bootstrap_skips_keygen_when_an_admin_key_already_exists(env):
    adapter, runner, cluster, config, admin_private = env
    adapter.admin_public_key_path.parent.mkdir(parents=True, exist_ok=True)
    adapter.admin_public_key_path.write_text("ssh-ed25519 AAAAexisting\n")
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not runner.ran("ssh-keygen")


def test_skip_if_present_creates_nothing_when_everything_already_runs(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not runner.ran("virsh -c qemu:///system define")
    assert not runner.ran("virsh -c qemu:///system start")


def test_a_stopped_controller_is_started_not_recreated(env):
    """`_ensure_started`'s third state: merely stopped (as opposed to
    absent, or already running) -- it should be started in place, never
    destroyed and recreated.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "shut off"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not runner.ran("virsh -c qemu:///system define")
    assert any(
        c.startswith("virsh -c qemu:///system start") and "controller" in c
        for c in runner.commands
    )


def test_deploy_runs_against_the_controller_every_time(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert any(c.startswith("bash -c") and "tar -czf -" in c for c in runner.commands)


def test_configure_writes_automation_json_for_the_given_repo(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert any("dd of=/data/config/automation.json" in c for c in runner.commands)


def test_configure_writes_cluster_toml_reflecting_the_real_sandbox_count(env):
    """The controller has no `--cluster-file` of its own otherwise
    (`grain/automation/configure.py`'s `configure_cluster` docstring) --
    without this, `grain-automation.service` would silently dispatch
    against `Cluster()`'s bare default of two sandboxes forever, no matter
    what this deployment's real `sandbox_count` is.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert any("dd of=/data/config/cluster.toml" in c for c in runner.commands)


def test_github_token_is_only_configured_when_supplied(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("credentials.json" in c for c in runner.commands)

    runner2 = FakeRunner()
    network2 = LinuxNetwork(cluster, runner2)
    adapter2 = LibvirtAdapter(
        cluster, runner2, network2, config_dir=adapter.config_dir,
        admin_public_key_path=adapter.admin_public_key_path,
        controller_public_key_path=adapter.controller_public_key_path,
    )
    prime_happy_path(
        runner2, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config2 = BootstrapConfig(
        task_repo="acme/widgets", github_token="ghp_tok",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter2, base_runner=runner2, config=config2)
    assert any("credentials.json" in c for c in runner2.commands)


def test_named_github_keys_are_only_configured_when_supplied(env):
    """Mirrors test_github_token_is_only_configured_when_supplied --
    bwsalmon/agents#134 threads BootstrapConfig.github_keys through the
    same stage 8, so a bare re-run with none must not write any, and a run
    that supplies some must write each one's token file but never touch
    credentials.json (configure_named_github_key's whole point).
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("workflow.token" in c for c in runner.commands)

    runner2 = FakeRunner()
    network2 = LinuxNetwork(cluster, runner2)
    adapter2 = LibvirtAdapter(
        cluster, runner2, network2, config_dir=adapter.config_dir,
        admin_public_key_path=adapter.admin_public_key_path,
        controller_public_key_path=adapter.controller_public_key_path,
    )
    prime_happy_path(
        runner2, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config2 = BootstrapConfig(
        task_repo="acme/widgets",
        github_keys={"workflow": "ghp_workflow", "release": "ghp_release"},
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter2, base_runner=runner2, config=config2)
    assert any("workflow.token" in c for c in runner2.commands)
    assert any("release.token" in c for c in runner2.commands)
    assert not any("credentials.json" in c for c in runner2.commands)


def test_enable_runs_after_sandboxes_are_up(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    enable_index = next(
        i for i, c in enumerate(runner.commands) if "grain-git-proxy.service" in c
    )
    sandbox_create_index = next(
        i for i, c in enumerate(runner.commands)
        if "virsh -c qemu:///system define" in c and "sandbox-0" in c
    )
    assert sandbox_create_index < enable_index


def test_stage_10_restarts_the_proxy_not_just_enables_it(env):
    """`enable --now` is a no-op on an already-running proxy -- exactly the
    re-bootstrap-to-add-a-sandbox case (docs/next-session.md, found live):
    the new sandbox's token, minted just above by `ensure_sandbox_tokens`,
    would otherwise be invisible to a proxy process that was already
    running before this bootstrap call started. `restart` is what actually
    picks it up, whether the proxy was already active or not.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert any("systemctl enable grain-git-proxy.service" in c for c in runner.commands)
    assert any("systemctl restart grain-git-proxy.service" in c for c in runner.commands)


def test_resume_after_a_mid_chain_failure_skips_already_converged_stages(env):
    """A failure injected at deploy (stage 7): re-running from the top must
    not recreate the controller, regenerate the admin key, or recreate any
    sandbox -- all already converged before the injected failure point.
    """
    adapter, runner, cluster, config, admin_private = env
    # Converge stage 2 (admin key) before either attempt, so only the
    # deploy-stage failure is under test here -- stage order is covered by
    # its own test above.
    adapter.admin_public_key_path.parent.mkdir(parents=True, exist_ok=True)
    adapter.admin_public_key_path.write_text("ssh-ed25519 AAAApreexisting\n")
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    runner.expect("bash -c", returncode=1, stderr="deploy failed")
    with pytest.raises(CommandError):
        bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not runner.ran("ssh-keygen")  # already converged before the failure

    runner.responses.pop("bash -c")
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    # Neither pass ever created anything -- both VMs report already RUNNING.
    assert not runner.ran("virsh -c qemu:///system define")
    assert not runner.ran("ssh-keygen")


def test_claude_credentials_reach_the_controllers_grain_agent_account_not_any_sandbox(env):
    # docs/roadmap.md item 8's "Update": claude -p runs on the controller
    # now, as the dedicated grain-agent account -- no sandbox gets any
    # Claude credential at all anymore.
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets", claude_token="sk-ant-oat01-fake",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    controller_dd_calls = [
        stdin for argv, stdin in runner.calls
        if argv and argv[0] == "ssh" and "dd" in argv[-1]
        and "/home/grain-agent/.claude-oauth-token" in argv[-1]
        and shlex.join(argv).startswith(controller_prefix)
    ]
    assert controller_dd_calls == ["sk-ant-oat01-fake"]

    sandbox_prefix = ssh_prefix("debian", str(cluster.address_of("sandbox-0")), admin_private)
    sandbox_claude_calls = [
        argv for argv, _ in runner.calls
        if argv and argv[0] == "ssh" and "claude" in argv[-1].lower()
        and shlex.join(argv).startswith(sandbox_prefix)
    ]
    assert sandbox_claude_calls == []


def test_gcp_service_account_is_only_configured_when_supplied(env):
    """Mirrors test_github_token_is_only_configured_when_supplied -- a bare
    re-run with no key must not try to place one.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("gcp-service-account.json" in c for c in runner.commands)


def test_no_gcp_key_config_is_written_without_the_minter_credential(env):
    """Found live (bwsalmon/agents#131): /data/config/gcp-key.json is the
    on/off switch `build_orchestrator` reads, and both minting and reaping
    authenticate as the key it names. deploy.sh's fetch of that key is
    best-effort (SECRET_WAIT_OPTIONAL), so a rollout that raced ahead of
    CI's push step used to write the switch with no credential behind it --
    turning the feature on in a state where every cycle failed on a missing
    key file, rather than leaving it off until the key arrived.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        gcp_agent_service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        gcp_project_id="acme",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not [c for c in runner.commands if "/data/config/gcp-key.json" in c], \
        "turned minting on without placing the credential it authenticates as"


def test_gcp_agent_key_config_reaches_the_controller(env):
    """bwsalmon/agents#126: independent of gcp_service_account_key above
    -- plain, non-secret config naming the agent account grain mints a
    fresh key for on every dispatch, written to
    /data/config/gcp-key.json."""
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        gcp_agent_service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        gcp_project_id="acme",
        # bwsalmon/agents#131: writing gcp-key.json is what turns minting on,
        # and minting authenticates as this key -- so the switch is only
        # written once the credential it names has been placed.
        gcp_key_minter_key='{"type": "service_account"}\n',
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)

    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    assert any(
        c.startswith(controller_prefix) and "gcp-key.json" in c
        for c in runner.commands
    )


def test_gemini_project_id_writes_gemini_key_config_on_the_controller(env):
    """bwsalmon/agents#49: terraform's enable_gemini_key flows through
    grain-config's gemini_project_id into `host bootstrap`, which must
    place /data/config/gemini-key.json the same way `grain controller
    configure --gemini-project-id` already does by hand.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        gcp_key_minter_key='{"type": "service_account"}',
        gcp_agent_service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        gcp_project_id="acme",
        gemini_project_id="acme",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)

    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    assert any(
        c.startswith(controller_prefix) and "gemini-key.json" in c
        for c in runner.commands
    )


def test_no_gemini_project_id_never_writes_gemini_key_config(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("gemini-key.json" in c for c in runner.commands)


def test_bootstrap_skips_the_agent_gcp_key_config_when_only_the_email_is_given(env):
    """Unlike before bwsalmon/agents#126, this is not a hard error: naming
    only one of the pair just leaves the feature off, the same "unusable
    config parks/skips" latitude every other optional bootstrap step
    already has (see BootstrapConfig's own docstring comment)."""
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        gcp_agent_service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("gcp-key.json" in c for c in runner.commands)


def test_janitor_ttl_hours_writes_janitor_config_on_the_controller(env):
    """bwsalmon/agents#113: terraform's enable_janitor flows through
    grain-config's janitor_ttl_hours into `host bootstrap`, which must
    place /data/config/janitor.json the same way `grain controller
    configure --janitor-ttl-hours` already does by hand.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        gcp_key_minter_key='{"type": "service_account"}',
        gcp_agent_service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        gcp_project_id="acme",
        janitor_ttl_hours=12,
        janitor_name_prefix="acme",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)

    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    assert any(
        c.startswith(controller_prefix) and "janitor.json" in c
        for c in runner.commands
    )


def test_janitor_ttl_hours_without_gcp_project_id_raises(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        janitor_ttl_hours=12,
        admin_private_key_path=admin_private,
    )
    with pytest.raises(ValueError):
        bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)


def test_no_janitor_ttl_hours_never_writes_janitor_config(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("janitor.json" in c for c in runner.commands)


# --- scratch-repo.json (bwsalmon/agents#159, #186) -------------------------

def test_scratch_repo_config_reaches_the_controller(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        task_repo="acme/widgets",
        scratch_repo_owner="acme",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)

    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    assert any(
        c.startswith(controller_prefix) and "scratch-repo.json" in c
        for c in runner.commands
    )


def test_no_scratch_repo_owner_never_writes_scratch_repo_config(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert not any("scratch-repo.json" in c for c in runner.commands)


# --- stage 5/9: what a failed boot wait actually tells the operator -------
#
# Reported live as "it fails on wait for controller (5/11) and prints
# storage info": the wait itself said nothing about why, and on GCP the only
# thing printed after it was deploy.sh's storage-ownership dump, which is
# about an entirely different failure. See grain/adapter/diagnostics.py.

def failing_config(admin_private: Path, **kwargs) -> BootstrapConfig:
    """`ssh_timeout=0` makes `wait_for_ssh` give up on the first refusal
    rather than sleeping through a real 180s budget.
    """
    return BootstrapConfig(
        task_repo="acme/widgets", admin_private_key_path=admin_private,
        ssh_timeout=0, **kwargs,
    )


def test_a_controller_that_never_answers_ssh_dumps_host_diagnostics(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    runner.expect(f"{controller_prefix} -- true", returncode=255,
                   stderr="Connection refused")
    lines: list[str] = []

    with pytest.raises(TimeoutError):
        bootstrap(cluster=cluster, adapter=adapter, base_runner=runner,
                   config=failing_config(admin_private), log=lines.append)

    assert runner.ran("virsh -c qemu:///system dominfo controller")
    assert runner.ran("ping -c 2 -W 2 10.100.0.2")
    assert any("serial console" in line for line in lines)


def test_the_wait_stage_names_the_address_and_the_timeout_in_play(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    lines: list[str] = []
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner,
               config=config, log=lines.append)
    stage5 = next(line for line in lines if line.startswith("stage 5/11"))
    assert "10.100.0.2" in stage5 and "180s" in stage5


def test_a_fresh_controller_whose_provisioning_failed_dumps_guest_diagnostics(env):
    """cloud-init reports `status: error` and nothing else; the reason is in
    the guest's own logs, which is where provision/controller.sh's `set -eux`
    output (an apt or egress failure, say) actually lands.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    runner.expect(
        f"{controller_prefix} -- "
        f"{shlex.quote('sudo timeout 900 cloud-init status --wait')}",
        returncode=1, stdout="status: error",
    )
    lines: list[str] = []

    with pytest.raises(RuntimeError, match="cloud-init did not finish cleanly"):
        bootstrap(cluster=cluster, adapter=adapter, base_runner=runner,
                   config=failing_config(admin_private), log=lines.append)

    assert any("cloud-init status --long" in c for c in runner.commands)
    assert any("cloud-init-output.log" in c for c in runner.commands)


def test_an_already_running_vm_is_still_checked_for_provisioning(env):
    """Found the hard way on the *re-run* after a failed provision: `fresh`
    is false the second time, so the check used to be skipped entirely and
    the run failed at stage 6 with `cat:
    /data/secrets/controller-ssh.pub: No such file or directory` instead.
    A warning, not a failure -- an already-running VM may have been
    provisioned by any earlier run, and whatever really needs it fails on
    its own if this mattered.
    """
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    controller_prefix = ssh_prefix("debian", str(cluster.controller_ip), admin_private)
    runner.expect(
        f"{controller_prefix} -- "
        f"{shlex.quote('sudo timeout 900 cloud-init status --wait')}",
        returncode=1, stdout="status: error",
    )
    lines: list[str] = []

    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner,
               config=config, log=lines.append)  # must not raise

    assert any(line.startswith("WARNING") and "cloud-init" in line for line in lines)
    assert any("cloud-init status --long" in c for c in runner.commands)
