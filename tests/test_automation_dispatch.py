import json

from grain.automation.dispatch import (
    CONTROLLER_AGENT_TOKEN_PATH, CONTROLLER_AGENT_USER, SandboxTarget, UnitState,
    branch_name, configure_git_credentials, dispatch, dispatch_pr, ensure_workspace,
    reap, transcript_path, unit_name, unit_status,
)
from grain.automation.github import Issue, PullRequestDetail, ReviewComment
from grain.run import FakeRunner

REMOTE_URL = "http://10.100.0.2:8080/o/r.git"
TOKEN = "sandbox-token"
UNIT = "grain-task-sandbox-0"
UNIT_DIR = "/data/state/automation/units/grain-task-sandbox-0"
PROMPT_PATH = f"{UNIT_DIR}/prompt.md"
MCP_CONFIG_PATH = f"{UNIT_DIR}/mcp-config.json"


def make_issue(number=1) -> Issue:
    return Issue(number=number, title="fix the thing", body="details here",
                 html_url="https://github.com/o/r/issues/1", labels=frozenset())


def make_pr(number=1, head_ref="feature-x") -> PullRequestDetail:
    return PullRequestDetail(
        number=number, title="a distinctive PR title", body="a distinctive PR body",
        html_url="https://github.com/o/r/pull/1", head_ref=head_ref, base_ref="main",
    )


def make_comments() -> list[ReviewComment]:
    return [ReviewComment(id=1, user="reviewer", body="please fix this",
                           path="src/thing.py", line=12)]


def make_target(**overrides) -> SandboxTarget:
    fields = {
        "address": "10.100.0.10", "ssh_user": "debian",
        "ssh_key_path": "/data/secrets/controller-ssh",
    }
    fields.update(overrides)
    return SandboxTarget(**fields)


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


def test_configure_git_credentials_sets_a_fixed_git_identity():
    # Found live: a fresh sandbox has no user.name/user.email configured
    # anywhere, which makes `git commit` fail outright.
    runner = FakeRunner()
    configure_git_credentials(runner, REMOTE_URL, TOKEN)
    assert runner.ran("git config --global user.name")
    assert runner.ran("git config --global user.email")


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


def test_dispatch_writes_the_prompt_to_a_controller_local_file_over_stdin_not_argv():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
    dd_calls = [(argv, stdin) for argv, stdin in runner.calls if argv[:2] == ["sudo", "dd"]]
    prompt_argv, prompt_stdin = next(
        c for c in dd_calls if c[0][2] == f"of={PROMPT_PATH}"
    )
    assert "fix the thing" in prompt_stdin
    assert "details here" in prompt_stdin
    # The issue content never appears as a literal argv element anywhere.
    for argv, _ in runner.calls:
        assert not any("fix the thing" in a for a in argv)


def test_dispatch_tells_the_agent_the_exact_branch_to_push():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(7),
             remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert "grain/issue-7" in prompt_stdin


def test_dispatch_writes_an_mcp_config_naming_the_assigned_sandbox():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(address="10.100.0.42"),
              make_issue(), remote_url=REMOTE_URL, token=TOKEN)
    mcp_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={MCP_CONFIG_PATH}"
    )
    mcp_config = json.loads(mcp_stdin)
    server = mcp_config["mcpServers"]["grain-sandbox"]
    assert server["args"][server["args"].index("--address") + 1] == "10.100.0.42"
    assert "grain.automation.mcp_server" in server["args"]


def test_dispatch_prepares_credentials_and_workspace_before_the_prompt():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
    commands = runner.commands
    credential_index = next(i for i, c in enumerate(commands) if c.startswith("git config"))
    clone_index = next(i for i, c in enumerate(commands) if c.startswith("bash -c"))
    prompt_index = next(
        i for i, (argv, _) in enumerate(runner.calls)
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert credential_index < clone_index < prompt_index


def test_dispatch_starts_a_systemd_unit_named_for_the_sandbox_as_the_agent_user():
    runner = FakeRunner()
    unit = dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
                     remote_url=REMOTE_URL, token=TOKEN)
    assert unit == "grain-task-sandbox-0"
    assert runner.ran(f"sudo systemd-run --unit={unit} --uid={CONTROLLER_AGENT_USER}")


def test_dispatch_runs_claude_from_opt_grain_with_only_mcp_and_native_exceptions():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(argv[-1] for argv, _ in runner.calls if "systemd-run" in argv)
    assert "cd /opt/grain &&" in unit_call
    # Named directly on --tools, not just --allowedTools -- found live:
    # --allowedTools alone does not add a native tool back once --tools
    # excludes it. TodoWrite tried and dropped -- confirmed live that no
    # --tools syntax admits it in -p/headless mode at all.
    assert "--tools Task" in unit_call
    assert f"--mcp-config {MCP_CONFIG_PATH}" in unit_call
    assert "--strict-mcp-config" in unit_call
    assert "mcp__grain-sandbox__run_command" in unit_call
    assert "mcp__grain-sandbox__edit_file" in unit_call
    assert "--allowedTools" in unit_call and "Task" in unit_call
    assert "--no-session-persistence" in unit_call
    # The permission-mode flag existed only to auto-approve the native
    # Edit/Write tools' prompts -- meaningless once --tools excludes them,
    # and must not be carried over.
    assert "--permission-mode" not in unit_call


def test_dispatch_exports_the_oauth_token_from_a_file_not_argv():
    # The token itself never appears in the systemd-run command -- only a
    # `cat` of CONTROLLER_AGENT_TOKEN_PATH does, read into the environment
    # at runtime so the raw value never lands in `ps` output.
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(argv[-1] for argv, _ in runner.calls if "systemd-run" in argv)
    export_line = f'export CLAUDE_CODE_OAUTH_TOKEN="$(cat {CONTROLLER_AGENT_TOKEN_PATH})"'
    assert export_line in unit_call
    # Exported before claude -p is ever invoked.
    assert unit_call.index(export_line) < unit_call.index("claude -p")


# --- captured trajectory (docs/roadmap.md item 10) -------------------------

def test_transcript_path_is_a_controller_local_pure_function_of_the_unit():
    assert transcript_path(UNIT) == f"{UNIT_DIR}/transcript.jsonl"
    assert transcript_path(UNIT) == transcript_path(UNIT)
    assert transcript_path("grain-task-sandbox-1") != transcript_path(UNIT)


def test_dispatch_asks_claude_for_stream_json_output():
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(c for c in runner.commands if "systemd-run" in c)
    assert "--output-format stream-json --verbose" in unit_call


def test_dispatch_redirects_claude_output_to_the_transcript_path():
    runner = FakeRunner()
    unit = dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
                     remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(c for c in runner.commands if "systemd-run" in c)
    assert f"> {transcript_path(unit)}" in unit_call


def test_dispatch_pr_also_redirects_to_the_transcript_path():
    runner = FakeRunner()
    unit = dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(),
                        make_comments(), remote_url=REMOTE_URL, token=TOKEN)
    unit_call = next(c for c in runner.commands if "systemd-run" in c)
    assert f"> {transcript_path(unit)}" in unit_call
    assert "--output-format stream-json --verbose" in unit_call


def test_dispatch_sets_remain_after_exit():
    # Without it, a *successful* unit self-collects within seconds — found
    # live against a real sandbox — and unit_status can no longer tell
    # "succeeded" from "never dispatched" once it's gone.
    runner = FakeRunner()
    dispatch(runner, runner, "sandbox-0", make_target(), make_issue(),
             remote_url=REMOTE_URL, token=TOKEN)
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


# --- ensure_workspace's branch parameter (docs/roadmap.md item 9) ---------

def test_ensure_workspace_with_no_branch_is_unchanged_from_before():
    # Exact regression guard for the default-branch path — item 9 must not
    # change a single byte of what the existing issue-dispatch path sends.
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace")
    script = runner.calls[0][0][2]
    assert "git clone http://10.100.0.2:8080/o/r.git /home/debian/workspace" in script
    assert "checkout -f --detach origin/HEAD" in script
    assert "-B" not in script


def test_ensure_workspace_with_a_branch_resets_to_that_branch_not_head():
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace", branch="feature-x")
    script = runner.calls[0][0][2]
    assert "checkout -f -B feature-x origin/feature-x" in script
    # The default-branch reset must not also be present.
    assert "checkout -f --detach" not in script
    assert "origin/HEAD" not in script


def test_ensure_workspace_with_a_branch_skips_the_remote_set_head_call():
    # remote set-head only matters for the default-branch path; a branch
    # dispatch has no use for origin/HEAD at all.
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace", branch="feature-x")
    script = runner.calls[0][0][2]
    assert "remote set-head" not in script


def test_ensure_workspace_checks_out_the_branch_on_first_clone_too():
    # A plain `git clone` lands on the remote's default branch, not the
    # PR's own — the first-dispatch (no existing workspace) path must also
    # explicitly check out the requested branch, or a sandbox's very first
    # PR dispatch would silently start on the wrong branch.
    runner = FakeRunner()
    ensure_workspace(runner, REMOTE_URL, path="/home/debian/workspace", branch="feature-x")
    script = runner.calls[0][0][2]
    else_clause = script.split("else\n", 1)[1]
    assert "checkout -f -B feature-x origin/feature-x" in else_clause


# --- dispatch_pr (docs/roadmap.md item 9) ----------------------------------

def test_dispatch_pr_checks_out_the_prs_own_branch():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(head_ref="feature-x"),
                make_comments(), remote_url=REMOTE_URL, token=TOKEN)
    clone_calls = [argv for argv, _ in runner.calls if argv[:2] == ["bash", "-c"]]
    assert clone_calls
    assert "checkout -f -B feature-x origin/feature-x" in clone_calls[0][2]


def test_dispatch_pr_writes_a_prompt_carrying_title_body_and_comments():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(), make_comments(),
                remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert "a distinctive PR title" in prompt_stdin
    assert "a distinctive PR body" in prompt_stdin
    assert "please fix this" in prompt_stdin
    assert "src/thing.py" in prompt_stdin
    # Untrusted PR/comment content never appears as a literal argv element.
    for argv, _ in runner.calls:
        assert not any("a distinctive PR title" in a for a in argv)
        assert not any("please fix this" in a for a in argv)


def test_dispatch_pr_prompt_tells_the_agent_this_is_not_a_fresh_task():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(), make_comments(),
                remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert "not a fresh task" in prompt_stdin.lower() or "not a fresh" in prompt_stdin.lower()


def test_dispatch_pr_prompt_tells_the_agent_the_exact_branch_to_push():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(head_ref="feature-x"),
                make_comments(), remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert "git push origin HEAD:feature-x" in prompt_stdin


def test_dispatch_pr_prompt_handles_no_review_comments():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(), [],
                remote_url=REMOTE_URL, token=TOKEN)
    prompt_stdin = next(
        stdin for argv, stdin in runner.calls
        if argv[0] == "sudo" and argv[1] == "dd" and argv[2] == f"of={PROMPT_PATH}"
    )
    assert "no inline review comments" in prompt_stdin


def test_dispatch_pr_starts_a_systemd_unit_named_for_the_sandbox():
    runner = FakeRunner()
    unit = dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(),
                        make_comments(), remote_url=REMOTE_URL, token=TOKEN)
    assert unit == "grain-task-sandbox-0"
    assert runner.ran(f"sudo systemd-run --unit={unit}")


def test_dispatch_pr_configures_credentials_before_the_workspace():
    runner = FakeRunner()
    dispatch_pr(runner, runner, "sandbox-0", make_target(), make_pr(), make_comments(),
                remote_url=REMOTE_URL, token=TOKEN)
    commands = runner.commands
    credential_index = next(i for i, c in enumerate(commands) if c.startswith("git config"))
    clone_index = next(i for i, c in enumerate(commands) if c.startswith("bash -c"))
    assert credential_index < clone_index
