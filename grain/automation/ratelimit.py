"""Caps runs started per hour.

docs/design.md: "a cap on runs started per hour, so bulk-labelling forty
issues cannot consume the pool and a month of spend at once." A sliding
60-minute window over run-start timestamps, not a fixed per-clock-hour
bucket — a fixed bucket lets a burst at 12:59 and another at 13:01 both
land inside their own "empty" hour and double the intended cap.
"""

from __future__ import annotations

from datetime import datetime, timedelta

WINDOW = timedelta(hours=1)


def allow(timestamps: list[datetime], now: datetime, per_hour: int) -> bool:
    recent = sum(1 for t in timestamps if now - t < WINDOW)
    return recent < per_hour
