import json

from grain.automation.dispatch import UnitState
from grain.inventory import Cluster
from grain.metadata.config import MetadataConfig
from grain.metadata.launcher import MetadataLauncher, build_launcher, unit_name
from grain.run import FakeRunner


def make_launcher(tmp_path, runner=None) -> MetadataLauncher:
    config = MetadataConfig(
        service_account_email="narrow@p.iam.gserviceaccount.com",
        project_id="p",
        numeric_project_id=1,
        key_path=tmp_path / "secrets" / "gcp-service-account.json",
    )
    return MetadataLauncher(
        cluster=Cluster(), config=config, runner=runner or FakeRunner(), data_dir=tmp_path,
    )


def test_unit_name_is_stable_per_sandbox():
    assert unit_name("sandbox-0") == "grain-metadata-sandbox-0"
    assert unit_name("sandbox-1") != unit_name("sandbox-0")


def test_start_writes_the_instance_config_before_launching(tmp_path):
    launcher = make_launcher(tmp_path)
    launcher.start("sandbox-0")
    config_path = tmp_path / "state" / "metadata-server" / "sandbox-0.config.json"
    assert config_path.exists()
    doc = json.loads(config_path.read_text())
    email = doc["computeMetadata"]["v1"]["instance"]["serviceAccounts"]["default"]["email"]
    assert email == "narrow@p.iam.gserviceaccount.com"


def test_start_runs_systemd_run_with_the_unit_name_and_property(tmp_path):
    runner = FakeRunner()
    launcher = make_launcher(tmp_path, runner)
    launcher.start("sandbox-0")
    assert runner.ran(f"sudo systemd-run --unit={unit_name('sandbox-0')}")
    assert any("--property=RemainAfterExit=yes" in argv for argv, _ in runner.calls)


def test_start_sets_the_metadata_user_not_the_sandbox_login_user(tmp_path):
    runner = FakeRunner()
    launcher = make_launcher(tmp_path, runner)
    launcher.start("sandbox-0")
    ((argv, _),) = [(a, s) for a, s in runner.calls]
    assert "--uid=grain-metadata" in argv
    assert "--uid=debian" not in argv


def test_start_passes_the_key_path_only_via_setenv_not_argv(tmp_path):
    runner = FakeRunner()
    launcher = make_launcher(tmp_path, runner)
    launcher.start("sandbox-0")
    ((argv, _),) = [(a, s) for a, s in runner.calls]
    key_path = tmp_path / "secrets" / "gcp-service-account.json"
    assert f"--setenv=GOOGLE_APPLICATION_CREDENTIALS={key_path}" in argv
    # And nowhere else on the command line (e.g. as a bare argument or a
    # -serviceAccountFile flag) -- only via the one --setenv flag.
    assert sum(1 for a in argv if str(key_path) in a) == 1


def test_start_binds_this_sandboxs_controller_port(tmp_path):
    runner = FakeRunner()
    launcher = make_launcher(tmp_path, runner)
    launcher.start("sandbox-1")
    ((argv, _),) = [(a, s) for a, s in runner.calls]
    cluster = Cluster()
    assert any(a == f"-port=:{cluster.metadata_port('sandbox-1')}" for a in argv)
    assert any(a == f"-interface={cluster.controller_ip}" for a in argv)


def test_stop_stops_and_clears_failed_state(tmp_path):
    runner = FakeRunner()
    launcher = make_launcher(tmp_path, runner)
    launcher.stop("sandbox-0")
    unit = unit_name("sandbox-0")
    assert runner.ran(f"sudo systemctl stop {unit}")
    assert runner.ran(f"sudo systemctl reset-failed {unit}")


def test_status_reflects_systemctl_show_output(tmp_path):
    runner = FakeRunner()
    runner.expect(
        "systemctl show",
        stdout="LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n",
    )
    launcher = make_launcher(tmp_path, runner)
    assert launcher.status("sandbox-0") is UnitState.ACTIVE


def test_status_absent_when_never_started(tmp_path):
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=not-found\nActiveState=inactive\n")
    launcher = make_launcher(tmp_path, runner)
    assert launcher.status("sandbox-0") is UnitState.ABSENT


def test_status_all_covers_every_sandbox_in_the_cluster(tmp_path):
    runner = FakeRunner()
    runner.expect("systemctl show", stdout="LoadState=not-found\n")
    launcher = make_launcher(tmp_path, runner)
    statuses = launcher.status_all()
    assert set(statuses) == set(Cluster().sandbox_names)
    assert all(s is UnitState.ABSENT for s in statuses.values())


def test_build_launcher_loads_config_from_data_dir(tmp_path):
    (tmp_path / "config").mkdir()
    (tmp_path / "config" / "metadata-server.json").write_text(json.dumps({
        "service_account_email": "narrow@p.iam.gserviceaccount.com",
        "project_id": "p",
    }))
    launcher = build_launcher(tmp_path, Cluster(), FakeRunner())
    assert launcher.config.service_account_email == "narrow@p.iam.gserviceaccount.com"
    assert launcher.data_dir == tmp_path
