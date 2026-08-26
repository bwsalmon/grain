import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from grain.automation.state import AutomationState, CompletedIssue, OpenPullRequest, TriggerKind


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


# --- Gemini API key (bwsalmon/agents#47) ------------------------------------

def test_assign_defaults_to_no_gemini_key():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    assert state.assignments["sandbox-0"].gemini_key_name is None


def test_assign_records_a_gemini_key_name():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now,
                 gemini_key_name="projects/1/locations/global/keys/abc")
    assert state.assignments["sandbox-0"].gemini_key_name == (
        "projects/1/locations/global/keys/abc"
    )


def test_save_and_load_round_trips_the_gemini_key_name(tmp_path: Path):
    state = AutomationState()
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0", now=now,
                 gemini_key_name="projects/1/locations/global/keys/abc")
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assert loaded.assignments["sandbox-0"].gemini_key_name == (
        "projects/1/locations/global/keys/abc"
    )


def test_load_of_a_pre_gemini_key_state_file_defaults_to_no_key(tmp_path: Path):
    # An assignment written before bwsalmon/agents#47 has no such field at
    # all -- must load as None, not KeyError.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({
        "assignments": {
            "sandbox-0": {
                "issue": 1, "unit": "u0", "started_at": "2026-01-01T00:00:00+00:00",
            },
        },
    }))
    loaded = AutomationState.load(path)
    assert loaded.assignments["sandbox-0"].gemini_key_name is None


# --- GCP service-account key (bwsalmon/agents#126) --------------------------

def test_assign_defaults_to_no_gcp_key():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    assert state.assignments["sandbox-0"].gcp_key_id is None


def test_assign_records_a_gcp_key_id():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now, gcp_key_id="abc123")
    assert state.assignments["sandbox-0"].gcp_key_id == "abc123"


def test_save_and_load_round_trips_the_gcp_key_id(tmp_path: Path):
    state = AutomationState()
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=7, unit="grain-task-sandbox-0", now=now,
                 gcp_key_id="abc123")
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assert loaded.assignments["sandbox-0"].gcp_key_id == "abc123"


def test_load_of_a_pre_gcp_key_state_file_defaults_to_no_key(tmp_path: Path):
    # An assignment written before bwsalmon/agents#126 has no such field at
    # all -- must load as None, not KeyError.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({
        "assignments": {
            "sandbox-0": {
                "issue": 1, "unit": "u0", "started_at": "2026-01-01T00:00:00+00:00",
            },
        },
    }))
    loaded = AutomationState.load(path)
    assert loaded.assignments["sandbox-0"].gcp_key_id is None


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


def test_assign_defaults_to_no_pr_number():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=1, unit="u0", now=now)
    assert state.assignments["sandbox-0"].pr_number is None


def test_assign_records_review_kind_and_pr_number():
    state = AutomationState()
    now = datetime(2026, 1, 1, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=9, unit="u0", now=now,
                 kind=TriggerKind.REVIEW, branch="feature-x", pr_number=42)
    assignment = state.assignments["sandbox-0"]
    assert assignment.kind is TriggerKind.REVIEW
    assert assignment.branch == "feature-x"
    assert assignment.pr_number == 42


def test_save_and_load_round_trips_review_kind_and_pr_number(tmp_path: Path):
    state = AutomationState()
    now = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0", now=now,
                 kind=TriggerKind.REVIEW, branch="feature-x", pr_number=42)
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assignment = loaded.assignments["sandbox-0"]
    assert assignment.kind is TriggerKind.REVIEW
    assert assignment.branch == "feature-x"
    assert assignment.pr_number == 42


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


# --- pending questions (docs/roadmap.md item 13) ---------------------------

def test_record_pending_question_defaults_to_issue_kind_with_no_branch():
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    pending = state.pending_questions["5"]
    assert pending.issue == 5
    assert pending.question_comment_id == 100
    assert pending.kind is TriggerKind.ISSUE
    assert pending.branch is None


def test_record_pending_question_records_pr_kind_and_branch():
    state = AutomationState()
    state.record_pending_question(9, question_comment_id=200,
                                   kind=TriggerKind.PR, branch="feature-x")
    pending = state.pending_questions["9"]
    assert pending.kind is TriggerKind.PR
    assert pending.branch == "feature-x"


def test_clear_pending_question_removes_it():
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100)
    state.clear_pending_question(5)
    assert state.pending_questions == {}


def test_clear_pending_question_on_an_absent_issue_is_a_no_op():
    state = AutomationState()
    state.clear_pending_question(5)  # must not raise
    assert state.pending_questions == {}


def test_save_and_load_round_trips_pending_questions(tmp_path: Path):
    state = AutomationState()
    state.record_pending_question(5, question_comment_id=100,
                                   kind=TriggerKind.PR, branch="feature-x")
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    pending = loaded.pending_questions["5"]
    assert pending.issue == 5
    assert pending.question_comment_id == 100
    assert pending.kind is TriggerKind.PR
    assert pending.branch == "feature-x"


def test_load_of_a_pre_item_13_state_file_has_no_pending_questions(tmp_path: Path):
    # A state file written before item 13 existed has no "pending_questions"
    # key at all -- loading it must default to empty rather than KeyError.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({"assignments": {}, "run_timestamps": []}))
    loaded = AutomationState.load(path)
    assert loaded.pending_questions == {}


# --- feedback triage (bwsalmon/agents#24) -----------------------------------

def test_track_pull_request_starts_at_a_zero_baseline():
    state = AutomationState()
    state.track_pull_request("o", "r", 42, origin_task_issue=7)
    tracked = state.tracked_prs["o/r#42"]
    assert tracked.owner == "o"
    assert tracked.repo == "r"
    assert tracked.number == 42
    assert tracked.origin_task_issue == 7
    assert tracked.last_review_comment_id == 0
    assert tracked.last_comment_id == 0


def test_track_pull_request_is_a_no_op_if_already_tracked():
    # Re-tracking must not reset an advanced baseline back to zero -- that
    # would replay the PR's whole comment history as "new" on the next
    # triage pass.
    state = AutomationState()
    state.track_pull_request("o", "r", 42, origin_task_issue=7)
    state.update_tracked_pull_request("o/r#42", last_review_comment_id=5, last_comment_id=9)
    state.track_pull_request("o", "r", 42, origin_task_issue=999)
    tracked = state.tracked_prs["o/r#42"]
    assert tracked.origin_task_issue == 7
    assert tracked.last_review_comment_id == 5
    assert tracked.last_comment_id == 9


def test_update_tracked_pull_request_on_an_absent_key_is_a_no_op():
    state = AutomationState()
    state.update_tracked_pull_request("o/r#42", last_review_comment_id=1, last_comment_id=1)
    assert state.tracked_prs == {}


def test_untrack_pull_request_removes_it():
    state = AutomationState()
    state.track_pull_request("o", "r", 42, origin_task_issue=7)
    state.untrack_pull_request("o/r#42")
    assert state.tracked_prs == {}


def test_untrack_pull_request_on_an_absent_key_is_a_no_op():
    state = AutomationState()
    state.untrack_pull_request("o/r#42")  # must not raise
    assert state.tracked_prs == {}


def test_save_and_load_round_trips_tracked_prs(tmp_path: Path):
    state = AutomationState()
    state.track_pull_request("o", "r", 42, origin_task_issue=7)
    state.update_tracked_pull_request("o/r#42", last_review_comment_id=5, last_comment_id=9)
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    tracked = loaded.tracked_prs["o/r#42"]
    assert tracked.owner == "o"
    assert tracked.repo == "r"
    assert tracked.number == 42
    assert tracked.origin_task_issue == 7
    assert tracked.last_review_comment_id == 5
    assert tracked.last_comment_id == 9


def test_load_of_a_pre_item_24_state_file_has_no_tracked_prs(tmp_path: Path):
    # A state file written before this item existed has no "tracked_prs"
    # key at all -- loading it must default to empty rather than KeyError.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({"assignments": {}, "run_timestamps": []}))
    loaded = AutomationState.load(path)
    assert loaded.tracked_prs == {}


# --- open PRs awaiting a close (bwsalmon/agents#54) -----------------------

def test_record_open_pr_records_target_and_pr_number():
    state = AutomationState()
    state.record_open_pr(5, "other", "service", 42)
    open_pr = state.open_pull_requests["5"]
    assert open_pr.issue == 5
    assert open_pr.target_owner == "other"
    assert open_pr.target_repo == "service"
    assert open_pr.pr_number == 42


def test_clear_open_pr_removes_it():
    state = AutomationState()
    state.record_open_pr(5, "o", "r", 42)
    state.clear_open_pr(5)
    assert state.open_pull_requests == {}


def test_clear_open_pr_on_an_absent_issue_is_a_no_op():
    state = AutomationState()
    state.clear_open_pr(5)  # must not raise
    assert state.open_pull_requests == {}


def test_save_and_load_round_trips_open_pull_requests(tmp_path: Path):
    state = AutomationState()
    state.record_open_pr(5, "other", "service", 42)
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    open_pr = loaded.open_pull_requests["5"]
    assert open_pr == OpenPullRequest(
        issue=5, target_owner="other", target_repo="service", pr_number=42,
    )


def test_load_of_a_pre_54_state_file_has_no_open_pull_requests(tmp_path: Path):
    # A state file written before bwsalmon/agents#54 existed has no
    # "open_pull_requests" key at all -- loading it must default to empty
    # rather than KeyError, the same tolerance `pending_questions` already
    # has for a state file written before item 13.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({"assignments": {}, "run_timestamps": []}))
    loaded = AutomationState.load(path)
    assert loaded.open_pull_requests == {}


# --- restart on comment after completion (bwsalmon/agents#135) -----------

def test_record_completed_issue_starts_with_no_baseline():
    state = AutomationState()
    state.record_completed_issue(5)
    completed = state.completed_issues["5"]
    assert completed.issue == 5
    assert completed.baseline_comment_id is None


def test_prime_completed_baseline_fills_it_in():
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    assert state.completed_issues["5"] == CompletedIssue(issue=5, baseline_comment_id=100)


def test_clear_completed_issue_removes_it():
    state = AutomationState()
    state.record_completed_issue(5)
    state.clear_completed_issue(5)
    assert state.completed_issues == {}


def test_clear_completed_issue_on_an_absent_issue_is_a_no_op():
    state = AutomationState()
    state.clear_completed_issue(5)  # must not raise
    assert state.completed_issues == {}


def test_save_and_load_round_trips_completed_issues(tmp_path: Path):
    state = AutomationState()
    state.record_completed_issue(5)
    state.prime_completed_baseline(5, 100)
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assert loaded.completed_issues["5"] == CompletedIssue(issue=5, baseline_comment_id=100)


def test_save_and_load_round_trips_an_unprimed_completed_issue(tmp_path: Path):
    state = AutomationState()
    state.record_completed_issue(5)
    path = tmp_path / "state.json"
    state.save(path)

    loaded = AutomationState.load(path)
    assert loaded.completed_issues["5"] == CompletedIssue(issue=5, baseline_comment_id=None)


def test_load_of_a_pre_135_state_file_has_no_completed_issues(tmp_path: Path):
    # A state file written before bwsalmon/agents#135 existed has no
    # "completed_issues" key at all -- loading it must default to empty
    # rather than KeyError, the same tolerance every other addition here
    # already has for a state file written before it existed.
    path = tmp_path / "state.json"
    path.write_text(json.dumps({"assignments": {}, "run_timestamps": []}))
    loaded = AutomationState.load(path)
    assert loaded.completed_issues == {}


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
