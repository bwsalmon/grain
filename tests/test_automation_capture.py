from pathlib import Path

import grain.automation.capture as capture_module
from grain.automation.capture import capture_trajectory

UNIT = "grain-task-sandbox-0"


def _point_transcript_at(monkeypatch, tmp_path: Path) -> Path:
    path = tmp_path / f"{UNIT}.transcript.jsonl"
    monkeypatch.setattr(capture_module, "transcript_path", lambda unit: str(path))
    return path


def test_capture_reads_the_exact_path_dispatch_writes_to(monkeypatch, tmp_path):
    path = _point_transcript_at(monkeypatch, tmp_path)
    path.write_text('{"type": "system"}\n{"type": "result", "result": "done"}\n')
    assert capture_trajectory(UNIT) == '{"type": "system"}\n{"type": "result", "result": "done"}\n'


def test_capture_returns_none_when_the_file_is_missing(monkeypatch, tmp_path):
    _point_transcript_at(monkeypatch, tmp_path)  # never written
    assert capture_trajectory(UNIT) is None


def test_capture_returns_none_for_an_empty_file(monkeypatch, tmp_path):
    path = _point_transcript_at(monkeypatch, tmp_path)
    path.write_text("")
    assert capture_trajectory(UNIT) is None


def test_capture_never_raises_even_when_unreadable(monkeypatch, tmp_path):
    path = _point_transcript_at(monkeypatch, tmp_path)
    path.mkdir()  # a directory at this path makes read_text() raise OSError
    assert capture_trajectory(UNIT) is None
