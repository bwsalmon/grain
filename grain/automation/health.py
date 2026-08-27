"""What "healthy" means for a sandbox, checked concretely rather than
inferred from libvirt state alone.

docs/design.md step 8 / docs/roadmap.md item 5. `grain host status` already
answers "is the VM running" from the hypervisor's point of view
(`HostAdapter.state()`, backed by `virsh list`); this answers "can it
actually do the job" — reachable over SSH, `docker` responsive, systemd not
wedged, and disk below the watermark docs/design.md calls "the failure this
design is most likely to actually hit" ("sandboxes accumulate state...
disk grows monotonically until something is done about it"). A sandbox
libvirt reports RUNNING that this reports DEGRADED or UNREACHABLE is exactly
the gap `sweeper.py` didn't cover before this module existed — it only
reacted to systemd *unit* state (was the dispatched task's own unit
active/done), never to the sandbox's general health. See `sweeper.py`'s
call site for how that gap is closed: informational (an audit-log entry),
not by adding a new state to `AutomationState`'s dispatch-eligibility
machinery — see that module's docstring for why.

Four checks, always all run rather than short-circuited after the first
failure (aside from `ssh` itself, which everything else depends on), so one
bad reading doesn't hide another -- except `docker`, which `check_health`'s
`include_docker=False` skips entirely for machines (the controller) that
never run a docker daemon in the first place, rather than reporting a
`FAIL` that could never mean anything:

- **ssh**: can a trivial remote command even run at all? If not, the whole
  report is `UNREACHABLE` and no other check is attempted — they would just
  be a second way of reporting the identical fact.
- **systemd**: `systemctl is-system-running`. Anything other than the
  literal string `running` is flagged — most commonly `degraded`, meaning
  some unit is in a failed state, which is a real, cheap signal: a
  `grain-task-*` unit stuck `failed` because `reap()` never reached it, for
  instance, shows up here.
- **docker**: `docker info` actually responding — not just installed, the
  daemon a sandbox's whole job depends on (docs/design.md, "why a VM per
  agent") being alive.
- **disk**: `df` against a watermark — the disk-watermark alarm
  (docs/roadmap.md item 5's third piece) is this check; nothing separate.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

from ..run import Runner

DEFAULT_DISK_WATERMARK_PERCENT = 85
DEFAULT_DISK_MOUNT = "/"


class HealthStatus(str, Enum):
    HEALTHY = "healthy"
    # Reachable, but at least one check failed (disk over watermark, docker
    # unresponsive, a degraded systemd).
    DEGRADED = "degraded"
    # SSH itself failed — the sandbox may be stopped, wedged, or off the
    # network entirely. Nothing else was attempted.
    UNREACHABLE = "unreachable"


@dataclass(frozen=True)
class CheckResult:
    name: str
    ok: bool
    detail: str


@dataclass(frozen=True)
class HealthReport:
    status: HealthStatus
    checks: list[CheckResult] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.status is HealthStatus.HEALTHY

    def summary(self) -> str:
        return "; ".join(
            f"{c.name}={'ok' if c.ok else 'FAIL'} ({c.detail})" for c in self.checks
        )


def _disk_usage_percent(df_stdout: str) -> int | None:
    """Parses the data line of `df -P <mount>` output.

    `-P` (POSIX format) is what keeps this to exactly one header line and
    one data line — plain `df` wraps onto a second line for a long
    filesystem/device name, which would otherwise split the row this parses
    in two. Returns `None` (rather than raising) on anything that doesn't
    look like that shape, so a check can report "couldn't parse" instead of
    crashing the whole health report over one odd `df` build.
    """
    lines = [line for line in df_stdout.splitlines() if line.strip()]
    if len(lines) < 2:
        return None
    fields = lines[1].split()
    if len(fields) < 5 or not fields[4].endswith("%"):
        return None
    try:
        return int(fields[4][:-1])
    except ValueError:
        return None


def check_disk(runner: Runner, *, mount: str = DEFAULT_DISK_MOUNT,
                watermark_percent: int = DEFAULT_DISK_WATERMARK_PERCENT) -> CheckResult:
    result = runner.run(["df", "-P", mount], check=False)
    if result.returncode != 0:
        return CheckResult(
            "disk", False, f"df failed: {result.stderr.strip() or result.returncode}"
        )
    percent = _disk_usage_percent(result.stdout)
    if percent is None:
        return CheckResult("disk", False, f"could not parse df output: {result.stdout!r}")
    detail = f"{percent}% used on {mount} (watermark {watermark_percent}%)"
    return CheckResult("disk", percent < watermark_percent, detail)


def check_systemd(runner: Runner) -> CheckResult:
    result = runner.run(["systemctl", "is-system-running"], check=False)
    # is-system-running's exit code is non-zero for "degraded"/"starting"/
    # etc, not just for a command that failed to run at all — the printed
    # state, not the exit code, is what actually distinguishes those.
    state = result.stdout.strip() or result.stderr.strip() or "unknown"
    return CheckResult("systemd", state == "running", f"state: {state}")


def check_docker(runner: Runner) -> CheckResult:
    result = runner.run(["docker", "info"], check=False)
    if result.returncode == 0:
        return CheckResult("docker", True, "responsive")
    return CheckResult("docker", False, result.stderr.strip() or f"exit {result.returncode}")


def check_health(runner: Runner, *, mount: str = DEFAULT_DISK_MOUNT,
                  watermark_percent: int = DEFAULT_DISK_WATERMARK_PERCENT,
                  include_docker: bool = True) -> HealthReport:
    """Runs every check over `runner` — already scoped to one sandbox, the
    same `Runner` `dispatch.py`/`sweeper.py` use. Never raises: an
    unreachable sandbox is a reported `HealthReport`, not an exception, so a
    caller (the CLI, the sweeper) can always get an answer back.

    `include_docker` defaults to `True` for every existing caller (a
    sandbox's whole job depends on its docker daemon, per the module
    docstring). The controller has no docker daemon to check at all --
    `provision/controller.sh` installs libvirt/qemu tooling, not
    `docker-ce` -- so `check_grain_health` passes `include_docker=False`
    when pointed at the controller rather than reporting a `docker=FAIL`
    that can never pass there.
    """
    probe = runner.run(["true"], check=False)
    if probe.returncode != 0:
        detail = probe.stderr.strip() or f"exit {probe.returncode}"
        return HealthReport(HealthStatus.UNREACHABLE, [CheckResult("ssh", False, detail)])
    checks = [
        CheckResult("ssh", True, "reachable"),
        check_systemd(runner),
    ]
    if include_docker:
        checks.append(check_docker(runner))
    checks.append(check_disk(runner, mount=mount, watermark_percent=watermark_percent))
    status = HealthStatus.HEALTHY if all(c.ok for c in checks) else HealthStatus.DEGRADED
    return HealthReport(status, checks)
