"""`grain host bootstrap` -- docs/bootstrap.md Phase 4, the sequencer that
collapses docs/runbook.md's fourteen-step first-time-setup checklist to one
command plus one interactive Claude Code login per sandbox pool (not per
sandbox -- see "The Claude credential" in docs/bootstrap.md).

**No state file.** Every stage converges from observed reality --
`adapter.state()`, key-file presence on the host, a live SSH read-back --
the same "having them guess would be worse" argument `adapter/base.py`
already makes for `state()`/`list_vms()` existing at all. That is what makes
a re-run after a failure at any stage resume cleanly instead of exploding:
stages before the failure observe themselves as already converged and do
nothing, and `create()`'s own "must fail rather than silently adopt an
existing one" contract (`adapter/base.py`) is respected by checking
`state()` first rather than calling `create()` speculatively and swallowing
the error.

Stage order matters and is deliberately not reorderable: the controller's
own SSH key (stage 6) has to be read back *before* any sandbox is created
(stage 9), because sandbox creation is what embeds it as an authorized key.
Getting this backwards is docs/bootstrap.md's "the regression that matters
most" -- see `tests/test_bootstrap.py`.
"""

from __future__ import annotations

import shlex
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from .adapter.base import VmState
from .adapter.deploy import deploy_tree
from .adapter.libvirt import LibvirtAdapter
from .adapter.wait import wait_for_provisioning, wait_for_ssh
from .automation.configure import (
    configure_claude_token, configure_github_credential, configure_repo,
    credential_repos,
    ensure_sandbox_tokens,
)
from .automation.ssh import SshRunner
from .inventory import Cluster, VmSpec
from .run import Runner

_REPO_ROOT = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class BootstrapConfig:
    # `"owner/name"` for the task repo -- the one queue polled for labelled
    # issues -- and for each target repo those tasks may dispatch into (see
    # `grain/automation/configure.py`'s `configure_repo`). `targets` empty
    # means "the task repo is also the code," the single-repo shape every
    # deployment had before the task/target split: it becomes the sole
    # allow-listed target *and* the default for a task with no `/repo`
    # directive, so such a deployment keeps working with none written.
    task_repo: str
    targets: tuple[str, ...] = ()
    default_target_repo: str | None = None
    github_token: str | None = None
    credential_name: str = "bot"
    claude_token: str | None = None
    github_host: str = "api.github.com"
    git_forward_host: str = "github.com"
    github_use_tls: bool = True
    ssh_user: str = "debian"
    admin_private_key_path: Path = Path("/var/lib/grain/admin-ssh")
    controller_provision_script: str | None = None
    sandbox_provision_script: str | None = None
    ssh_timeout: float = 180.0


Logger = Callable[[str], None]


def _default_log(message: str) -> None:
    print(f"[bootstrap] {message}")


def _ensure_admin_key(admin_public_key_path: Path, admin_private_key_path: Path,
                       runner: Runner, log: Logger) -> None:
    """Stage 2. Supplying your own key (pointing `--admin-ssh-public-key` at
    it) stays the better habit; generating one is what makes a bare host
    bootstrap with no prior setup at all.
    """
    if admin_public_key_path.exists():
        return
    admin_private_key_path.parent.mkdir(parents=True, exist_ok=True)
    admin_public_key_path.parent.mkdir(parents=True, exist_ok=True)
    runner.run([
        "ssh-keygen", "-t", "ed25519", "-f", str(admin_private_key_path),
        "-N", "", "-q",
    ])
    log(f"generated a new admin keypair at {admin_private_key_path}"
        f"{{,.pub}} -- back this up, it is the only way in if the "
        f"controller's own key is ever lost")


def _ensure_started(adapter: LibvirtAdapter, name: str, spec: VmSpec,
                     provision_script: str | None) -> bool:
    """Converges one VM to RUNNING. Returns whether this call actually
    created it (a fresh boot needs `wait_for_provisioning`; a VM that was
    merely stopped, or already running, does not).
    """
    state = adapter.state(name)
    if state is VmState.ABSENT:
        adapter.create(spec, provision_script)
        adapter.start(name)
        return True
    if state is VmState.STOPPED:
        adapter.start(name)
    return False


def bootstrap(*, cluster: Cluster, adapter: LibvirtAdapter, base_runner: Runner,
              config: BootstrapConfig, log: Logger = _default_log) -> None:
    # Stage 1: preflight. Deliberately thin -- /dev/kvm, required binaries,
    # and a present base image are exactly what `adapter.create()` and
    # `runner.run()` already fail loudly on; a base-image fetch-on-miss is
    # named in docs/bootstrap.md's "Deferred" section as future work, not
    # silently assumed here.
    log("stage 1/11: preflight")

    # Stage 2: admin key.
    log("stage 2/11: admin key")
    _ensure_admin_key(
        adapter.admin_public_key_path, config.admin_private_key_path, base_runner, log,
    )

    # Stage 3: network.
    log("stage 3/11: network")
    adapter.network_up()

    # Stage 4: controller.
    log("stage 4/11: controller")
    controller_name = cluster.controller_name
    controller_fresh = _ensure_started(
        adapter, controller_name, cluster.spec_of(controller_name),
        config.controller_provision_script,
    )

    # Stage 5: wait.
    log("stage 5/11: wait for the controller")
    admin_ssh = SshRunner(
        inner=base_runner, user=config.ssh_user,
        address=cluster.address_of(controller_name),
        key_path=config.admin_private_key_path,
    )
    wait_for_ssh(admin_ssh, timeout=config.ssh_timeout)
    if controller_fresh:
        wait_for_provisioning(admin_ssh)

    # Stage 6: controller key. Only reachable now because stage 2's admin
    # key is trusted by the controller too (docs/bootstrap.md's "key roles"
    # fix) -- before that fix there was no non-interactive way to read this
    # back at all. Writing it here, before stage 9, is what lets a fresh
    # sandbox trust it; detecting a *change* is what lets stage 9 repair an
    # already-existing sandbox after a controller recreate (docs/bootstrap.md,
    # "Repairing a recreated controller").
    log("stage 6/11: read back the controller's own key")
    result = admin_ssh.run(["cat", "/data/secrets/controller-ssh.pub"])
    controller_key = result.stdout
    previous_controller_key = (
        adapter.controller_public_key_path.read_text()
        if adapter.controller_public_key_path.exists() else None
    )
    controller_key_changed = previous_controller_key != controller_key
    if controller_key_changed:
        adapter.controller_public_key_path.parent.mkdir(parents=True, exist_ok=True)
        adapter.controller_public_key_path.write_text(controller_key)
        if previous_controller_key is not None:
            log("controller key changed since the last run -- repairing "
                "existing sandboxes in stage 9")

    # Stage 7: deploy. Unconditional -- it's a sync, not a create, so
    # skip-if-present does not apply here.
    log("stage 7/11: deploy this tree to the controller")
    deploy_tree(
        base_runner, _REPO_ROOT, user=config.ssh_user,
        address=cluster.address_of(controller_name),
        key_path=config.admin_private_key_path,
    )

    # Stage 8: configure. Token/Claude-credential only if one was supplied
    # -- a bare re-run with neither should not clobber what's already there.
    log("stage 8/11: configure /data")
    targets = list(config.targets) or [config.task_repo]
    default_target = config.default_target_repo or (
        config.task_repo if not config.targets else None
    )
    configure_repo(admin_ssh, config.task_repo, targets,
                    default_target_repo=default_target,
                    github_host=config.github_host, git_forward_host=config.git_forward_host,
                    github_use_tls=config.github_use_tls)
    if config.github_token:
        configure_github_credential(
            admin_ssh, credential_repos(config.task_repo, targets), config.github_token,
            credential_name=config.credential_name,
        )
    if config.claude_token:
        configure_claude_token(admin_ssh, config.claude_token)

    # Stage 9: sandboxes.
    log("stage 9/11: sandboxes")
    for name in cluster.sandbox_names:
        spec = cluster.spec_of(name)
        sandbox_fresh = _ensure_started(adapter, name, spec, config.sandbox_provision_script)
        sandbox_ssh = SshRunner(
            inner=base_runner, user=config.ssh_user,
            address=cluster.address_of(name), key_path=config.admin_private_key_path,
        )
        wait_for_ssh(sandbox_ssh, timeout=config.ssh_timeout)
        if sandbox_fresh:
            wait_for_provisioning(sandbox_ssh)
        elif controller_key_changed:
            # Repair over the admin key, the only second way in
            # (docs/bootstrap.md's Phase 1 fix) -- append rather than
            # recreate, since recreating would also throw away everything
            # sequential-task state the design already accepts sandboxes
            # keep (docs/design.md, "sandbox lifecycle").
            trimmed = controller_key.strip()
            sandbox_ssh.run([
                "bash", "-c",
                f"grep -qxF {shlex.quote(trimmed)} ~/.ssh/authorized_keys "
                f"2>/dev/null || echo {shlex.quote(trimmed)} >> ~/.ssh/authorized_keys",
            ])
        # No Claude credential is ever placed on a sandbox (docs/roadmap.md
        # item 8's "Update") -- `claude -p` runs on the controller now, and
        # stage 8's `configure_claude_token` call already places both the
        # controller's own reference copy and the live copy the
        # controller-side `grain-agent` account reads into its environment
        # at dispatch time. A sandbox holds nothing worth protecting anymore.

    # Mint every sandbox's git-proxy token before the proxy itself starts
    # (stage 10) -- see `ensure_sandbox_tokens`'s own docstring for the
    # live-found bug this closes: the proxy loads sandbox-tokens.json once
    # at startup, so a token minted only lazily on first dispatch (the
    # normal `dispatch.py` path) would otherwise make that very first
    # dispatch fail authentication against a proxy that started before the
    # token existed.
    ensure_sandbox_tokens(admin_ssh, list(cluster.sandbox_names))

    # Stage 10: enable. `restart`, not `enable --now`, for the proxy
    # specifically -- `--now` only starts a not-yet-running unit; on a
    # controller re-bootstrapped to add a sandbox (the proxy is already
    # active from the first run), it is a no-op, and the already-running
    # process never re-reads `sandbox-tokens.json` on its own (same
    # once-at-startup caching `configure.py`'s docstring already flags for
    # `automation.json`'s `git_forward_host`). Found live
    # (docs/next-session.md): every dispatch to a sandbox added by a second
    # `host bootstrap` call failed "Authentication failed" against the
    # proxy until it was restarted by hand, because `ensure_sandbox_tokens`
    # just above mints that sandbox's token only on disk, not into the
    # already-running proxy's memory. `restart` covers both cases: it
    # starts a stopped unit exactly like `--now` would, and reloads a
    # running one. `enable` (separately, no `--now`) still only needs to
    # run once to persist across reboots; running it again on an
    # already-enabled unit is a harmless no-op.
    log("stage 10/11: enable the git proxy and the automation timer")
    admin_ssh.run(["sudo", "systemctl", "enable", "grain-git-proxy.service"])
    admin_ssh.run(["sudo", "systemctl", "restart", "grain-git-proxy.service"])
    admin_ssh.run(["sudo", "systemctl", "enable", "--now", "grain-automation.timer"])

    # Stage 11: verify -- left to the CLI command, which runs `automation
    # status`/`github audit`/`host health` itself over the same paths once
    # this function returns without raising.
    log("stage 11/11: bootstrap converged -- verify with 'automation status', "
        "'github audit', and 'host health'")
