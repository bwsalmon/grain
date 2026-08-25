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
    raise TimeoutError(
        f"{ssh.address} never became reachable over SSH within {timeout:.0f}s: "
        f"{last_err.strip() or '(no error output)'}"
    )


# `cloud-init status --wait` has no bound of its own -- it blocks for as long
# as cloud-init keeps running, which for a guest wedged mid-provision is
# forever. Unbounded here means the whole bootstrap hangs until something
# further out kills it (on GCP, config-sync's deploy_timeout_minutes), and
# that killer reports a bare timeout with none of the context below. `timeout`
# is coreutils, already on any guest this runs against.
PROVISIONING_TIMEOUT = 900.0
_TIMEOUT_EXIT = 124  # coreutils' own "the command outlived its budget"


def wait_for_provisioning(ssh: SshRunner, *,
                           timeout: float = PROVISIONING_TIMEOUT) -> None:
    """Blocks on cloud-init's own completion signal rather than guessing a
    sleep -- the same mechanism `tests/test_controller_integration.py`
    already uses live. Applies equally to a sandbox: `provision/sandbox.sh`
    is delivered the same way (a raw shebang script via cloud-init's
    scripts-user module), so `cloud-init status --wait` covers it too.

    A non-zero exit is cloud-init reporting `error` (or `degraded`), which
    means the provisioning script itself failed -- the *reason* is in the
    guest's own logs, not in what surfaces here, so a caller that can still
    reach the guest should follow this with
    `diagnostics.dump_guest_diagnostics`.
    """
    result = ssh.run(
        ["sudo", "timeout", str(int(timeout)), "cloud-init", "status", "--wait"],
        check=False,
    )
    if result.returncode == _TIMEOUT_EXIT:
        raise TimeoutError(
            f"cloud-init on {ssh.address} was still running after "
            f"{timeout:.0f}s -- provisioning is wedged, not merely slow"
        )
    if result.returncode != 0:
        raise RuntimeError(
            f"cloud-init did not finish cleanly on {ssh.address}: "
            f"{result.stdout}\n{result.stderr}"
        )
