"""Between-task hygiene for a long-lived sandbox.

docs/design.md, "Between-task hygiene": most of what a task leaves behind is
cheap to clear without recreating anything —

    kind delete clusters --all
    docker system prune -af --volumes
    rm -rf "$WORKDIR"

"Hygiene, not isolation." It stops the disk filling and stops the common
cross-task collisions (a `kind` cluster still named `kind`, a container
already bound to the port the next task wants). It does not make the
previous task's data unrecoverable — `HostAdapter.recreate()` is the actual
isolation boundary, an explicit, occasional operation that takes a reboot.

**Where this runs, and why:** automatically, from `sweeper.py`, the instant
a sandbox's slot is freed — a task succeeded, failed, or was found stranded
— before that sandbox is eligible for the next dispatch. That is the
natural point per docs/design.md step 8, and it is the shape this codebase
already has: the sweeper is where every other "a run just finished, now
what" decision already lives (label moves, and — since docs/roadmap.md item
2 — PR creation on success). Folding cleanup into the same pass means a
freed sandbox is *guaranteed* clean before its next task, not "clean once
some separate cron job gets around to it." `grain host cleanup <name>` is
also exposed standalone (`grain/cli.py`) for the cases the sweeper's own
loop doesn't reach — a sandbox an operator wants tidied without waiting for
a sweep cycle, or one that's sitting free already.

**One deliberate departure from the design.md snippet above: this does not
`rm -rf` the workspace.** That snippet predates `dispatch.py`'s
`ensure_workspace()` (docs/roadmap.md item 2), which already resets
`WORKSPACE_PATH` to a known-clean state on *every* dispatch — `git clean
-fdx` plus a forced checkout of `origin/HEAD` — regardless of what a
previous task left there. Wiping the directory here would only force a full
re-clone on the next dispatch instead of `ensure_workspace()`'s cheaper
incremental fetch, with no correctness benefit: the accumulation risk that
actually matters (docker images, `kind` clusters, build layers) lives
entirely outside the git workspace.

Every step runs with `check=False` and a failure is recorded, not raised:
this hook must never be the reason a sandbox fails to get freed for reuse,
and a step failing here (`kind` not on a bare/unprovisioned image, docker
already quiet) is informational, not fatal to the sweep.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from ..run import Runner

# Order matters: a `kind` cluster's nodes are themselves docker containers,
# so stopping them first means `docker system prune` has nothing still
# "in use" left to skip.
_STEPS: tuple[tuple[str, list[str]], ...] = (
    ("kind", ["kind", "delete", "clusters", "--all"]),
    ("docker", ["docker", "system", "prune", "-af", "--volumes"]),
)


@dataclass(frozen=True)
class StepResult:
    name: str
    ok: bool
    detail: str


@dataclass(frozen=True)
class CleanupResult:
    steps: list[StepResult] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return all(step.ok for step in self.steps)

    def summary(self) -> str:
        return "; ".join(
            f"{s.name}={'ok' if s.ok else 'FAIL'} ({s.detail})" for s in self.steps
        ) or "no steps"


def cleanup(runner: Runner) -> CleanupResult:
    """Runs the between-task hygiene commands over `runner` — already
    scoped to one sandbox, the same `Runner` `dispatch.py`/`sweeper.py` use
    (typically an `SshRunner`). Never raises: a step's own failure is
    captured in the returned `CleanupResult` rather than propagated, so one
    bad sandbox can't take down the sweep pass that is trying to free it.
    """
    steps = []
    for name, argv in _STEPS:
        result = runner.run(argv, check=False)
        ok = result.returncode == 0
        detail = (
            (result.stdout.strip() or "done") if ok
            else (result.stderr.strip() or f"exit {result.returncode}")
        )
        steps.append(StepResult(name, ok, detail))
    return CleanupResult(steps)
