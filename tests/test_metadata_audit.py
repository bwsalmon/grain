import json

from grain.metadata.audit import (
    FileAuditLog, RecordingAuditLog, iter_mint_events, sync,
)

# A real trace captured live against gce_metadata_server v4.2.5, run with
# -jsonLog -debug -logTarget, impersonating a (fake, non-functional) key --
# see docs/roadmap.md item 4. Three requests: a plain project-id lookup (not
# a mint), a service-account "email" lookup (touches
# /service-accounts/... but isn't a token/identity mint), and a token mint
# that fails because the key is fake. This is the exact shape
# iter_mint_events has to parse correctly, including the
# "using service account context" line that sits between a mint request
# and its outcome line.
REAL_TRACE = [
    '{"time":"2026-08-21T22:49:30.206337122Z","level":"INFO","msg":"Starting GCP metadataserver"}',
    '{"time":"2026-08-21T22:49:30.207002078Z","level":"INFO","msg":"credential_configuration","credential":{"type":"impersonated_credential","target_principal":"narrow@p.iam.gserviceaccount.com"}}',
    '{"time":"2026-08-21T22:49:30.209876158Z","level":"INFO","msg":"tcp socket specified","interface":"127.0.0.2","port":":9000"}',
    '{"time":"2026-08-21T22:49:33.459719359Z","level":"DEBUG","msg":"request","path":"/computeMetadata/v1/project/project-id","query":""}',
    '{"time":"2026-08-21T22:49:33.467435142Z","level":"DEBUG","msg":"request","path":"/computeMetadata/v1/instance/service-accounts/default/email","query":""}',
    '{"time":"2026-08-21T22:49:33.467520381Z","level":"DEBUG","msg":"using service account context: [default]"}',
    '{"time":"2026-08-21T22:49:36.29856649Z","level":"DEBUG","msg":"request","path":"/computeMetadata/v1/instance/service-accounts/default/token","query":""}',
    '{"time":"2026-08-21T22:49:36.298645077Z","level":"DEBUG","msg":"using service account context: [default]"}',
    '{"time":"2026-08-21T22:49:36.298891025Z","level":"ERROR","msg":"Error getting access_token ","raw_error":"impersonate: unable to generate access token: private key should be a PEM or plain PKCS1 or PKCS8"}',
    '{"time":"2026-08-21T22:49:37.528740238Z","level":"INFO","msg":"Server Exited Properly"}',
]


def test_iter_mint_events_ignores_non_mint_paths():
    events = list(iter_mint_events(REAL_TRACE))
    paths = [e.path for e in events]
    assert "/computeMetadata/v1/project/project-id" not in paths
    assert "/computeMetadata/v1/instance/service-accounts/default/email" not in paths


def test_iter_mint_events_reports_a_failed_mint_from_the_real_trace():
    events = list(iter_mint_events(REAL_TRACE))
    assert len(events) == 1
    event = events[0]
    assert event.path == "/computeMetadata/v1/instance/service-accounts/default/token"
    assert event.outcome == "error"
    assert "private key should be a PEM" in event.detail


def test_iter_mint_events_reports_success_when_no_error_follows():
    lines = [
        '{"level":"DEBUG","msg":"request","path":"/computeMetadata/v1/instance/service-accounts/default/token"}',
        '{"level":"DEBUG","msg":"using service account context: [default]"}',
        '{"level":"DEBUG","msg":"request","path":"/computeMetadata/v1/project/project-id"}',
    ]
    events = list(iter_mint_events(lines))
    assert len(events) == 1
    assert events[0].outcome == "minted"
    assert events[0].detail is None


def test_iter_mint_events_recognizes_identity_endpoint_too():
    lines = [
        '{"level":"DEBUG","msg":"request","path":"/computeMetadata/v1/instance/service-accounts/default/identity"}',
    ]
    events = list(iter_mint_events(lines))
    assert len(events) == 1
    assert events[0].path.endswith("/identity")


def test_iter_mint_events_skips_malformed_lines():
    lines = ["not json at all", "", *REAL_TRACE]
    events = list(iter_mint_events(lines))
    assert len(events) == 1


def test_file_audit_log_appends_one_json_line_per_record(tmp_path):
    path = tmp_path / "audit.log"
    audit = FileAuditLog(path)
    audit.record(sandbox="sandbox-0", path="/x/token", outcome="minted", detail=None)
    audit.record(sandbox="sandbox-1", path="/x/token", outcome="error", detail="boom")
    lines = path.read_text().splitlines()
    assert len(lines) == 2
    first = json.loads(lines[0])
    assert first["sandbox"] == "sandbox-0"
    assert first["outcome"] == "minted"
    assert "time" in first


def test_recording_audit_log_collects_entries_for_tests():
    audit = RecordingAuditLog()
    audit.record(sandbox="sandbox-0", path="/x/token", outcome="minted", detail=None)
    assert audit.entries == [
        {"sandbox": "sandbox-0", "path": "/x/token", "outcome": "minted", "detail": None}
    ]


def test_sync_forwards_new_events_and_is_idempotent(tmp_path):
    log_path = tmp_path / "sandbox-0.log"
    log_path.write_text("\n".join(REAL_TRACE) + "\n")
    state_path = tmp_path / "sandbox-0.offset.json"
    audit = RecordingAuditLog()

    forwarded = sync(log_path, state_path, "sandbox-0", audit)
    assert forwarded == 1
    assert audit.entries[0]["sandbox"] == "sandbox-0"
    assert audit.entries[0]["outcome"] == "error"

    # Calling again with no new lines forwards nothing more.
    again = sync(log_path, state_path, "sandbox-0", audit)
    assert again == 0
    assert len(audit.entries) == 1


def test_sync_picks_up_only_new_lines_appended_since_last_call(tmp_path):
    log_path = tmp_path / "sandbox-0.log"
    log_path.write_text("\n".join(REAL_TRACE[:6]) + "\n")  # up through the email lookup
    state_path = tmp_path / "sandbox-0.offset.json"
    audit = RecordingAuditLog()

    assert sync(log_path, state_path, "sandbox-0", audit) == 0  # no mint yet

    with log_path.open("a") as f:
        f.write("\n".join(REAL_TRACE[6:]) + "\n")
    assert sync(log_path, state_path, "sandbox-0", audit) == 1
    assert audit.entries[0]["outcome"] == "error"


def test_sync_on_a_log_that_does_not_exist_yet_is_zero_not_an_error(tmp_path):
    audit = RecordingAuditLog()
    forwarded = sync(tmp_path / "never-started.log", tmp_path / "offset.json",
                      "sandbox-0", audit)
    assert forwarded == 0
    assert audit.entries == []


def test_sync_does_not_consume_a_trailing_partial_line(tmp_path):
    log_path = tmp_path / "sandbox-0.log"
    log_path.write_text("\n".join(REAL_TRACE) + "\n")
    state_path = tmp_path / "sandbox-0.offset.json"
    audit = RecordingAuditLog()
    sync(log_path, state_path, "sandbox-0", audit)

    # A partial line appended (as if the writer were mid-append) must not
    # be forwarded, and must still be picked up whole once it's completed.
    with log_path.open("a") as f:
        f.write('{"level":"DEBUG","msg":"request","path":"/computeMetadata/v1/instance/ser')
    assert sync(log_path, state_path, "sandbox-0", audit) == 0

    with log_path.open("a") as f:
        f.write('vice-accounts/default/token"}\n')
    assert sync(log_path, state_path, "sandbox-0", audit) == 1
