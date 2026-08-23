import ipaddress
import json
from pathlib import Path

from grain.automation.configure import (
    configure_claude_credentials, configure_github_credential, configure_repo,
)
from grain.automation.ssh import SshRunner
from grain.run import FakeRunner


def make_ssh():
    inner = FakeRunner()
    runner = SshRunner(
        inner=inner, user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    return runner, inner


def stdin_for(inner: FakeRunner, path: str) -> str:
    # SshRunner wraps the real argv into one shell-quoted trailing string
    # (`ssh ... -- 'sudo dd of=<path> status=none'`) -- match on that,
    # not a plain argv prefix, since that's what actually reaches `inner`.
    for argv, stdin in inner.calls:
        if argv[0] == "ssh" and argv[-1] == f"sudo dd of={path} status=none":
            return stdin
    raise AssertionError(f"no dd write to {path} among {inner.commands}")


def test_configure_repo_writes_automation_json_and_allowlist():
    ssh, inner = make_ssh()
    configure_repo(ssh, "acme", "widgets")
    automation = json.loads(stdin_for(inner, "/data/config/automation.json"))
    assert automation == {"owner": "acme", "repo": "widgets"}
    allowlist = json.loads(stdin_for(inner, "/data/config/repo-allowlist.json"))
    assert allowlist == ["acme/widgets"]


def test_configure_repo_uses_sudo_and_chmod_644_for_config():
    ssh, inner = make_ssh()
    configure_repo(ssh, "acme", "widgets")
    assert any(
        c.startswith("ssh") and "sudo chmod 644 /data/config/automation.json" in c
        for c in inner.commands
    )


def test_configure_github_credential_writes_token_and_credentials_json():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, "acme", "widgets", "  ghp_secrettoken  \n")
    token = stdin_for(inner, "/data/secrets/github/bot.token")
    assert token == "ghp_secrettoken\n"  # stripped, then a single trailing newline
    mapping = json.loads(stdin_for(inner, "/data/secrets/github/credentials.json"))
    assert mapping == {"acme/widgets": "bot"}


def test_configure_github_credential_honours_a_custom_credential_name():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, "acme", "widgets", "tok", credential_name="personal")
    stdin_for(inner, "/data/secrets/github/personal.token")  # does not raise
    mapping = json.loads(stdin_for(inner, "/data/secrets/github/credentials.json"))
    assert mapping == {"acme/widgets": "personal"}


def test_github_token_file_is_never_in_argv():
    """Mirrors test_automation_dispatch.py's stdin-not-argv assertion for
    the git-proxy token -- the same property must hold for the GitHub
    token, which is at least as sensitive.
    """
    ssh, inner = make_ssh()
    secret = "ghp_supersecretvalue"
    configure_github_credential(ssh, "acme", "widgets", secret)
    for argv, _ in inner.calls:
        assert all(secret not in arg for arg in argv)


def test_configure_github_credential_sets_mode_600_on_the_token():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, "acme", "widgets", "tok")
    assert any(
        "sudo chmod 600 /data/secrets/github/bot.token" in c for c in inner.commands
    )


def test_configure_claude_credentials_writes_to_the_fixed_path_mode_600():
    ssh, inner = make_ssh()
    configure_claude_credentials(ssh, '{"accessToken": "x"}')
    content = stdin_for(inner, "/data/secrets/claude-credentials.json")
    assert content == '{"accessToken": "x"}'
    assert any(
        "sudo chmod 600 /data/secrets/claude-credentials.json" in c for c in inner.commands
    )
