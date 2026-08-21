from datetime import datetime, timedelta, timezone

from grain.automation.ratelimit import allow

NOW = datetime(2026, 1, 1, 12, 0, tzinfo=timezone.utc)


def test_allows_when_under_the_cap():
    timestamps = [NOW - timedelta(minutes=5)]
    assert allow(timestamps, NOW, per_hour=2) is True


def test_denies_at_the_cap():
    timestamps = [NOW - timedelta(minutes=5), NOW - timedelta(minutes=10)]
    assert allow(timestamps, NOW, per_hour=2) is False


def test_ignores_runs_outside_the_sliding_window():
    timestamps = [NOW - timedelta(hours=2)]
    assert allow(timestamps, NOW, per_hour=1) is True


def test_a_burst_spanning_a_clock_hour_boundary_is_still_capped():
    # A fixed-clock-hour bucket would treat these as two separate "empty"
    # hours; the sliding window must not.
    just_before_the_hour = datetime(2026, 1, 1, 12, 59, tzinfo=timezone.utc)
    just_after_the_hour = datetime(2026, 1, 1, 13, 1, tzinfo=timezone.utc)
    timestamps = [just_before_the_hour]
    assert allow(timestamps, just_after_the_hour, per_hour=1) is False
