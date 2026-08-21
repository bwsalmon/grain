from datetime import datetime, timedelta, timezone
from pathlib import Path

from grain.automation.state import AutomationState


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
