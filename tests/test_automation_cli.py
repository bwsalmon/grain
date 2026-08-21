from datetime import datetime, timezone
from pathlib import Path

from grain.automation.state import AutomationState
from grain.cli import main


def test_status_reports_free_sandboxes_with_no_state_file(tmp_path: Path, capsys):
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "automation", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split() == ["sandbox-0", "free"]
    assert lines[1].split() == ["sandbox-1", "free"]


def test_status_reports_an_assigned_sandbox(tmp_path: Path, capsys):
    state = AutomationState()
    state.assign("sandbox-0", issue=42, unit="grain-task-sandbox-0",
                 now=datetime(2026, 1, 1, tzinfo=timezone.utc))
    state.save(tmp_path / "state" / "automation" / "state.json")

    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "automation", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert lines[0].split()[:2] == ["sandbox-0", "issue"]
    assert "42" in lines[0]
    assert lines[1].split() == ["sandbox-1", "free"]
