import json

from grain.proxy.audit import FileAuditLog, RecordingAuditLog


def test_file_audit_log_appends_one_json_line_per_record(tmp_path):
    path = tmp_path / "state" / "git-proxy" / "audit.log"
    audit = FileAuditLog(path)
    audit.record(sandbox="sandbox-0", owner="acme", repo="widgets",
                 action="upload-pack", credential="acme-widgets", outcome="forwarded: 200")
    audit.record(sandbox="sandbox-1", owner="acme", repo="widgets",
                 action="upload-pack", credential=None, outcome="denied: not allow-listed")
    lines = path.read_text().splitlines()
    assert len(lines) == 2
    first = json.loads(lines[0])
    assert first["sandbox"] == "sandbox-0"
    assert first["repo"] == "acme/widgets"
    assert first["action"] == "upload-pack"
    assert first["credential"] == "acme-widgets"
    assert first["outcome"] == "forwarded: 200"
    assert "time" in first


def test_file_audit_log_creates_its_parent_directory(tmp_path):
    path = tmp_path / "nested" / "state" / "git-proxy" / "audit.log"
    FileAuditLog(path)  # must not raise for a not-yet-existing parent
    assert path.parent.is_dir()


def test_recording_audit_log_collects_entries_for_tests():
    audit = RecordingAuditLog()
    audit.record(sandbox="sandbox-0", owner="acme", repo="widgets",
                 action="upload-pack", credential="acme-widgets", outcome="forwarded: 200")
    assert audit.entries == [
        {"sandbox": "sandbox-0", "repo": "acme/widgets", "action": "upload-pack",
         "credential": "acme-widgets", "outcome": "forwarded: 200"}
    ]
