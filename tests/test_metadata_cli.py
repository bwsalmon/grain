import json

import pytest

from grain.cli import main


def write_config(tmp_path):
    (tmp_path / "config").mkdir(parents=True, exist_ok=True)
    (tmp_path / "config" / "metadata-server.json").write_text(json.dumps({
        "service_account_email": "narrow@p.iam.gserviceaccount.com",
        "project_id": "p",
    }))


def test_metadata_start_defaults_to_every_sandbox(tmp_path):
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2", "--dry-run",
                 "metadata", "start"]) == 0
    for sandbox in ("sandbox-0", "sandbox-1"):
        config_path = tmp_path / "state" / "metadata-server" / f"{sandbox}.config.json"
        assert config_path.exists()


def test_metadata_start_a_single_named_sandbox(tmp_path):
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2", "--dry-run",
                 "metadata", "start", "sandbox-0"]) == 0
    assert (tmp_path / "state" / "metadata-server" / "sandbox-0.config.json").exists()
    assert not (tmp_path / "state" / "metadata-server" / "sandbox-1.config.json").exists()


def test_metadata_start_rejects_an_unknown_name(tmp_path):
    write_config(tmp_path)
    with pytest.raises(SystemExit):
        main(["--data-dir", str(tmp_path), "--sandboxes", "2", "--dry-run",
              "metadata", "start", "nope"])


def test_metadata_stop_reaps_only_the_named_sandboxs_unit(tmp_path, capsys):
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2", "--dry-run",
                 "metadata", "stop", "sandbox-0"]) == 0
    out = capsys.readouterr().out
    assert "grain-metadata-sandbox-0" in out
    assert "grain-metadata-sandbox-1" not in out


def test_metadata_stop_defaults_to_every_sandbox(tmp_path, capsys):
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2", "--dry-run",
                 "metadata", "stop"]) == 0
    out = capsys.readouterr().out
    assert "grain-metadata-sandbox-0" in out
    assert "grain-metadata-sandbox-1" in out


def test_metadata_status_prints_a_line_per_sandbox(tmp_path, capsys):
    # No --dry-run: `systemctl show` is a read-only query (like `host
    # status`'s `virsh` calls), safe to actually run -- there's no unit by
    # this name on the machine running the test, so it reads ABSENT.
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "2",
                 "metadata", "status"]) == 0
    lines = capsys.readouterr().out.splitlines()
    assert len(lines) == 2
    assert lines[0].split() == ["sandbox-0", "absent"]
    assert lines[1].split() == ["sandbox-1", "absent"]


def test_metadata_sync_audit_reports_zero_events_with_no_log_yet(tmp_path, capsys):
    write_config(tmp_path)
    assert main(["--data-dir", str(tmp_path), "--sandboxes", "1",
                 "metadata", "sync-audit"]) == 0
    out = capsys.readouterr().out
    assert "forwarded 0 event(s)" in out
