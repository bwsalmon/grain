import io
import json
import shlex
import sys

from grain.automation.mcp_server import (
    TOOLS, McpServer, ask_question, check_grain_health, complete_analysis, edit_file,
    main, read_automation_audit_log, read_file, read_grain_config, read_grain_logs,
    run_command, serve, write_file,
)
from grain.automation.ssh import SshRunner
from grain.run import FakeRunner

WORKSPACE = "/home/debian/workspace"


def healthy_runner() -> FakeRunner:
    runner = FakeRunner()
    runner.expect("true", returncode=0)
    runner.expect("systemctl is-system-running", stdout="running\n")
    runner.expect("docker info", stdout="Server Version: 27.0.0\n")
    runner.expect(
        "df -P /",
        stdout="Filesystem     1024-blocks     Used Available Capacity Mounted on\n"
               "/dev/vda1         20642428  8258764  11316384      43% /\n",
    )
    return runner


def test_run_command_runs_inside_the_workspace():
    runner = FakeRunner()
    runner.expect("bash -c", stdout="hi\n")
    result = run_command(runner, WORKSPACE, "echo hi")
    argv, _ = runner.calls[0]
    assert argv[:2] == ["bash", "-c"]
    assert argv[2].startswith(f"cd {shlex.quote(WORKSPACE)} && ")
    assert "hi" in result.text
    assert not result.is_error


def test_run_command_surfaces_nonzero_exit_without_raising():
    runner = FakeRunner()
    runner.expect("bash -c", returncode=1, stderr="boom")
    result = run_command(runner, WORKSPACE, "false")
    assert result.is_error
    assert "exit=1" in result.text
    assert "boom" in result.text


def test_run_command_wraps_with_timeout_coreutil_in_milliseconds():
    runner = FakeRunner()
    runner.expect("bash -c")
    run_command(runner, WORKSPACE, "sleep 100", timeout=5000)
    argv, _ = runner.calls[0]
    assert "timeout 5 bash -c" in argv[2]


def test_read_file_returns_line_numbered_content():
    runner = FakeRunner()
    runner.expect("cat --", stdout="alpha\nbeta\ngamma\n")
    result = read_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt")
    assert runner.calls[0][0] == ["cat", "--", f"{WORKSPACE}/f.txt"]
    assert result.text == "     1\talpha\n     2\tbeta\n     3\tgamma"


def test_read_file_honors_offset_and_limit():
    runner = FakeRunner()
    runner.expect("cat --", stdout="a\nb\nc\nd\ne\n")
    result = read_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", offset=1, limit=2)
    assert result.text == "     2\tb\n     3\tc"


def test_read_file_reports_a_remote_read_failure():
    runner = FakeRunner()
    runner.expect("cat --", returncode=1, stderr="No such file or directory")
    result = read_file(runner, WORKSPACE, f"{WORKSPACE}/missing.txt")
    assert result.is_error
    assert "missing.txt" in result.text


def test_edit_file_replaces_a_unique_string_and_writes_back():
    runner = FakeRunner()
    runner.expect("cat --", stdout="hello world\n")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", "world", "there")
    assert not result.is_error
    dd_calls = [(argv, stdin) for argv, stdin in runner.calls if argv[0] == "dd"]
    assert len(dd_calls) == 1
    dd_argv, dd_stdin = dd_calls[0]
    assert dd_argv == ["dd", f"of={WORKSPACE}/f.txt", "status=none"]
    assert dd_stdin == "hello there\n"


def test_edit_file_errors_when_old_string_is_not_found():
    runner = FakeRunner()
    runner.expect("cat --", stdout="hello world\n")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", "nope", "x")
    assert result.is_error
    assert "not found" in result.text
    assert not any(argv[0] == "dd" for argv, _ in runner.calls)


def test_edit_file_errors_when_old_string_is_not_unique_without_replace_all():
    runner = FakeRunner()
    runner.expect("cat --", stdout="foo foo foo\n")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", "foo", "bar")
    assert result.is_error
    assert "3 times" in result.text
    assert not any(argv[0] == "dd" for argv, _ in runner.calls)


def test_edit_file_replace_all_replaces_every_occurrence():
    runner = FakeRunner()
    runner.expect("cat --", stdout="foo foo foo\n")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", "foo", "bar", replace_all=True)
    assert not result.is_error
    dd_stdin = next(stdin for argv, stdin in runner.calls if argv[0] == "dd")
    assert dd_stdin == "bar bar bar\n"


def test_edit_file_propagates_a_remote_read_failure():
    runner = FakeRunner()
    runner.expect("cat --", returncode=1, stderr="No such file or directory")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/missing.txt", "a", "b")
    assert result.is_error
    assert "missing.txt" in result.text
    assert not any(argv[0] == "dd" for argv, _ in runner.calls)


def test_edit_file_reports_a_remote_write_failure():
    runner = FakeRunner()
    runner.expect("cat --", stdout="hello world\n")
    runner.expect("dd", returncode=1, stderr="disk full")
    result = edit_file(runner, WORKSPACE, f"{WORKSPACE}/f.txt", "world", "there")
    assert result.is_error
    assert "disk full" in result.text


def test_write_file_creates_the_parent_directory_then_writes():
    runner = FakeRunner()
    result = write_file(runner, WORKSPACE, f"{WORKSPACE}/sub/f.txt", "content\n")
    assert not result.is_error
    assert runner.calls[0][0] == ["mkdir", "-p", f"{WORKSPACE}/sub"]
    dd_argv, dd_stdin = next((argv, s) for argv, s in runner.calls if argv[0] == "dd")
    assert dd_argv == ["dd", f"of={WORKSPACE}/sub/f.txt", "status=none"]
    assert dd_stdin == "content\n"


def test_write_file_reports_a_mkdir_failure():
    runner = FakeRunner()
    runner.expect("mkdir -p", returncode=1, stderr="permission denied")
    result = write_file(runner, WORKSPACE, f"{WORKSPACE}/sub/f.txt", "content\n")
    assert result.is_error
    assert "permission denied" in result.text
    assert not any(argv[0] == "dd" for argv, _ in runner.calls)


def test_ask_question_writes_the_question_to_the_fixed_path_not_the_sandbox(tmp_path):
    """Unlike every other tool here, this never touches a `Runner` at all --
    the question is for a human, not the sandbox.
    """
    path = tmp_path / "question.txt"
    result = ask_question(str(path), "Should I use approach A or B?")
    assert path.read_text() == "Should I use approach A or B?"
    assert not result.is_error
    assert "recorded" in result.text.lower()


def test_ask_question_overwrites_a_prior_question_in_the_same_dispatch(tmp_path):
    path = tmp_path / "question.txt"
    ask_question(str(path), "first question")
    ask_question(str(path), "second question")
    assert path.read_text() == "second question"


def test_complete_analysis_writes_the_summary_to_the_fixed_path_not_the_sandbox(tmp_path):
    """Same shape as `ask_question` -- never touches a `Runner` at all,
    since the summary is for a human via GitHub, not the sandbox.
    """
    path = tmp_path / "analysis.txt"
    result = complete_analysis(str(path), "Approach A is already in place; no change needed.")
    assert path.read_text() == "Approach A is already in place; no change needed."
    assert not result.is_error
    assert "recorded" in result.text.lower()


def test_complete_analysis_overwrites_a_prior_summary_in_the_same_dispatch(tmp_path):
    path = tmp_path / "analysis.txt"
    complete_analysis(str(path), "first summary")
    complete_analysis(str(path), "second summary")
    assert path.read_text() == "second summary"


# --- read_grain_logs (bwsalmon/agents#62) -----------------------------------

def test_read_grain_logs_reads_the_journal_for_an_allowed_unit():
    runner = FakeRunner()
    runner.expect("journalctl -u grain-automation.service", stdout="a log line\n")
    result = read_grain_logs(runner, "grain-automation")
    assert not result.is_error
    assert "a log line" in result.text
    argv, _ = runner.calls[0]
    assert argv == ["journalctl", "-u", "grain-automation.service", "-n", "200", "--no-pager"]


def test_read_grain_logs_honors_a_custom_line_count():
    runner = FakeRunner()
    runner.expect("journalctl -u grain-git-proxy.service", stdout="line\n")
    read_grain_logs(runner, "grain-git-proxy", lines=50)
    argv, _ = runner.calls[0]
    assert argv == ["journalctl", "-u", "grain-git-proxy.service", "-n", "50", "--no-pager"]


def test_read_grain_logs_rejects_an_unknown_unit_without_running_anything():
    runner = FakeRunner()
    result = read_grain_logs(runner, "grain-agent-nope")
    assert result.is_error
    assert "Unknown unit" in result.text
    assert runner.calls == []


def test_read_grain_logs_surfaces_a_nonzero_exit_without_raising():
    runner = FakeRunner()
    runner.expect("journalctl -u grain-automation.service", returncode=1, stderr="no journal")
    result = read_grain_logs(runner, "grain-automation")
    assert result.is_error
    assert "no journal" in result.text


def test_tools_list_excludes_read_grain_logs_by_default():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    names = {t["name"] for t in response["result"]["tools"]}
    assert "read_grain_logs" not in names
    assert response["result"]["tools"] == TOOLS


def test_tools_list_includes_read_grain_logs_when_self_debug_is_enabled():
    server = McpServer(FakeRunner(), WORKSPACE, self_debug=True)
    response = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    names = {t["name"] for t in response["result"]["tools"]}
    assert "read_grain_logs" in names


# --- check_grain_health, read_grain_config, read_automation_audit_log
# (bwsalmon/agents#86) ---------------------------------------------------

def test_check_grain_health_checks_the_sandbox_runner_not_the_local_one():
    sandbox_runner = healthy_runner()
    local_runner = FakeRunner()
    result = check_grain_health(sandbox_runner, local_runner, "sandbox")
    assert not result.is_error
    assert "status=healthy" in result.text
    assert local_runner.calls == []


def test_check_grain_health_checks_the_local_runner_for_the_controller():
    sandbox_runner = FakeRunner()
    local_runner = healthy_runner()
    result = check_grain_health(sandbox_runner, local_runner, "controller")
    assert not result.is_error
    assert "status=healthy" in result.text
    assert sandbox_runner.calls == []


def test_check_grain_health_flags_a_degraded_report_as_an_error():
    local_runner = healthy_runner()
    local_runner.expect("docker info", returncode=1, stderr="docker daemon down")
    result = check_grain_health(FakeRunner(), local_runner, "controller")
    assert result.is_error
    assert "status=degraded" in result.text


def test_check_grain_health_rejects_an_unknown_target_without_running_anything():
    sandbox_runner = FakeRunner()
    local_runner = FakeRunner()
    result = check_grain_health(sandbox_runner, local_runner, "the-moon")
    assert result.is_error
    assert "Unknown target" in result.text
    assert sandbox_runner.calls == []
    assert local_runner.calls == []


def test_read_grain_config_reads_an_allowed_file():
    runner = FakeRunner()
    runner.expect("cat -- /data/config/automation.json", stdout='{"task_owner": "bwsalmon"}\n')
    result = read_grain_config(runner, "automation")
    assert not result.is_error
    assert "bwsalmon" in result.text
    assert runner.calls[0][0] == ["cat", "--", "/data/config/automation.json"]


def test_read_grain_config_reports_a_missing_optional_file_without_erroring():
    runner = FakeRunner()
    runner.expect("cat -- /data/config/gemini-key.json", returncode=1, stderr="No such file")
    result = read_grain_config(runner, "gemini-key")
    assert not result.is_error
    assert "does not exist" in result.text


def test_read_grain_config_rejects_an_unknown_file_without_running_anything():
    runner = FakeRunner()
    result = read_grain_config(runner, "controller-ssh")
    assert result.is_error
    assert "Unknown file" in result.text
    assert runner.calls == []


def test_read_automation_audit_log_reads_the_fixed_path():
    runner = FakeRunner()
    runner.expect("tail -n 200", stdout='{"outcome": "dispatched"}\n')
    result = read_automation_audit_log(runner)
    assert not result.is_error
    assert "dispatched" in result.text
    assert runner.calls[0][0] == ["tail", "-n", "200", "--", "/data/state/automation/audit.log"]


def test_read_automation_audit_log_honors_a_custom_line_count():
    runner = FakeRunner()
    runner.expect("tail -n 10", stdout="line\n")
    read_automation_audit_log(runner, lines=10)
    assert runner.calls[0][0] == ["tail", "-n", "10", "--", "/data/state/automation/audit.log"]


def test_read_automation_audit_log_surfaces_a_nonzero_exit_without_raising():
    runner = FakeRunner()
    runner.expect("tail -n 200", returncode=1, stderr="No such file or directory")
    result = read_automation_audit_log(runner)
    assert result.is_error
    assert "No such file or directory" in result.text


def test_tools_list_includes_all_four_self_debug_tools_when_enabled():
    server = McpServer(FakeRunner(), WORKSPACE, self_debug=True)
    response = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    names = {t["name"] for t in response["result"]["tools"]}
    assert {
        "read_grain_logs", "check_grain_health", "read_grain_config",
        "read_automation_audit_log",
    } <= names


def test_tools_call_check_grain_health_is_refused_when_self_debug_is_disabled():
    local_runner = FakeRunner()
    server = McpServer(FakeRunner(), WORKSPACE, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "check_grain_health", "arguments": {"target": "controller"}},
    })
    assert response["result"]["isError"] is True
    assert local_runner.calls == []


def test_tools_call_read_grain_config_is_refused_when_self_debug_is_disabled():
    local_runner = FakeRunner()
    server = McpServer(FakeRunner(), WORKSPACE, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_grain_config", "arguments": {"file": "automation"}},
    })
    assert response["result"]["isError"] is True
    assert local_runner.calls == []


def test_tools_call_read_automation_audit_log_is_refused_when_self_debug_is_disabled():
    local_runner = FakeRunner()
    server = McpServer(FakeRunner(), WORKSPACE, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_automation_audit_log", "arguments": {}},
    })
    assert response["result"]["isError"] is True
    assert local_runner.calls == []


def test_tools_call_routes_check_grain_health_to_the_local_runner_for_the_controller():
    sandbox_runner = FakeRunner()
    local_runner = healthy_runner()
    server = McpServer(sandbox_runner, WORKSPACE, self_debug=True, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "check_grain_health", "arguments": {"target": "controller"}},
    })
    assert response["result"]["isError"] is False
    assert "status=healthy" in response["result"]["content"][0]["text"]
    assert sandbox_runner.calls == []


def test_tools_call_routes_read_grain_config_to_the_local_runner_not_the_sandbox_runner():
    sandbox_runner = FakeRunner()
    local_runner = FakeRunner()
    local_runner.expect("cat -- /data/config/repo-allowlist.json", stdout='["owner/repo"]\n')
    server = McpServer(sandbox_runner, WORKSPACE, self_debug=True, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_grain_config", "arguments": {"file": "repo-allowlist"}},
    })
    assert response["result"]["isError"] is False
    assert "owner/repo" in response["result"]["content"][0]["text"]
    assert sandbox_runner.calls == []


def test_tools_call_routes_read_automation_audit_log_to_the_local_runner_not_the_sandbox_runner():
    sandbox_runner = FakeRunner()
    local_runner = FakeRunner()
    local_runner.expect("tail -n 200", stdout="hi\n")
    server = McpServer(sandbox_runner, WORKSPACE, self_debug=True, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_automation_audit_log", "arguments": {}},
    })
    assert response["result"]["isError"] is False
    assert "hi" in response["result"]["content"][0]["text"]
    assert sandbox_runner.calls == []


def test_tools_call_read_grain_logs_is_refused_when_self_debug_is_disabled():
    local_runner = FakeRunner()
    server = McpServer(FakeRunner(), WORKSPACE, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_grain_logs", "arguments": {"unit": "grain-automation"}},
    })
    assert response["result"]["isError"] is True
    assert local_runner.calls == []


def test_tools_call_routes_read_grain_logs_to_the_local_runner_not_the_sandbox_runner():
    sandbox_runner = FakeRunner()
    local_runner = FakeRunner()
    local_runner.expect("journalctl -u grain-automation.service", stdout="hi\n")
    server = McpServer(sandbox_runner, WORKSPACE, self_debug=True, local_runner=local_runner)
    response = server.handle({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "read_grain_logs", "arguments": {"unit": "grain-automation"}},
    })
    assert response["result"]["isError"] is False
    assert "hi" in response["result"]["content"][0]["text"]
    assert sandbox_runner.calls == []


# --- McpServer JSON-RPC dispatch -------------------------------------------

def test_initialize_reports_protocol_and_server_info():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 1, "method": "initialize"})
    assert response["result"]["serverInfo"]["name"] == "grain-sandbox"


def test_notifications_initialized_produces_no_response():
    server = McpServer(FakeRunner(), WORKSPACE)
    assert server.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}) is None


def test_tools_list_returns_exactly_the_six_tools():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
    names = {t["name"] for t in response["result"]["tools"]}
    assert names == {
        "run_command", "read_file", "edit_file", "write_file", "ask_question",
        "complete_analysis",
    }
    assert response["result"]["tools"] == TOOLS


def test_tools_call_routes_to_the_named_tool():
    runner = FakeRunner()
    runner.expect("bash -c", stdout="ok\n")
    server = McpServer(runner, WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 3, "method": "tools/call",
        "params": {"name": "run_command", "arguments": {"command": "echo ok"}},
    })
    assert response["result"]["isError"] is False
    assert "ok" in response["result"]["content"][0]["text"]


def test_tools_call_with_unknown_tool_returns_an_error():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 4, "method": "tools/call",
        "params": {"name": "delete_everything", "arguments": {}},
    })
    assert "error" in response
    assert response["error"]["code"] == -32602


def test_tools_call_with_a_missing_required_argument_returns_an_error():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 5, "method": "tools/call",
        "params": {"name": "run_command", "arguments": {}},
    })
    assert "error" in response


def test_unknown_method_returns_an_error_when_it_has_an_id():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 6, "method": "not/a/real/method"})
    assert response["error"]["code"] == -32601


def test_unknown_method_with_no_id_produces_no_response():
    # A JSON-RPC notification gets no response at all, even for an unknown
    # method -- there is no id for an error to be correlated back to.
    server = McpServer(FakeRunner(), WORKSPACE)
    assert server.handle({"jsonrpc": "2.0", "method": "not/a/real/method"}) is None


def test_tools_call_routes_ask_question_to_the_configured_path(tmp_path):
    path = tmp_path / "question.txt"
    server = McpServer(FakeRunner(), WORKSPACE, question_path=str(path))
    response = server.handle({
        "jsonrpc": "2.0", "id": 7, "method": "tools/call",
        "params": {"name": "ask_question", "arguments": {"question": "which way?"}},
    })
    assert response["result"]["isError"] is False
    assert path.read_text() == "which way?"


def test_tools_call_ask_question_without_a_configured_path_errors_not_crashes():
    server = McpServer(FakeRunner(), WORKSPACE)  # no question_path
    response = server.handle({
        "jsonrpc": "2.0", "id": 8, "method": "tools/call",
        "params": {"name": "ask_question", "arguments": {"question": "which way?"}},
    })
    assert response["result"]["isError"] is True


def test_tools_call_routes_complete_analysis_to_the_configured_path(tmp_path):
    path = tmp_path / "analysis.txt"
    server = McpServer(FakeRunner(), WORKSPACE, analysis_path=str(path))
    response = server.handle({
        "jsonrpc": "2.0", "id": 9, "method": "tools/call",
        "params": {"name": "complete_analysis", "arguments": {"summary": "no change needed"}},
    })
    assert response["result"]["isError"] is False
    assert path.read_text() == "no change needed"


def test_tools_call_complete_analysis_without_a_configured_path_errors_not_crashes():
    server = McpServer(FakeRunner(), WORKSPACE)  # no analysis_path
    response = server.handle({
        "jsonrpc": "2.0", "id": 10, "method": "tools/call",
        "params": {"name": "complete_analysis", "arguments": {"summary": "no change needed"}},
    })
    assert response["result"]["isError"] is True


def test_tools_call_routes_read_file_to_the_sandbox_runner():
    runner = FakeRunner()
    runner.expect("cat --", stdout="alpha\n")
    server = McpServer(runner, WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 11, "method": "tools/call",
        "params": {"name": "read_file", "arguments": {"file_path": f"{WORKSPACE}/f.txt"}},
    })
    assert response["result"]["isError"] is False
    assert "alpha" in response["result"]["content"][0]["text"]


def test_tools_call_routes_edit_file_to_the_sandbox_runner():
    runner = FakeRunner()
    runner.expect("cat --", stdout="hello world\n")
    server = McpServer(runner, WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 12, "method": "tools/call",
        "params": {"name": "edit_file", "arguments": {
            "file_path": f"{WORKSPACE}/f.txt", "old_string": "world", "new_string": "there",
        }},
    })
    assert response["result"]["isError"] is False
    dd_stdin = next(stdin for argv, stdin in runner.calls if argv[0] == "dd")
    assert dd_stdin == "hello there\n"


def test_tools_call_routes_write_file_to_the_sandbox_runner():
    runner = FakeRunner()
    server = McpServer(runner, WORKSPACE)
    response = server.handle({
        "jsonrpc": "2.0", "id": 13, "method": "tools/call",
        "params": {"name": "write_file", "arguments": {
            "file_path": f"{WORKSPACE}/f.txt", "content": "hi\n",
        }},
    })
    assert response["result"]["isError"] is False
    dd_stdin = next(stdin for argv, stdin in runner.calls if argv[0] == "dd")
    assert dd_stdin == "hi\n"


# --- serve() (stdin/stdout loop) --------------------------------------------

def test_serve_reads_a_request_and_writes_one_response_line():
    runner = FakeRunner()
    runner.expect("bash -c", stdout="ok\n")
    request = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "run_command", "arguments": {"command": "echo ok"}},
    })
    stdin = io.StringIO(request + "\n")
    stdout = io.StringIO()
    serve(runner, WORKSPACE, stdin=stdin, stdout=stdout)
    lines = stdout.getvalue().splitlines()
    assert len(lines) == 1
    response = json.loads(lines[0])
    assert response["result"]["isError"] is False


def test_serve_skips_blank_lines_and_malformed_json_without_responding():
    stdin = io.StringIO("\n   \nnot json at all\n")
    stdout = io.StringIO()
    serve(FakeRunner(), WORKSPACE, stdin=stdin, stdout=stdout)
    assert stdout.getvalue() == ""


def test_serve_writes_nothing_for_a_notification():
    stdin = io.StringIO(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
    stdout = io.StringIO()
    serve(FakeRunner(), WORKSPACE, stdin=stdin, stdout=stdout)
    assert stdout.getvalue() == ""


# --- main() (CLI wiring) -----------------------------------------------------

def test_main_wires_cli_args_into_serve(monkeypatch):
    captured = {}

    def fake_serve(runner, workspace, *, question_path, analysis_path, self_debug):
        captured.update(
            runner=runner, workspace=workspace, question_path=question_path,
            analysis_path=analysis_path, self_debug=self_debug,
        )

    monkeypatch.setattr("grain.automation.mcp_server.serve", fake_serve)
    monkeypatch.setattr(sys, "argv", [
        "mcp_server", "--address", "10.100.0.5", "--user", "agent",
        "--key-path", "/tmp/key", "--workspace", WORKSPACE,
        "--question-path", "/tmp/q.txt", "--analysis-path", "/tmp/a.txt",
        "--self-debug",
    ])
    main()
    assert captured["workspace"] == WORKSPACE
    assert captured["question_path"] == "/tmp/q.txt"
    assert captured["analysis_path"] == "/tmp/a.txt"
    assert captured["self_debug"] is True
    assert isinstance(captured["runner"], SshRunner)


def test_main_self_debug_defaults_to_off(monkeypatch):
    captured = {}

    def fake_serve(runner, workspace, *, question_path, analysis_path, self_debug):
        captured["self_debug"] = self_debug

    monkeypatch.setattr("grain.automation.mcp_server.serve", fake_serve)
    monkeypatch.setattr(sys, "argv", [
        "mcp_server", "--address", "10.100.0.5", "--user", "agent",
        "--key-path", "/tmp/key", "--workspace", WORKSPACE,
        "--question-path", "/tmp/q.txt", "--analysis-path", "/tmp/a.txt",
    ])
    main()
    assert captured["self_debug"] is False
