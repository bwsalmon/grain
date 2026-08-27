"""`TaskState` is computed twice on purpose -- once as a SQL view, so the
store makes "state is never written" structural, and once in Python, so
code holding a `Task` needs no database. Two implementations of one rule
is a drift risk, and this suite is what pays for it.
"""

import re

from grain.model import schema
from grain.model.types import (
    Attribution, Observation, Origin, OriginReason, PrincipalKind,
    PrincipalRef, Task, TaskIntent, TaskState, state_of,
)

HUMAN = PrincipalRef(PrincipalKind.HUMAN, "someone")
NOW = __import__("datetime").datetime(2026, 8, 27, 12, 0,
                                      tzinfo=__import__("datetime").timezone.utc)


def _task(*, approved: bool) -> Task:
    return Task(
        id="a1b2", intent=TaskIntent.IMPLEMENT, title="t",
        origin=Origin(Attribution(HUMAN), OriginReason.DIRECT),
        approval=Attribution(HUMAN) if approved else None,
    )


def _view_precedence() -> list[str]:
    """The state strings in `task_state`'s CASE, in the order the CASE
    tests them -- which *is* the view's precedence.
    """
    view = [v for v in schema.VIEWS if "`task_state`" in v][0]
    return re.findall(r"THEN '(\w+)'", view) + re.findall(r"ELSE '(\w+)'", view)


def _python_precedence() -> list[str]:
    """The same precedence, discovered by turning each condition on from
    the most significant down and seeing what `state_of` returns.
    """
    approved = _task(approved=True)
    order = [
        state_of(approved, Observation("a1b2", closed_at=NOW), True),
        state_of(approved, Observation("a1b2", completed_at=NOW), True),
        state_of(approved, Observation("a1b2", pending_question_comment_id=5), True),
        state_of(approved, None, True),
        state_of(_task(approved=False), None, False),
        state_of(approved, None, False),
    ]
    return [s.value for s in order]


def test_the_view_and_the_python_derivation_agree_on_precedence():
    # If somebody reorders one CASE branch and not the other, this is
    # what says so -- the failure is otherwise a task in the wrong state
    # only when two conditions happen to be true at once.
    assert _view_precedence() == _python_precedence()


def test_every_task_state_is_reachable_from_the_derivation():
    assert set(_python_precedence()) == {s.value for s in TaskState}


def test_blocked_is_not_one_of_the_states():
    # Blocked is an annotation on QUEUED, derived from links, and lives in
    # its own view for exactly that reason.
    assert "blocked" not in {s.value for s in TaskState}
    assert "blocked" not in _view_precedence()


def test_state_is_a_view_and_not_a_column():
    # The invariant this schema exists to make structural: there is no
    # column to write, so no finish path can write one.
    task_table = [t for t in schema.TABLES if "CREATE TABLE IF NOT EXISTS `task` " in t][0]
    assert "`state`" not in task_table
    assert "state_since" not in task_table
    assert any("VIEW `task_state`" in v for v in schema.VIEWS)


def test_declaration_and_observation_are_separate_tables():
    # They answer to different records, and keeping them apart is what
    # would let a declaration change be branched and reviewed while
    # observations keep landing on the trunk.
    task_table = [t for t in schema.TABLES if "CREATE TABLE IF NOT EXISTS `task` " in t][0]
    for observed in ("closed_at", "completed_at", "baseline_comment_id"):
        assert observed not in task_table
    assert any("`task_observation`" in t for t in schema.TABLES)


def test_no_table_uses_a_reserved_word_unquoted():
    # GRANT, READ and READS are all reserved and all nouns this model
    # wants, which is why every table is prefixed and backtick-quoted.
    for statement in schema.statements():
        names = re.findall(r"CREATE (?:TABLE IF NOT EXISTS|OR REPLACE VIEW) (\S+)", statement)
        assert names, statement[:60]
        for name in names:
            assert name.startswith("`") and name.endswith("`"), name


def test_ready_is_derived_from_state_and_blockers_not_stored():
    ready = [v for v in schema.VIEWS if "`task_ready`" in v][0]
    assert "'queued'" in ready
    assert "open_blockers" in ready
