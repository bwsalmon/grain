import json
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from grain.automation.janitor import DeletedResource, JanitorConfig, run_janitor
from grain.run import FakeRunner

NOW = datetime(2026, 8, 26, 12, 0, 0, tzinfo=timezone.utc)
OLD = "2026-08-20T00:00:00Z"   # more than 24h before NOW
FRESH = "2026-08-26T11:00:00Z"  # within the last hour


def config(**overrides) -> JanitorConfig:
    fields = {"project_id": "proj"}
    fields.update(overrides)
    return JanitorConfig(**fields)


def _instances(*entries) -> str:
    """entries: (name, zone, creationTimestamp, labels)"""
    return json.dumps([
        {"name": name, "zone": f"https://www.googleapis.com/compute/v1/projects/proj/zones/{zone}",
         "creationTimestamp": created, "labels": labels}
        for name, zone, created, labels in entries
    ])


def _disks(*entries) -> str:
    """entries: (name, zone, creationTimestamp, labels, users)"""
    return json.dumps([
        {"name": name, "zone": f"https://www.googleapis.com/compute/v1/projects/proj/zones/{zone}",
         "creationTimestamp": created, "labels": labels, "users": users}
        for name, zone, created, labels, users in entries
    ])


def _keys(*entries) -> str:
    """entries: (name, displayName, createTime)"""
    return json.dumps([
        {"name": name, "displayName": display_name, "createTime": created}
        for name, display_name, created in entries
    ])


def _empty_runner(**overrides) -> FakeRunner:
    runner = FakeRunner()
    runner.expect("gcloud compute instances list", stdout="[]")
    runner.expect("gcloud compute disks list", stdout="[]")
    runner.expect("gcloud services api-keys list", stdout="[]")
    for prefix, kwargs in overrides.items():
        runner.expect(prefix, **kwargs)
    return runner


# --- authentication ---------------------------------------------------------

def test_run_janitor_activates_the_service_account_first():
    runner = _empty_runner()
    run_janitor(runner, config(), NOW)
    assert runner.commands[0].startswith("gcloud auth activate-service-account")


def test_run_janitor_authenticates_from_the_configured_key_file():
    runner = _empty_runner()
    run_janitor(runner, config(key_path=Path("/custom/key.json")), NOW)
    assert runner.ran("gcloud auth activate-service-account --key-file=/custom/key.json")


# --- instances ---------------------------------------------------------------

def test_deletes_an_instance_older_than_the_ttl():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("agent-thing", "us-central1-a", OLD, {})))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == [DeletedResource("instance", "agent-thing")]
    delete_call = next(c for c in runner.commands if "instances delete" in c)
    assert "agent-thing" in delete_call
    assert "--zone=us-central1-a" in delete_call
    assert "--project=proj" in delete_call
    assert "--quiet" in delete_call


def test_leaves_an_instance_younger_than_the_ttl():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("agent-thing", "us-central1-a", FRESH, {})))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert not any("instances delete" in c for c in runner.commands)


def test_never_deletes_the_grain_host_instance_by_name_even_if_old():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("grain-host", "us-central1-a", OLD, {})))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert not any("instances delete" in c for c in runner.commands)


def test_never_deletes_an_instance_labelled_managed_by_terraform():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list", stdout=_instances(
        ("something-else", "us-central1-a", OLD, {"managed-by": "terraform"}),
    ))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []


def test_respects_a_custom_name_prefix():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("acme-host", "us-central1-a", OLD, {})))
    result = run_janitor(runner, config(name_prefix="acme"), NOW)
    assert result.deleted == []


def test_respects_a_custom_ttl():
    runner = _empty_runner()
    # 3 hours old, ttl of 1 hour -> deletable
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("agent-thing", "us-central1-a", "2026-08-26T09:00:00Z", {})))
    result = run_janitor(runner, config(ttl_hours=1), NOW)
    assert len(result.deleted) == 1


def test_an_instance_delete_failure_is_recorded_as_a_warning_not_raised():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list",
                  stdout=_instances(("agent-thing", "us-central1-a", OLD, {})))
    runner.expect("gcloud compute instances delete", returncode=1, stderr="quota exceeded")
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert len(result.warnings) == 1
    assert result.warnings[0].kind == "instance"
    assert "quota exceeded" in result.warnings[0].detail


def test_an_unparseable_instance_listing_is_a_warning_not_a_crash():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list", stdout="not json")
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert any(w.kind == "instance" for w in result.warnings)


def test_a_permission_denied_listing_is_a_warning_not_a_crash():
    runner = _empty_runner()
    runner.expect("gcloud compute instances list", returncode=1, stderr="PERMISSION_DENIED")
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert any(w.kind == "instance" and "PERMISSION_DENIED" in w.detail for w in result.warnings)


# --- disks ---------------------------------------------------------------

def test_deletes_an_unattached_disk_older_than_the_ttl():
    runner = _empty_runner()
    runner.expect("gcloud compute disks list",
                  stdout=_disks(("agent-disk", "us-central1-a", OLD, {}, [])))
    result = run_janitor(runner, config(), NOW)
    assert len(result.deleted) == 1
    assert result.deleted[0].kind == "disk"
    delete_call = next(c for c in runner.commands if "disks delete" in c)
    assert "agent-disk" in delete_call


def test_never_deletes_an_attached_disk():
    runner = _empty_runner()
    runner.expect("gcloud compute disks list", stdout=_disks(
        ("agent-disk", "us-central1-a", OLD, {}, ["some-instance"]),
    ))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert not any("disks delete" in c for c in runner.commands)


def test_never_deletes_the_grain_data_disk_by_name_even_if_old_and_unattached():
    runner = _empty_runner()
    runner.expect("gcloud compute disks list",
                  stdout=_disks(("grain-data", "us-central1-a", OLD, {}, [])))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []


def test_never_deletes_a_disk_labelled_managed_by_terraform():
    runner = _empty_runner()
    runner.expect("gcloud compute disks list", stdout=_disks(
        ("something-else", "us-central1-a", OLD, {"managed-by": "terraform"}, []),
    ))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []


# --- gemini keys ---------------------------------------------------------

def test_deletes_a_grain_minted_gemini_key_older_than_the_ttl():
    runner = _empty_runner()
    runner.expect("gcloud services api-keys list", stdout=_keys(
        ("projects/1/locations/global/keys/abc", "grain-sandbox-0-issue-1", OLD),
    ))
    result = run_janitor(runner, config(), NOW)
    assert len(result.deleted) == 1
    assert result.deleted[0].kind == "gemini-key"
    delete_call = next(c for c in runner.commands if "api-keys delete" in c)
    assert "projects/1/locations/global/keys/abc" in delete_call


def test_never_deletes_a_key_with_no_grain_prefixed_display_name():
    runner = _empty_runner()
    runner.expect("gcloud services api-keys list", stdout=_keys(
        ("projects/1/locations/global/keys/abc", "some-other-teams-key", OLD),
    ))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []
    assert not any("api-keys delete" in c for c in runner.commands)


def test_never_deletes_a_gemini_key_still_referenced_by_a_live_assignment():
    runner = _empty_runner()
    live_key = "projects/1/locations/global/keys/abc"
    runner.expect("gcloud services api-keys list", stdout=_keys(
        (live_key, "grain-sandbox-0-issue-1", OLD),
    ))
    result = run_janitor(runner, config(), NOW,
                          protected_gemini_key_names=frozenset({live_key}))
    assert result.deleted == []


def test_leaves_a_gemini_key_younger_than_the_ttl():
    runner = _empty_runner()
    runner.expect("gcloud services api-keys list", stdout=_keys(
        ("projects/1/locations/global/keys/abc", "grain-sandbox-0-issue-1", FRESH),
    ))
    result = run_janitor(runner, config(), NOW)
    assert result.deleted == []


# --- config ---------------------------------------------------------------

def test_config_defaults_to_the_controllers_one_gcp_credential():
    assert str(config().key_path) == "/data/secrets/gcp-key-minter.json"


def test_config_defaults_to_a_24_hour_ttl():
    assert config().ttl_hours == 24


def test_config_defaults_to_the_grain_name_prefix():
    assert config().name_prefix == "grain"


def test_config_loads_from_json():
    path = Path(tempfile.mkdtemp()) / "janitor.json"
    path.write_text(json.dumps({
        "project_id": "proj", "ttl_hours": 48, "name_prefix": "acme",
        "key_path": "/custom/key.json",
    }))
    loaded = JanitorConfig.load(path)
    assert loaded.project_id == "proj"
    assert loaded.ttl_hours == 48
    assert loaded.name_prefix == "acme"
    assert loaded.key_path == Path("/custom/key.json")
