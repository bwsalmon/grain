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
from grain.adapter.net_linux import LinuxNetwork
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
        owner="acme", repo="widgets",
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


def test_bootstrap_writes_the_controller_key_read_back_to_the_host_path(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(runner, cluster, admin_private)
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    assert adapter.controller_public_key_path.read_text() == "ssh-ed25519 AAAAcontrollerkey\n"


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
        owner="acme", repo="widgets", github_token="ghp_tok",
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter2, base_runner=runner2, config=config2)
    assert any("credentials.json" in c for c in runner2.commands)


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


def test_claude_credentials_are_injected_into_every_sandbox(env):
    adapter, runner, cluster, config, admin_private = env
    prime_happy_path(
        runner, cluster, admin_private,
        controller_state=("controller", "running"),
        sandbox_states=[("sandbox-0", "running")],
    )
    config = BootstrapConfig(
        owner="acme", repo="widgets", claude_credentials='{"accessToken": "x"}',
        admin_private_key_path=admin_private,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=runner, config=config)
    sandbox_prefix = ssh_prefix("debian", str(cluster.address_of("sandbox-0")), admin_private)
    dd_calls = [
        stdin for argv, stdin in runner.calls
        if argv and argv[0] == "ssh"
        and argv[-1] == "dd of=/home/debian/.claude/.credentials.json status=none"
        and shlex.join(argv).startswith(sandbox_prefix)
    ]
    assert dd_calls == ['{"accessToken": "x"}']
