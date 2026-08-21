from grain.automation.dispatch import (
    UnitState, branch_name, configure_git_credentials, dispatch,
    ensure_workspace, reap, unit_name, unit_status,
)
from grain.automation.github import Issue
from grain.run import FakeRunner

REMOTE_URL = "http://10.100.0.2:8080/o/r.git"
TOKEN = "sandbox-token"


def make_issue(number=1) -> Issue:
    return Issue(number=number, title="fix the thing", body="details here",
                 html_url="https://github.com/o/r/issues/1", labels=frozenset())


def test_unit_name_is_stable_per_sandbox():
    assert unit_name("sandbox-0") == "grain-task-sandbox-0"
    assert unit_name("sandbox-1") != unit_name("sandbox-0")


def test_branch_name_is_a_pure_function_of_the_issue_number():
    assert branch_name(7) == "grain/issue-7"
    assert branch_name(7) == branch_name(7)
    assert branch_name(7) != branch_name(8)


def test_configure_git_credentials_sets_the_store_helper():
    runner = FakeRunner()
    configure_git_credentials(runner, REMOTE_URL, TOKEN)
    assert runner.ran("git config --global credential.helper store")


def test_configure_git_credentials_writes_the_token_over_stdin_not_argv():
    runner = FakeRunner()
    configure_git_credentials(runner, REMOTE_URL, TOKEN)
    dd_calls = [(argv, stdin) for argv, stdin in runner.calls if argv[0] == "dd"]
    assert len(dd_calls) == 1
    dd_argv, dd_stdin = dd_calls[0]
    assert dd_argv == ["dd", "of=/home/debian/.git-credentials", "status=none"]
    assert dd_stdin == "http://sandbox:sandbox-token@10.100.0.2:8080\n"
    # The token never appears as a literal argv element anywhere.
    for argv, _ in runner.calls:
        assert not any(TOKEN in a for a in argv)


def test_configure_git_credentials_restricts_the_file_mode():
    runner = FakeRunner()
    configure_git_credentials(runner, REMOTE_URL, TOKEN)
    assert runner.ran("chmod 600 /home/debian/.git-credentials")


def test_ensure_workspace_runs_one_bash_script():
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace")
    assert len(runner.calls) == 1
    argv, _ = runner.calls[0]
    assert argv[0] == "bash" and argv[1] == "-c"


def test_ensure_workspace_script_clones_when_absent_and_resets_when_present():
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace")
    script = runner.calls[0][0][2]
    assert "git clone" in script
    assert "git -C /home/debian/workspace fetch --prune origin" in script
    assert "git -C /home/debian/workspace clean -fdx" in script
    assert "checkout -f --detach origin/HEAD" in script
    # Reset to the remote's own default branch, never a hardcoded name —
    # dispatch() must not assume the target repo calls it "main".
    assert "origin/main" not in script


def test_dispatch_writes_the_prompt_over_stdin_not_argv():
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue(), remote_url=REMOTE_URL, token=TOKEN)
    dd_calls = [(argv, stdin) for argv, stdin in runner.calls if argv[0] == "dd"]
    prompt_argv, prompt_stdin = next(
        c for c in dd_calls if c[0][1] == "of=/tmp/grain-task-sandbox-0.md"
    )
    assert "fix the thing" in prompt_stdin
    assert "details here" in prompt_stdin
    # The issue content never appears as a literal argv element anywhere.
    for argv, _ in runner.calls:
        assert not any("fix the thing" in a for a in argv)


def test_dispatch_tells_the_agent_the_exact_branch_to_push():
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue(7), remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "dd" and argv[1] == "of=/tmp/grain-task-sandbox-0.md"
    )
    assert "grain/issue-7" in prompt_stdin


def test_dispatch_prepares_credentials_and_workspace_before_the_prompt():
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue(), remote_url=REMOTE_URL, token=TOKEN)
    commands = runner.commands
    credential_index = next(i for i, c in enumerate(commands) if c.startswith("git config"))
    clone_index = next(i for i, c in enumerate(commands) if c.startswith("bash -c"))
    prompt_index = next(
        i for i, (argv, _) in enumerate(runner.calls)
        if argv[0] == "dd" and argv[1] == "of=/tmp/grain-task-sandbox-0.md"
    )
    assert credential_index < clone_index < prompt_index


def test_dispatch_starts_a_systemd_unit_named_for_the_sandbox():
    runner = FakeRunner()
    unit = dispatch(runner, "sandbox-0", make_issue(), remote_url=REMOTE_URL, token=TOKEN)
    assert unit == "grain-task-sandbox-0"
    assert runner.ran(f"sudo systemd-run --unit={unit}")


def test_dispatch_runs_claude_from_inside_the_workspace():
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue(), remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(c for c in runner.commands if "systemd-run" in c)
    assert "cd /home/debian/workspace &&" in unit_call


def test_dispatch_sets_remain_after_exit():
    # Without it, a *successful* unit self-collects within seconds — found
    # live against a real sandbox — and unit_status can no longer tell
    # "succeeded" from "never dispatched" once it's gone.
    runner = FakeRunner()
    dispatch(runner, "sandbox-0", make_issue(), remote_url=REMOTE_URL, token=TOKEN)
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
