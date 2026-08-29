import pytest

from grain.run import CommandError, DryRunRunner, FakeRunner, RealRunner, Result


# --- RealRunner --------------------------------------------------------

def test_real_runner_captures_stdout_stderr_and_returncode():
    runner = RealRunner()
    result = runner.run(["sh", "-c", "echo out; echo err >&2; exit 3"], check=False)
    assert result.returncode == 3
    assert result.stdout == "out\n"
    assert result.stderr == "err\n"


def test_real_runner_raises_command_error_on_nonzero_exit_by_default():
    runner = RealRunner()
    with pytest.raises(CommandError) as excinfo:
        runner.run(["sh", "-c", "echo boom >&2; exit 1"])
    assert excinfo.value.returncode == 1
    assert "boom" in excinfo.value.stderr


def test_real_runner_does_not_raise_when_check_is_false():
    runner = RealRunner()
    result = runner.run(["sh", "-c", "exit 1"], check=False)
    assert result.returncode == 1


def test_real_runner_passes_stdin_through():
    runner = RealRunner()
    result = runner.run(["cat"], stdin="hello\n")
    assert result.stdout == "hello\n"


def test_real_runner_reports_missing_binary_as_returncode_127():
    runner = RealRunner()
    result = runner.run(["grain-agent-208c33c1-no-such-binary"], check=False)
    assert result.returncode == 127
    assert "not found on PATH" in result.stderr


def test_real_runner_missing_binary_still_raises_when_check_is_true():
    runner = RealRunner()
    with pytest.raises(CommandError) as excinfo:
        runner.run(["grain-agent-208c33c1-no-such-binary"])
    assert excinfo.value.returncode == 127


# --- DryRunRunner --------------------------------------------------------

def test_dry_run_runner_executes_read_only_commands_for_real():
    runner = DryRunRunner(inner=RealRunner())
    result = runner.run(["ip", "-j", "link", "show"], check=False)
    # A real `ip -j` invocation produces JSON, not the fake success result
    # a non-read-only command would get.
    assert result.stdout != ""
    assert runner.printed == []


def test_dry_run_runner_tolerates_a_missing_binary_for_read_only_commands():
    # Read-only probes must degrade to "nothing here" rather than crash when
    # the underlying tool (e.g. limactl) isn't installed yet.
    runner = DryRunRunner(inner=RealRunner())
    result = runner.run(["limactl", "list", "--json"], check=False)
    assert result.returncode == 127


def test_dry_run_runner_does_not_execute_mutating_commands():
    inner = FakeRunner()
    runner = DryRunRunner(inner=inner)
    result = runner.run(["limactl", "start", "sandbox-0"])
    assert inner.calls == []
    assert result.returncode == 0
    assert runner.printed == [["limactl", "start", "sandbox-0"]]


def test_dry_run_runner_prints_the_command_it_would_run(capsys):
    runner = DryRunRunner(inner=FakeRunner())
    runner.run(["nft", "add", "rule", "inet", "grain", "forward"])
    out = capsys.readouterr().out
    assert "nft add rule inet grain forward" in out


def test_dry_run_runner_prints_stdin_as_a_heredoc(capsys):
    runner = DryRunRunner(inner=FakeRunner())
    runner.run(["bash", "-c", "cat"], stdin="line one\nline two\n")
    out = capsys.readouterr().out
    assert "<<'EOF'" in out
    assert "line one\nline two\nEOF" in out


def test_dry_run_runner_only_matches_read_only_prefixes_at_word_boundaries():
    # "nft" alone is not in the read-only prefix list; only "nft list" is.
    # A prefix match must not fire just because "nftables" starts with "nft".
    inner = FakeRunner()
    runner = DryRunRunner(inner=inner)
    runner.run(["nftables-helper", "flush"])
    assert inner.calls == []
    assert runner.printed == [["nftables-helper", "flush"]]


def test_dry_run_runner_matches_a_read_only_prefix_regardless_of_trailing_args():
    runner = DryRunRunner(inner=FakeRunner())
    result = runner.run(["limactl", "list", "--json"])
    # Reaches the (fake) inner runner rather than being printed & short-circuited.
    assert runner.printed == []
    assert result.returncode == 0


# --- FakeRunner: recording and default behaviour -------------------------

def test_fake_runner_defaults_to_a_zero_exit_success():
    runner = FakeRunner()
    result = runner.run(["echo", "hi"])
    assert result == Result(["echo", "hi"], 0, "", "")


def test_fake_runner_records_every_call_including_stdin():
    runner = FakeRunner()
    runner.run(["a"], stdin="x")
    runner.run(["b", "c"])
    assert runner.calls == [(["a"], "x"), (["b", "c"], None)]
    assert runner.commands == ["a", "b c"]


def test_fake_runner_ran_checks_a_command_prefix():
    runner = FakeRunner()
    runner.run(["git", "clone", "https://example.com/repo"])
    assert runner.ran("git clone")
    assert not runner.ran("git push")


def test_fake_runner_default_response_used_when_nothing_configured():
    runner = FakeRunner(default=Result([], 9, "", "custom default"))
    result = runner.run(["whatever"], check=False)
    assert result.returncode == 9
    assert result.stderr == "custom default"


# --- FakeRunner: expect() prefix matching --------------------------------

def test_fake_runner_expect_matches_an_exact_command():
    runner = FakeRunner()
    runner.expect("git status", stdout="clean")
    assert runner.run(["git", "status"]).stdout == "clean"


def test_fake_runner_expect_matches_a_longer_command_with_that_prefix():
    runner = FakeRunner()
    runner.expect("git log", stdout="commits")
    result = runner.run(["git", "log", "-n", "1"])
    assert result.stdout == "commits"


def test_fake_runner_expect_does_not_match_across_word_boundaries():
    # "git log" is a string-prefix of "git logs", but they are different
    # commands and must not be confused with one another.
    runner = FakeRunner()
    runner.expect("git log", stdout="wrong")
    result = runner.run(["git", "logs", "-n", "5"])
    assert result.stdout != "wrong"


def test_fake_runner_expect_longest_matching_prefix_wins():
    runner = FakeRunner()
    runner.expect("gcloud", stdout="generic")
    runner.expect("gcloud services api-keys create", stdout="specific")
    result = runner.run(["gcloud", "services", "api-keys", "create", "--display-name=x"])
    assert result.stdout == "specific"


def test_fake_runner_expect_raises_command_error_on_nonzero_by_default():
    runner = FakeRunner()
    runner.expect("false", returncode=1, stderr="nope")
    with pytest.raises(CommandError):
        runner.run(["false"])


def test_fake_runner_expect_does_not_raise_when_check_is_false():
    runner = FakeRunner()
    runner.expect("false", returncode=1, stderr="nope")
    result = runner.run(["false"], check=False)
    assert result.returncode == 1
    assert result.stderr == "nope"
