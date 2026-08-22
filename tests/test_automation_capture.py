from grain.automation.capture import capture_trajectory
from grain.automation.dispatch import transcript_path
from grain.run import FakeRunner


def test_capture_reads_the_exact_path_dispatch_writes_to():
    runner = FakeRunner()
    runner.expect(
        f"cat {transcript_path('grain-task-sandbox-0')}",
        stdout='{"type": "system"}\n{"type": "result", "result": "done"}\n',
    )
    text = capture_trajectory(runner, "grain-task-sandbox-0")
    assert text == '{"type": "system"}\n{"type": "result", "result": "done"}\n'


def test_capture_returns_none_when_the_file_is_missing():
    runner = FakeRunner()
    runner.expect("cat", returncode=1, stderr="cat: No such file or directory")
    assert capture_trajectory(runner, "grain-task-sandbox-0") is None


def test_capture_returns_none_for_an_empty_file():
    runner = FakeRunner()
    runner.expect("cat", stdout="")
    assert capture_trajectory(runner, "grain-task-sandbox-0") is None


def test_capture_never_raises_even_on_failure():
    runner = FakeRunner()
    runner.expect("cat", returncode=1, stderr="permission denied")
    # Must not raise -- capture_trajectory always calls with check=False.
    assert capture_trajectory(runner, "grain-task-sandbox-0") is None


def test_capture_uses_cat_not_a_new_channel():
    runner = FakeRunner()
    runner.expect("cat", stdout="content\n")
    capture_trajectory(runner, "grain-task-sandbox-0")
    assert runner.calls[0][0][0] == "cat"
