from datetime import datetime, timezone
from pathlib import Path

import pytest

from grain.automation.scheduled_jobs import ScheduledJobsConfig

NOW = datetime(2026, 8, 27, 12, 30, 0, tzinfo=timezone.utc)


def write_job(dir_path: Path, filename: str, text: str) -> None:
    dir_path.mkdir(parents=True, exist_ok=True)
    (dir_path / filename).write_text(text)


def test_loads_a_well_formed_job_file(tmp_path: Path):
    write_job(tmp_path, "weekly-audit.md",
              "Title: Weekly audit\nInterval-Hours: 168\n\nPlease audit things.")

    config = ScheduledJobsConfig.load(tmp_path)

    assert len(config.jobs) == 1
    job = config.jobs[0]
    assert job.name == "weekly-audit"
    assert job.interval_hours == 168
    assert job.needs_approval is False
    assert job.render_title(NOW) == "Weekly audit"
    assert job.render_body(NOW) == "Please audit things."


def test_needs_approval_true_is_parsed_case_insensitively(tmp_path: Path):
    write_job(tmp_path, "risky.md",
              "Title: Risky job\nInterval-Hours: 24\nNeeds-Approval: True\n\nBe careful.")

    config = ScheduledJobsConfig.load(tmp_path)

    assert config.jobs[0].needs_approval is True


def test_marker_label_is_derived_from_the_file_name(tmp_path: Path):
    write_job(tmp_path, "dependency-audit.md",
              "Title: t\nInterval-Hours: 1\n\nb")

    config = ScheduledJobsConfig.load(tmp_path)

    assert config.jobs[0].marker_label == "grain-scheduled-dependency-audit"


def test_title_and_body_templates_substitute_date_and_datetime(tmp_path: Path):
    write_job(tmp_path, "dated.md",
              "Title: Report for $date\nInterval-Hours: 24\n\n"
              "Generated at $datetime.\nBraces are fine: {\"a\": 1}. Shell vars too: $1 $FOO ${BAR}")

    config = ScheduledJobsConfig.load(tmp_path)
    job = config.jobs[0]

    assert job.render_title(NOW) == "Report for 2026-08-27"
    body = job.render_body(NOW)
    assert "Generated at 2026-08-27T12:30:00+00:00." in body
    # $1 isn't a valid Template identifier (doesn't start with a letter/
    # underscore) and $FOO/${BAR} aren't in the substitution mapping --
    # safe_substitute leaves all three untouched rather than raising.
    assert '{"a": 1}' in body
    assert "$1 $FOO ${BAR}" in body


def test_an_empty_directory_loads_to_no_jobs(tmp_path: Path):
    config = ScheduledJobsConfig.load(tmp_path)

    assert config.jobs == ()


def test_jobs_load_in_sorted_filename_order(tmp_path: Path):
    write_job(tmp_path, "zeta.md", "Title: z\nInterval-Hours: 1\n\nb")
    write_job(tmp_path, "alpha.md", "Title: a\nInterval-Hours: 1\n\nb")

    config = ScheduledJobsConfig.load(tmp_path)

    assert [job.name for job in config.jobs] == ["alpha", "zeta"]


def test_missing_title_header_raises(tmp_path: Path):
    write_job(tmp_path, "bad.md", "Interval-Hours: 1\n\nbody")

    with pytest.raises(ValueError, match="Title"):
        ScheduledJobsConfig.load(tmp_path)


def test_missing_interval_hours_header_raises(tmp_path: Path):
    write_job(tmp_path, "bad.md", "Title: t\n\nbody")

    with pytest.raises(ValueError, match="Interval-Hours"):
        ScheduledJobsConfig.load(tmp_path)


def test_non_integer_interval_hours_raises(tmp_path: Path):
    write_job(tmp_path, "bad.md", "Title: t\nInterval-Hours: soon\n\nbody")

    with pytest.raises(ValueError, match="integer"):
        ScheduledJobsConfig.load(tmp_path)


def test_a_header_line_with_no_colon_raises(tmp_path: Path):
    write_job(tmp_path, "bad.md", "Title t\nInterval-Hours: 1\n\nbody")

    with pytest.raises(ValueError, match="malformed header"):
        ScheduledJobsConfig.load(tmp_path)
