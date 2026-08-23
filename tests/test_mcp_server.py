import shlex

from grain.automation.mcp_server import (
    TOOLS, McpServer, ask_question, edit_file, read_file, run_command, write_file,
)
from grain.run import FakeRunner

WORKSPACE = "/home/debian/workspace"


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


def test_write_file_creates_the_parent_directory_then_writes():
    runner = FakeRunner()
    result = write_file(runner, WORKSPACE, f"{WORKSPACE}/sub/f.txt", "content\n")
    assert not result.is_error
    assert runner.calls[0][0] == ["mkdir", "-p", f"{WORKSPACE}/sub"]
    dd_argv, dd_stdin = next((argv, s) for argv, s in runner.calls if argv[0] == "dd")
    assert dd_argv == ["dd", f"of={WORKSPACE}/sub/f.txt", "status=none"]
    assert dd_stdin == "content\n"


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


# --- McpServer JSON-RPC dispatch -------------------------------------------

def test_initialize_reports_protocol_and_server_info():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 1, "method": "initialize"})
    assert response["result"]["serverInfo"]["name"] == "grain-sandbox"


def test_notifications_initialized_produces_no_response():
    server = McpServer(FakeRunner(), WORKSPACE)
    assert server.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}) is None


def test_tools_list_returns_exactly_the_five_tools():
    server = McpServer(FakeRunner(), WORKSPACE)
    response = server.handle({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
    names = {t["name"] for t in response["result"]["tools"]}
    assert names == {
        "run_command", "read_file", "edit_file", "write_file", "ask_question",
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
