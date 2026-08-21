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
from .adapter.lima import LimaAdapter
from .adapter.net_linux import LinuxNetwork, render_host_input_rules, render_ruleset
from .inventory import Cluster
from .run import DryRunRunner, RealRunner, Runner


def build_cluster(args: argparse.Namespace) -> Cluster:
    return Cluster(sandbox_count=args.sandboxes)


def build_adapter(cluster: Cluster, runner: Runner, args: argparse.Namespace):
    # Only a Linux adapter exists today. A darwin one implements the same
    # interface with socket_vmnet and pf; see docs/design.md.
    network = LinuxNetwork(cluster, runner)
    return LimaAdapter(cluster, runner, network, config_dir=Path(args.config_dir))


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
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
