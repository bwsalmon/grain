from grain.automation.dispatch import (
    UnitState, dispatch, reap, unit_name, unit_status,
)
from grain.automation.github import Issue
from grain.run import FakeRunner


def make_issue(number=1) -> Issue:
    return Issue(number=number, title="fix the thing", body="details here",
                 html_url="https://github.com/o/r/issues/1", labels=frozenset())


def test_unit_name_is_stable_per_sandbox():
    assert unit_name("sandbox-0") == "grain-task-sandbox-0"
    assert unit_name("sandbox-1") != unit_name("sandbox-0")


def test_dispatch_writes_the_prompt_over_stdin_not_argv():
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue())
    dd_argv, dd_stdin = runner.calls[0]
    assert dd_argv[0] == "dd"
    assert "fix the thing" in dd_stdin
    assert "details here" in dd_stdin
    # The issue content never appears as a literal argv element anywhere.
    for argv, _ in runner.calls:
        assert not any("fix the thing" in a for a in argv)


def test_dispatch_starts_a_systemd_unit_named_for_the_sandbox():
    runner = FakeRunner()
    unit = dispatch(runner, "sandbox-0", make_issue())
    assert unit == "grain-task-sandbox-0"
    assert runner.ran(f"sudo systemd-run --unit={unit}")


def test_dispatch_sets_remain_after_exit():
    # Without it, a *successful* unit self-collects within seconds — found
    # live against a real sandbox — and unit_status can no longer tell
    # "succeeded" from "never dispatched" once it's gone.
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue())
    assert any("--property=RemainAfterExit=yes" in argv for argv, _ in runner.calls)


def test_unit_status_active_while_still_running():
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n",
    )
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.ACTIVE


def test_unit_status_done_success_via_remain_after_exit():
    # RemainAfterExit's steady state on success: ActiveState stays "active"
    # — SubState=exited is what actually says the command is done.
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=exited\nResult=success\n",
    )
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.DONE_SUCCESS


def test_unit_status_done_success_defensive_fallback_without_remain_after_exit():
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=inactive\nResult=success\n")
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.DONE_SUCCESS


def test_unit_status_done_failed_via_active_state():
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=loaded\nActiveState=failed\nResult=exit-code\n")
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.DONE_FAILED


def test_unit_status_absent_when_never_loaded():
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=not-found\nActiveState=inactive\n")
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.ABSENT


def test_unit_status_absent_when_the_command_itself_fails():
    runner = FakeRunner()
    runner.expect("systemctl show", returncode=1, stderr="Could not resolve hostname")
    assert unit_status(runner, "grain-task-sandbox-0") is UnitState.ABSENT


def test_reap_stops_and_clears_failed_state():
    runner = FakeRunner()
    reap(runner, "grain-task-sandbox-0")
    assert runner.ran("sudo systemctl stop grain-task-sandbox-0")
    assert runner.ran("sudo systemctl reset-failed grain-task-sandbox-0")
