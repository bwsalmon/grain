from grain.automation.cleanup import cleanup
from grain.run import FakeRunner


def test_cleanup_runs_kind_then_docker_prune():
    runner = FakeRunner()
    runner.expect("kind delete clusters --all", stdout="deleted cluster\n")
    runner.expect("docker system prune -af --volumes", stdout="Total reclaimed space: 0B\n")
    result = cleanup(runner)
    assert [c[0] for c in runner.calls] == [
        ["kind", "delete", "clusters", "--all"],
        ["docker", "system", "prune", "-af", "--volumes"],
    ]
    assert result.ok
    assert [s.name for s in result.steps] == ["kind", "docker"]
    assert all(s.ok for s in result.steps)


def test_kind_runs_before_docker_prune():
    # A kind cluster's nodes are docker containers; pruning first would
    # leave them "in use" and skipped.
    runner = FakeRunner()
    result = cleanup(runner)
    names = [step.name for step in result.steps]
    assert names.index("kind") < names.index("docker")


def test_a_missing_binary_is_captured_not_raised():
    runner = FakeRunner()
    runner.expect("kind delete clusters --all", returncode=127,
                   stderr="kind: command not found")
    result = cleanup(runner)  # must not raise
    kind_step = next(s for s in result.steps if s.name == "kind")
    assert not kind_step.ok
    assert "command not found" in kind_step.detail
    assert not result.ok


def test_a_failed_step_does_not_stop_the_rest():
    runner = FakeRunner()
    runner.expect("kind delete clusters --all", returncode=1, stderr="boom")
    runner.expect("docker system prune -af --volumes", stdout="ok\n")
    result = cleanup(runner)
    assert [s.ok for s in result.steps] == [False, True]


def test_every_step_runs_with_check_false():
    # cleanup() must never raise even when every command fails outright —
    # a bad sandbox can't be allowed to take down the sweep pass freeing it.
    runner = FakeRunner()
    runner.default = None
    for prefix in ("kind delete clusters --all", "docker system prune -af --volumes"):
        runner.expect(prefix, returncode=255, stderr="connection refused")
    result = cleanup(runner)  # would raise CommandError if check weren't False
    assert not result.ok


def test_summary_reports_pass_and_fail_per_step():
    runner = FakeRunner()
    runner.expect("kind delete clusters --all", returncode=1, stderr="boom")
    result = cleanup(runner)
    summary = result.summary()
    assert "kind=FAIL (boom)" in summary
    assert "docker=ok" in summary
