from grain.automation.health import (
    HealthStatus, _disk_usage_percent, check_disk, check_docker,
    check_health, check_systemd,
)
from grain.run import FakeRunner

DF_LOW = "Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/vda1         20642428  8258764  11316384      43% /\n"
DF_HIGH = "Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/vda1         20642428 18578184   1000000      95% /\n"


def healthy_runner() -> FakeRunner:
    runner = FakeRunner()
    runner.expect("true", returncode=0)
    runner.expect("systemctl is-system-running", stdout="running\n")
    runner.expect("docker info", stdout="Server Version: 27.0.0\n")
    runner.expect("df -P /", stdout=DF_LOW)
    return runner


# --- the df parser, in isolation ------------------------------------------

def test_disk_usage_percent_parses_a_normal_df_line():
    assert _disk_usage_percent(DF_LOW) == 43


def test_disk_usage_percent_returns_none_for_garbage():
    assert _disk_usage_percent("not df output at all") is None


def test_disk_usage_percent_returns_none_for_empty_output():
    assert _disk_usage_percent("") is None


def test_disk_usage_percent_returns_none_with_no_percent_sign():
    # A malformed/foreign df build — must not crash, just fail to parse.
    assert _disk_usage_percent("Filesystem Used\n/dev/vda1 1234\n") is None


def test_disk_usage_percent_returns_none_when_the_percent_field_is_not_a_number():
    # Ends in "%" (passes the shape check) but isn't parseable as an int --
    # must fail to parse, not raise.
    header = "Filesystem     1024-blocks     Used Available Capacity Mounted on\n"
    assert _disk_usage_percent(header + "/dev/vda1 1 2 3 ab% /\n") is None


# --- individual checks -----------------------------------------------------

def test_check_disk_ok_below_watermark():
    runner = FakeRunner()
    runner.expect("df -P /", stdout=DF_LOW)
    result = check_disk(runner, watermark_percent=85)
    assert result.ok
    assert "43%" in result.detail


def test_check_disk_flags_at_or_above_watermark():
    runner = FakeRunner()
    runner.expect("df -P /", stdout=DF_HIGH)
    result = check_disk(runner, watermark_percent=85)
    assert not result.ok
    assert "95%" in result.detail


def test_check_disk_exactly_at_watermark_is_flagged():
    df = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda1 100 85 15 85% /\n"
    runner = FakeRunner()
    runner.expect("df -P /", stdout=df)
    result = check_disk(runner, watermark_percent=85)
    assert not result.ok  # >= watermark, not just >


def test_check_disk_reports_a_df_command_failure():
    runner = FakeRunner()
    runner.expect("df -P /", returncode=1, stderr="df: /: No such file or directory")
    result = check_disk(runner)
    assert not result.ok
    assert "df failed" in result.detail


def test_check_systemd_running_is_ok():
    runner = FakeRunner()
    runner.expect("systemctl is-system-running", stdout="running\n")
    assert check_systemd(runner).ok


def test_check_systemd_degraded_is_flagged():
    runner = FakeRunner()
    runner.expect("systemctl is-system-running", stdout="degraded\n", returncode=1)
    result = check_systemd(runner)
    assert not result.ok
    assert "degraded" in result.detail


def test_check_docker_responsive_is_ok():
    runner = FakeRunner()
    runner.expect("docker info", stdout="Server Version: 27.0.0\n")
    assert check_docker(runner).ok


def test_check_docker_daemon_down_is_flagged():
    runner = FakeRunner()
    runner.expect(
        "docker info", returncode=1,
        stderr="Cannot connect to the Docker daemon. Is the docker daemon running?",
    )
    result = check_docker(runner)
    assert not result.ok
    assert "Cannot connect" in result.detail


# --- the composed report -----------------------------------------------

def test_a_fully_healthy_sandbox_reports_healthy():
    report = check_health(healthy_runner())
    assert report.status is HealthStatus.HEALTHY
    assert report.ok
    assert {c.name for c in report.checks} == {"ssh", "systemd", "docker", "disk"}


def test_unreachable_ssh_short_circuits_every_other_check():
    runner = FakeRunner()
    runner.expect("true", returncode=255, stderr="ssh: connect to host port 22: "
                                                    "Connection timed out")
    report = check_health(runner)
    assert report.status is HealthStatus.UNREACHABLE
    assert not report.ok
    assert [c.name for c in report.checks] == ["ssh"]
    # None of the other probes were even attempted.
    assert not any(call[0][0] in ("systemctl", "docker", "df") for call in runner.calls)


def test_one_failing_check_degrades_but_does_not_hide_the_others():
    runner = healthy_runner()
    runner.expect("df -P /", stdout=DF_HIGH)  # override: disk now over watermark
    report = check_health(runner)
    assert report.status is HealthStatus.DEGRADED
    assert not report.ok
    names_ok = {c.name: c.ok for c in report.checks}
    assert names_ok == {"ssh": True, "systemd": True, "docker": True, "disk": False}


def test_disk_watermark_is_configurable():
    runner = healthy_runner()  # 43% used
    report = check_health(runner, watermark_percent=40)
    assert report.status is HealthStatus.DEGRADED
    disk = next(c for c in report.checks if c.name == "disk")
    assert not disk.ok


def test_check_health_never_raises_even_when_every_probe_fails():
    runner = FakeRunner()
    runner.expect("true", returncode=0)
    runner.expect("systemctl is-system-running", returncode=1, stderr="boom")
    runner.expect("docker info", returncode=1, stderr="boom")
    runner.expect("df -P /", returncode=1, stderr="boom")
    report = check_health(runner)  # must not raise
    assert report.status is HealthStatus.DEGRADED
