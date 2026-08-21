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
import sys
from pathlib import Path

from .adapter.base import EgressMode, VmState
from .adapter.libvirt import LibvirtAdapter
from .adapter.net_linux import LinuxNetwork, render_host_input_rules, render_ruleset
from .automation.audit import FileAuditLog
from .automation.cleanup import cleanup
from .automation.config import AutomationConfig
from .automation.core import Orchestrator
from .automation.credential_audit import Verdict, audit_secrets_dir
from .automation.github import DryRunGitHubClient, GitHubClient, RealTransport
from .automation.health import DEFAULT_DISK_WATERMARK_PERCENT, check_health
from .automation.ssh import SshRunner
from .automation.state import AutomationState, utcnow
from .inventory import Cluster
from .metadata.audit import FileAuditLog as MetadataFileAuditLog
from .metadata.audit import sync as metadata_sync
from .metadata.config import instance_paths
from .metadata.launcher import MetadataLauncher, build_launcher
from .proxy.credentials import CredentialSet
from .proxy.tokens import SandboxTokenStore
from .run import DryRunRunner, RealRunner, Runner

# The same defaults `AutomationConfig`'s dataclass fields carry — read from
# there rather than repeated as literals, so `grain host health`/`cleanup`
# (which need SSH access but nothing else automation.json holds: no owner,
# no repo) can't quietly drift from what `grain automation run-once`
# actually uses.
_DEFAULT_SSH_USER = AutomationConfig.__dataclass_fields__["ssh_user"].default
_DEFAULT_SSH_KEY_PATH = AutomationConfig.__dataclass_fields__["ssh_key_path"].default


def build_cluster(args: argparse.Namespace) -> Cluster:
    return Cluster(sandbox_count=args.sandboxes)


def build_adapter(cluster: Cluster, runner: Runner, args: argparse.Namespace):
    # libvirt, not Lima: `networks[].lima` (the mechanism lima.py needed) is
    # macOS-only, verified against Lima 2.2.0. A darwin adapter still
    # implements the same interface, with socket_vmnet and pf; see
    # docs/design.md and docs/host-adapter.md.
    network = LinuxNetwork(cluster, runner)
    return LibvirtAdapter(cluster, runner, network, config_dir=Path(args.config_dir))


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
    orchestrator = Orchestrator(
        cluster=cluster, github=github, config=config,
        state=AutomationState.load(state_path), base_runner=runner,
        token_store=token_store, audit=audit,
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
            print(f"{name:<12} issue #{assignment.issue:<6} "
                  f"since {assignment.started_at.isoformat()}")
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
    adapter = build_adapter(cluster, _runner(args), args)
    script = Path(args.provision).read_text() if args.provision else None
    for name in _targets(cluster, args.name):
        adapter.create(cluster.spec_of(name), script)
    return 0


def cmd_start(args: argparse.Namespace) -> int:
    return _lifecycle(args, "start")


def cmd_stop(args: argparse.Namespace) -> int:
    return _lifecycle(args, "stop")


def cmd_destroy(args: argparse.Namespace) -> int:
    return _lifecycle(args, "destroy")


def cmd_recreate(args: argparse.Namespace) -> int:
    cluster = build_cluster(args)
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
    parser.add_argument("--sandboxes", type=int, default=2,
                        help="number of sandbox VMs (default: 2)")
    parser.add_argument("--config-dir", default="/var/lib/grain/instances")
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

    automation = sub.add_parser(
        "automation", help="issue intake and dispatch"
    ).add_subparsers(dest="command", required=True)

    p = automation.add_parser(
        "run-once", help="sweep stranded work, then poll and dispatch once"
    )
    p.set_defaults(func=cmd_automation_run_once)

    p = automation.add_parser("status", help="show the current pool assignments")
    p.set_defaults(func=cmd_automation_status)

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

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
