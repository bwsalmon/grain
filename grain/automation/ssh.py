"""Wraps any `Runner` so its argv runs on a sandbox instead of locally.

The net_linux.py ruleset already permits this: "controller may reach any
sandbox — it drives the agent servers" (see `net_linux.py`, rule 3). This is
the one piece that was missing — a way to actually place a command there —
and composing with `Runner` rather than inventing a parallel execution path
means `DryRunRunner` and `FakeRunner` keep working unchanged on top of it.

`UserKnownHostsFile=/dev/null`, not the default `~/.ssh/known_hosts` — found
live, not reasoned about: a sandbox at a given inventory address gets a
*new* SSH host key every `grain sandbox recreate` (a normal operation, see
docs/design.md), but the address stays fixed. The default known_hosts file
pins the first key it sees to that address, so the very next recreate turns
every dispatch into "REMOTE HOST IDENTIFICATION HAS CHANGED... Host key
verification failed" — reproduced against a real VM while building the live
integration suite. Host-key TOFU pinning is also not a boundary this design
relies on: a sandbox is authenticated by its fixed address on a firewalled
private bridge and the controller's own key, not by remembering an
ephemeral, disposable VM's host identity — see docs/design.md, "sandbox
identity."

Two more, both found live against a real sandbox:

- **The remote command is one shell-quoted string, not a trailing argv.**
  SSH's protocol has no concept of an argv array for exec requests — only a
  single command string, which the client builds by joining its trailing
  arguments. OpenSSH does this with a plain space, *unquoted*: `ssh host --
  bash -c "sleep 5"` sends the remote shell `bash -c sleep 5`, which
  word-splits back into three tokens — `-c`'s argument becomes bare
  `sleep`, and `5` becomes bash's `$0`. Reproduced directly: that exact
  command failed instantly with `sleep: missing operand` instead of
  sleeping. `shlex.join(argv)` before handing it to `ssh` builds one
  properly quoted string, which round-trips correctly through the remote
  shell's own re-parsing.
- **`IdentityAgent=none`.** An ambient `SSH_AUTH_SOCK` — set in this very
  environment — can leave `ssh` probing a stale or unresponsive agent
  socket before it ever gets to authentication, hanging the *entire*
  connection indefinitely (reproduced: `ssh -vvv` sat forever right after
  `ssh_get_authentication_socket_path`, with no controlling timeout for it
  — `ConnectTimeout` doesn't cover this phase). This runner already brings
  its own key; it has no use for an agent, forwarded or otherwise.
"""

from __future__ import annotations

import ipaddress
import shlex
from dataclasses import dataclass
from pathlib import Path

from ..run import CommandError, Result, Runner


@dataclass
class SshRunner:
    inner: Runner
    user: str
    address: ipaddress.IPv4Address
    key_path: Path

    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result:
        ssh_argv = [
            "ssh",
            "-i", str(self.key_path),
            "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null",
            "-o", "IdentityAgent=none",
            "-o", "ConnectTimeout=10",
            f"{self.user}@{self.address}",
            "--",
            shlex.join(argv),
        ]
        # check=False on the inner call: CommandError raised there would
        # carry the ssh-wrapped argv, not the caller's. Remap first, then
        # decide whether to raise, so a failure reports the remote command
        # that failed rather than `ssh` (exit 255 on its own connection
        # errors) as argv[0].
        result = self.inner.run(ssh_argv, stdin=stdin, check=False)
        remapped = Result(argv, result.returncode, result.stdout, result.stderr)
        if check and remapped.returncode != 0:
            raise CommandError(argv, remapped.returncode, remapped.stderr)
        return remapped
