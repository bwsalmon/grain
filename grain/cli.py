"""`grain host` — the operator surface for the host adapter.

Two commands exist mainly for safety rather than convenience:

  grain host rules     print the firewall ruleset without applying it
  --dry-run            print the commands that would run, execute none

Both are there because this program rewrites the firewall of a machine you
may only be able to reach through that firewall. Being able to read exactly
what will happen, before it happens, is worth the small amount of code.
"""

from __future__ import annotations

import argparse
import dataclasses
import os
import shlex
import sys
from pathlib import Path

from .adapter.base import EgressMode, VmState
from .adapter.deploy import DEFAULT_DEST, deploy_tree
from .adapter.libvirt import LibvirtAdapter
from .adapter.net_linux import LinuxNetwork, render_host_input_rules, render_ruleset
from .adapter.wait import wait_for_provisioning, wait_for_ssh
from .automation.audit import FileAuditLog
from .automation.cleanup import cleanup
from .automation.config import AutomationConfig
from .automation.configure import (
    configure_claude_credentials, configure_github_credential, configure_repo,
)
from .automation.core import Orchestrator
from .automation.credential_audit import Verdict, audit_secrets_dir
from .automation.github import DryRunGitHubClient, GitHubClient, RealTransport
from .automation.health import DEFAULT_DISK_WATERMARK_PERCENT, check_health
from .automation.history import FileSessionHistory
from .automation.ssh import SshRunner
from .automation.state import AutomationState, utcnow
from .automation import tui as sessions_tui
from .bootstrap import BootstrapConfig, bootstrap
from .inventory import Cluster
from .metadata.audit import FileAuditLog as MetadataFileAuditLog
from .metadata.audit import sync as metadata_sync
from .metadata.config import instance_paths
from .metadata.launcher import MetadataLauncher, build_launcher
from .proxy.credentials import CredentialSet
from .proxy.tokens import SandboxTokenStore
from .run import DryRunRunner, RealRunner, Runner

_REPO_ROOT = Path(__file__).resolve().parent.parent

# The same defaults `AutomationConfig`'s dataclass fields carry — read from
# there rather than repeated as literals, so `grain host health`/`cleanup`
# (which need SSH access but nothing else automation.json holds: no owner,
# no repo) can't quietly drift from what `grain automation run-once`
# actually uses.
_DEFAULT_SSH_USER = AutomationConfig.__dataclass_fields__["ssh_user"].default
_DEFAULT_SSH_KEY_PATH = AutomationConfig.__dataclass_fields__["ssh_key_path"].default
# The admin *private* key's default path -- the counterpart of
# LibvirtAdapter's `admin_public_key_path` default one directory up
# (`.pub`). Used by commands that reach a VM directly from the host/operator
# side (`host wait`, `host deploy`, `sandbox login`, `host bootstrap`)
# rather than from the controller's own automation dispatch path, which is
# what `_DEFAULT_SSH_KEY_PATH` above is for.
_DEFAULT_ADMIN_SSH_KEY_PATH = Path("/var/lib/grain/admin-ssh")


def build_cluster(args: argparse.Namespace) -> Cluster:
    cluster = Cluster.load(Path(args.cluster_file))
    overrides = {}
    if args.sandboxes is not None:
        overrides["sandbox_count"] = args.sandboxes
    if args.image is not None:
        overrides["image"] = args.image
    if overrides:
        cluster = dataclasses.replace(cluster, **overrides)
    return cluster


def build_adapter(cluster: Cluster, runner: Runner, args: argparse.Namespace):
    # libvirt, not Lima: `networks[].lima` (the mechanism lima.py needed) is
    # macOS-only, verified against Lima 2.2.0. A darwin adapter still
    # implements the same interface, with socket_vmnet and pf; see
    # docs/design.md and docs/host-adapter.md.
    network = LinuxNetwork(cluster, runner)
    return LibvirtAdapter(
        cluster, runner, network, config_dir=Path(args.config_dir),
        admin_public_key_path=Path(args.admin_ssh_public_key),
        controller_public_key_path=Path(args.controller_ssh_public_key),
    )


def build_orchestrator(cluster: Cluster, runner: Runner,
                        args: argparse.Namespace) -> tuple[Orchestrator, Path]:
    data_dir = Path(args.data_dir)
    config = AutomationConfig.load(data_dir / "config" / "automation.json")
    credentials = CredentialSet(data_dir / "secrets" / "github")
    credential = credentials.select(config.owner, config.repo)
    github: GitHubClient | DryRunGitHubClient = GitHubClient(
        RealTransport(), credential.token if credential else None
    )
    if args.dry_run:
        github = DryRunGitHubClient(github)
    state_path = data_dir / "state" / "automation" / "state.json"
    audit = FileAuditLog(data_dir / "state" / "automation" / "audit.log")
    token_store = SandboxTokenStore(data_dir / "secrets" / "sandbox-tokens.json")
    history = FileSessionHistory(data_dir / "state" / "automation" / "sessions")
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=AutomationState.load(state_path), base_runner=runner,
        token_store=token_store, audit=audit, history=history,
    )
    return orchestrator, state_path


def cmd_automation_run_once(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    orchestrator, state_path = build_orchestrator(cluster, _runner(args), args)
    orchestrator.run_once(utcnow())
    if not args.dry_run:
        orchestrator.state.save(state_path)
    return 0


def cmd_automation_status(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    data_dir = Path(args.data_dir)
    state = AutomationState.load(data_dir / "state" / "automation" / "state.json")
    for name in cluster.sandbox_names:
        assignment = state.assignments.get(name)
        if assignment is None:
            print(f"{name:<12} free")
        else:
            print(f"{name:<12} {assignment.kind.value} #{assignment.issue:<6} "
                  f"since {assignment.started_at.isoformat()}")
    return 0


def build_session_history(args: argparse.Namespace) -> FileSessionHistory:
    return FileSessionHistory(Path(args.data_dir) / "state" / "automation" / "sessions")


def cmd_sessions_list(args: argparse.Namespace) -> int:
    history = build_session_history(args)
    records = history.all()
    if args.kind:
        records = [r for r in records if r.kind.value == args.kind]
    if args.outcome:
        records = [r for r in records if r.outcome == args.outcome]
    if args.trigger is not None:
        records = [r for r in records if r.issue == args.trigger]
    if not records:
        print("no sessions recorded yet")
        return 0
    for record in records:
        print(sessions_tui.format_row(record))
    return 0


def cmd_sessions_browse(args: argparse.Namespace) -> int:
    history = build_session_history(args)
    sessions_tui.run(history)
    return 0


def build_metadata_launcher(cluster: Cluster, runner: Runner,
                             args: argparse.Namespace) -> MetadataLauncher:
    return build_launcher(Path(args.data_dir), cluster, runner)


def cmd_metadata_start(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    launcher = build_metadata_launcher(cluster, _runner(args), args)
    for name in _sandbox_targets(cluster, args.name):
        launcher.start(name)
    return 0


def cmd_metadata_stop(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    launcher = build_metadata_launcher(cluster, _runner(args), args)
    for name in _sandbox_targets(cluster, args.name):
        launcher.stop(name)
    return 0


def cmd_metadata_status(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    launcher = build_metadata_launcher(cluster, _runner(args), args)
    for name in _sandbox_targets(cluster, args.name):
        print(f"{name:<12} {launcher.status(name).value}")
    return 0


def cmd_metadata_sync_audit(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    data_dir = Path(args.data_dir)
    audit = MetadataFileAuditLog(data_dir / "state" / "metadata-server" / "audit.log")
    total = 0
    for name in _sandbox_targets(cluster, args.name):
        paths = instance_paths(data_dir, name)
        state_path = data_dir / "state" / "metadata-server" / f"{name}.audit-offset.json"
        total += metadata_sync(paths.log_path, state_path, name, audit)
    print(f"forwarded {total} event(s)")
    return 0


def cmd_github_audit(args: argparse.Namespace) -> int:
    secrets_dir = Path(args.data_dir) / "secrets" / "github"
    results = audit_secrets_dir(RealTransport(), secrets_dir)
    if not results:
        print(f"no *.token files found under {secrets_dir}")
        return 0
    exit_code = 0
    for r in results:
        print(f"{r.name:<12} {r.kind.value:<45} {r.verdict.value:<12} {r.detail}")
        if r.verdict is Verdict.FLAGGED:
            exit_code = 1
    return exit_code


def cmd_rules(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    if args.input_chain:
        sys.stdout.write(render_host_input_rules(cluster, ssh_port=args.ssh_port))
    else:
        sys.stdout.write(render_ruleset(cluster, EgressMode(args.egress)))
    return 0


def cmd_up(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    runner = _runner(args)
    LinuxNetwork(cluster, runner).up(EgressMode(args.egress))
    return 0


def cmd_egress(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    adapter.egress_policy(EgressMode(args.mode))
    return 0


def cmd_create(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    _check_provision_target(cluster, args)
    adapter = build_adapter(cluster, _runner(args), args)
    script = Path(args.provision).read_text() if args.provision else None
    for name in _targets(cluster, args.name):
        adapter.create(cluster.spec_of(name), script)
    return 0


def _check_provision_target(cluster: Cluster, args: argparse.Namespace) -> None:
    """`--provision` applies one script to every named target. The
    controller and the sandboxes are provisioned by different scripts
    (`provision/controller.sh` vs. `provision/sandbox.sh`) — 'all' would
    silently apply whichever one was given to both, which is wrong for
    either. Create/recreate the controller and the sandboxes as separate
    invocations instead.
    """
    if args.name == "all" and args.provision:
        raise SystemExit(
            "--provision with target 'all' would apply one script to both "
            "the controller and the sandboxes, which is almost certainly "
            "wrong now that they differ. Run this once for 'controller' "
            "with provision/controller.sh, and once for 'sandboxes' with "
            "provision/sandbox.sh."
        )


def cmd_start(args: argparse.Namespace) -> int:
    return _lifecycle(args, "start")


def cmd_stop(args: argparse.Namespace) -> int:
    return _lifecycle(args, "stop")


def cmd_destroy(args: argparse.Namespace) -> int:
    return _lifecycle(args, "destroy")


def cmd_recreate(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    _check_provision_target(cluster, args)
    adapter = build_adapter(cluster, _runner(args), args)
    script = Path(args.provision).read_text() if args.provision else None
    for name in _targets(cluster, args.name):
        adapter.recreate(name, script)
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    known = {info.name: info for info in adapter.list_vms()}
    for name in cluster.names:
        info = known.get(name)
        state = info.state.value if info else VmState.ABSENT.value
        print(f"{name:<12} {state:<8} {cluster.address_of(name)}")
    return 0


def build_ssh_runner(cluster: Cluster, base_runner: Runner, name: str,
                      args: argparse.Namespace) -> Runner:
    """The same shape as `Orchestrator._ssh_runner_for`, but standalone: a
    health check or a cleanup run needs SSH access, not a `GitHubClient` or
    a sandbox git-proxy token, so this avoids pulling `build_orchestrator`'s
    whole GitHub/credential wiring into a plain host-lifecycle command —
    and avoids requiring `automation.json` to exist at all, since `owner`/
    `repo` (its only fields with no default) are irrelevant here.
    """
    return SshRunner(
        inner=base_runner, user=args.ssh_user,
        address=cluster.address_of(name), key_path=Path(args.ssh_key),
    )


def cmd_host_cleanup(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    base_runner = _runner(args)
    exit_code = 0
    for name in _sandbox_targets(cluster, args.name):
        runner = build_ssh_runner(cluster, base_runner, name, args)
        result = cleanup(runner)
        for step in result.steps:
            print(f"{name:<12} {step.name:<8} {'ok' if step.ok else 'FAIL':<5} {step.detail}")
        if not result.ok:
            exit_code = 1
    return exit_code


def cmd_host_health(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
    base_runner = _runner(args)
    exit_code = 0
    for name in _sandbox_targets(cluster, args.name):
        runner = build_ssh_runner(cluster, base_runner, name, args)
        report = check_health(runner, watermark_percent=args.disk_watermark)
        print(f"{name:<12} {report.status.value:<12} {report.summary()}")
        if not report.ok:
            exit_code = 1
    return exit_code


def cmd_host_wait(args: argparse.Namespace) -> int:
    """`grain host wait <name>` -- docs/bootstrap.md Phase 3's first missing
    verb: block until a VM answers SSH and has finished cloud-init. Lifted
    from `tests/loadtest.py`'s own `_wait_for_ssh`/`_wait_for_provisioning`
    into `grain/adapter/wait.py` so both this command and `host bootstrap`'s
    sequencer share one implementation.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    for name in _targets(cluster, args.name):
        ssh = build_ssh_runner(cluster, base_runner, name, args)
        print(f"{name:<12} waiting for SSH ...")
        wait_for_ssh(ssh, timeout=args.timeout)
        print(f"{name:<12} waiting for cloud-init ...")
        wait_for_provisioning(ssh)
        print(f"{name:<12} ready")
    return 0


def cmd_host_deploy(args: argparse.Namespace) -> int:
    """`grain host deploy [name]` -- docs/bootstrap.md Phase 3's second
    missing verb: push this working tree to `/opt/grain` on the controller.
    No credential; see `grain/adapter/deploy.py`.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    if args.name != cluster.controller_name:
        raise SystemExit(
            f"deploy target must be '{cluster.controller_name}' -- this repo's "
            "code only ever runs there, not on a sandbox"
        )
    deploy_tree(
        base_runner, Path(args.source), user=args.ssh_user,
        address=cluster.address_of(cluster.controller_name),
        key_path=Path(args.ssh_key), dest=args.dest,
    )
    return 0


def cmd_controller_configure(args: argparse.Namespace) -> int:
    """`grain controller configure` -- docs/bootstrap.md Phase 3's third
    missing verb: writes `/data/config/automation.json`,
    `repo-allowlist.json`, and, if supplied, the GitHub token/credential
    mapping and the Claude Code credential file. See
    `grain/automation/configure.py`.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    ssh = build_ssh_runner(cluster, base_runner, cluster.controller_name, args)
    owner, _, repo = args.repo.partition("/")
    if not owner or not repo:
        raise SystemExit(f"--repo must be 'owner/name', got {args.repo!r}")
    configure_repo(ssh, owner, repo)
    if args.github_token_file:
        token = (
            sys.stdin.read() if args.github_token_file == "-"
            else Path(args.github_token_file).read_text()
        )
        configure_github_credential(
            ssh, owner, repo, token, credential_name=args.credential_name,
        )
    if args.claude_credentials_file:
        configure_claude_credentials(ssh, Path(args.claude_credentials_file).read_text())
    return 0


def cmd_host_bootstrap(args: argparse.Namespace) -> int:
    """`grain host bootstrap` -- docs/bootstrap.md Phase 4: the sequencer
    that replaces docs/runbook.md's fourteen-step checklist. See
    `grain/bootstrap.py` for the stage-by-stage reasoning.
    """
    cluster = build_cluster(args)
    base_runner = _runner(args)
    adapter = build_adapter(cluster, base_runner, args)
    github_token = None
    if args.github_token_file:
        github_token = (
            sys.stdin.read() if args.github_token_file == "-"
            else Path(args.github_token_file).read_text()
        )
    claude_credentials = (
        Path(args.claude_credentials_file).read_text()
        if args.claude_credentials_file else None
    )
    owner, _, repo = args.repo.partition("/")
    if not owner or not repo:
        raise SystemExit(f"--repo must be 'owner/name', got {args.repo!r}")
    config = BootstrapConfig(
        owner=owner, repo=repo, github_token=github_token,
        credential_name=args.credential_name, claude_credentials=claude_credentials,
        ssh_user=args.ssh_user, admin_private_key_path=Path(args.admin_ssh_private_key),
        controller_provision_script=(
            Path(args.controller_provision).read_text() if args.controller_provision else None
        ),
        sandbox_provision_script=(
            Path(args.sandbox_provision).read_text() if args.sandbox_provision else None
        ),
        ssh_timeout=args.ssh_timeout,
    )
    bootstrap(cluster=cluster, adapter=adapter, base_runner=base_runner, config=config)

    # Stage 11: verify -- the sequencer itself stops at "converged"; this is
    # the CLI command's own tail, using the admin key it already has rather
    # than re-deriving `grain automation status`/`github audit`/`host
    # health`'s own separate SSH-key flags (which name a different default,
    # the controller's dispatch key -- see `_DEFAULT_SSH_KEY_PATH`).
    print("[bootstrap] verify:")
    data_dir = Path(args.data_dir)
    state = AutomationState.load(data_dir / "state" / "automation" / "state.json")
    for name in cluster.sandbox_names:
        assignment = state.assignments.get(name)
        print(f"  automation: {name:<12} {'free' if assignment is None else assignment.kind.value}")
    for r in audit_secrets_dir(RealTransport(), data_dir / "secrets" / "github"):
        print(f"  github:     {r.name:<12} {r.verdict.value}")
    for name in cluster.sandbox_names:
        ssh = SshRunner(
            inner=base_runner, user=args.ssh_user,
            address=cluster.address_of(name), key_path=Path(args.admin_ssh_private_key),
        )
        report = check_health(ssh)
        print(f"  health:     {name:<12} {report.status.value}")
    return 0


def cmd_sandbox_login(args: argparse.Namespace) -> int:
    """`grain sandbox login <name>` -- direct, interactive admin SSH access
    to one sandbox or the controller, using the admin key rather than
    hopping through the controller first. For debugging: a stuck `kind`
    cluster, a wedged docker daemon, anything `grain host health`/`cleanup`/
    `sessions browse` doesn't give enough visibility into.

    This is deliberately possible only because of docs/bootstrap.md's Phase
    1 fix -- the admin key is now embedded as an authorized key on every VM
    at create time, not just informally reachable by whoever happens to hold
    the controller's own dispatch key. Holding the admin *private* key is
    what gates this, the same way holding any other SSH key gates any other
    login; this command adds no new capability the key itself doesn't
    already grant, it just removes the "hop through the controller" tax.

    Execs `ssh` directly rather than going through `Runner` -- this needs a
    real interactive terminal (raw stdio, a pty, signal passthrough), not a
    captured command result, so it is the one command in this CLI that isn't
    built on `Runner.run`.
    """
    cluster = build_cluster(args)
    if args.name not in cluster.names:
        raise SystemExit(f"unknown VM: {args.name} (known: {', '.join(cluster.names)})")
    address = cluster.address_of(args.name)
    argv = [
        "ssh", "-i", args.ssh_key,
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null",
        f"{args.ssh_user}@{address}",
    ]
    if args.dry_run:
        print("+ " + shlex.join(argv))
        return 0
    os.execvp("ssh", argv)  # noqa: S606 -- replaces this process with an interactive ssh session


# --- helpers --------------------------------------------------------------

def _runner(args: argparse.Namespace) -> Runner:
    return DryRunRunner() if args.dry_run else RealRunner()


def _targets(cluster: Cluster, name: str) -> list[str]:
    if name == "all":
        return cluster.names
    if name == "sandboxes":
        return cluster.sandbox_names
    if name not in cluster.names:
        raise SystemExit(f"unknown VM: {name} (known: {', '.join(cluster.names)})")
    return [name]


def _sandbox_targets(cluster: Cluster, name: str) -> list[str]:
    # Metadata servers exist only for sandboxes -- there is no controller
    # instance (Cluster.metadata_port raises for the controller's own
    # name), so this is the same shape as _targets minus "all" meaning
    # "including the controller."
    if name in ("all", "sandboxes"):
        return cluster.sandbox_names
    if name not in cluster.sandbox_names:
        raise SystemExit(
            f"unknown sandbox: {name} (known: {', '.join(cluster.sandbox_names)})"
        )
    return [name]


def _lifecycle(args: argparse.Namespace, action: str) -> int:
    cluster = build_cluster(args)
    adapter = build_adapter(cluster, _runner(args), args)
    for name in _targets(cluster, args.name):
        getattr(adapter, action)(name)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="grain", description=__doc__)
    parser.add_argument(
        "--cluster-file", default="/var/lib/grain/cluster.toml",
        help="TOML file overriding Cluster's dataclass defaults -- sandbox "
             "count, subnet, bridge, image, per-role sizing (docs/bootstrap.md "
             "Phase 2); falls back to the built-in defaults if absent",
    )
    parser.add_argument("--sandboxes", type=int, default=None,
                        help="number of sandbox VMs (default: 2, or --cluster-file's "
                             "sandbox_count)")
    parser.add_argument("--image", default=None,
                        help="base image path, overriding --cluster-file's image")
    parser.add_argument("--config-dir", default="/var/lib/grain/instances")
    parser.add_argument(
        "--admin-ssh-public-key", default="/var/lib/grain/admin-ssh.pub",
        help="host-local admin SSH public key, embedded as an authorized "
             "key on the controller and every sandbox -- setup, repair, "
             "and admin debugging access (see docs/bootstrap.md, 'key "
             "roles'; 'grain host bootstrap' generates one if absent)",
    )
    parser.add_argument(
        "--controller-ssh-public-key", default="/var/lib/grain/controller-ssh.pub",
        help="host-local copy of the controller's own SSH public key, "
             "embedded as an authorized key on every sandbox only -- the "
             "automation dispatch path (see docs/runbook.md)",
    )
    parser.add_argument("--data-dir", default="/data",
                        help="where automation config/secrets/state live")
    parser.add_argument("--dry-run", action="store_true",
                        help="print commands instead of running them")
    sub = parser.add_subparsers(dest="group", required=True)
    host = sub.add_parser("host", help="VM lifecycle and networking").add_subparsers(
        dest="command", required=True
    )

    p = host.add_parser("rules", help="print the firewall ruleset, apply nothing")
    p.add_argument("--egress", choices=[m.value for m in EgressMode], default="open")
    p.add_argument("--input-chain", action="store_true",
                   help="render a host INPUT policy (never applied automatically)")
    p.add_argument("--ssh-port", type=int, default=22)
    p.set_defaults(func=cmd_rules)

    p = host.add_parser("up", help="create the private network and apply the policy")
    p.add_argument("--egress", choices=[m.value for m in EgressMode], default="open")
    p.set_defaults(func=cmd_up)

    p = host.add_parser("egress", help="switch the egress policy")
    p.add_argument("mode", choices=[m.value for m in EgressMode])
    p.set_defaults(func=cmd_egress)

    for name, fn, help_text in (
        ("create", cmd_create, "create VMs"),
        ("start", cmd_start, "start VMs"),
        ("stop", cmd_stop, "stop VMs"),
        ("destroy", cmd_destroy, "destroy VMs and their disks"),
        ("recreate", cmd_recreate, "destroy, rebuild and start VMs"),
    ):
        p = host.add_parser(name, help=help_text)
        p.add_argument("name", help="VM name, or 'all' / 'sandboxes'")
        if name in ("create", "recreate"):
            p.add_argument("--provision", help="path to a provisioning script")
        p.set_defaults(func=fn)

    p = host.add_parser("status", help="show VM states and addresses")
    p.set_defaults(func=cmd_status)

    for name, fn, help_text in (
        ("cleanup", cmd_host_cleanup,
         "between-task hygiene: kind delete + docker system prune, over SSH"),
        ("health", cmd_host_health,
         "check SSH/docker/systemd/disk on sandbox(es); nonzero exit if unhealthy"),
    ):
        p = host.add_parser(name, help=f"{help_text} (sandboxes only)")
        p.add_argument("name", nargs="?", default="sandboxes",
                        help="sandbox name, or 'sandboxes' for all (default)")
        p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER,
                        help=f"default: {_DEFAULT_SSH_USER}")
        p.add_argument("--ssh-key", default=str(_DEFAULT_SSH_KEY_PATH),
                        help=f"default: {_DEFAULT_SSH_KEY_PATH}")
        if name == "health":
            p.add_argument("--disk-watermark", type=int,
                            default=DEFAULT_DISK_WATERMARK_PERCENT,
                            help=f"percent-used threshold to flag "
                                 f"(default: {DEFAULT_DISK_WATERMARK_PERCENT})")
        p.set_defaults(func=fn)

    p = host.add_parser(
        "wait", help="block until VM(s) answer SSH and finish cloud-init"
    )
    p.add_argument("name", nargs="?", default="all", help="VM name, or 'all' / 'sandboxes'")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.add_argument("--timeout", type=float, default=180.0,
                    help="seconds to wait for SSH per VM (default: 180)")
    p.set_defaults(func=cmd_host_wait)

    p = host.add_parser(
        "deploy", help="push this working tree to /opt/grain on the controller"
    )
    p.add_argument("name", nargs="?", default="controller", help="must be 'controller'")
    p.add_argument("--source", default=str(_REPO_ROOT), help="tree to deploy (default: this checkout)")
    p.add_argument("--dest", default=DEFAULT_DEST, help=f"default: {DEFAULT_DEST}")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_host_deploy)

    p = host.add_parser(
        "bootstrap",
        help="one command: network, controller, deploy, configure, sandboxes, enable (docs/bootstrap.md)",
    )
    p.add_argument("--repo", required=True, help="owner/name -- written to automation.json")
    p.add_argument("--github-token-file",
                    help="path to a file holding the GitHub token, or '-' for stdin")
    p.add_argument("--credential-name", default="bot",
                    help="credentials.json entry name for --github-token-file (default: bot)")
    p.add_argument("--claude-credentials-file",
                    help="path to a Claude Code ~/.claude/.credentials.json to place on the "
                         "controller and inject into every sandbox")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--admin-ssh-private-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.add_argument("--controller-provision", default=str(_REPO_ROOT / "provision" / "controller.sh"))
    p.add_argument("--sandbox-provision", default=str(_REPO_ROOT / "provision" / "sandbox.sh"))
    p.add_argument("--ssh-timeout", type=float, default=180.0,
                    help="seconds to wait for SSH per VM (default: 180)")
    p.set_defaults(func=cmd_host_bootstrap)

    automation = sub.add_parser(
        "automation", help="issue intake and dispatch"
    ).add_subparsers(dest="command", required=True)

    p = automation.add_parser(
        "run-once", help="sweep stranded work, then poll and dispatch once"
    )
    p.set_defaults(func=cmd_automation_run_once)

    p = automation.add_parser("status", help="show the current pool assignments")
    p.set_defaults(func=cmd_automation_status)

    sessions = sub.add_parser(
        "sessions", help="browse past dispatch sessions by trigger (docs/roadmap.md item 10)"
    ).add_subparsers(dest="command", required=True)

    p = sessions.add_parser(
        "list", help="list recorded sessions, optionally filtered"
    )
    p.add_argument("--kind", choices=["issue", "pr"], help="only this trigger kind")
    p.add_argument("--outcome", choices=["succeeded", "failed", "stranded"],
                    help="only this outcome")
    p.add_argument("--trigger", type=int, help="only sessions for this issue/PR number")
    p.set_defaults(func=cmd_sessions_list)

    p = sessions.add_parser(
        "browse", help="interactive text UI (curses) to browse sessions and view trajectories"
    )
    p.set_defaults(func=cmd_sessions_browse)

    metadata = sub.add_parser(
        "metadata", help="per-sandbox gce_metadata_server instances"
    ).add_subparsers(dest="command", required=True)

    for name, fn, help_text in (
        ("start", cmd_metadata_start, "start metadata server instance(s)"),
        ("stop", cmd_metadata_stop, "stop metadata server instance(s)"),
        ("status", cmd_metadata_status, "show instance unit states"),
    ):
        p = metadata.add_parser(name, help=help_text)
        p.add_argument("name", nargs="?", default="sandboxes",
                        help="sandbox name, or 'sandboxes' for all (default)")
        p.set_defaults(func=fn)

    p = metadata.add_parser(
        "sync-audit",
        help="forward new per-mint log lines into the audit log",
    )
    p.add_argument("name", nargs="?", default="sandboxes",
                    help="sandbox name, or 'sandboxes' for all (default)")
    p.set_defaults(func=cmd_metadata_sync_audit)

    github = sub.add_parser(
        "github", help="GitHub credential hardening"
    ).add_subparsers(dest="command", required=True)

    p = github.add_parser(
        "audit",
        help="check every credential under secrets/github for withheld scopes",
    )
    p.set_defaults(func=cmd_github_audit)

    controller = sub.add_parser(
        "controller", help="one-time per-deployment /data configuration"
    ).add_subparsers(dest="command", required=True)

    p = controller.add_parser(
        "configure",
        help="write automation.json, repo-allowlist.json, and (optionally) "
             "GitHub/Claude credentials to /data on the controller, over SSH",
    )
    p.add_argument("--repo", required=True, help="owner/name")
    p.add_argument("--github-token-file",
                    help="path to a file holding the GitHub token, or '-' for stdin")
    p.add_argument("--credential-name", default="bot",
                    help="credentials.json entry name for --github-token-file (default: bot)")
    p.add_argument("--claude-credentials-file",
                    help="path to a Claude Code ~/.claude/.credentials.json to place on the "
                         "controller")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_controller_configure)

    sandbox = sub.add_parser(
        "sandbox", help="direct admin access to one sandbox or the controller"
    ).add_subparsers(dest="command", required=True)

    p = sandbox.add_parser(
        "login",
        help="interactive SSH, using the admin key, for debugging "
             "(docs/bootstrap.md, 'grain sandbox login <name>')",
    )
    p.add_argument("name", help="VM name, e.g. 'sandbox-0' or 'controller'")
    p.add_argument("--ssh-user", default=_DEFAULT_SSH_USER, help=f"default: {_DEFAULT_SSH_USER}")
    p.add_argument("--ssh-key", default=str(_DEFAULT_ADMIN_SSH_KEY_PATH),
                    help=f"default: {_DEFAULT_ADMIN_SSH_KEY_PATH}")
    p.set_defaults(func=cmd_sandbox_login)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
