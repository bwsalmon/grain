import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from grain.automation.state import AutomationState, TriggerKind


def test_free_sandbox_skips_assigned_ones():
    state = AutomationState()
    state.assign("sandbox-0", issue=1, unit="grain-task-sandbox-0",
                 now=datetime(2026, 1, 1, tzinfo=timezone.utc))
    assert state.free_sandbox(["sandbox-0", "sandbox-1"]) == "sandbox-1"


def test_free_sandbox_is_none_when_the_pool_is_full():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    state.assign("sandbox-1", issue=2, unit="u1", now=now)
    assert state.free_sandbox(["sandbox-0", "sandbox-1"]) is None


def test_release_frees_the_slot():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    state.release("sandbox-0")
    assert state.free_sandbox(["sandbox-0"]) == "sandbox-0"


def test_in_progress_issues_reflects_current_assignments():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=42, unit="u0", now=now)
    assert state.in_progress_issues() == {42}


def test_record_run_trims_entries_older_than_a_day():
    state = AutomationState()
    now = datetime(2026, 1, 2, tzinfo=timezone.utc)
    state.record_run(now - timedelta(hours=25))
    state.record_run(now - timedelta(minutes=5))
    state.record_run(now)
    assert len(state.run_timestamps) == 2


def test_save_and_load_round_trip(tmp_path: Path):
    state = AutomationState()
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0", now=now)
    state.record_run(now)
    path = tmp_path / "state" / "automation" / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assert loaded.assignments["sandbox-0"].issue == 7
    assert loaded.assignments["sandbox-0"].unit == "grain-task-sandbox-0"
    assert loaded.assignments["sandbox-0"].started_at == now
    assert loaded.run_timestamps == [now]


def test_load_of_a_missing_file_is_an_empty_state(tmp_path: Path):
    state = AutomationState.load(tmp_path / "does-not-exist.json")
    assert state.assignments == {}
    assert state.run_timestamps == []


def test_save_is_atomic_no_partial_file_left_behind(tmp_path: Path):
    path = tmp_path / "state.json"
    AutomationState().save(path)
    assert path.exists()
    assert not (tmp_path / "state.json.tmp").exists()


# --- PR-triggered assignments (docs/roadmap.md item 9) ---------------------

def test_assign_defaults_to_issue_kind_with_no_branch():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    assert state.assignments["sandbox-0"].kind is TriggerKind.ISSUE
    assert state.assignments["sandbox-0"].branch is None


def test_assign_records_pr_kind_and_branch():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=9, unit="u0", now=now,
                 kind=TriggerKind.PR, branch="feature-x")
    assignment = state.assignments["sandbox-0"]
    assert assignment.kind is TriggerKind.PR
    assert assignment.branch == "feature-x"


def test_in_progress_issues_includes_pr_numbers_too():
    # docs/roadmap.md item 9's key fact: issues and PRs share one number
    # sequence per repo, so `in_progress_issues()` needs no change at all to
    # already dedupe correctly across both trigger kinds.
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=42, unit="u0", now=now, kind=TriggerKind.PR, branch="b")
    assert state.in_progress_issues() == {42}


def test_save_and_load_round_trips_pr_kind_and_branch(tmp_path: Path):
    state = AutomationState()
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0", now=now,
                 kind=TriggerKind.PR, branch="feature-x")
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assignment = loaded.assignments["sandbox-0"]
    assert assignment.kind is TriggerKind.PR
    assert assignment.branch == "feature-x"


def test_load_of_a_pre_item_9_state_file_defaults_to_issue_kind(tmp_path: Path):
    # A state file written before item 9 existed has neither "kind" nor
    # "branch" keys at all — every assignment it could hold was necessarily
    # an issue dispatch, so loading it must default exactly that way rather
    # than raising a KeyError.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({
        "assignments": {
            "sandbox-0": {
                "issue": 7, "unit": "grain-task-sandbox-0",
                "started_at": "2026-01-01T12:00:00+00:00",
            },
        },
        "run_timestamps": [],
    }))
    loaded = AutomationState.load(path)
    assignment = loaded.assignments["sandbox-0"]
    assert assignment.kind is TriggerKind.ISSUE
    assert assignment.branch is None
