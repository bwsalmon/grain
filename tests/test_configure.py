import ipaddress
import json
import shlex
from pathlib import Path

from grain.automation.configure import (
    configure_claude_token, configure_cluster, configure_gcp_service_account,
    configure_gemini_key, configure_github_credential, configure_named_github_key,
    configure_repo, ensure_sandbox_tokens,
)
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.run import FakeRunner

SSH_PREFIX = (
    "ssh -i /var/lib/grain/admin-ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new "
    "-o UserKnownHostsFile=/dev/null -o IdentityAgent=none -o ConnectTimeout=10 debian@10.100.0.2"
)


def make_ssh():
    inner = FakeRunner()
    runner = SshRunner(
        inner=inner, user="debian",
        address=ipaddress.IPv4Address("10.100.0.2"),
        key_path=Path("/var/lib/grain/admin-ssh"),
    )
    return runner, inner


def expect_remote(inner: FakeRunner, remote_command: str, **kwargs) -> None:
    inner.expect(f"{SSH_PREFIX} -- {shlex.quote(remote_command)}", **kwargs)


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
    configure_repo(ssh, "acme/widgets", ["acme/widgets"])
    automation = json.loads(stdin_for(inner, "/data/config/automation.json"))
    assert automation == {
        "task_owner": "acme", "task_repo": "widgets",
        "default_target_repo": None,
        "github_host": "api.github.com", "git_forward_host": "github.com",
        "github_use_tls": True,
    }
    allowlist = json.loads(stdin_for(inner, "/data/config/repo-allowlist.json"))
    assert allowlist == ["acme/widgets"]


def test_configure_repo_honours_a_github_host_override():
    """A live test pointed at a mock GitHub server (docs/roadmap.md item 8)
    -- unset in every real deployment, so this must be an explicit opt-in
    that lands in automation.json, not a default that could silently point
    production at the wrong host.
    """
    ssh, inner = make_ssh()
    configure_repo(
        ssh, "acme/widgets", ["acme/widgets"],
        github_host="10.100.0.1:8443", git_forward_host="10.100.0.1:8443",
        github_use_tls=False,
    )
    automation = json.loads(stdin_for(inner, "/data/config/automation.json"))
    assert automation["github_host"] == "10.100.0.1:8443"
    assert automation["git_forward_host"] == "10.100.0.1:8443"
    assert automation["github_use_tls"] is False


def test_configure_repo_uses_sudo_and_chmod_644_for_config():
    ssh, inner = make_ssh()
    configure_repo(ssh, "acme/widgets", ["acme/widgets"])
    assert any(
        c.startswith("ssh") and "sudo chmod 644 /data/config/automation.json" in c
        for c in inner.commands
    )


def test_configure_cluster_writes_sandbox_count_and_subnet():
    ssh, inner = make_ssh()
    configure_cluster(ssh, Cluster(sandbox_count=4))
    written = stdin_for(inner, "/data/config/cluster.toml")
    assert written == 'sandbox_count = 4\nsubnet = "10.100.0.0/24"\n'


def test_configure_cluster_honours_a_non_default_subnet():
    ssh, inner = make_ssh()
    configure_cluster(ssh, Cluster(sandbox_count=2, subnet=ipaddress.IPv4Network("10.200.0.0/24")))
    written = stdin_for(inner, "/data/config/cluster.toml")
    assert written == 'sandbox_count = 2\nsubnet = "10.200.0.0/24"\n'


def test_configure_cluster_uses_sudo_and_chmod_644():
    ssh, inner = make_ssh()
    configure_cluster(ssh, Cluster())
    assert any(
        c.startswith("ssh") and "sudo chmod 644 /data/config/cluster.toml" in c
        for c in inner.commands
    )


def test_configure_cluster_round_trips_through_cluster_load(tmp_path):
    """The whole point: a controller pointed at this file with
    `--cluster-file` has to derive the same `sandbox_names` the host's own
    `Cluster` does.
    """
    ssh, inner = make_ssh()
    original = Cluster(sandbox_count=4)
    configure_cluster(ssh, original)
    written = stdin_for(inner, "/data/config/cluster.toml")
    cluster_file = tmp_path / "cluster.toml"
    cluster_file.write_text(written)
    reloaded = Cluster.load(cluster_file)
    assert reloaded.sandbox_names == original.sandbox_names


def test_configure_github_credential_writes_token_and_credentials_json():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, ["acme/widgets"], "  ghp_secrettoken  \n")
    token = stdin_for(inner, "/data/secrets/github/bot.token")
    assert token == "ghp_secrettoken\n"  # stripped, then a single trailing newline
    mapping = json.loads(stdin_for(inner, "/data/secrets/github/credentials.json"))
    assert mapping == {"acme/widgets": "bot"}


def test_configure_github_credential_honours_a_custom_credential_name():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, ["acme/widgets"], "tok", credential_name="personal")
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
    configure_github_credential(ssh, ["acme/widgets"], secret)
    for argv, _ in inner.calls:
        assert all(secret not in arg for arg in argv)


def test_configure_github_credential_sets_mode_600_on_the_token():
    ssh, inner = make_ssh()
    configure_github_credential(ssh, ["acme/widgets"], "tok")
    assert any(
        "sudo chmod 600 /data/secrets/github/bot.token" in c for c in inner.commands
    )


def test_configure_named_github_key_writes_only_the_token_file():
    """Unlike configure_github_credential, this must never touch
    credentials.json (bwsalmon/agents#52) -- a grain-github-<name> label
    selects it directly, and writing a mapping entry would make it that
    repo's *default* credential too, defeating the whole point of a named
    override carrying a scope the default deliberately withholds.
    """
    ssh, inner = make_ssh()
    configure_named_github_key(ssh, "  ghp_workflowtoken  \n", name="workflow")
    token = stdin_for(inner, "/data/secrets/github/workflow.token")
    assert token == "ghp_workflowtoken\n"  # stripped, then a single trailing newline
    assert not any(
        "credentials.json" in c for c in inner.commands
    )


def test_configure_named_github_key_sets_mode_600_on_the_token():
    ssh, inner = make_ssh()
    configure_named_github_key(ssh, "tok", name="workflow")
    assert any(
        "sudo chmod 600 /data/secrets/github/workflow.token" in c for c in inner.commands
    )


def test_named_github_key_is_never_in_argv():
    ssh, inner = make_ssh()
    secret = "ghp_supersecretworkflowvalue"
    configure_named_github_key(ssh, secret, name="workflow")
    for argv, _ in inner.calls:
        assert all(secret not in arg for arg in argv)


def test_ensure_sandbox_tokens_mints_one_per_sandbox_when_the_file_is_absent():
    """The live bug this closes: the proxy loads sandbox-tokens.json once at
    startup (grain/proxy/server.py's build_proxy), so a token minted only
    lazily on first dispatch would make that dispatch fail auth against a
    proxy that already started with none. Bootstrap must mint every
    sandbox's token before stage 10 enables the proxy.
    """
    ssh, inner = make_ssh()
    expect_remote(inner, "sudo cat /data/secrets/sandbox-tokens.json", returncode=1)
    ensure_sandbox_tokens(ssh, ["sandbox-0", "sandbox-1"])
    tokens = json.loads(stdin_for(inner, "/data/secrets/sandbox-tokens.json"))
    assert set(tokens) == {"sandbox-0", "sandbox-1"}
    assert tokens["sandbox-0"] != tokens["sandbox-1"]
    assert any(
        "sudo chmod 644 /data/secrets/sandbox-tokens.json" in c for c in inner.commands
    )


def test_ensure_sandbox_tokens_preserves_an_existing_token():
    """A bootstrap re-run (or a sandbox already dispatched to) must not
    replace a token in use -- that would strand the credential the
    sandbox's own git credential helper still presents.
    """
    ssh, inner = make_ssh()
    expect_remote(
        inner, "sudo cat /data/secrets/sandbox-tokens.json",
        stdout=json.dumps({"sandbox-0": "existing-token"}),
    )
    ensure_sandbox_tokens(ssh, ["sandbox-0", "sandbox-1"])
    tokens = json.loads(stdin_for(inner, "/data/secrets/sandbox-tokens.json"))
    assert tokens["sandbox-0"] == "existing-token"
    assert "sandbox-1" in tokens


def test_ensure_sandbox_tokens_is_a_no_op_when_every_sandbox_already_has_one():
    ssh, inner = make_ssh()
    expect_remote(
        inner, "sudo cat /data/secrets/sandbox-tokens.json",
        stdout=json.dumps({"sandbox-0": "existing-token"}),
    )
    ensure_sandbox_tokens(ssh, ["sandbox-0"])
    assert not any("dd of=/data/secrets/sandbox-tokens.json" in c for c in inner.commands)


def test_configure_claude_token_writes_the_reference_copy_mode_600():
    ssh, inner = make_ssh()
    configure_claude_token(ssh, "sk-ant-oat01-fake\n")
    content = stdin_for(inner, "/data/secrets/claude-oauth-token")
    assert content == "sk-ant-oat01-fake"  # stripped
    assert any(
        "sudo chmod 600 /data/secrets/claude-oauth-token" in c for c in inner.commands
    )


def test_configure_claude_token_writes_the_live_copy_owned_by_grain_agent():
    ssh, inner = make_ssh()
    configure_claude_token(ssh, "sk-ant-oat01-fake")
    content = stdin_for(inner, "/home/grain-agent/.claude-oauth-token")
    assert content == "sk-ant-oat01-fake"
    assert any(
        "sudo chmod 600 /home/grain-agent/.claude-oauth-token" in c for c in inner.commands
    )
    assert any(
        "sudo chown grain-agent:grain-agent /home/grain-agent/.claude-oauth-token" in c
        for c in inner.commands
    )


def test_configure_claude_token_is_never_in_argv():
    ssh, inner = make_ssh()
    secret = "sk-ant-oat01-supersecretvalue"
    configure_claude_token(ssh, secret)
    for argv, _ in inner.calls:
        assert all(secret not in arg for arg in argv)


def test_configure_gcp_service_account_writes_the_key_mode_640():
    ssh, inner = make_ssh()
    configure_gcp_service_account(
        ssh, '{"type": "service_account"}\n',
        service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        project_id="acme",
    )
    content = stdin_for(inner, "/data/secrets/gcp-service-account.json")
    assert content == '{"type": "service_account"}\n'  # stripped, single trailing newline
    assert any(
        "sudo chmod 640 /data/secrets/gcp-service-account.json" in c for c in inner.commands
    )


def test_configure_gcp_service_account_chowns_only_the_file_not_the_shared_secrets_dir():
    """Found live while writing this: _write_remote_file's owner= kwarg
    also chowns the file's *parent*, which is correct for
    CONTROLLER_AGENT_TOKEN_PATH (a private grain-agent home directory) but
    would be wrong here -- /data/secrets is shared with the GitHub and
    Claude credentials, which must stay root-owned.
    """
    ssh, inner = make_ssh()
    configure_gcp_service_account(
        ssh, "{}", service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        project_id="acme",
    )
    chown_commands = [argv[-1] for argv, _ in inner.calls if "chown" in argv[-1]]
    assert chown_commands == [
        "sudo chown grain-metadata:grain-metadata /data/secrets/gcp-service-account.json"
    ]  # exactly one chown, on the file -- never on the shared /data/secrets directory


def test_configure_gcp_service_account_honours_a_custom_metadata_user():
    ssh, inner = make_ssh()
    configure_gcp_service_account(
        ssh, "{}", service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        project_id="acme", metadata_user="custom-metadata",
    )
    assert any(
        "sudo chown custom-metadata:custom-metadata /data/secrets/gcp-service-account.json" in c
        for c in inner.commands
    )


def test_configure_gcp_service_account_writes_the_metadata_config():
    ssh, inner = make_ssh()
    configure_gcp_service_account(
        ssh, "{}", service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        project_id="acme", numeric_project_id=123456,
    )
    config = json.loads(stdin_for(inner, "/data/config/metadata-server.json"))
    assert config == {
        "service_account_email": "grain-agent@acme.iam.gserviceaccount.com",
        "project_id": "acme",
        "numeric_project_id": 123456,
        "metadata_user": "grain-metadata",
    }
    assert any(
        "sudo chmod 644 /data/config/metadata-server.json" in c for c in inner.commands
    )


def test_configure_gcp_service_account_key_is_never_in_argv():
    ssh, inner = make_ssh()
    secret = '{"private_key": "supersecretvalue"}'
    configure_gcp_service_account(
        ssh, secret, service_account_email="grain-agent@acme.iam.gserviceaccount.com",
        project_id="acme",
    )
    for argv, _ in inner.calls:
        assert all(secret not in arg for arg in argv)


def test_configure_gemini_key_writes_the_project_id():
    ssh, inner = make_ssh()
    configure_gemini_key(ssh, "acme")
    config = json.loads(stdin_for(inner, "/data/config/gemini-key.json"))
    assert config == {"project_id": "acme"}
    assert any(
        "sudo chmod 644 /data/config/gemini-key.json" in c for c in inner.commands
    )
