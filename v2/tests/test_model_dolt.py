"""The store, exercised without a `dolt` binary.

`FakeRunner` matches one response per argv prefix, and every query here
shares the same prefix (`dolt sql -r json`), so this file uses a small
scripted runner of its own that returns queued stdouts in order. The
integration test at the bottom gates itself on a real `dolt` being
present and skips otherwise -- the same shape the live suites in this
repo already use for `/dev/kvm` and libvirt.
"""

import json
import shutil
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

import pytest

from v2.model.dolt import DoltCli, DoltDatabase, StoreError, TaskStore
from v2.model.types import (
    Attribution, CredentialRef, FolderRef, Grant, GrantSource, Lease,
    LinkKind, Observation, Origin, OriginReason, PrincipalKind,
    PrincipalRef, RepoBinding, RepoRef, Run, Task, TaskIntent, TaskLink,
    TaskState,
)
from grain.run import Result

UTC = timezone.utc
NOW = datetime(2026, 8, 27, 12, 0, tzinfo=UTC)
HUMAN = PrincipalRef(PrincipalKind.HUMAN, "bwsalmon")
BOT = PrincipalRef(PrincipalKind.AUTOMATION, "grain")


@dataclass
class ScriptedRunner:
    """Returns queued stdouts in order, recording every statement."""
    queued: list[str] = field(default_factory=list)
    statements: list[str] = field(default_factory=list)
    argvs: list[list[str]] = field(default_factory=list)
    fail_with: str | None = None

    def queue(self, rows: list[dict]) -> None:
        self.queued.append(json.dumps({"rows": rows}))

    def run(self, argv, *, stdin=None, check=True):
        self.argvs.append(list(argv))
        if stdin:
            self.statements.append(stdin)
        if self.fail_with is not None:
            return Result(list(argv), 1, "", self.fail_with)
        # Only a query asks for `-r json`; a write returns no rows, so it
        # must not consume a queued response meant for the next SELECT.
        wants_rows = "json" in argv
        out = self.queued.pop(0) if (wants_rows and self.queued) else ""
        return Result(list(argv), 0, out, "")

    @property
    def sql(self) -> str:
        return "\n".join(self.statements)


def store(runner: ScriptedRunner) -> TaskStore:
    return TaskStore(DoltDatabase(runner, DoltCli()))


def a_task(**over) -> Task:
    base = dict(
        id="a1b2", intent=TaskIntent.IMPLEMENT, title="Rename the endpoint",
        origin=Origin(Attribution(HUMAN), OriginReason.DIRECT),
        approval=Attribution(HUMAN),
        target=RepoRef("owner", "payments-api"), binding=RepoBinding.DIRECTIVE,
        created_at=NOW,
    )
    base.update(over)
    return Task(**base)


# --- how statements reach dolt ------------------------------------------

def test_statements_go_over_stdin_never_argv():
    # argv has a length limit a long issue body will find, and a
    # statement on the command line is visible in the process table.
    runner = ScriptedRunner()
    store(runner).put_task(a_task(body="x" * 5000))
    assert runner.statements, "nothing was sent on stdin"
    for argv in runner.argvs:
        assert not any("REPLACE INTO" in part for part in argv)
    assert runner.argvs[0][:2] == ["dolt", "sql"]


def test_a_failing_statement_reports_it_readably():
    runner = ScriptedRunner(fail_with="Table 'task' doesn't exist")
    with pytest.raises(StoreError) as excinfo:
        store(runner).put_task(a_task())
    # Hex is unreadable; the error decodes it so an operator can see what
    # the statement actually said.
    assert "Rename the endpoint" in str(excinfo.value)


def test_untrusted_task_text_is_never_interpolated_as_sql():
    runner = ScriptedRunner()
    store(runner).put_task(a_task(title="'; DROP TABLE `task`; --"))
    assert "DROP TABLE" not in runner.sql


# --- writing a task ------------------------------------------------------

def test_put_task_writes_the_task_and_replaces_its_child_rows():
    runner = ScriptedRunner()
    task = a_task(
        reads=(RepoRef("owner", "shared-lib"),),
        grants=frozenset({Grant("gemini-key", GrantSource.LABEL)}),
        links=(TaskLink(LinkKind.DEPENDS_ON, "c3d4"),
               TaskLink(LinkKind.PROPOSED_BY, "e5f6")),
        tags=frozenset({"nightly"}),
    )
    store(runner).put_task(task)
    sql = runner.sql
    assert "REPLACE INTO `task`" in sql
    # Child rows are deleted and re-inserted so the row set equals the
    # object outright, rather than being maintained.
    for table in ("task_read", "task_grant", "task_link", "task_tag"):
        assert f"DELETE FROM `{table}`" in sql
        assert f"INSERT INTO `{table}`" in sql
    assert sql.count("INSERT INTO `task_link`") == 2


def test_a_blocking_link_is_stored_as_blocking_and_a_provenance_link_is_not():
    # `blocks` is stored rather than recomputed in SQL so `task_blocked`
    # is a plain join and the kind vocabulary lives in one place.
    runner = ScriptedRunner()
    store(runner).put_task(a_task(links=(
        TaskLink(LinkKind.DEPENDS_ON, "c3d4"),
        TaskLink(LinkKind.MERGE_WITH, "f7a8"),
    )))
    inserts = [s for s in runner.sql.splitlines() if "INSERT INTO `task_link`" in s]
    assert any("TRUE" in s for s in inserts)     # depends-on blocks
    assert any("FALSE" in s for s in inserts)    # merge-with gates the merge


def test_an_unapproved_task_writes_a_null_approval():
    runner = ScriptedRunner()
    store(runner).put_task(a_task(approval=None))
    # NULL is the whole difference between PROPOSED and QUEUED.
    assert "NULL" in runner.sql


def test_approve_records_who_and_on_whose_behalf():
    runner = ScriptedRunner()
    store(runner).approve("a1b2", Attribution(actor=BOT, on_behalf_of=HUMAN))
    sql = runner.sql
    assert "UPDATE `task` SET `approval_actor_kind`" in sql
    # grain performed it, a human meant it -- the two-principal shape.
    from v2.model.sql import explain
    assert "automation" in explain(sql) and "human" in explain(sql)


# --- reading back --------------------------------------------------------

def test_get_task_hydrates_every_collection():
    runner = ScriptedRunner()
    runner.queue([{
        "id": "a1b2", "intent": "implement", "title": "t", "body": "",
        "origin_actor_kind": "human", "origin_actor_id": "bwsalmon",
        "origin_behalf_kind": None, "origin_behalf_id": None,
        "origin_reason": "direct",
        "approval_actor_kind": "human", "approval_actor_id": "bwsalmon",
        "approval_behalf_kind": None, "approval_behalf_id": None,
        "target_owner": "owner", "target_name": "payments-api",
        "binding": "directive", "base": None, "folder": "payments/owner-api",
        "auto_merge": 0, "external_ref": "owner/agents#42",
        "created_at": "2026-08-27 12:00:00.000000",
    }])
    runner.queue([{"owner": "owner", "name": "shared-lib"}])
    runner.queue([{"capability": "gemini-key", "via": "folder", "folder": "payments"}])
    runner.queue([{"kind": "depends-on", "target": "c3d4"}])
    runner.queue([{"tag": "nightly"}])
    task = store(runner).get_task("a1b2")
    assert task is not None
    assert task.reads == (RepoRef("owner", "shared-lib"),)
    assert next(iter(task.grants)).via is GrantSource.FOLDER
    assert task.links[0].kind is LinkKind.DEPENDS_ON
    assert task.links[0].blocks is True
    assert task.tags == frozenset({"nightly"})
    assert task.folder == FolderRef(("payments", "owner-api"))
    assert task.created_at == NOW
    # The external ref is a projection, not the identity.
    assert task.id == "a1b2" and task.external_ref == "owner/agents#42"


def test_get_task_returns_none_when_absent():
    runner = ScriptedRunner()
    runner.queue([])
    assert store(runner).get_task("nope") is None


def test_empty_output_decodes_as_no_rows():
    # Some result formats print nothing for a SELECT that matched nothing.
    runner = ScriptedRunner()
    assert store(runner).ready() == []


def test_unparseable_output_is_reported_against_its_statement():
    runner = ScriptedRunner()
    runner.queued.append("not json at all")
    with pytest.raises(StoreError, match="could not decode"):
        store(runner).ready()


# --- runs and leases -----------------------------------------------------

def test_start_run_writes_the_run_and_its_leases_in_one_batch():
    runner = ScriptedRunner()
    run = Run(
        id="r001", task_id="a1b2", slot="sandbox-1", sandbox="sandbox-1",
        started_at=NOW, unit="grain-task-sandbox-1",
        leases=(Lease(
            capability="gemini-key", resource="projects/p/keys/k",
            minted_by=CredentialRef("gcp-host-service-account"),
            issued_at=NOW, expires_at=NOW + timedelta(hours=24),
        ),),
    )
    store(runner).start_run(run)
    # One invocation: a crash leaves the whole change or none of it, and
    # the last dolt commit is a known-good point regardless.
    assert len(runner.statements) == 1
    assert "REPLACE INTO `task_run`" in runner.sql
    assert "REPLACE INTO `lease`" in runner.sql


def test_a_live_run_is_one_with_no_finished_at():
    runner = ScriptedRunner()
    store(runner).start_run(Run(
        id="r001", task_id="a1b2", slot="s1", sandbox="s1", started_at=NOW,
    ))
    assert "NULL" in runner.sql  # finished_at -- this is the assignment


def test_dropping_a_lease_is_idempotent_by_construction():
    # Release and the expiry reaper can both reach the same lease; a
    # DELETE that matches nothing is not an error.
    runner = ScriptedRunner()
    store(runner).drop_lease("r001", "gemini-key", "projects/p/keys/k")
    assert runner.sql.startswith("DELETE FROM `lease`")


def test_live_leases_can_be_asked_for_by_minting_credential():
    # "What would I break by rotating this?" -- a query, where nothing
    # could answer it before.
    runner = ScriptedRunner()
    runner.queue([{"minted_by": "gcp-host-service-account", "resource": "k"}])
    rows = store(runner).live_leases(CredentialRef("gcp-host-service-account"))
    assert rows and rows[0]["resource"] == "k"
    assert "`lease_live`" in runner.sql


def test_attempts_counts_runs_for_a_task():
    runner = ScriptedRunner()
    runner.queue([{"n": 4}])
    assert store(runner).attempts("a1b2") == 4


# --- derived -------------------------------------------------------------

def test_state_is_read_from_the_view_never_from_a_column():
    runner = ScriptedRunner()
    runner.queue([{"state": "queued"}])
    assert store(runner).state("a1b2") is TaskState.QUEUED
    assert "`task_state`" in runner.sql
    assert "FROM `task` " not in runner.sql


def test_ready_returns_dispatchable_ids():
    runner = ScriptedRunner()
    runner.queue([{"task_id": "a1b2"}, {"task_id": "c3d4"}])
    assert store(runner).ready() == ["a1b2", "c3d4"]
    assert "`task_ready`" in runner.sql


def test_open_blockers_defaults_to_zero_when_the_view_has_no_row():
    runner = ScriptedRunner()
    runner.queue([])
    assert store(runner).open_blockers("a1b2") == 0


# --- schema versioning ---------------------------------------------------

def test_initialize_stamps_the_version_on_a_fresh_database():
    runner = ScriptedRunner()
    runner.queue([])  # no grain_schema row yet
    DoltDatabase(runner, DoltCli()).initialize()
    assert "CREATE TABLE IF NOT EXISTS `task`" in runner.sql
    assert "CREATE OR REPLACE VIEW `task_state`" in runner.sql
    assert "INSERT INTO `grain_schema`" in runner.sql


def test_initialize_refuses_a_database_from_a_newer_build():
    # Better than failing later with a confusing missing column.
    runner = ScriptedRunner()
    runner.queue([{"version": 999}])
    with pytest.raises(StoreError, match="schema version 999"):
        DoltDatabase(runner, DoltCli()).initialize()


def test_observations_round_trip_their_baselines():
    runner = ScriptedRunner()
    store(runner).observe(Observation(
        task_id="a1b2", pending_question_comment_id=12345, observed_at=NOW,
    ))
    assert "REPLACE INTO `task_observation`" in runner.sql
    assert "12345" in runner.sql


# --- against a real dolt, when there is one ------------------------------

@pytest.mark.skipif(shutil.which("dolt") is None, reason="no dolt binary")
def test_schema_applies_and_the_views_answer(tmp_path):
    """The one test that proves the DDL is valid and the views work.

    Everything above tests the SQL grain *generates*; only this proves
    Dolt accepts it. It skips where there is no binary, the same way this
    repo's libvirt and KVM suites already do.
    """
    from grain.run import RealRunner
    runner = RealRunner()
    cli = DoltCli(data_dir=tmp_path)
    runner.run(cli.init())
    db = DoltDatabase(runner, cli)
    db.initialize()
    store_ = TaskStore(db)

    blocker = a_task(id="c3d4", title="the blocker")
    blocked = a_task(id="a1b2", links=(TaskLink(LinkKind.DEPENDS_ON, "c3d4"),))
    store_.put_task(blocker)
    store_.put_task(blocked)

    assert store_.state("a1b2") is TaskState.QUEUED
    assert store_.open_blockers("a1b2") == 1
    assert store_.ready() == ["c3d4"]          # blocked task is not ready

    store_.observe(Observation(task_id="c3d4", closed_at=NOW))
    assert store_.state("c3d4") is TaskState.CLOSED
    assert store_.open_blockers("a1b2") == 0
    assert store_.ready() == ["a1b2"]          # unblocks itself, no reply needed

    store_.start_run(Run(id="r1", task_id="a1b2", slot="s1", sandbox="s1",
                         started_at=NOW))
    assert store_.state("a1b2") is TaskState.RUNNING
    assert store_.attempts("a1b2") == 1
