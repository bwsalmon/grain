"""Deploying this repo's own code to `/opt/grain` on the controller --
docs/bootstrap.md Phase 3, closing docs/runbook.md's step 7. No credential
is involved: the host already holds the tree (this checkout), and the
deployment is a plain file copy over the SSH path that already has to exist
for every other admin operation -- `provision/controller.sh` deliberately
never does this itself, since it would need a deploy credential baked into a
provisioning script.

Shelled out as one `bash -c` pipeline (`tar -c ... | ssh ... tar -x ...`)
rather than composed from `SshRunner` + the stdin-not-argv `dd` shape the
credential-injection paths elsewhere in this repo use: tar's payload is
binary, and `Runner.run`'s `stdin` parameter is text (`RealRunner` passes
`text=True` to `subprocess.run`), so routing it through Python's captured
stdin/stdout would corrupt it. A single shell pipeline keeps the binary
stream inside a real OS pipe, entirely outside Python, while the whole thing
still goes through `Runner.run` like every other command in this project --
testable with `FakeRunner`, previewable with `DryRunRunner`.
"""

from __future__ import annotations

import ipaddress
import shlex
from pathlib import Path

from ..run import Runner

DEFAULT_DEST = "/opt/grain"
# What a deploy must never carry across: version control metadata (the
# controller has no use for it and it can be large), the docs tree (not
# needed to run the code), and any bytecode cache from a local run.
EXCLUDES = (".git", "docs", "__pycache__", ".pytest_cache")


def _ssh_prefix(user: str, address: ipaddress.IPv4Address, key_path: Path) -> list[str]:
    # Mirrors `SshRunner`'s own flags exactly (grain/automation/ssh.py) --
    # same host-key-TOFU and ambient-agent caveats apply here.
    return [
        "ssh", "-i", str(key_path),
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "IdentityAgent=none",
        "-o", "ConnectTimeout=10",
        f"{user}@{address}",
    ]


def deploy_tree(runner: Runner, source_root: Path, *, user: str,
                 address: ipaddress.IPv4Address, key_path: Path,
                 dest: str = DEFAULT_DEST) -> None:
    """`tar -cz`s `source_root` (excluding `EXCLUDES`), pipes it over SSH,
    and extracts it to `dest` on the controller. `sudo` on the remote side:
    `dest` is root-owned (`provision/controller.sh` creates it that way so a
    `git clone` run as root lands cleanly), and the admin SSH user is not.
    """
    tar_cmd = shlex.join([
        "tar", "-czf", "-", *[f"--exclude={e}" for e in EXCLUDES],
        "-C", str(source_root), ".",
    ])
    remote_cmd = f"sudo mkdir -p {shlex.quote(dest)} && sudo tar -xzf - -C {shlex.quote(dest)}"
    ssh_cmd = shlex.join([*_ssh_prefix(user, address, key_path), "--", remote_cmd])
    runner.run(["bash", "-c", f"{tar_cmd} | {ssh_cmd}"])
