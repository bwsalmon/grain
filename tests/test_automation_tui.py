from datetime import datetime, timezone

from grain.automation.history import SessionRecord
from grain.automation.state import TriggerKind
from grain.automation.transcript import TranscriptEvent
from grain.automation.tui import (
    DetailState, KindFilter, OutcomeFilter, SessionListState, format_detail_title,
    format_header, format_row,
)

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def record(issue=1, kind=TriggerKind.ISSUE, sandbox="sandbox-0", outcome="succeeded",
           transcript_path=None) -> SessionRecord:
    return SessionRecord(
        issue=issue, kind=kind, sandbox=sandbox, unit=f"grain-task-{sandbox}",
        started_at=NOW, finished_at=NOW, outcome=outcome, transcript_path=transcript_path,
    )


# --- filter cycling ---------------------------------------------------------

def test_kind_filter_cycles_through_all_values():
    assert KindFilter.ALL.next() is KindFilter.ISSUE
    assert KindFilter.ISSUE.next() is KindFilter.PR
    assert KindFilter.PR.next() is KindFilter.REVIEW
    assert KindFilter.REVIEW.next() is KindFilter.ALL


def test_outcome_filter_cycles_through_all_values():
    f = OutcomeFilter.ALL
    seen = [f]
    for _ in range(4):
        f = f.next()
        seen.append(f)
    assert seen == [
        OutcomeFilter.ALL, OutcomeFilter.SUCCEEDED, OutcomeFilter.FAILED,
        OutcomeFilter.STRANDED, OutcomeFilter.ALL,
    ]


# --- SessionListState --------------------------------------------------------

def test_visible_with_no_filter_shows_everything():
    state = SessionListState(records=[record(issue=1), record(issue=2)])
    assert len(state.visible()) == 2


def test_visible_filters_by_kind():
    state = SessionListState(records=[
        record(issue=1, kind=TriggerKind.ISSUE),
        record(issue=2, kind=TriggerKind.PR),
    ])
    state.kind_filter = KindFilter.PR
    assert [r.issue for r in state.visible()] == [2]


def test_visible_filters_by_review_kind():
    state = SessionListState(records=[
        record(issue=1, kind=TriggerKind.PR),
        record(issue=2, kind=TriggerKind.REVIEW),
    ])
    state.kind_filter = KindFilter.REVIEW
    assert [r.issue for r in state.visible()] == [2]


def test_visible_filters_by_outcome():
    state = SessionListState(records=[
        record(issue=1, outcome="succeeded"),
        record(issue=2, outcome="failed"),
    ])
    state.outcome_filter = OutcomeFilter.FAILED
    assert [r.issue for r in state.visible()] == [2]


def test_visible_combines_kind_and_outcome_filters():
    state = SessionListState(records=[
        record(issue=1, kind=TriggerKind.PR, outcome="succeeded"),
        record(issue=2, kind=TriggerKind.PR, outcome="failed"),
        record(issue=3, kind=TriggerKind.ISSUE, outcome="failed"),
    ])
    state.kind_filter = KindFilter.PR
    state.outcome_filter = OutcomeFilter.FAILED
    assert [r.issue for r in state.visible()] == [2]


def test_cycle_kind_resets_selection():
    state = SessionListState(records=[record(issue=1), record(issue=2)], selected=1)
    state.cycle_kind()
    assert state.selected == 0
    assert state.kind_filter is KindFilter.ISSUE


def test_cycle_outcome_resets_selection():
    state = SessionListState(records=[record(issue=1), record(issue=2)], selected=1)
    state.cycle_outcome()
    assert state.selected == 0


def test_move_clamps_at_the_bounds():
    state = SessionListState(records=[record(issue=1), record(issue=2)])
    state.move(-5)
    assert state.selected == 0
    state.move(5)
    assert state.selected == 1
    state.move(5)
    assert state.selected == 1  # already at the last row


def test_move_on_an_empty_list_stays_at_zero():
    state = SessionListState(records=[])
    state.move(1)
    assert state.selected == 0


def test_selected_record_returns_the_row_under_the_cursor():
    state = SessionListState(records=[record(issue=1), record(issue=2)], selected=1)
    assert state.selected_record().issue == 2


def test_selected_record_is_none_when_the_filtered_list_is_empty():
    state = SessionListState(records=[record(issue=1, kind=TriggerKind.ISSUE)])
    state.kind_filter = KindFilter.PR
    assert state.selected_record() is None


def test_selected_record_reflects_the_filtered_index_not_the_unfiltered_one():
    # Selecting row 0 while filtered to PR must return the PR row, not
    # whatever sits at index 0 of the unfiltered list.
    state = SessionListState(records=[
        record(issue=1, kind=TriggerKind.ISSUE),
        record(issue=2, kind=TriggerKind.PR),
    ])
    state.kind_filter = KindFilter.PR
    assert state.selected_record().issue == 2


# --- formatting --------------------------------------------------------------

def test_format_row_includes_trigger_kind_and_number():
    row = format_row(record(issue=9, kind=TriggerKind.PR))
    assert "pr#9" in row


def test_format_row_shows_whether_a_transcript_was_captured():
    with_transcript = format_row(record(transcript_path="/data/x.jsonl"))
    without = format_row(record(transcript_path=None))
    assert "transcript=yes" in with_transcript
    assert "transcript=no" in without


def test_format_header_reports_filter_state_and_counts():
    state = SessionListState(records=[record(issue=1), record(issue=2)])
    state.kind_filter = KindFilter.ISSUE
    header = format_header(state)
    assert "kind=issue" in header
    assert "2/2" in header


def test_format_detail_title_includes_the_trigger_and_outcome():
    title = format_detail_title(record(issue=3, outcome="failed"))
    assert "issue#3" in title
    assert "outcome=failed" in title


# --- DetailState --------------------------------------------------------------

def test_detail_state_scroll_by_clamps_at_bounds():
    events = [TranscriptEvent(role="user", summary="a", detail="a"),
              TranscriptEvent(role="assistant", summary="b", detail="b")]
    state = DetailState(record=record(), events=events)
    state.scroll_by(-5)
    assert state.scroll == 0
    state.scroll_by(5)
    assert state.scroll == 1


def test_detail_state_scroll_by_on_empty_events_stays_at_zero():
    state = DetailState(record=record(), events=[])
    state.scroll_by(3)
    assert state.scroll == 0
