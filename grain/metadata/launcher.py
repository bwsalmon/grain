"""Starts, stops, and reports on one `gce_metadata_server` instance per
sandbox -- the controller-side half of docs/design.md's "GCP credentials"
and docs/roadmap.md item 4.

Each instance is a transient systemd unit, the same durability argument
`grain/automation/dispatch.py` makes for a dispatched sandbox task: it
outlives this process, and its status is recoverable from `systemctl`
alone rather than needing a PID this launcher remembers. `unit_status` and
`reap` are imported unchanged from `automation.dispatch` -- both are
already fully generic (`runner`, `unit` in, nothing sandbox- or
task-specific), so re-implementing `systemctl show` parsing a second time
here would be exactly the "third shape" this project's own conventions
warn against. Starting is *not* reused as-is: `dispatch.start_unit`
hardcodes `--uid=debian` (the sandbox login user) and shells out through
`bash -c`, neither of which fits a controller-local process running as its
own dedicated `metadata_user` with no untrusted content anywhere in its
command line.

Each instance is bound to `cluster.controller_ip:cluster.metadata_port
(sandbox)` -- never the sandbox's own address, because these processes run
on the *controller*, one per sandbox, and it's `net_linux.py`'s DNAT rule
that makes a sandbox's request to the metadata anycast address land on the
right one. See `inventory.py` (`Cluster.metadata_port`) and
`net_linux.py`'s "Traffic to the metadata anycast address is DNAT'd per
source sandbox" for why binding to the sandbox's own address, as
docs/design.md's prose literally reads, would be wrong on this adapter --
the controller cannot bind an address that belongs to a different VM.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .config import MetadataConfig, build_argv, instance_paths, render_instance_config
from ..automation.dispatch import UnitState, reap, unit_status
from ..inventory import Cluster
from ..run import Runner


def unit_name(sandbox: str) -> str:
    return f"grain-metadata-{sandbox}"


@dataclass
class MetadataLauncher:
    cluster: Cluster
    config: MetadataConfig
    runner: Runner
    data_dir: Path

    def start(self, sandbox: str) -> None:
        """Writes this sandbox's instance config, then starts it as a
        transient systemd unit. Safe to call again for an already-running
        instance: `systemd-run` with a unit name already in use fails
        loudly rather than silently doubling up a listener on the same
        port, which is the right failure mode here -- call `stop` first
        for an intentional restart.
        """
        paths = instance_paths(self.data_dir, sandbox)
        paths.config_path.parent.mkdir(parents=True, exist_ok=True)
        paths.config_path.write_text(render_instance_config(self.config, sandbox))
        argv = build_argv(self.config, self.cluster, sandbox, paths)
        self.runner.run([
            "sudo", "systemd-run", f"--unit={unit_name(sandbox)}",
            f"--uid={self.config.metadata_user}",
            f"--setenv=GOOGLE_APPLICATION_CREDENTIALS={self.config.key_path}",
            "--property=RemainAfterExit=yes", "--",
            *argv,
        ])

    def stop(self, sandbox: str) -> None:
        reap(self.runner, unit_name(sandbox))

    def status(self, sandbox: str) -> UnitState:
        return unit_status(self.runner, unit_name(sandbox))

    def status_all(self) -> dict[str, UnitState]:
        return {name: self.status(name) for name in self.cluster.sandbox_names}


def build_launcher(data_dir: Path, cluster: Cluster, runner: Runner) -> MetadataLauncher:
    """Wires the real files under a /data-shaped directory -- see
    docs/design.md, "Secrets on /data", and `grain/proxy/server.py`'s
    `build_proxy` for the pattern this mirrors.
    """
    config = MetadataConfig.load(data_dir / "config" / "metadata-server.json")
    return MetadataLauncher(cluster=cluster, config=config, runner=runner, data_dir=data_dir)
