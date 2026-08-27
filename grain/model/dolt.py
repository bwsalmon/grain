"""The task store, on Dolt, reached by shelling out to the `dolt` CLI.

**Why the CLI and not a driver.** Dolt embeds only in Go, so a Python
controller has two ways in: run `dolt sql-server` and speak the MySQL
wire protocol with a driver, or run the `dolt` binary. This project
already made the equivalent choice once and wrote down why -- see
`gcp_keys.py` on shelling to `gcloud` rather than hand-rolling an OAuth2
exchange. The same reasoning applies more strongly here: a driver is a
non-stdlib dependency on the VM that holds every credential
(`pyproject.toml`: `dependencies = []`), and a server is a second daemon
on it. The binary is neither, and it is what an operator debugging this
by hand would reach for anyway.

The cost is that there are no bind parameters, which `sql.py` exists to
pay -- read that module's docstring before adding a query here.

**Every statement goes over stdin, never argv.** Two reasons, and the
second is the one that matters: argv has a length limit a long issue body
will find, and a statement on the command line is visible in the process
table to anything that can read `/proc`. Task text is not a credential,
but this project's own placement discipline is "material never becomes a
shell-interpolated argument", and there is no reason for the store to be
the exception.

**Durability is a Dolt commit, not a SQL transaction.** One `dolt sql`
invocation is one session, so a batch that dies partway can leave the
working set half-written. That is recoverable in a way it would not be in
an ordinary database: the last `dolt commit` is a known-good point and
`dolt reset --hard` returns to it. So writes are batched per logical
change and `commit()` is called at the end of a cycle -- the same shape
as `AutomationState`'s atomic temp-file-and-rename, one level up.

**The CLI surface is deliberately in one class.** `DoltCli` below builds
every argv this module runs. The exact flags have not been exercised
against a live `dolt` from this environment, so if a flag name is wrong,
that class is the only place to fix it.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, replace
from datetime import datetime, timezone
from pathlib import Path

from ..run import Runner
from . import schema
from .sql import ident, literal, render, explain
from .types import (
    Attribution, CredentialRef, FolderRef, Grant, GrantSource, Lease,
    LinkKind, Observation, Origin, OriginReason, PrincipalKind,
    PrincipalRef, RepoBinding, RepoRef, Run, Task, TaskIntent, TaskLink,
    TaskState, blocks,
)


class StoreError(RuntimeError):
    """A store operation that failed, with the statement made readable.

    `sql.explain()` is applied to the statement before it reaches the
    message: hex literals are unreadable, and an operator looking at a
    failure needs to see what it actually said.
    """

    def __init__(self, statement: str, detail: str) -> None:
        self.statement = statement
        self.detail = detail
        super().__init__(f"{detail}\nstatement: {explain(statement).strip()}")


@dataclass(frozen=True)
class DoltCli:
    """Every `dolt` invocation this module makes, in one place.

    Unverified against a live binary from the environment this was written
    in; if a flag is wrong, it is wrong here and only here.
    """

    binary: str = "dolt"
    data_dir: Path | None = None

    def _base(self) -> list[str]:
        argv = [self.binary]
        if self.data_dir is not None:
            argv += ["--data-dir", str(self.data_dir)]
        return argv

    def sql(self, *, json_out: bool) -> list[str]:
        """Run SQL supplied on stdin. `-r json` asks for machine-readable
        results; writes ask for none, since their output is noise.
        """
        argv = self._base() + ["sql"]
        if json_out:
            argv += ["-r", "json"]
        return argv

    def init(self) -> list[str]:
        return self._base() + ["init"]

    def commit(self, message: str) -> list[str]:
        # `-A` stages every table; `--allow-empty` keeps a cycle that
        # changed nothing from being an error the caller has to special-case.
        return self._base() + ["commit", "-A", "--allow-empty", "-m", message]


class DoltDatabase:
    """Statements in, rows out. Knows nothing about the task model."""

    def __init__(self, runner: Runner, cli: DoltCli | None = None) -> None:
        self._runner = runner
        self._cli = cli or DoltCli()

    def execute(self, *statements: str) -> None:
        """Run statements as one batch, discarding output."""
        batch = _batch(statements)
        if not batch:
            return
        result = self._runner.run(
            self._cli.sql(json_out=False), stdin=batch, check=False,
        )
        if result.returncode != 0:
            raise StoreError(batch, result.stderr.strip() or "dolt sql failed")

    def query(self, statement: str) -> list[dict]:
        """Run one statement and decode its rows."""
        result = self._runner.run(
            self._cli.sql(json_out=True), stdin=_batch([statement]), check=False,
        )
        if result.returncode != 0:
            raise StoreError(statement, result.stderr.strip() or "dolt sql failed")
        return _rows(result.stdout, statement)

    def commit(self, message: str) -> None:
        self._runner.run(self._cli.commit(message))

    def initialize(self) -> None:
        """Create the schema if it is absent, and refuse a database
        written by a newer version rather than failing later with a
        confusing missing column.
        """
        self.execute(*schema.statements())
        rows = self.query("SELECT `version` FROM `grain_schema` WHERE `id` = 1")
        if not rows:
            self.execute(render(
                "INSERT INTO `grain_schema` (`id`, `version`) VALUES (1, {v})",
                v=schema.SCHEMA_VERSION,
            ))
            return
        found = int(rows[0]["version"])
        if found > schema.SCHEMA_VERSION:
            raise StoreError(
                "SELECT `version` FROM `grain_schema`",
                f"database is at schema version {found}, this build knows "
                f"{schema.SCHEMA_VERSION} -- upgrade grain rather than "
                "letting it write a shape it does not understand",
            )


def _batch(statements: tuple[str, ...] | list[str]) -> str:
    parts = [s.strip().rstrip(";") for s in statements]
    parts = [p for p in parts if p]
    return ";\n".join(parts) + ";\n" if parts else ""


def _rows(stdout: str, statement: str) -> list[dict]:
    """Decode `dolt sql -r json`.

    Written to accept either a `{"rows": [...]}` envelope or a bare list,
    because the exact shape has not been confirmed against a live binary
    and both are plausible. Empty output means no rows -- a `SELECT` that
    matched nothing prints nothing in some formats.
    """
    text = stdout.strip()
    if not text:
        return []
    try:
        payload = json.loads(text)
    except json.JSONDecodeError as exc:
        raise StoreError(statement, f"could not decode dolt output: {exc}") from exc
    if isinstance(payload, dict):
        payload = payload.get("rows", [])
    if not isinstance(payload, list):
        raise StoreError(statement, f"unexpected dolt output shape: {type(payload).__name__}")
    return [row for row in payload if isinstance(row, dict)]


# --- decoding helpers ---------------------------------------------------

def _dt(value) -> datetime | None:
    if value in (None, ""):
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    text = str(value).replace("T", " ").rstrip("Z")
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S"):
        try:
            return datetime.strptime(text, fmt).replace(tzinfo=timezone.utc)
        except ValueError:
            continue
    raise ValueError(f"unparseable datetime from dolt: {value!r}")


def _int(value) -> int | None:
    return None if value in (None, "") else int(value)


def _bool(value) -> bool:
    return str(value).lower() in ("1", "true", "t", "yes")


def _principal(kind, pid) -> PrincipalRef | None:
    if kind in (None, ""):
        return None
    return PrincipalRef(kind=PrincipalKind(kind), id=str(pid or ""))


def _attribution(row: dict, prefix: str) -> Attribution | None:
    actor = _principal(row.get(f"{prefix}_actor_kind"), row.get(f"{prefix}_actor_id"))
    if actor is None:
        return None
    return Attribution(
        actor=actor,
        on_behalf_of=_principal(
            row.get(f"{prefix}_behalf_kind"), row.get(f"{prefix}_behalf_id")
        ),
    )


class TaskStore:
    """The model's read and write surface over `DoltDatabase`.

    Writes are batched per logical change -- a task and its reads, grants,
    links and tags go in one `execute` -- so a crash mid-cycle leaves
    either the whole change or none of it in the working set, and the last
    `commit()` is a known-good point regardless.
    """

    def __init__(self, db: DoltDatabase) -> None:
        self._db = db

    # --- tasks ----------------------------------------------------------
    def put_task(self, task: Task) -> None:
        """Insert or replace a task and everything hanging off it.

        Child rows are deleted and re-inserted rather than diffed: the
        sets are tiny, and "the row set equals the object" is a property
        worth having outright rather than maintaining.
        """
        statements = [
            render(
                "REPLACE INTO `task` ("
                "`id`, `intent`, `title`, `body`,"
                "`origin_actor_kind`, `origin_actor_id`,"
                "`origin_behalf_kind`, `origin_behalf_id`, `origin_reason`,"
                "`approval_actor_kind`, `approval_actor_id`,"
                "`approval_behalf_kind`, `approval_behalf_id`,"
                "`target_owner`, `target_name`, `binding`, `base`, `folder`,"
                "`auto_merge`, `external_ref`, `created_at`"
                ") VALUES ("
                "{id}, {intent}, {title}, {body},"
                "{oak}, {oai}, {obk}, {obi}, {reason},"
                "{aak}, {aai}, {abk}, {abi},"
                "{towner}, {tname}, {binding}, {base}, {folder},"
                "{auto_merge}, {external_ref}, {created_at})",
                id=task.id, intent=task.intent, title=task.title, body=task.body,
                oak=task.origin.attribution.actor.kind,
                oai=task.origin.attribution.actor.id,
                obk=_kind_of(task.origin.attribution.on_behalf_of),
                obi=_id_of(task.origin.attribution.on_behalf_of),
                reason=task.origin.reason,
                aak=_kind_of(task.approval.actor if task.approval else None),
                aai=_id_of(task.approval.actor if task.approval else None),
                abk=_kind_of(task.approval.on_behalf_of if task.approval else None),
                abi=_id_of(task.approval.on_behalf_of if task.approval else None),
                towner=task.target.owner if task.target else None,
                tname=task.target.name if task.target else None,
                binding=task.binding, base=task.base,
                folder=str(task.folder) if task.folder else None,
                auto_merge=task.auto_merge, external_ref=task.external_ref,
                created_at=task.created_at,
            ),
        ]
        for table in ("task_read", "task_grant", "task_link", "task_tag"):
            statements.append(render(
                f"DELETE FROM {ident(table)} WHERE `task_id` = {{id}}", id=task.id,
            ))
        for repo in task.reads:
            statements.append(render(
                "INSERT INTO `task_read` (`task_id`, `owner`, `name`) "
                "VALUES ({id}, {owner}, {name})",
                id=task.id, owner=repo.owner, name=repo.name,
            ))
        for grant in sorted(task.grants, key=lambda g: g.capability):
            statements.append(render(
                "INSERT INTO `task_grant` (`task_id`, `capability`, `via`, `folder`) "
                "VALUES ({id}, {cap}, {via}, {folder})",
                id=task.id, cap=grant.capability, via=grant.via,
                folder=str(grant.folder) if grant.folder else None,
            ))
        for link in task.links:
            statements.append(render(
                "INSERT INTO `task_link` (`task_id`, `kind`, `target`, `blocks`) "
                "VALUES ({id}, {kind}, {target}, {blocks})",
                id=task.id, kind=link.kind, target=link.target,
                blocks=blocks(link.kind),
            ))
        for tag in sorted(task.tags):
            statements.append(render(
                "INSERT INTO `task_tag` (`task_id`, `tag`) VALUES ({id}, {tag})",
                id=task.id, tag=tag,
            ))
        self._db.execute(*statements)

    def get_task(self, task_id: str) -> Task | None:
        rows = self._db.query(render(
            "SELECT * FROM `task` WHERE `id` = {id}", id=task_id,
        ))
        if not rows:
            return None
        return self._hydrate(rows[0])

    def _hydrate(self, row: dict) -> Task:
        task_id = str(row["id"])
        reads = tuple(
            RepoRef(owner=str(r["owner"]), name=str(r["name"]))
            for r in self._db.query(render(
                "SELECT `owner`, `name` FROM `task_read` WHERE `task_id` = {id} "
                "ORDER BY `owner`, `name`", id=task_id,
            ))
        )
        grants = frozenset(
            Grant(
                capability=str(g["capability"]), via=GrantSource(g["via"]),
                folder=FolderRef(tuple(str(g["folder"]).split("/"))) if g.get("folder") else None,
            )
            for g in self._db.query(render(
                "SELECT * FROM `task_grant` WHERE `task_id` = {id}", id=task_id,
            ))
        )
        links = tuple(
            TaskLink(kind=LinkKind(l["kind"]), target=str(l["target"]))
            for l in self._db.query(render(
                "SELECT `kind`, `target` FROM `task_link` WHERE `task_id` = {id} "
                "ORDER BY `kind`, `target`", id=task_id,
            ))
        )
        tags = frozenset(
            str(t["tag"]) for t in self._db.query(render(
                "SELECT `tag` FROM `task_tag` WHERE `task_id` = {id}", id=task_id,
            ))
        )
        origin_attribution = _attribution(row, "origin")
        assert origin_attribution is not None, "task.origin_actor_kind is NOT NULL"
        target = None
        if row.get("target_owner"):
            target = RepoRef(owner=str(row["target_owner"]), name=str(row["target_name"]))
        return Task(
            id=task_id,
            intent=TaskIntent(row["intent"]),
            origin=Origin(
                attribution=origin_attribution,
                reason=OriginReason(row["origin_reason"]),
            ),
            title=str(row.get("title") or ""),
            body=str(row.get("body") or ""),
            approval=_attribution(row, "approval"),
            target=target,
            binding=RepoBinding(row["binding"]),
            base=row.get("base") or None,
            folder=FolderRef(tuple(str(row["folder"]).split("/"))) if row.get("folder") else None,
            reads=reads, grants=grants, links=links, tags=tags,
            auto_merge=_bool(row.get("auto_merge")),
            external_ref=row.get("external_ref") or None,
            created_at=_dt(row.get("created_at")),
        )

    def approve(self, task_id: str, approval: Attribution) -> None:
        """Record who approved a task -- the whole difference between
        PROPOSED and QUEUED, and the thing withdrawing would cancel it.
        """
        self._db.execute(render(
            "UPDATE `task` SET `approval_actor_kind` = {ak}, `approval_actor_id` = {ai},"
            " `approval_behalf_kind` = {bk}, `approval_behalf_id` = {bi}"
            " WHERE `id` = {id}",
            ak=approval.actor.kind, ai=approval.actor.id,
            bk=_kind_of(approval.on_behalf_of), bi=_id_of(approval.on_behalf_of),
            id=task_id,
        ))

    # --- observations ---------------------------------------------------
    def observe(self, observation: Observation) -> None:
        self._db.execute(render(
            "REPLACE INTO `task_observation` (`task_id`, `closed_at`, `completed_at`,"
            " `pending_question_comment_id`, `baseline_comment_id`, `observed_at`)"
            " VALUES ({id}, {closed}, {completed}, {pending}, {baseline}, {observed})",
            id=observation.task_id, closed=observation.closed_at,
            completed=observation.completed_at,
            pending=observation.pending_question_comment_id,
            baseline=observation.baseline_comment_id,
            observed=observation.observed_at,
        ))

    def get_observation(self, task_id: str) -> Observation | None:
        rows = self._db.query(render(
            "SELECT * FROM `task_observation` WHERE `task_id` = {id}", id=task_id,
        ))
        if not rows:
            return None
        row = rows[0]
        return Observation(
            task_id=task_id,
            closed_at=_dt(row.get("closed_at")),
            completed_at=_dt(row.get("completed_at")),
            pending_question_comment_id=_int(row.get("pending_question_comment_id")),
            baseline_comment_id=_int(row.get("baseline_comment_id")),
            observed_at=_dt(row.get("observed_at")),
        )

    # --- runs and leases ------------------------------------------------
    def start_run(self, run: Run) -> None:
        statements = [render(
            "REPLACE INTO `task_run` (`id`, `task_id`, `slot`, `sandbox`, `unit`,"
            " `attempt`, `started_at`, `finished_at`, `outcome`)"
            " VALUES ({id}, {task}, {slot}, {sandbox}, {unit}, {attempt},"
            " {started}, {finished}, {outcome})",
            id=run.id, task=run.task_id, slot=run.slot, sandbox=run.sandbox,
            unit=run.unit, attempt=run.attempt, started=run.started_at,
            finished=run.finished_at, outcome=run.outcome,
        )]
        for lease in run.leases:
            statements.append(_insert_lease(run.id, lease))
        self._db.execute(*statements)

    def finish_run(self, run_id: str, *, finished_at: datetime,
                   outcome: str) -> None:
        self._db.execute(render(
            "UPDATE `task_run` SET `finished_at` = {finished}, `outcome` = {outcome}"
            " WHERE `id` = {id}",
            finished=finished_at, outcome=outcome, id=run_id,
        ))

    def add_lease(self, run_id: str, lease: Lease) -> None:
        self._db.execute(_insert_lease(run_id, lease))

    def drop_lease(self, run_id: str, capability: str, resource: str) -> None:
        """Forget a lease once its resource is actually revoked.

        Idempotent by construction -- a `DELETE` that matches nothing is
        not an error, which is what lets release and the expiry reaper
        both reach the same lease without coordinating.
        """
        self._db.execute(render(
            "DELETE FROM `lease` WHERE `run_id` = {run} AND `capability` = {cap}"
            " AND `resource` = {res}",
            run=run_id, cap=capability, res=resource,
        ))

    def attempts(self, task_id: str) -> int:
        """How many times this task has been run. Answerable because runs
        are rows, where previously the records existed as files nothing
        aggregated.
        """
        rows = self._db.query(render(
            "SELECT COUNT(*) AS `n` FROM `task_run` WHERE `task_id` = {id}",
            id=task_id,
        ))
        return int(rows[0]["n"]) if rows else 0

    def live_leases(self, minted_by: CredentialRef | None = None) -> list[dict]:
        """Outstanding leases, optionally only those from one credential --
        which is what makes "what would rotating this break?" a query.
        """
        if minted_by is None:
            return self._db.query("SELECT * FROM `lease_live` ORDER BY `issued_at`")
        return self._db.query(render(
            "SELECT * FROM `lease_live` WHERE `minted_by` = {name} ORDER BY `issued_at`",
            name=minted_by.name,
        ))

    def expired_leases(self, now: datetime) -> list[dict]:
        return self._db.query(render(
            "SELECT * FROM `lease_live` WHERE `expires_at` IS NOT NULL"
            " AND `expires_at` <= {now} ORDER BY `expires_at`",
            now=now,
        ))

    # --- derived --------------------------------------------------------
    def state(self, task_id: str) -> TaskState | None:
        rows = self._db.query(render(
            "SELECT `state` FROM `task_state` WHERE `task_id` = {id}", id=task_id,
        ))
        return TaskState(rows[0]["state"]) if rows else None

    def ready(self) -> list[str]:
        """Task ids dispatchable right now: approved, not running, with no
        open blocker. The whole dispatch query, as one view.
        """
        return [
            str(row["task_id"])
            for row in self._db.query("SELECT `task_id` FROM `task_ready`")
        ]

    def open_blockers(self, task_id: str) -> int:
        rows = self._db.query(render(
            "SELECT `open_blockers` FROM `task_blocked` WHERE `task_id` = {id}",
            id=task_id,
        ))
        return int(rows[0]["open_blockers"]) if rows else 0

    def commit(self, message: str) -> None:
        self._db.commit(message)


def _insert_lease(run_id: str, lease: Lease) -> str:
    return render(
        "REPLACE INTO `lease` (`run_id`, `capability`, `resource`, `minted_by`,"
        " `issued_at`, `expires_at`)"
        " VALUES ({run}, {cap}, {res}, {by}, {issued}, {expires})",
        run=run_id, cap=lease.capability, res=lease.resource,
        by=lease.minted_by.name, issued=lease.issued_at, expires=lease.expires_at,
    )


def _kind_of(principal: PrincipalRef | None):
    return principal.kind if principal else None


def _id_of(principal: PrincipalRef | None):
    return principal.id if principal else None
