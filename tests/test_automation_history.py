from datetime import datetime, timezone
from pathlib import Path

from grain.automation.history import (
    FileSessionHistory, NullSessionHistory, RecordingSessionHistory,
)
from grain.automation.state import TriggerKind

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
LATER = datetime(2026, 1, 1, 12, 5, tzinfo=timezone.utc)


def test_record_with_a_transcript_writes_both_files(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    record = history.record(
        issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="grain-task-sandbox-0",
        started_at=NOW, finished_at=LATER, outcome="succeeded",
        transcript_text='{"type": "result", "result": "done"}\n',
    )
    assert record.transcript_path is not None
    assert Path(record.transcript_path).exists()
    assert Path(record.transcript_path).read_text() == '{"type": "result", "result": "done"}\n'
    json_files = list(tmp_path.glob("*.json"))
    assert len(json_files) == 1


def test_record_with_no_transcript_leaves_transcript_path_none(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    record = history.record(
        issue=7, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="grain-task-sandbox-0",
        started_at=NOW, finished_at=LATER, outcome="stranded", transcript_text=None,
    )
    assert record.transcript_path is None
    assert not list(tmp_path.glob("*.jsonl"))


def test_all_round_trips_a_record(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    history.record(
        issue=7, kind=TriggerKind.PR, sandbox="sandbox-0", unit="grain-task-sandbox-0",
        started_at=NOW, finished_at=LATER, outcome="succeeded", transcript_text="hi\n",
    )
    [loaded] = history.all()
    assert loaded.issue == 7
    assert loaded.kind is TriggerKind.PR
    assert loaded.sandbox == "sandbox-0"
    assert loaded.unit == "grain-task-sandbox-0"
    assert loaded.started_at == NOW
    assert loaded.finished_at == LATER
    assert loaded.outcome == "succeeded"


def test_all_is_sorted_newest_first(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    older = NOW
    newer = LATER
    history.record(issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=older, finished_at=older, outcome="succeeded",
                    transcript_text=None)
    history.record(issue=2, kind=TriggerKind.ISSUE, sandbox="sandbox-1", unit="u1",
                    started_at=newer, finished_at=newer, outcome="succeeded",
                    transcript_text=None)
    records = history.all()
    assert [r.issue for r in records] == [2, 1]


def test_for_trigger_filters_to_one_issue_number(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    history.record(issue=5, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=NOW, finished_at=NOW, outcome="succeeded",
                    transcript_text=None)
    history.record(issue=6, kind=TriggerKind.ISSUE, sandbox="sandbox-1", unit="u1",
                    started_at=NOW, finished_at=NOW, outcome="failed",
                    transcript_text=None)
    records = history.for_trigger(5)
    assert [r.issue for r in records] == [5]


def test_repeated_dispatches_of_the_same_trigger_do_not_collide(tmp_path: Path):
    # A requeued issue redispatched later must get its own record, not
    # overwrite the earlier attempt's.
    history = FileSessionHistory(tmp_path)
    history.record(issue=5, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=NOW, finished_at=NOW, outcome="failed",
                    transcript_text=None)
    history.record(issue=5, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=LATER, finished_at=LATER, outcome="succeeded",
                    transcript_text="second attempt\n")
    records = history.for_trigger(5)
    assert len(records) == 2
    outcomes = {r.outcome for r in records}
    assert outcomes == {"failed", "succeeded"}


def test_read_transcript_returns_empty_string_when_none_captured(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    record = history.record(
        issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
        started_at=NOW, finished_at=NOW, outcome="stranded", transcript_text=None,
    )
    assert history.read_transcript(record) == ""


def test_read_transcript_returns_the_captured_content(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    record = history.record(
        issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
        started_at=NOW, finished_at=NOW, outcome="succeeded",
        transcript_text='{"type": "assistant"}\n',
    )
    assert history.read_transcript(record) == '{"type": "assistant"}\n'


def test_save_is_atomic_no_partial_file_left_behind(tmp_path: Path):
    history = FileSessionHistory(tmp_path)
    history.record(issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=NOW, finished_at=NOW, outcome="succeeded",
                    transcript_text=None)
    assert not list(tmp_path.glob("*.tmp"))


def test_root_directory_is_created_if_missing(tmp_path: Path):
    root = tmp_path / "sessions"
    assert not root.exists()
    FileSessionHistory(root)
    assert root.exists()


def test_null_session_history_records_nothing():
    history = NullSessionHistory()
    assert history.record(
        issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
        started_at=NOW, finished_at=NOW, outcome="succeeded", transcript_text="x",
    ) is None


def test_recording_session_history_collects_calls_in_memory():
    history = RecordingSessionHistory()
    history.record(issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", unit="u0",
                    started_at=NOW, finished_at=NOW, outcome="succeeded",
                    transcript_text="the raw content")
    assert len(history.calls) == 1
    assert history.calls[0]["issue"] == 1
    assert history.calls[0]["transcript_text"] == "the raw content"
