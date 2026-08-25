"""Slash directives in a task issue's text: which repo the work is for.

The orchestrator polls exactly one repo — the *task repo*, the agent set's
queue (docs/design.md, "Issue intake"). The work itself almost never
belongs to that repo, so every task has to name its own **target repo**:
where the clone comes from, where the branch is pushed, and where the pull
request is opened. This module is the parser for how a task says so.

    /repo owner/name     the target repo (required, unless the deployment
                         configures `default_target_repo`)
    /pr 42               optional: continue that PR in the target repo,
                         instead of starting a fresh branch
    /base develop        optional: PR base override, instead of the target
                         repo's own default branch
    /gemini-key          optional: mint a short-lived Gemini API key for
                         this task (bwsalmon/agents#47), placed in the
                         sandbox and revoked when the task ends. Takes no
                         value -- unlike the three above, there is nothing
                         to name, just on or off.

A body line, not a label: a `repo:owner/name` label would have to exist in
the task repo before it could be applied, is awkward to create once per
target, and could not carry a PR number or a base branch besides. One
mechanism covers all three.

**Where directives are read from.** The issue body, plus the bodies of
comments from *trusted* authors (`core.py`'s `_TRUSTED_REPLY_ASSOCIATIONS`
— the same "can apply a label" tier the trigger gate itself relies on).
Later texts override earlier ones, so a maintainer can repair a task by
replying `/repo owner/name` rather than editing the original body, and the
existing awaiting-reply promotion (docs/roadmap.md item 13) picks it back
up on the next cycle. This opens no new gate: an arbitrary public
commenter's `author_association` keeps them out of `texts` entirely, so
directives are only ever read from someone who could have labelled the
issue in the first place.

`parse_directives` raises `DirectiveError` rather than returning a partial
result — its message is written to be read by a human, since `core.py`
posts it verbatim as the comment explaining why a task was parked.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Sequence

# `owner/name`, in the character set GitHub itself allows for both halves.
# Anchored: a repo reference with anything else in it (a URL, a trailing
# comment, a `#42` suffix) is a mistake worth reporting, not something to
# silently salvage a prefix from.
_REPO_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$")

# A directive line: `/name value`, optionally indented, nothing after the
# value. Only the names below are directives — any other `/...` line is
# left alone (a prose line starting with an absolute path, a Markdown list
# of shell commands), which is why the name is matched from a fixed set
# here rather than by shape.
_DIRECTIVE_RE = re.compile(r"^\s*/(repo|pr|base)\s+(\S+)\s*$")

# `/gemini-key` takes no value -- a flag, not a `name value` pair, so it
# needs its own line shape rather than folding into `_DIRECTIVE_RE`.
_FLAG_DIRECTIVE_RE = re.compile(r"^\s*/(gemini-key)\s*$")


@dataclass(frozen=True)
class RepoRef:
    """An `owner/name` pair, kept together because nothing ever wants one
    half alone — every GitHub call, the allowlist check, and the audit line
    all take both.
    """

    owner: str
    name: str

    def __str__(self) -> str:
        return f"{self.owner}/{self.name}"

    @classmethod
    def parse(cls, text: str, *, what: str = "repo") -> "RepoRef":
        candidate = text.strip()
        if not _REPO_RE.match(candidate):
            raise DirectiveError(
                f"{what} must be `owner/name` (for example "
                f"`grain-project/some-service`), got `{candidate}`"
            )
        owner, _, name = candidate.partition("/")
        return cls(owner=owner, name=name)


@dataclass(frozen=True)
class Directives:
    """What a task's text asked for. Every field is optional here —
    "a target repo is required" is `core.py`'s rule (it also has the
    deployment's `default_target_repo` to fall back on), not this parser's.
    """

    target: RepoRef | None = None
    pr: int | None = None
    base: str | None = None
    # A flag, not a value: once any trusted text asks for it, it stays
    # asked-for -- there is no `/no-gemini-key` to retract it, the same way
    # there is no way to un-apply the trigger label itself.
    gemini_key: bool = False


class DirectiveError(ValueError):
    """A directive that can't be honoured. The message is human-facing:
    `core.py` posts it as the comment explaining why the task was parked,
    so it names what was wrong and what a valid line looks like.
    """


def parse_directives(texts: Sequence[str]) -> Directives:
    """Directives across `texts`, later texts overriding earlier ones.

    Within a *single* text, two directives of the same name conflict rather
    than override: `/repo a/b` and `/repo c/d` in one issue body is an
    ambiguity about where the work lands, and guessing at it is exactly the
    kind of thing worth failing closed on. Repeating the identical value is
    fine (a quoted body, a restated line). Across texts the ordering is
    deliberate and unambiguous — a later comment is a maintainer amending
    an earlier instruction — so that case overrides silently.
    """
    result = Directives()
    for text in texts:
        result = _apply(result, _parse_one(text))
    return result


def strip_directives(text: str) -> str:
    """`text` with its directive lines removed — what the agent's prompt
    carries. The directives are addressed to the orchestrator, not to the
    agent: it never chooses its own target repo (it has no GitHub API
    access to act on one anyway, per docs/design.md's split surface), and a
    stray `/repo ...` line in a prompt reads as an instruction it might try
    to follow.
    """
    kept = [
        line for line in text.splitlines()
        if not _DIRECTIVE_RE.match(line) and not _FLAG_DIRECTIVE_RE.match(line)
    ]
    return "\n".join(kept).strip()


def _parse_one(text: str) -> Directives:
    found: dict[str, str] = {}
    gemini_key = False
    for line in text.splitlines():
        if _FLAG_DIRECTIVE_RE.match(line):
            gemini_key = True
            continue
        match = _DIRECTIVE_RE.match(line)
        if match is None:
            continue
        name, value = match.group(1), match.group(2)
        if name in found and found[name] != value:
            raise DirectiveError(
                f"two different `/{name}` directives in the same text "
                f"(`{found[name]}` and `{value}`) -- which one applies is "
                "ambiguous, so nothing was dispatched"
            )
        found[name] = value
    return Directives(
        target=RepoRef.parse(found["repo"], what="`/repo`") if "repo" in found else None,
        pr=_parse_pr(found["pr"]) if "pr" in found else None,
        base=found["base"] if "base" in found else None,
        gemini_key=gemini_key,
    )


def _parse_pr(value: str) -> int:
    number = value.lstrip("#")
    if not number.isdigit() or int(number) <= 0:
        raise DirectiveError(
            f"`/pr` must be a pull request number (for example `/pr 42`), "
            f"got `{value}`"
        )
    return int(number)


def _apply(base: Directives, override: Directives) -> Directives:
    return Directives(
        target=override.target if override.target is not None else base.target,
        pr=override.pr if override.pr is not None else base.pr,
        base=override.base if override.base is not None else base.base,
        gemini_key=base.gemini_key or override.gemini_key,
    )
