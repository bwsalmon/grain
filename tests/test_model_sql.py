"""The literal renderer is the only thing standing between untrusted text
and a concatenated statement, so it is tested adversarially rather than
happily -- see `grain/model/sql.py`'s docstring for why hex and not quotes.
"""

from datetime import datetime, timezone

import pytest

from grain.model.sql import SqlError, explain, ident, literal, render
from grain.model.types import LinkKind, TaskIntent

UTC = timezone.utc


@pytest.mark.parametrize("hostile", [
    "'; DROP TABLE `task`; --",
    "\\'; DROP TABLE `task`; --",
    "o'brien",
    'say "hi"',
    "back\\slash",
    "null\x00byte",
    "newline\nand\ttab",
    "-- comment",
    "/* block */",
    "🤖 emoji",
    "`backtick`",
])
def test_no_hostile_string_can_end_its_own_literal(hostile: str):
    rendered = render("SELECT {v}", v=hostile)
    body = rendered[len("SELECT "):]
    assert body.startswith("_utf8mb4 0x")
    # The whole point: the literal is hex digits, so there is no character
    # in it that could terminate the literal or start a new statement.
    assert set(body[len("_utf8mb4 0x"):]) <= set("0123456789abcdef")
    assert explain(rendered) is not None


def test_a_hostile_string_round_trips_through_explain():
    hostile = "'; DROP TABLE `task`; --"
    assert hostile in explain(render("SELECT {v}", v=hostile)).replace("''", "'")


def test_the_empty_string_is_the_one_quoted_case():
    # `0x` with no digits is not a valid literal, and quoting is
    # unambiguously safe when there is nothing to quote.
    assert render("SELECT {v}", v="") == "SELECT ''"


def test_none_is_null_not_the_string_none():
    # The distinction carries meaning: a NULL approval is what makes a
    # task PROPOSED rather than QUEUED.
    assert render("SELECT {v}", v=None) == "SELECT NULL"


def test_booleans_render_before_integers():
    # bool is an int subclass; rendering True as 1 would still work but
    # reads badly in an explain() dump.
    assert render("SELECT {v}", v=True) == "SELECT TRUE"
    assert render("SELECT {v}", v=False) == "SELECT FALSE"
    assert render("SELECT {v}", v=0) == "SELECT 0"


def test_enums_render_as_their_values():
    assert render("SELECT {v}", v=TaskIntent.IMPLEMENT) == render("SELECT {v}", v="implement")
    assert render("SELECT {v}", v=LinkKind.DEPENDS_ON) == render("SELECT {v}", v="depends-on")


def test_an_aware_datetime_renders_as_utc():
    naive_utc = datetime(2026, 8, 27, 12, 0, tzinfo=UTC)
    assert literal(naive_utc) == "'2026-08-27 12:00:00.000000'"


def test_a_naive_datetime_is_refused_rather_than_assumed():
    with pytest.raises(SqlError, match="naive"):
        literal(datetime(2026, 8, 27, 12, 0))


def test_an_unknown_type_is_refused_rather_than_stringified():
    with pytest.raises(SqlError):
        literal({"a": 1})


def test_a_missing_parameter_is_an_error_not_a_literal_brace():
    with pytest.raises(SqlError, match="unbound"):
        render("SELECT {a}, {b}", a=1)


def test_an_unused_parameter_is_an_error():
    # The usual cause is a renamed placeholder, whose symptom would
    # otherwise be a silently unfiltered query.
    with pytest.raises(SqlError, match="unused"):
        render("SELECT {a}", a=1, b=2)


def test_braces_that_are_not_placeholders_survive():
    # SQL has braces of its own; str.format would choke on these.
    assert render("SELECT JSON_OBJECT() /* {not_a_field */") == \
        "SELECT JSON_OBJECT() /* {not_a_field */"


def test_a_string_parameter_can_never_smuggle_sql():
    # A caller wanting dynamic SQL must compose templates, which is
    # visible in review; passing a fragment as a value cannot work.
    assert "DROP" not in render("SELECT {v}", v="DROP TABLE `task`")


def test_identifiers_reject_backticks_rather_than_escaping_them():
    assert ident("task_run") == "`task_run`"
    for bad in ("has`tick", "", "nul\x00"):
        with pytest.raises(SqlError):
            ident(bad)
