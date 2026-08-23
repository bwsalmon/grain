import json
from pathlib import Path

from grain.proxy.server import build_proxy


def test_build_proxy_defaults_to_the_real_github_forward_target(tmp_path: Path):
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "github.com"
    assert proxy.forwarder.use_tls is True


def test_build_proxy_honours_a_mock_git_forward_host_from_automation_json(tmp_path: Path):
    """The git-transport half of the mock-GitHub live-test seam
    (docs/roadmap.md item 8) -- `automation.json`'s `git_forward_host`/
    `github_use_tls` (written by `configure_repo`) should reach the real
    proxy the same way `github_host` reaches `RealTransport` in
    `grain/cli.py`'s `build_orchestrator`.
    """
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    (tmp_path / "config" / "automation.json").write_text(json.dumps({
        "owner": "acme", "repo": "widgets",
        "github_host": "10.100.0.1:8443", "git_forward_host": "10.100.0.1:8443",
        "github_use_tls": False,
    }))
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "10.100.0.1:8443"
    assert proxy.forwarder.use_tls is False


def test_build_proxy_falls_back_to_real_github_when_automation_json_is_absent(tmp_path: Path):
    """The git-proxy service can be enabled before `controller configure`
    has ever run -- provisioning installs it disabled, and nothing
    sequences configure before enable except `bootstrap()`'s own stage
    order. A missing config file must not crash the proxy.
    """
    (tmp_path / "config").mkdir()
    (tmp_path / "secrets" / "github").mkdir(parents=True)
    proxy = build_proxy(tmp_path)
    assert proxy.forwarder.host == "github.com"
