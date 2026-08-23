"""Boot-completion waits, lifted out of `tests/loadtest.py` (docs/bootstrap.md
Phase 3, `grain host wait`) so `grain host bootstrap`'s sequencer can use the
same "is this VM actually ready" checks the live test suite already proved
live against a real VM, instead of a fixed sleep or duplicating the logic.

Both take an `SshRunner` rather than a bare `Runner` + address/key, so a
caller already holding one (as `bootstrap()` does, for the admin SSH path)
has nothing further to assemble, and a `DryRunRunner`-backed one degrades
correctly for free: `DryRunRunner.run` on a non-read-only command prints and
returns success without touching the network, so a dry-run bootstrap moves
through both waits instantly instead of sleeping for real.
"""

from __future__ import annotations

import time

from ..automation.ssh import SshRunner


def wait_for_ssh(ssh: SshRunner, timeout: float, *, interval: float = 3.0) -> None:
    """Polls `ssh ... true` until it succeeds or `timeout` elapses."""
    deadline = time.monotonic() + timeout
    last_err = ""
    while True:
        result = ssh.run(["true"], check=False)
        if result.returncode == 0:
            return
        last_err = result.stderr
        if time.monotonic() >= deadline:
            break
        time.sleep(interval)
    raise TimeoutError(f"{ssh.address} never became reachable over SSH: {last_err}")


def wait_for_provisioning(ssh: SshRunner) -> None:
    """Blocks on cloud-init's own completion signal rather than guessing a
    sleep -- the same mechanism `tests/test_controller_integration.py`
    already uses live. Applies equally to a sandbox: `provision/sandbox.sh`
    is delivered the same way (a raw shebang script via cloud-init's
    scripts-user module), so `cloud-init status --wait` covers it too.
    """
    result = ssh.run(["sudo", "cloud-init", "status", "--wait"], check=False)
    if result.returncode != 0:
        raise RuntimeError(
            f"cloud-init did not finish cleanly on {ssh.address}: "
            f"{result.stdout}\n{result.stderr}"
        )
