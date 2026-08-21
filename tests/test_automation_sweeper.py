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


# --- between-task cleanup + health, run on every release (docs/roadmap.md
# --- item 5 / docs/design.md step 8) ---------------------------------------

def test_a_freed_sandbox_gets_the_cleanup_hook_run_on_it():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success")
    sweep(state, lambda name: runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")
    assert runner.ran("docker system prune -af --volumes")


def test_cleanup_runs_for_a_failed_run_too():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("failed", "exit-code")
    sweep(state, lambda name: runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")


def test_cleanup_runs_for_a_stranded_absent_unit_too():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1)
    for prefix, (stdout, returncode) in _HEALTHY_EXTRAS.items():
        runner.expect(prefix, stdout=stdout, returncode=returncode)
    sweep(state, lambda name: runner, config(), NOW)
    assert runner.ran("kind delete clusters --all")
    assert runner.ran("docker system prune -af --volumes")


def test_a_still_active_run_gets_no_cleanup_or_health_probe():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("active")
    sweep(state, lambda name: runner, config(), NOW)
    assert not runner.ran("kind")
    assert not runner.ran("docker")
    assert not runner.ran("true")


def test_a_healthy_freed_sandbox_reports_no_health_warning():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success", healthy=True)
    result = sweep(state, lambda name: runner, config(), NOW)
    assert result.health_warnings == []


def test_an_unhealthy_freed_sandbox_is_reported_but_still_freed():
    state = state_with("sandbox-0", issue=1, started_at=NOW)
    runner = runner_reporting("inactive", "success", healthy=False)
    # No health extras scripted: systemd/docker/disk probes fall through to
    # FakeRunner's empty-output default, which fails to parse -> degraded.
    result = sweep(state, lambda name: runner, config(), NOW)
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
    result = sweep(state, lambda name: runner, config(), NOW)  # must not raise
    assert len(result.health_warnings) == 1
    assert "unreachable" in result.health_warnings[0].detail
