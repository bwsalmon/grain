import json

from grain.automation.audit import FileAuditLog, NullAuditLog, RecordingAuditLog


def test_file_audit_log_appends_one_json_line_per_record(tmp_path):
    path = tmp_path / "state" / "automation" / "audit.log"
    audit = FileAuditLog(path)
    audit.record(sandbox="sandbox-0", issue=42, outcome="dispatched")
    audit.record(sandbox=None, issue=None, outcome="no free sandbox")
    lines = path.read_text().splitlines()
    assert len(lines) == 2
    first = json.loads(lines[0])
    assert first["sandbox"] == "sandbox-0"
    assert first["issue"] == 42
    assert first["outcome"] == "dispatched"
    assert "time" in first
    second = json.loads(lines[1])
    assert second["sandbox"] is None
    assert second["issue"] is None


def test_file_audit_log_creates_its_parent_directory(tmp_path):
    path = tmp_path / "nested" / "state" / "automation" / "audit.log"
    FileAuditLog(path)  # must not raise for a not-yet-existing parent
    assert path.parent.is_dir()


def test_null_audit_log_accepts_any_keyword_arguments_and_does_nothing():
    audit = NullAuditLog()
    audit.record(sandbox="sandbox-0", issue=1, outcome="dispatched")  # must not raise


def test_recording_audit_log_collects_entries_for_tests():
    audit = RecordingAuditLog()
    audit.record(sandbox="sandbox-0", issue=42, outcome="dispatched")
    assert audit.entries == [{"sandbox": "sandbox-0", "issue": 42, "outcome": "dispatched"}]
