"""Command execution, behind an interface so the adapter can be tested.

Nothing in this project should call `subprocess` directly. Every driver takes
a Runner, which means the whole adapter can be exercised without a
hypervisor — useful in general, and necessary here, since the security-
critical part (the firewall ruleset) must be verifiable without root.
"""

from __future__ import annotations

import shlex
import subprocess
from dataclasses import dataclass, field
from typing import Protocol


class CommandError(RuntimeError):
    def __init__(self, argv: list[str], returncode: int, stderr: str) -> None:
        self.argv = argv
        self.returncode = returncode
        self.stderr = stderr
        super().__init__(
            f"command failed ({returncode}): {shlex.join(argv)}\n{stderr.strip()}"
        )


@dataclass(frozen=True)
class Result:
    argv: list[str]
    returncode: int
    stdout: str
    stderr: str


class Runner(Protocol):
    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result: ...


class RealRunner:
    """Actually runs commands."""

    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result:
        try:
            proc = subprocess.run(
                argv, input=stdin, capture_output=True, text=True, check=False
            )
            result = Result(argv, proc.returncode, proc.stdout, proc.stderr)
        except FileNotFoundError:
            # A missing binary is a failed command, not a different kind of
            # event: honour `check` the same way any other failure would, so
            # probes that tolerate failure keep tolerating it.
            result = Result(argv, 127, "", f"{argv[0]}: not found on PATH")
        if check and result.returncode != 0:
            raise CommandError(argv, result.returncode, result.stderr)
        return result


@dataclass
class DryRunRunner:
    """Prints what would run. Read-only commands still execute.

    Worth having for a host whose firewall you are about to rewrite: it lets
    you review the exact commands before any of them touch the machine.

    A dry run is most useful on a host that is not set up yet, so a missing
    binary must not be fatal — an absent `limactl` means "no VMs exist",
    which is the correct answer, not a crash.
    """

    inner: Runner = field(default_factory=RealRunner)
    read_only_prefixes: tuple[tuple[str, ...], ...] = (
        ("limactl", "list"),
        ("ip", "-j"),
        ("nft", "list"),
    )
    printed: list[list[str]] = field(default_factory=list)

    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result:
        if self._is_read_only(argv):
            return self.inner.run(argv, stdin=stdin, check=check)
        self.printed.append(argv)
        rendered = shlex.join(argv)
        if stdin:
            print(f"+ {rendered}  <<'EOF'\n{stdin}EOF")
        else:
            print(f"+ {rendered}")
        return Result(argv, 0, "", "")

    def _is_read_only(self, argv: list[str]) -> bool:
        return any(
            tuple(argv[: len(prefix)]) == prefix for prefix in self.read_only_prefixes
        )


@dataclass
class FakeRunner:
    """Records commands and replays scripted output. For tests."""

    # Keyed by the joined argv prefix; longest matching prefix wins.
    responses: dict[str, Result] = field(default_factory=dict)
    calls: list[tuple[list[str], str | None]] = field(default_factory=list)
    default: Result | None = None

    def expect(self, prefix: str, stdout: str = "", returncode: int = 0,
               stderr: str = "") -> None:
        self.responses[prefix] = Result(
            shlex.split(prefix), returncode, stdout, stderr
        )

    def run(self, argv: list[str], *, stdin: str | None = None,
            check: bool = True) -> Result:
        self.calls.append((list(argv), stdin))
        rendered = shlex.join(argv)
        match = None
        for prefix, response in self.responses.items():
            if rendered.startswith(prefix) and (
                match is None or len(prefix) > len(match)
            ):
                match = prefix
        result = (
            self.responses[match]
            if match is not None
            else (self.default or Result(list(argv), 0, "", ""))
        )
        result = Result(list(argv), result.returncode, result.stdout, result.stderr)
        if check and result.returncode != 0:
            raise CommandError(argv, result.returncode, result.stderr)
        return result

    # --- assertions used by tests ----------------------------------------
    @property
    def commands(self) -> list[str]:
        return [shlex.join(argv) for argv, _ in self.calls]

    def ran(self, prefix: str) -> bool:
        return any(c.startswith(prefix) for c in self.commands)
