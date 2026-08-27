"""Rendering SQL literals safely, for a database reached by shelling out.

`dolt` embeds only in Go, and this project's controller is Python, so the
store talks to Dolt the same way `gcp_keys.py` talks to GCP: by running
the vendor's own CLI (see `dolt.py`'s docstring for why that, and not a
MySQL driver). The cost of that choice is the thing this module exists to
pay -- **a CLI has no bind parameters**, so every value is concatenated
into a statement, and every value in this model ultimately comes from
somewhere untrusted: an issue title, a repo name, a comment body, an
agent's own proposed task.

**Strings are rendered as hex, not as quoted text.** The usual approach
is to escape a value into `'...'`, and it is the approach that goes wrong
quietly. Doubling `'` is portable; backslash escaping is *not* -- whether
`\\` is an escape character at all depends on the server's
`NO_BACKSLASH_ESCAPES` sql_mode, which this code does not set and cannot
see. Getting that wrong does not fail loudly, it produces a statement
that means something else.

A hex literal with a character-set introducer -- `_utf8mb4 0x68656c6c6f`
-- sidesteps the question entirely. There is no quote character in the
output, so there is nothing to escape, nothing for a sql_mode to
reinterpret, and no way for input to end the literal early. It is
unreadable in a log, which is a real cost and the reason `explain()`
exists below; it is also the only form this module could be confident was
correct without a live server to test escaping against.

Two values are rendered as ordinary quoted text, and both are safe by
construction rather than by escaping: timestamps and identifiers. A
`datetime` is formatted from its own numeric fields, so it cannot contain
a quote; an identifier is checked for backticks and rejected rather than
escaped, because every identifier in this codebase is a literal written
in `schema.py` and one containing a backtick is a bug, not input.
"""

from __future__ import annotations

import re
from datetime import datetime, timezone
from enum import Enum
from typing import Any

# `{name}` in a template. Deliberately not `str.format`: SQL contains
# braces of its own (JSON paths, `JSON_OBJECT` bodies), and `format` would
# treat them as fields and fail -- or worse, substitute one.
_PLACEHOLDER = re.compile(r"\{([a-z_][a-z0-9_]*)\}")

# What `render` refuses to leave unfilled. A template naming a parameter
# the caller did not supply is a bug that must not reach the server as a
# literal `{name}`.
class SqlError(ValueError):
    """A template or value this module will not render."""


def literal(value: Any) -> str:
    """One SQL literal for `value`, safe to concatenate into a statement.

    `None` is `NULL`, not the string "None" -- the distinction carries
    real meaning throughout this model (an unapproved task's
    `approval_actor` versus an approved one's).
    """
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        # Before int: bool is an int subclass, and `TRUE`/`FALSE` read
        # better in an `explain()` dump than 1/0.
        return "TRUE" if value else "FALSE"
    if isinstance(value, Enum):
        return literal(value.value)
    if isinstance(value, int):
        return str(value)
    if isinstance(value, datetime):
        return _timestamp(value)
    if isinstance(value, str):
        return _hex(value)
    raise SqlError(
        f"no SQL literal for {type(value).__name__}; convert it at the call "
        "site so the conversion is visible where the schema is known"
    )


def ident(name: str) -> str:
    """A backtick-quoted identifier. Rejects backticks rather than
    escaping them: every identifier here is a constant in `schema.py`, so
    one arriving with a backtick in it is a programming error and should
    say so rather than being quietly doubled into something that works.
    """
    if not name or "`" in name or "\x00" in name:
        raise SqlError(f"unusable SQL identifier: {name!r}")
    return f"`{name}`"


def render(template: str, **params: Any) -> str:
    """`template` with each `{name}` replaced by `literal(params[name])`.

    The only sanctioned way to build a statement in this package. It is
    deliberately not possible to pass a pre-rendered fragment through
    here as a value -- a `str` parameter becomes a *string literal*, never
    SQL -- so a caller that wants dynamic SQL has to compose templates,
    which is visible in review in a way a string argument would not be.
    """
    missing: list[str] = []

    def substitute(match: re.Match[str]) -> str:
        name = match.group(1)
        if name not in params:
            missing.append(name)
            return ""
        return literal(params[name])

    rendered = _PLACEHOLDER.sub(substitute, template)
    if missing:
        raise SqlError(
            f"template referenced unbound parameter(s): {', '.join(sorted(set(missing)))}"
        )
    unused = set(params) - set(_PLACEHOLDER.findall(template))
    if unused:
        # Loud, because the usual cause is a renamed placeholder and the
        # symptom would otherwise be a silently unfiltered query.
        raise SqlError(f"unused parameter(s): {', '.join(sorted(unused))}")
    return rendered


def explain(sql: str) -> str:
    """`sql` with hex literals decoded back to text, for logs and errors.

    Hex is unreadable, and an operator reading a failed statement needs to
    see what it said. Never used to build anything -- this is output only.
    """
    def decode(match: re.Match[str]) -> str:
        try:
            text = bytes.fromhex(match.group(1)).decode("utf-8")
        except (ValueError, UnicodeDecodeError):
            return match.group(0)
        return "'" + text.replace("'", "''") + "'"

    return re.sub(r"_utf8mb4 0x([0-9a-f]+)", decode, sql)


def _hex(text: str) -> str:
    # `0x` with no digits is not a valid literal, so the empty string is
    # the one case that has to be spelled the ordinary way -- and it is
    # the one case where quoting is unambiguously safe.
    if text == "":
        return "''"
    return "_utf8mb4 0x" + text.encode("utf-8").hex()


def _timestamp(value: datetime) -> str:
    """A `DATETIME(6)` literal in UTC.

    Naive datetimes are refused rather than assumed local or assumed UTC:
    this project stamps everything with `state.utcnow()`, so a naive value
    reaching here means somebody built one by hand and the intended zone
    is genuinely unknown.
    """
    if value.tzinfo is None:
        raise SqlError(
            "refusing to store a naive datetime; use an aware one "
            "(grain.automation.state.utcnow)"
        )
    utc = value.astimezone(timezone.utc)
    # Built from the value's own integer fields, so it cannot contain a
    # quote and needs no escaping -- see the module docstring.
    return "'" + utc.strftime("%Y-%m-%d %H:%M:%S.%f") + "'"
