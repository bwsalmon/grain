import ipaddress
from pathlib import Path

import pytest

from grain.automation.ssh import SshRunner
from grain.run import CommandError, FakeRunner


@pytest.fixture
def ssh():
    inner = FakeRunner()
    runner = SshRunner(
        inner=inner, user="debian",
        address=ipaddress.IPv4Address("10.100.0.10"),
        key_path=Path("/data/secrets/controller-ssh"),
    )
    return runner, inner


def test_wraps_the_argv_with_ssh_and_the_target(ssh):
    runner, inner = ssh
    runner.run(["systemctl", "show", "grain-task-sandbox-0"])
    argv = inner.calls[0][0]
    assert argv[0] == "ssh"
    assert "-i" in argv and str(Path("/data/secrets/controller-ssh")) in argv
    assert "debian@10.100.0.10" in argv
    # One shell-quoted string, not a trailing argv: SSH's exec protocol has
    # no argv array, only a single command string, and OpenSSH joins
    # multiple trailing arguments with an unquoted space — verified live,
    # this is what silently turned `bash -c "sleep 5"` into bare `sleep`
    # word-split from a stray `5`. shlex.join is what makes it round-trip.
    assert argv[-1] == "systemctl show grain-task-sandbox-0"


def test_a_multi_word_argument_survives_as_one_token(ssh):
    runner, inner = ssh
    runner.run(["bash", "-c", "sleep 5"])
    argv = inner.calls[0][0]
    assert argv[-1] == "bash -c 'sleep 5'"


def test_never_trusts_a_persisted_host_key(ssh):
    # A sandbox at a fixed address gets a new host key on every recreate;
    # pinning the first one seen in the default known_hosts would break
    # every dispatch after that. Verified live against a real VM.
    runner, inner = ssh
    runner.run(["true"])
    argv = inner.calls[0][0]
    assert "UserKnownHostsFile=/dev/null" in argv


def test_never_touches_an_ambient_ssh_agent(ssh):
    # A stale/unresponsive SSH_AUTH_SOCK can hang the whole connection
    # before authentication even starts — reproduced live, no ConnectTimeout
    # covers that phase. This runner brings its own key; it has no use for
    # an agent.
    runner, inner = ssh
    runner.run(["true"])
    argv = inner.calls[0][0]
    assert "IdentityAgent=none" in argv


def test_stdin_passes_through_to_the_inner_runner(ssh):
    runner, inner = ssh
    runner.run(["dd", "of=/tmp/x"], stdin="hello")
    assert inner.calls[0][1] == "hello"


def test_result_reports_the_callers_own_argv_not_the_ssh_wrapper(ssh):
    runner, inner = ssh
    result = runner.run(["systemctl", "show", "unit"])
    assert result.argv == ["systemctl", "show", "unit"]


def test_a_remote_failure_raises_with_the_callers_argv(ssh):
    runner, inner = ssh
    inner.expect("ssh", returncode=1, stderr="boom")
    with pytest.raises(CommandError) as exc:
        runner.run(["claude", "-p"])
    assert exc.value.argv == ["claude", "-p"]


def test_check_false_suppresses_the_raise(ssh):
    runner, inner = ssh
    inner.expect("ssh", returncode=1, stderr="boom")
    result = runner.run(["claude", "-p"], check=False)
    assert result.returncode == 1
