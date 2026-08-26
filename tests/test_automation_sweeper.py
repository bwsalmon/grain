import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

import grain.automation.capture as capture_module
from grain.automation.config import AutomationConfig
from grain.automation.dispatch import unit_name
from grain.automation.gcp_keys import GcpKeyConfig
from grain.automation.gemini_keys import GeminiKeyConfig
from grain.automation.history import RecordingSessionHistory
from grain.automation.state import AutomationState, TriggerKind
from grain.automation.sweeper import Outcome, sweep
from grain.proxy.tokens import SandboxCredentialOverrides, SandboxCredentialStore
from grain.run import FakeRunner

GEMINI_KEY_NAME = "projects/1/locations/global/keys/abc"
EMAIL = "grain-agent@proj.iam.gserviceaccount.com"

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def config(**overrides) -> AutomationConfig:
    return AutomationConfig(task_owner="o", task_repo="r", **overrides)


def state_with(sandbox: str, issue: int, started_at: datetime) -> AutomationState:
    state = AutomationState()
    state.assign(sandbox, issue, unit=f"grain-task-{sandbox}", now=started_at)
    return state


def _point_transcript_at(monkeypatch, tmp_path, unit: str):
    """`claude -p` now runs on the controller (docs/roadmap.md item 8's
    "Update"), so `capture_trajectory` reads a plain local file, not
    something over a `Runner` -- these tests point that read at a real
    tmp_path file instead of scripting a `cat` response on a FakeRunner.
    """
    path = tmp_path / f"{unit}.transcript.jsonl"
    monkeypatch.setattr(capture_module, "transcript_path", lambda u: str(path))
    return path


# A fully-healthy sandbox, so tests that don't care about health/cleanup
# don't have to script every probe just to keep `sweep()`'s post-release
# hook quiet — see test_automation_health.py's own `healthy_runner` for the
# equivalent used against `check_health` directly.
_HEALTHY_EXTRAS = {
    "true": ("", 0),
    "systemctl is-system-running": ("running\n", 0),
    "docker info": ("Server Version: 27.0.0\n", 0),
    "df -P /": (
        "Filesystem 1024-blocks Used Available Capacity Mounted on\n"
        "/dev/vda1 100 10 90 10% /\n",
        0,
    ),
}


def runner_reporting(active_state: str, result: str = "success", *,
                      healthy: bool = True) -> FakeRunner:
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout=f"LoadState=loaded\nActiveState={active_state}\nResult={result}\n",
    )
    if healthy:
        for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
            runner.expect(prefix, stdout=stdout, returncode=returncode)
    return runner


def test_a_still_active_run_within_budget_is_left_alone():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.succeeded == result.failed == result.stranded == []
    assert "sandbox-0" in state.assignments


def test_a_successful_unit_frees_the_slot_and_is_reported_succeeded():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.succeeded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")


def test_a_failed_unit_frees_the_slot_and_is_reported_failed():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.failed == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments


def test_an_absent_unit_is_stranded_without_reaping():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.stranded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert not runner.ran("sudo systemctl stop")


def test_a_run_past_max_runtime_is_stranded_even_if_still_active():
    started = NOW - timedelta(minutes=200)
    state = state_with("sandbox-0", issue=1, started_at=started)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(max_runtime_minutes=120), NOW)
    assert result.stranded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")


def test_a_run_within_a_custom_max_runtime_stays_active():
    started = NOW - timedelta(minutes=30)
    state = state_with("sandbox-0", issue=1, started_at=started)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(max_runtime_minutes=120), NOW)
    assert result.stranded == []
    assert "sandbox-0" in state.assignments


# --- cancel-on-close (bwsalmon/agents#82) -----------------------------------

def test_a_still_active_run_whose_issue_closed_is_cancelled_and_reaped():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    is_issue_closed=lambda number: number == 1)
    assert result.cancelled == [Outcome("sandbox-0", 1)]
    assert result.succeeded == result.failed == result.stranded == []
    assert "sandbox-0" not in state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")


def test_a_still_active_run_whose_issue_is_still_open_is_left_alone():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    is_issue_closed=lambda number: False)
    assert result.cancelled == []
    assert "sandbox-0" in state.assignments
    assert not runner.ran("sudo systemctl stop")


def test_a_still_active_run_is_left_alone_with_no_is_issue_closed_hook():
    # Every caller that predates bwsalmon/agents#82 leaves this unset --
    # must behave exactly as before, with no cancel-on-close check at all.
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.cancelled == []
    assert "sandbox-0" in state.assignments


def test_a_cancelled_runs_slot_gets_the_same_cleanup_hook_as_any_other_release():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    sweep(state, lambda name: runner, runner, config(), NOW,
          is_issue_closed=lambda number: True)
    assert runner.ran("kind delete clusters --all")
    assert runner.ran("docker system prune -af --volumes")


def test_a_unit_that_already_finished_is_not_reported_cancelled_even_if_closed():
    # is_issue_closed is only ever consulted for a unit that would
    # otherwise be left running untouched -- a run that already exited is
    # reported succeeded/failed as usual, not cancelled, regardless of the
    # issue's state by sweep time.
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    is_issue_closed=lambda number: True)
    assert result.succeeded == [Outcome("sandbox-0", 1)]
    assert result.cancelled == []


def test_sweep_uses_the_per_sandbox_runner_factory():
    now = NOW
    state = AutomationState()
    state.assign("sandbox-0", 1, "grain-task-sandbox-0", now)
    state.assign("sandbox-1", 2, "grain-task-sandbox-1", now)
    runners = {
        "sandbox-0": runner_reporting("inactive", "success"),
        "sandbox-1": runner_reporting("active"),
    }
    # unit_status/reap now go through one shared controller_runner (the
    # unit lives on the controller regardless of which sandbox it targets)
    # -- scripted to answer for both units at once.
    controller_runner = FakeRunner()
    controller_runner.expect(
        "systemctl show grain-task-sandbox-0",
        stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n",
    )
    controller_runner.expect(
        "systemctl show grain-task-sandbox-1",
        stdout="LoadState=loaded\nActiveState=active\nResult=success\n",
    )
    result = sweep(state, lambda name: runners[name], controller_runner, config(), now)
    assert [o.sandbox for o in result.succeeded] == ["sandbox-0"]
    assert "sandbox-1" in state.assignments


# --- between-task cleanup + health, run on every release (docs/roadmap.md
# --- item 5 / docs/design.md step 8) ---------------------------------------

def test_a_freed_sandbox_gets_the_cleanup_hook_run_on_it():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")
    assert runner.ran("docker system prune -af --volumes")


def test_cleanup_runs_for_a_failed_run_too():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    sweep(state, lambda name: runner, runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")


def test_cleanup_runs_for_a_stranded_absent_unit_too():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    sweep(state, lambda name: runner, runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")
    assert runner.ran("docker system prune -af --volumes")


def test_a_still_active_run_gets_no_cleanup_or_health_probe():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    sweep(state, lambda name: runner, runner, config(), NOW)
    assert not runner.ran("kind")
    assert not runner.ran("docker")
    assert not runner.ran("true")


def test_a_healthy_freed_sandbox_reports_no_health_warning():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success", healthy=True)
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.health_warnings == []


def test_an_unhealthy_freed_sandbox_is_reported_but_still_freed():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success", healthy=False)
    # No health extras scripted: systemd/docker/disk probes fall through to
    # FakeRunner's empty-output default, which fails to parse -> degraded.
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert len(result.health_warnings) == 1
    assert result.health_warnings[0].sandbox == "sandbox-0"
    # The slot is freed regardless — a health problem doesn't gate reuse,
    # only surfaces (see this module's own docstring for why).
    assert "sandbox-0" not in state.assignments
    assert result.succeeded == [Outcome("sandbox-0", 1)]


def test_an_unreachable_sandbox_is_reported_unreachable_not_crashed():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n",
    )
    runner.expect("true", returncode=255, stderr="ssh: connect timed out")
    result = sweep(state, lambda name: runner, runner, config(), NOW)  # must not raise
    assert len(result.health_warnings) == 1
    assert "unreachable" in result.health_warnings[0].detail


# --- PR-triggered assignments carry their kind/branch through a sweep
# --- (docs/roadmap.md item 9) ----------------------------------------------

def test_a_swept_pr_assignment_carries_its_kind_and_branch_into_the_outcome():
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0", now=NOW,
                 kind=TriggerKind.PR, branch="feature-x")
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.succeeded == [Outcome("sandbox-0", 9, kind=TriggerKind.PR, branch="feature-x")]


def test_an_issue_assignment_still_defaults_to_issue_kind_with_no_branch():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.succeeded[0].kind is TriggerKind.ISSUE
    assert result.succeeded[0].branch is None


# --- trajectory capture on release (docs/roadmap.md item 10) --------------

def test_sweep_with_no_history_argument_still_works_unchanged():
    # Every existing caller of sweep() (including every test above) doesn't
    # pass history= at all -- must keep working exactly as before.
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.succeeded == [Outcome("sandbox-0", 1)]


def test_a_successful_release_captures_the_transcript_and_records_it(monkeypatch, tmp_path):
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    transcript = _point_transcript_at(monkeypatch, tmp_path, unit_name("sandbox-0"))
    transcript.write_text('{"type": "result", "result": "done"}\n')
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(), NOW, history=history)
    assert len(history.calls) == 1
    call = history.calls[0]
    assert call["issue"] == 1
    assert call["sandbox"] == "sandbox-0"
    assert call["unit"] == "grain-task-sandbox-0"
    assert call["outcome"] == "succeeded"
    assert call["started_at"] == NOW
    assert call["finished_at"] == NOW
    assert call["transcript_text"] == '{"type": "result", "result": "done"}\n'


def test_a_failed_release_is_recorded_with_outcome_failed(monkeypatch, tmp_path):
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    _point_transcript_at(monkeypatch, tmp_path, unit_name("sandbox-0"))
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(), NOW, history=history)
    assert history.calls[0]["outcome"] == "failed"


def test_a_stranded_absent_release_is_recorded_with_no_transcript(monkeypatch, tmp_path):
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    # transcript_path pointed at a file that's never written -- a
    # never-started unit never wrote one; capture_trajectory -> None.
    _point_transcript_at(monkeypatch, tmp_path, unit_name("sandbox-0"))
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(), NOW, history=history)
    assert history.calls[0]["outcome"] == "stranded"
    assert history.calls[0]["transcript_text"] is None


def test_a_run_past_max_runtime_is_captured_before_release_too(monkeypatch, tmp_path):
    started = NOW - timedelta(minutes=200)
    state = state_with("sandbox-0", issue=1, started_at=started)
    runner = runner_reporting("active")
    transcript = _point_transcript_at(monkeypatch, tmp_path, unit_name("sandbox-0"))
    transcript.write_text('{"type": "system"}\n')
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(max_runtime_minutes=120), NOW, history=history)
    assert history.calls[0]["outcome"] == "stranded"
    assert history.calls[0]["transcript_text"] == '{"type": "system"}\n'


def test_capture_happens_before_the_slot_is_freed_in_state(monkeypatch, tmp_path):
    # A capture step that reads history.record()'s own record of the
    # assignment must see the pre-release Assignment (kind/branch included)
    # -- checked indirectly here via a PR-kind assignment carrying its
    # branch through into the history call.
    state = AutomationState()
    state.assign("sandbox-0", issue=9, unit="grain-task-sandbox-0", now=NOW,
                 kind=TriggerKind.PR, branch="feature-x")
    runner = runner_reporting("inactive", "success")
    _point_transcript_at(monkeypatch, tmp_path, unit_name("sandbox-0"))
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(), NOW, history=history)
    assert history.calls[0]["kind"] is TriggerKind.PR


def test_a_still_active_run_within_budget_captures_nothing():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    history = RecordingSessionHistory()
    sweep(state, lambda name: runner, runner, config(), NOW, history=history)
    assert history.calls == []


# --- Gemini API key revocation on release (bwsalmon/agents#47) -------------

def state_with_gemini_key(sandbox: str, issue: int, started_at) -> AutomationState:
    state = AutomationState()
    state.assign(sandbox, issue, unit=f"grain-task-{sandbox}", now=started_at,
                 gemini_key_name=GEMINI_KEY_NAME)
    return state


def test_a_task_with_no_gemini_key_never_calls_gcloud_on_release():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gemini_key_config=GeminiKeyConfig(project_id="proj"))
    assert not runner.ran("gcloud")


def test_a_successful_release_revokes_the_minted_gemini_key():
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    gemini_key_config=GeminiKeyConfig(project_id="proj"))
    delete_call = next(c for c in runner.commands if "api-keys delete" in c)
    assert GEMINI_KEY_NAME in delete_call
    assert "--project=proj" in delete_call
    assert result.credential_warnings == []


def test_a_failed_release_still_revokes_the_minted_gemini_key():
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gemini_key_config=GeminiKeyConfig(project_id="proj"))
    assert any("api-keys delete" in c for c in runner.commands)


def test_a_stranded_release_still_revokes_the_minted_gemini_key():
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    sweep(state, lambda name: runner, runner, config(), NOW,
          gemini_key_config=GeminiKeyConfig(project_id="proj"))
    assert any("api-keys delete" in c for c in runner.commands)


def test_gemini_key_revocation_happens_before_the_slot_is_freed_in_state():
    # The assignment (carrying gemini_key_name) must still be on hand when
    # revocation runs -- checked indirectly by asserting revocation
    # actually happened at all, since state.release() would otherwise have
    # already dropped it by the time _revoke_gemini_key ran.
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gemini_key_config=GeminiKeyConfig(project_id="proj"))
    assert "sandbox-0" not in state.assignments
    assert any("api-keys delete" in c for c in runner.commands)


def test_gemini_key_revocation_with_no_config_is_skipped_not_crashed():
    # A key was minted (an assignment carrying gemini_key_name) but this
    # sweep call was never given a GeminiKeyConfig -- must not raise, and
    # must not guess at a project id.
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert not any("api-keys delete" in c for c in runner.commands)
    assert result.credential_warnings == []


def test_gemini_key_revocation_failure_is_a_credential_warning_not_a_crash():
    state = state_with_gemini_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    runner.expect("gcloud auth activate-service-account", returncode=0)
    runner.expect("gcloud services api-keys delete", returncode=1,
                   stderr="PERMISSION_DENIED")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    gemini_key_config=GeminiKeyConfig(project_id="proj"))
    assert len(result.credential_warnings) == 1
    assert result.credential_warnings[0].sandbox == "sandbox-0"
    assert "PERMISSION_DENIED" in result.credential_warnings[0].detail
    # The slot still frees even though revocation failed.
    assert "sandbox-0" not in state.assignments
    assert result.succeeded == [Outcome("sandbox-0", 1)]


def test_credential_warnings_default_to_empty_for_every_existing_caller():
    # Every caller of sweep() written before this feature doesn't pass
    # gemini_key_config at all -- must keep working exactly as before.
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert result.credential_warnings == []


# --- GCP service-account key revocation on release (bwsalmon/agents#126) ---

GCP_KEY_ID = "abc123def456"


def state_with_gcp_key(sandbox: str, issue: int, started_at) -> AutomationState:
    state = AutomationState()
    state.assign(sandbox, issue, unit=f"grain-task-{sandbox}", now=started_at,
                 gcp_key_id=GCP_KEY_ID)
    return state


def test_a_task_with_no_gcp_key_never_calls_gcloud_on_release():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert not runner.ran("gcloud")


def test_a_successful_release_revokes_the_minted_gcp_key():
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    delete_call = next(c for c in runner.commands if "keys delete" in c)
    assert GCP_KEY_ID in delete_call
    assert f"--iam-account={EMAIL}" in delete_call
    assert result.credential_warnings == []


def test_a_failed_release_still_revokes_the_minted_gcp_key():
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert any("keys delete" in c for c in runner.commands)


def test_a_stranded_release_still_revokes_the_minted_gcp_key():
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    sweep(state, lambda name: runner, runner, config(), NOW,
          gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert any("keys delete" in c for c in runner.commands)


def test_gcp_key_revocation_happens_before_the_slot_is_freed_in_state():
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW,
          gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert "sandbox-0" not in state.assignments
    assert any("keys delete" in c for c in runner.commands)


def test_gcp_key_revocation_with_no_config_is_skipped_not_crashed():
    # A key was minted (an assignment carrying gcp_key_id) but this sweep
    # call was never given a GcpKeyConfig -- must not raise, and must not
    # guess at a service account.
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW)
    assert not any("keys delete" in c for c in runner.commands)
    assert result.credential_warnings == []


def test_gcp_key_revocation_failure_is_a_credential_warning_not_a_crash():
    state = state_with_gcp_key("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    runner.expect("gcloud iam service-accounts keys delete", returncode=1,
                  stderr="PERMISSION_DENIED")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert len(result.credential_warnings) == 1
    assert result.credential_warnings[0].sandbox == "sandbox-0"
    assert "PERMISSION_DENIED" in result.credential_warnings[0].detail
    # The slot still frees even though revocation failed.
    assert "sandbox-0" not in state.assignments
    assert result.succeeded == [Outcome("sandbox-0", 1)]


def test_a_release_revokes_both_a_gemini_key_and_a_gcp_key_when_both_were_minted():
    state = AutomationState()
    state.assign("sandbox-0", 1, unit="grain-task-sandbox-0", now=NOW,
                 gemini_key_name=GEMINI_KEY_NAME, gcp_key_id=GCP_KEY_ID)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, runner, config(), NOW,
                    gemini_key_config=GeminiKeyConfig(project_id="proj"),
                    gcp_key_config=GcpKeyConfig(service_account_email=EMAIL, project_id="proj"))
    assert any("api-keys delete" in c for c in runner.commands)
    assert any("iam service-accounts keys delete" in c for c in runner.commands)
    assert result.credential_warnings == []


# --- credential_store.clear() on release (bwsalmon/agents#52) ---------------

def test_a_successful_release_clears_any_credential_override():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    store.set("sandbox-0", "workflow")

    sweep(state, lambda name: runner, runner, config(), NOW, credential_store=store)

    assert SandboxCredentialOverrides(store._path).for_sandbox("sandbox-0") is None


def test_a_failed_release_also_clears_any_credential_override():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    store.set("sandbox-0", "workflow")

    sweep(state, lambda name: runner, runner, config(), NOW, credential_store=store)

    assert SandboxCredentialOverrides(store._path).for_sandbox("sandbox-0") is None


def test_a_stranded_release_also_clears_any_credential_override():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    store.set("sandbox-0", "workflow")

    sweep(state, lambda name: runner, runner, config(), NOW, credential_store=store)

    assert SandboxCredentialOverrides(store._path).for_sandbox("sandbox-0") is None


def test_release_with_no_override_set_does_not_crash():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")

    sweep(state, lambda name: runner, runner, config(), NOW, credential_store=store)  # must not raise


def test_release_with_no_credential_store_does_not_crash():
    # Every caller of sweep() written before this feature doesn't pass
    # credential_store at all -- must keep working exactly as before.
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, runner, config(), NOW)  # must not raise
    assert "sandbox-0" not in state.assignments


def test_a_release_clears_only_this_sandboxs_override():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    store = SandboxCredentialStore(Path(tempfile.mkdtemp()) / "sandbox-github-key.json")
    store.set("sandbox-0", "workflow")
    store.set("sandbox-1", "release")

    sweep(state, lambda name: runner, runner, config(), NOW, credential_store=store)

    overrides = SandboxCredentialOverrides(store._path)
    assert overrides.for_sandbox("sandbox-0") is None
    assert overrides.for_sandbox("sandbox-1") == "release"
