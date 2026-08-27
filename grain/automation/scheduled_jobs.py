"""Scheduled jobs (bwsalmon/agents#163): issues filed automatically on a
recurring interval, from templates an operator drops in the repo, rather
than only ever starting from a human filing one by hand.

**Config: a directory of template files, not one JSON blob.** Unlike
`JanitorConfig`/`GeminiKeyConfig`, a job's own content (a title and a
markdown body that may itself contain code blocks, JSON snippets, or other
`{...}`/`"..."` text a JSON string would need escaping) doesn't fit
comfortably as a JSON value. Each job is instead one plain-text file --
`/data/config/scheduled-jobs/<name>.md` -- with a handful of `Key: Value`
header lines, a blank line, then the issue body verbatim. `<name>` (the
file's stem) is the job's identity: what `AutomationState`'s
`scheduled_job_last_fired` is keyed by, and what names the GitHub label
(`ScheduledJob.marker_label`) `core.py`'s `_scheduled_jobs` uses to find an
issue this job already filed. Adding, editing, or removing a job is
therefore adding, editing, or removing one file -- no separate manifest to
keep in sync, and (per the operator's own steer on bwsalmon/agents#163) no
inline string shoehorned into a Terraform variable either.

**Header format is deliberately not YAML frontmatter.** This project is
stdlib-only Python (`pyproject.toml`: `dependencies = []`), and a plain
`Key: Value` header block needs nothing beyond `str.partition` to read --
the same reasoning `janitor.py`/`gemini_keys.py`'s own docstrings give for
shelling out to `gcloud` rather than adding a client-library dependency.

**Rendering uses `string.Template`, not f-strings/`str.format`.** An issue
body routinely contains literal `{...}` (JSON, code blocks) that
`str.format` would choke on or silently mangle, and often contains `$1`,
`$VAR`, `${THING}` style shell text a naive substitution could corrupt.
`Template.safe_substitute` sidesteps both: `$identifier`/`${identifier}` is
the only syntax it ever treats specially, and a name outside what this
module supplies (`date`, `datetime`) is left in the output untouched
rather than raising or being blanked out.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from string import Template

# Every job's GitHub label is derived from its name, never configured
# separately -- see the module docstring on why the name (the file's own
# stem) is the one thing worth keying anything off of.
MARKER_LABEL_PREFIX = "grain-scheduled-"


@dataclass(frozen=True)
class ScheduledJob:
    name: str
    interval_hours: int
    title_template: str
    body_template: str
    # Configurable per job, in the repo, per the operator's own steer on
    # bwsalmon/agents#163 -- not a single deployment-wide default the way
    # the initial design draft proposed. `False` (routine chores dispatch
    # the moment they're filed) unless a job's own file opts in.
    needs_approval: bool = False

    @property
    def marker_label(self) -> str:
        """The label every issue this job files carries, alongside
        whichever of `trigger_label`/`needs_approval_label` it dispatches
        with. `core.py`'s `_scheduled_jobs` lists issues by this label to
        find a previous firing that hasn't finished yet, before ever
        checking `interval_hours` -- see its own docstring.
        """
        return f"{MARKER_LABEL_PREFIX}{self.name}"

    def render_title(self, now: datetime) -> str:
        return _render(self.title_template, now)

    def render_body(self, now: datetime) -> str:
        return _render(self.body_template, now)


def _render(template_text: str, now: datetime) -> str:
    return Template(template_text).safe_substitute(
        date=now.date().isoformat(), datetime=now.isoformat(),
    )


def _parse_job_file(name: str, text: str) -> ScheduledJob:
    header_text, _, body = text.partition("\n\n")
    headers: dict[str, str] = {}
    for line in header_text.splitlines():
        key, sep, value = line.partition(":")
        if not sep:
            raise ValueError(
                f"scheduled job {name!r}: malformed header line {line!r} "
                "(expected 'Key: Value')"
            )
        headers[key.strip().lower()] = value.strip()

    title = headers.get("title")
    if not title:
        raise ValueError(f"scheduled job {name!r}: missing required 'Title' header")

    interval_raw = headers.get("interval-hours")
    if not interval_raw:
        raise ValueError(f"scheduled job {name!r}: missing required 'Interval-Hours' header")
    try:
        interval_hours = int(interval_raw)
    except ValueError:
        raise ValueError(
            f"scheduled job {name!r}: 'Interval-Hours' must be an integer, got {interval_raw!r}"
        ) from None

    needs_approval = headers.get("needs-approval", "false").strip().lower() == "true"

    return ScheduledJob(
        name=name, interval_hours=interval_hours, title_template=title,
        body_template=body, needs_approval=needs_approval,
    )


@dataclass(frozen=True)
class ScheduledJobsConfig:
    """`core.py`'s `Orchestrator.scheduled_jobs_config` -- same "presence
    of the config is the on/off switch" shape `JanitorConfig`/
    `GeminiKeyConfig` already have, just over a directory instead of a
    single file: `cli.py`'s `build_orchestrator` only calls `load` when
    `/data/config/scheduled-jobs/` exists at all, and an existing but
    empty directory loads to zero jobs -- a harmless no-op, not an error.
    """

    jobs: tuple[ScheduledJob, ...]

    @classmethod
    def load(cls, dir_path: Path) -> "ScheduledJobsConfig":
        # Sorted for a deterministic firing order across jobs whose
        # intervals happen to line up on the same cycle -- otherwise
        # `Path.glob`'s own order (filesystem-dependent) would make that
        # order incidental.
        jobs = tuple(
            _parse_job_file(path.stem, path.read_text())
            for path in sorted(dir_path.glob("*.md"))
        )
        return cls(jobs=jobs)
