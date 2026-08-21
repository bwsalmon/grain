from datetime import datetime, timedelta, timezone

from grain.automation.config import AutomationConfig
from grain.automation.state import AutomationState
from grain.automation.sweeper import Outcome, sweep
from grain.run import FakeRunner

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def config(**overrides) -> AutomationConfig:
    return AutomationConfig(owner="o", repo="r", **overrides)


def state_with(sandbox: str, issue: int, started_at: datetime) -> AutomationState:
    state = AutomationState()
    state.assign(sandbox, issue, unit=f"grain-task-{sandbox}", now=started_at)
    return state


def runner_reporting(active_state: str, result: str = "success") -> FakeRunner:
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout=f"LoadState=loaded\nActiveState={active_state}\nResult={result}\n",
    )
    return runner


def test_a_still_active_run_within_budget_is_left_alone():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, config(), NOW)
    assert result.succeeded == result.failed == result.stranded == []
    assert "sandbox-0" in state.assignments


def test_a_successful_unit_frees_the_slot_and_is_reported_succeeded():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    result = sweep(state, lambda name: runner, config(), NOW)
    assert result.succeeded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")


def test_a_failed_unit_frees_the_slot_and_is_reported_failed():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    result = sweep(state, lambda name: runner, config(), NOW)
    assert result.failed == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments


def test_an_absent_unit_is_stranded_without_reaping():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    result = sweep(state, lambda name: runner, config(), NOW)
    assert result.stranded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert not runner.ran("sudo systemctl stop")


def test_a_run_past_max_runtime_is_stranded_even_if_still_active():
    started = NOW - timedelta(minutes=200)
    state = state_with("sandbox-0", issue=1, started_at=started)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, config(max_runtime_minutes=120), NOW)
    assert result.stranded == [Outcome("sandbox-0", 1)]
    assert "sandbox-0" not in state.assignments
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")


def test_a_run_within_a_custom_max_runtime_stays_active():
    started = NOW - timedelta(minutes=30)
    state = state_with("sandbox-0", issue=1, started_at=started)
    runner = runner_reporting("active")
    result = sweep(state, lambda name: runner, config(max_runtime_minutes=120), NOW)
    assert result.stranded == []
    assert "sandbox-0" in state.assignments


def test_sweep_uses_the_per_sandbox_runner_factory():
    now = NOW
    state = AutomationState()
    state.assign("sandbox-0", 1, "grain-task-sandbox-0", now)
    state.assign("sandbox-1", 2, "grain-task-sandbox-1", now)
    runners = {
        "sandbox-0": runner_reporting("inactive", "success"),
        "sandbox-1": runner_reporting("active"),
    }
    result = sweep(state, lambda name: runners[name], config(), now)
    assert [o.sandbox for o in result.succeeded] == ["sandbox-0"]
    assert "sandbox-1" in state.assignments
