import ipaddress
from pathlib import Path

from grain.adapter.deploy import deploy_tree
from grain.run import FakeRunner


def test_deploy_pipes_a_tar_stream_through_ssh_as_one_bash_command():
    runner = FakeRunner()
    deploy_tree(
        runner, Path("/repo"), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    assert len(runner.calls) == 1
    argv, stdin = runner.calls[0]
    assert argv[:2] == ["bash", "-c"]
    script = argv[2]
    assert "tar -czf -" in script
    assert "-C /repo ." in script
    assert " | ssh " in script
    assert "debian@10.100.0.2" in script
    assert stdin is None  # the tar payload travels through a real shell pipe, not Python stdin


def test_deploy_excludes_git_docs_and_pycache():
    runner = FakeRunner()
    deploy_tree(
        runner, Path("/repo"), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    script = runner.calls[0][0][2]
    assert "--exclude=.git" in script
    assert "--exclude=docs" in script
    assert "--exclude=__pycache__" in script


def test_deploy_extracts_with_sudo_to_the_default_dest():
    runner = FakeRunner()
    deploy_tree(
        runner, Path("/repo"), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    script = runner.calls[0][0][2]
    assert "sudo mkdir -p /opt/grain" in script
    assert "sudo tar -xzf - -C /opt/grain" in script


def test_deploy_makes_dest_readable_to_the_unprivileged_agent_user():
    """Found live: the host's checkout was 0700/0600, tar restored those
    modes onto /opt/grain, and every task unit -- which runs as
    dispatch.py's CONTROLLER_AGENT_USER and does `cd /opt/grain` -- died on
    "Permission denied" before claude -p started. The extract must not
    leave the deployed tree at whatever the source tree happened to be."""
    runner = FakeRunner()
    deploy_tree(
        runner, Path("/repo"), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    script = runner.calls[0][0][2]
    assert "sudo chmod -R u=rwX,go=rX /opt/grain" in script
    # After the extract, not before -- tar would otherwise overwrite it.
    assert script.index("sudo tar -xzf -") < script.index("sudo chmod -R")


def test_deploy_dest_is_overridable():
    runner = FakeRunner()
    deploy_tree(
        runner, Path("/repo"), user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"), dest="/opt/other",
    )
    script = runner.calls[0][0][2]
    assert "/opt/other" in script
    assert "/opt/grain" not in script


def test_deploy_never_touches_the_source_tree(tmp_path):
    """No stdin content, no writes to source_root -- this is read-only on
    the local side (tar reads, it does not modify)."""
    runner = FakeRunner()
    marker = tmp_path / "marker"
    marker.write_text("untouched")
    deploy_tree(
        runner, tmp_path, user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    assert marker.read_text() == "untouched"
