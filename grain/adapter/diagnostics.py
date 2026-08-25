"""Why a boot wait failed, gathered from whichever side can still answer.

`wait_for_ssh` timing out on the controller -- stage 5 of `grain host
bootstrap` -- is the least self-explanatory failure the sequencer has: the
one channel that could explain it is, by definition, the thing not
answering. What an operator actually saw was `stage 5/11: wait for the
controller` followed by a `TimeoutError` carrying one line of `ssh` stderr,
and then (on GCP) `dump_storage_diagnostics`' ownership tables from
terraform/gcp/files/deploy.sh -- facts about a *different* failure, printed
because that dump ran on any non-zero exit at all.

Same trick as that dump, aimed at the right facts this time: print them to
stdout, which on GCP is already tailed into Cloud Logging (the host journal,
via the ops agent -- terraform/gcp/files/startup.sh) and locally is just the
terminal. No SSH or IAP path to the host is needed to read either, which is
the property that made `dump_storage_diagnostics` worth having: the operator
hitting that failure had no such path.

Two collectors, because a wait can fail on either side of the SSH boundary:

- `dump_host_diagnostics` -- for a VM that never answered SSH at all. Runs
  on the *host*: domain state, whether the guest ever ARPed or answered a
  ping on its assigned address, and the tail of its serial console log (the
  file `render_domain_xml` points the guest console at, bwsalmon/agents#58).
  Those separate the cases an operator would otherwise guess between: never
  booted (nothing in the console log), booted but never got its address
  (console log full, nothing in the neighbour table), or up and routing but
  refusing the admin key (ping answers, SSH does not).
- `dump_guest_diagnostics` -- for a VM that answers SSH but whose cloud-init
  ended in `error`/`degraded`. `cloud-init status --long` names the failing
  module, and `/var/log/cloud-init-output.log` carries the provisioning
  script's own output (`provision/controller.sh` runs under `set -eux`),
  which is where an apt or egress failure actually shows up -- neither of
  which reaches the caller, since `wait_for_provisioning` only ever sees
  `status: error` on stdout.

Nothing here raises. A diagnostic that fails must not replace the error it
was called to explain, so every command runs with `check=False` and each
collector is exception-guarded as a whole.
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path

from ..automation.ssh import SshRunner
from .libvirt import LIBVIRT_URI, LibvirtAdapter

Logger = Callable[[str], None]

# Enough of the console to carry a kernel panic, cloud-init's own final
# lines, or a `set -eux` provisioning script's last commands -- and bounded,
# because on a healthy-but-unreachable guest this file grows without limit
# (the controller forwards its whole journal there; see
# provision/controller.sh's ForwardToConsole).
CONSOLE_LOG_LINES = 120
GUEST_LOG_LINES = 80


def _default_log(message: str) -> None:
    print(f"[diagnostics] {message}")


def _section(log: Logger, label: str, text: str) -> None:
    log(f"--- {label} ---")
    body = text.strip()
    if not body:
        log("  (no output)")
        return
    for line in body.splitlines():
        log(f"  {line}")


def dump_host_diagnostics(adapter: LibvirtAdapter, name: str,
                          log: Logger = _default_log) -> None:
    """Host-side facts about a VM that never answered SSH."""
    log(f"diagnostics for {name}: it never answered SSH -- what the host can see")
    try:
        address = adapter.cluster.address_of(name)
        runner = adapter.runner
        _section(log, "domain state", adapter.state(name).value)
        for label, argv in (
            ("virsh dominfo", ["virsh", "-c", LIBVIRT_URI, "dominfo", name]),
            (f"host bridge {adapter.cluster.bridge}",
             ["ip", "-br", "addr", "show", adapter.cluster.bridge]),
            (f"neighbour table for {address} (did the guest ever ARP?)",
             ["ip", "neigh", "show", str(address)]),
            (f"ping {address}", ["ping", "-c", "2", "-W", "2", str(address)]),
        ):
            result = runner.run(argv, check=False)
            _section(log, label, result.stdout + result.stderr)
        _dump_console_log(adapter, name, log)
    except Exception as exc:  # never mask the failure this explains
        log(f"host diagnostics for {name} could not be collected: {exc!r}")


def _dump_console_log(adapter: LibvirtAdapter, name: str, log: Logger) -> None:
    console = Path(adapter.config_dir) / f"{name}-console.log"
    label = f"serial console, last {CONSOLE_LOG_LINES} lines ({console})"
    if not console.exists():
        _section(log, label,
                 "(missing -- this domain was defined without a console log "
                 "sink, so nothing the guest printed was captured; "
                 "`grain host recreate` redefines it with one)")
        return
    if console.stat().st_size == 0:
        _section(log, label,
                 "(empty -- the guest never wrote a byte to its serial "
                 "console, so it did not get as far as a kernel boot: look at "
                 "the disk and the host's own libvirt/qemu journal, not at the "
                 "guest)")
        return
    result = adapter.runner.run(
        ["tail", "-n", str(CONSOLE_LOG_LINES), str(console)], check=False,
    )
    _section(log, label, result.stdout + result.stderr)


def dump_guest_diagnostics(ssh: SshRunner, log: Logger = _default_log) -> None:
    """Guest-side facts about a VM whose provisioning did not finish."""
    log(f"diagnostics for {ssh.address}: provisioning did not finish -- "
        f"what the guest itself reports")
    try:
        for label, argv in (
            ("cloud-init status --long",
             ["sudo", "cloud-init", "status", "--long"]),
            (f"/var/log/cloud-init-output.log, last {GUEST_LOG_LINES} lines",
             ["sudo", "tail", "-n", str(GUEST_LOG_LINES),
              "/var/log/cloud-init-output.log"]),
            ("failed systemd units", ["systemctl", "list-units", "--failed",
                                      "--no-legend", "--no-pager"]),
        ):
            result = ssh.run(argv, check=False)
            _section(log, label, result.stdout + result.stderr)
    except Exception as exc:  # never mask the failure this explains
        log(f"guest diagnostics for {ssh.address} could not be collected: {exc!r}")
