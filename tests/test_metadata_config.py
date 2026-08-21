import ipaddress
import json
from pathlib import Path

import pytest

from grain.inventory import Cluster
from grain.metadata.config import (
    MetadataConfig, build_argv, instance_paths, render_instance_config,
)


def make_config(**overrides) -> MetadataConfig:
    defaults = dict(
        service_account_email="narrow@my-project.iam.gserviceaccount.com",
        project_id="my-project",
        numeric_project_id=123456,
    )
    defaults.update(overrides)
    return MetadataConfig(**defaults)


def test_load_reads_json_and_coerces_key_path(tmp_path):
    path = tmp_path / "metadata-server.json"
    path.write_text(json.dumps({
        "service_account_email": "narrow@p.iam.gserviceaccount.com",
        "project_id": "p",
        "key_path": "/data/secrets/gcp-service-account.json",
    }))
    config = MetadataConfig.load(path)
    assert config.service_account_email == "narrow@p.iam.gserviceaccount.com"
    assert config.key_path == Path("/data/secrets/gcp-service-account.json")


def test_load_applies_defaults_for_omitted_fields(tmp_path):
    path = tmp_path / "metadata-server.json"
    path.write_text(json.dumps({
        "service_account_email": "narrow@p.iam.gserviceaccount.com",
        "project_id": "p",
    }))
    config = MetadataConfig.load(path)
    assert config.numeric_project_id == 0
    assert config.binary == "gce_metadata_server"
    assert config.metadata_user == "grain-metadata"


def test_render_instance_config_carries_the_target_principal_and_project():
    config = make_config()
    doc = json.loads(render_instance_config(config, "sandbox-0"))
    instance = doc["computeMetadata"]["v1"]["instance"]
    project = doc["computeMetadata"]["v1"]["project"]
    default_sa = instance["serviceAccounts"]["default"]
    assert default_sa["email"] == config.service_account_email
    assert project["projectId"] == config.project_id
    assert project["numericProjectId"] == config.numeric_project_id


def test_render_instance_config_never_mentions_the_key_path():
    # The whole point of impersonation mode is that the config file names
    # the *target* principal, never the broad source key -- the primary
    # key only ever reaches the process via GOOGLE_APPLICATION_CREDENTIALS
    # (build_argv/launcher.py), never through this file.
    config = make_config(key_path=Path("/data/secrets/gcp-service-account.json"))
    rendered = render_instance_config(config, "sandbox-0")
    assert "gcp-service-account" not in rendered


def test_render_instance_config_is_stable_and_distinct_per_sandbox():
    config = make_config()
    a1 = json.loads(render_instance_config(config, "sandbox-0"))
    a2 = json.loads(render_instance_config(config, "sandbox-0"))
    b = json.loads(render_instance_config(config, "sandbox-1"))
    assert a1 == a2  # deterministic across calls/processes
    instance_a = a1["computeMetadata"]["v1"]["instance"]
    instance_b = b["computeMetadata"]["v1"]["instance"]
    assert instance_a["id"] != instance_b["id"]


def test_instance_paths_are_per_sandbox_under_state_metadata_server():
    paths = instance_paths(Path("/data"), "sandbox-1")
    assert paths.config_path == Path("/data/state/metadata-server/sandbox-1.config.json")
    assert paths.log_path == Path("/data/state/metadata-server/sandbox-1.log")


def test_build_argv_binds_the_controller_ip_and_this_sandboxs_port():
    cluster = Cluster(subnet=ipaddress.IPv4Network("10.100.0.0/24"))
    config = make_config()
    paths = instance_paths(Path("/data"), "sandbox-1")
    argv = build_argv(config, cluster, "sandbox-1", paths)
    assert f"-interface={cluster.controller_ip}" in argv
    assert f"-port=:{cluster.metadata_port('sandbox-1')}" in argv
    # Not the sandbox's own address -- these processes run on the
    # controller, one per sandbox; see net_linux.py's DNAT rule.
    assert str(cluster.address_of("sandbox-1")) not in " ".join(argv)


def test_build_argv_uses_impersonate_not_service_account_file():
    cluster = Cluster()
    config = make_config()
    paths = instance_paths(Path("/data"), "sandbox-0")
    argv = build_argv(config, cluster, "sandbox-0", paths)
    assert "--impersonate" in argv
    assert not any(a.startswith("-serviceAccountFile") for a in argv)


def test_build_argv_points_logtarget_at_the_per_sandbox_log_path():
    cluster = Cluster()
    config = make_config()
    paths = instance_paths(Path("/data"), "sandbox-0")
    argv = build_argv(config, cluster, "sandbox-0", paths)
    assert f"-logTarget={paths.log_path}" in argv


def test_build_argv_does_not_carry_the_primary_key_at_all():
    # GOOGLE_APPLICATION_CREDENTIALS is an environment variable, set by
    # the launcher via systemd-run --setenv -- it must never show up on
    # argv, which (unlike the environment) ends up in `ps` output.
    cluster = Cluster()
    config = make_config(key_path=Path("/data/secrets/gcp-service-account.json"))
    paths = instance_paths(Path("/data"), "sandbox-0")
    argv = build_argv(config, cluster, "sandbox-0", paths)
    assert not any("gcp-service-account" in a for a in argv)


def test_metadata_port_raises_for_the_controller_confirming_sandbox_only():
    cluster = Cluster()
    with pytest.raises(ValueError):
        cluster.metadata_port(cluster.controller_name)
