"""Live SSH/dispatch verification against a real, disposable GCE VM.

docs/roadmap.md item 2/8's own live suites (`tests/test_vm_integration.py`,
`tests/test_live_issue_to_pr.py`) already prove `grain.automation.dispatch`'s
SSH-driven mechanics -- `configure_git_credentials`, `ensure_workspace` --
against a real machine, but only ever run where the *grain host itself* is
already set up: `/dev/kvm`, `qemu:///system`, and `br-grain` already up
(their own `_host_ready()` check). Wherever that isn't provisioned -- which
includes an ordinary agent sandbox like the one this suite was first
written from (confirmed live: `/dev/kvm` present, but no `qemu-system-x86_64`
or `br-grain`) -- those suites are silently and permanently skipped, so
nothing in this repo's own dispatch/credential/workspace path ever actually
runs anywhere else.

This suite is that "everywhere else" version: its one real machine is a
disposable Google Compute Engine instance, created and destroyed by the
test itself with whatever GCP credentials the running agent/CI already has
(`gcloud`, already authenticated) -- the same "stand up the one real thing
this environment is missing" precedent `test_live_issue_to_pr.py`'s own
module docstring sets, just for a different missing piece (a KVM host
instead of a GitHub credential).

**Why a fresh VM, never this machine's own loopback.** `dispatch.py`'s
`_CREDENTIALS_PATH` (`/home/debian/.git-credentials`) is a fixed absolute
path, written unconditionally regardless of which remote
`configure_git_credentials` is pointed at -- correct for its real use (a
sandbox's own path, always that same sandbox), but a real hazard for a test
that might aim it at the machine running the test suite itself: on a host
whose own git push credential happens to live at that exact path (an agent
sandbox provisioned by this very project is exactly such a host), pointing
this code at loopback silently overwrites that real credential with a
throwaway test value, with no way to recover the original from inside the
sandbox afterwards. Found the hard way, not hypothetically -- see
bwsalmon/agents#153's issue thread. Every SSH target in this suite is
instead a brand-new GCE instance this test itself created a few lines
above and tears down at the end of the run; its `/home/debian` is a
different machine's filesystem entirely, so there is no path by which this
suite can ever touch the sandbox's own credentials, no matter what it
writes.

**Opt-in, like every other live suite that costs real money or real
infrastructure** (`test_gcp_live.py`'s own docstring makes the identical
argument for API-key minting): skipped unless `GRAIN_LIVE_GCP_PROJECT` is
set. `GRAIN_LIVE_GCP_ZONE`/`GRAIN_LIVE_GCP_NETWORK` default to
`us-central1-a`/`default`. The network needs a firewall rule allowing
inbound `tcp:22` from the internet (the `default` network's own
`default-allow-ssh` already does) since this test SSHes to the instance's
external IP directly -- the simplest thing that works for a VM this test
both creates and owns outright, with no shared inventory or proxy to
collide with a concurrent run of anything else in this file (each instance
gets its own random-suffixed name).

    GRAIN_LIVE_GCP_PROJECT=my-project python -m pytest tests/test_dispatch_live_gce.py

**What this proves, and what it doesn't.** Real: SSH command execution
against a real machine (`grain.automation.ssh.SshRunner`, unmodified),
`configure_git_credentials` writing a credential-store line a real `git`
actually authenticates with against a real HTTP Basic-Auth challenge (and
is genuinely rejected by when it's wrong), and `ensure_workspace`'s full
clone / fetch-reset-clean / stale-file-discard behaviour against real git
history on a real remote filesystem. Not exercised here: the real
`grain.proxy.core.GitProxy` token layer (already covered live by
`test_vm_integration.py`) or anything involving `claude`/GitHub -- this
suite's HTTP server checks a fixed bearer token directly rather than
re-implementing the proxy, so as not to duplicate that coverage.
"""

from __future__ import annotations

import ipaddress
import os
import secrets
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

from grain.automation.dispatch import (
    WORKSPACE_PATH, _CREDENTIALS_PATH, _GIT_IDENTITY_EMAIL, _GIT_IDENTITY_NAME,
    configure_git_credentials, ensure_workspace,
)
from grain.automation.ssh import SshRunner
from grain.run import CommandError, RealRunner

PROJECT = os.environ.get("GRAIN_LIVE_GCP_PROJECT")
ZONE = os.environ.get("GRAIN_LIVE_GCP_ZONE", "us-central1-a")
NETWORK = os.environ.get("GRAIN_LIVE_GCP_NETWORK", "default")
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180
SERVER_PORT = 8123

pytestmark = pytest.mark.skipif(
    not PROJECT, reason="needs GRAIN_LIVE_GCP_PROJECT pointed at a real GCP project",
)


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    kwargs.setdefault("timeout", 60)
    return subprocess.run(argv, capture_output=True, text=True, **kwargs)


def _run_ok(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    result = _run(argv, **kwargs)
    assert result.returncode == 0, f"{argv} failed: {result.stderr}"
    return result


def _wait_for_ssh(address: ipaddress.IPv4Address, key_path: Path,
                   timeout: float = BOOT_TIMEOUT_S) -> None:
    deadline = time.monotonic() + timeout
    last_error = ""
    while time.monotonic() < deadline:
        result = _run([
            "ssh", "-i", str(key_path), "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5",
            f"{SSH_USER}@{address}", "--", "true",
        ], timeout=10)
        if result.returncode == 0:
            return
        last_error = result.stderr
        time.sleep(3)
    raise TimeoutError(f"SSH never came up on {address}: {last_error}")


@dataclass
class LiveVm:
    name: str
    address: ipaddress.IPv4Address
    key_path: Path


@pytest.fixture(scope="module")
def live_vm(tmp_path_factory):
    """Boots one disposable GCE instance and waits for real SSH to come up
    -- the slow part every test in this module shares, same "one VM,
    module-scoped" precedent `test_vm_integration.py`'s own fixture sets.
    `--no-service-account --no-scopes`: this VM only needs to answer SSH
    commands, and attaching the project's default compute service account
    would need an `iam.serviceAccountUser` grant this task's own narrow
    agent-account key does not have (found live: instance creation fails
    outright without this flag, with exactly that permission error).
    """
    workdir = tmp_path_factory.mktemp("live-vm")
    key_path = workdir / "id_ed25519"
    _run_ok(["ssh-keygen", "-t", "ed25519", "-N", "", "-f", str(key_path), "-C", SSH_USER])
    ssh_keys_file = workdir / "ssh-keys.txt"
    ssh_keys_file.write_text(f"{SSH_USER}:{key_path.with_suffix('.pub').read_text()}")

    name = f"grain-live-dispatch-test-{secrets.token_hex(4)}"
    _run_ok([
        "gcloud", "compute", "instances", "create", name,
        f"--project={PROJECT}", f"--zone={ZONE}",
        "--machine-type=e2-small",
        "--image-family=debian-12", "--image-project=debian-cloud",
        f"--network={NETWORK}",
        "--no-service-account", "--no-scopes",
        f"--metadata-from-file=ssh-keys={ssh_keys_file}",
    ], timeout=120)
    try:
        address = ipaddress.IPv4Address(_run_ok([
            "gcloud", "compute", "instances", "describe", name,
            f"--project={PROJECT}", f"--zone={ZONE}",
            "--format=value(networkInterfaces[0].accessConfigs[0].natIP)",
        ]).stdout.strip())
        _wait_for_ssh(address, key_path)
        yield LiveVm(name=name, address=address, key_path=key_path)
    finally:
        _run(["gcloud", "compute", "instances", "delete", name,
              f"--project={PROJECT}", f"--zone={ZONE}", "--quiet"], timeout=120)


@pytest.fixture(scope="module")
def live_ssh_runner(live_vm: LiveVm) -> SshRunner:
    return SshRunner(inner=RealRunner(), user=SSH_USER,
                      address=live_vm.address, key_path=live_vm.key_path)


@pytest.fixture(scope="module")
def git_installed(live_ssh_runner: SshRunner) -> None:
    """Debian's own cloud image ships with no `git` at all -- the identical
    finding `test_vm_integration.py`'s own `git_installed` fixture already
    recorded for the KVM-based image."""
    live_ssh_runner.run(["sudo", "apt-get", "update", "-qq"])
    live_ssh_runner.run(["sudo", "apt-get", "install", "-y", "-qq", "git"])


# A minimal git-smart-HTTP server, gated by a fixed bearer token, run on the
# live VM itself via `systemd-run` -- the same `git http-backend` CGI
# contract `test_vm_integration.py`'s `_GitBackendHandler` uses, with one
# addition (the token check) standing in for the real `GitProxy`'s
# enforcement, which is already covered live by that other suite.
_AUTH_SERVER_SCRIPT = """
import base64
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
PROJECT_ROOT = sys.argv[2]
TOKEN = sys.argv[3]
_EXPECTED = "Basic " + base64.b64encode(f"sandbox:{TOKEN}".encode()).decode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _authed(self) -> bool:
        if self.headers.get("Authorization") == _EXPECTED:
            return True
        body = b"auth required"
        self.send_response(401)
        self.send_header("WWW-Authenticate", 'Basic realm="git"')
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        return False

    def _cgi(self, method: str) -> None:
        if not self._authed():
            return
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        path, _, query = self.path.partition("?")
        env = {
            "GIT_PROJECT_ROOT": PROJECT_ROOT,
            "GIT_HTTP_EXPORT_ALL": "1",
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "REQUEST_METHOD": method,
            "CONTENT_TYPE": self.headers.get("Content-Type", ""),
            "GATEWAY_INTERFACE": "CGI/1.1",
            "SERVER_PROTOCOL": "HTTP/1.1",
        }
        proc = subprocess.run(
            ["git", "http-backend"], input=body, env=env, capture_output=True
        )
        sep = b"\\r\\n\\r\\n"
        head_end = proc.stdout.find(sep)
        if head_end == -1:
            sep = b"\\n\\n"
            head_end = proc.stdout.find(sep)
        header_block = proc.stdout[:head_end].decode("latin1")
        resp_body = proc.stdout[head_end + len(sep):]
        status = 200
        headers = []
        for line in header_block.splitlines():
            key, _, value = line.partition(":")
            value = value.strip()
            if key.lower() == "status":
                status = int(value.split()[0])
            elif key:
                headers.append((key, value))
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(resp_body)))
        self.end_headers()
        self.wfile.write(resp_body)

    def do_GET(self):
        self._cgi("GET")

    def do_POST(self):
        self._cgi("POST")

    def log_message(self, *a):
        pass


HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
"""

_REPO_ROOT = "/tmp/grain-live-repo"
_BARE_REPO = f"{_REPO_ROOT}/o/r.git"
_SEED_CLONE = "/tmp/grain-live-seed"


@dataclass
class GitTarget:
    remote_url: str
    token: str


@pytest.fixture(scope="module")
def git_target(live_ssh_runner: SshRunner, git_installed: None) -> GitTarget:
    """A real bare repo with one seed commit, served over real smart-HTTP,
    entirely on the one VM this module already booted -- no second network
    hop or shared proxy needed to prove `configure_git_credentials`/
    `ensure_workspace` really authenticate and really clone/fetch/reset,
    not just format a string correctly.
    """
    token = secrets.token_hex(16)
    setup_script = "\n".join([
        "set -eu",
        f"rm -rf {_REPO_ROOT} {_SEED_CLONE}",
        f"mkdir -p $(dirname {_BARE_REPO})",
        f"git init --bare -q {_BARE_REPO}",
        f"git -C {_BARE_REPO} config http.receivepack true",
        f"git clone -q {_BARE_REPO} {_SEED_CLONE}",
        f"git -C {_SEED_CLONE} config user.email seed@example.com",
        f"git -C {_SEED_CLONE} config user.name seed",
        f"echo hello > {_SEED_CLONE}/README.md",
        f"git -C {_SEED_CLONE} add README.md",
        f"git -C {_SEED_CLONE} commit -q -m seed",
        f"git -C {_SEED_CLONE} branch -M main",
        f"git -C {_SEED_CLONE} push -q origin main",
        f"git -C {_BARE_REPO} symbolic-ref HEAD refs/heads/main",
    ])
    live_ssh_runner.run(["bash", "-c", setup_script])
    live_ssh_runner.run(
        ["dd", "of=/tmp/grain_live_git_auth_server.py", "status=none"],
        stdin=_AUTH_SERVER_SCRIPT,
    )
    live_ssh_runner.run([
        "sudo", "systemd-run", "--unit=grain-live-test-gitserver",
        "--property=User=debian", "python3", "/tmp/grain_live_git_auth_server.py",
        str(SERVER_PORT), _REPO_ROOT, token,
    ])
    try:
        yield GitTarget(remote_url=f"http://127.0.0.1:{SERVER_PORT}/o/r.git", token=token)
    finally:
        live_ssh_runner.run(
            ["sudo", "systemctl", "stop", "grain-live-test-gitserver"], check=False,
        )


def test_configure_git_credentials_and_ensure_workspace_over_real_ssh(
    live_ssh_runner: SshRunner, git_target: GitTarget,
):
    """Runs the real `configure_git_credentials`/`ensure_workspace`
    (`grain/automation/dispatch.py`) against the real VM/git-server rig
    above, in the sequence a real dispatch actually uses them in: configure
    credentials once, clone, then (as a reused long-lived sandbox would see
    on its next dispatch) reconfigure and fetch-and-reset again. Kept as
    one test sharing one VM/repo rather than several, the same "boot is the
    expensive part" reasoning `test_live_issue_to_pr.py`'s own combined
    test gives for its three scenarios.
    """
    # --- configure_git_credentials writes a real, working credential ------
    configure_git_credentials(live_ssh_runner, git_target.remote_url, git_target.token)

    creds = live_ssh_runner.run(["cat", _CREDENTIALS_PATH]).stdout
    assert creds == f"http://sandbox:{git_target.token}@127.0.0.1:{SERVER_PORT}\n"
    perms = live_ssh_runner.run(["stat", "-c", "%a", _CREDENTIALS_PATH]).stdout.strip()
    assert perms == "600", "credential file should not be group/world readable"
    name = live_ssh_runner.run(["git", "config", "--global", "user.name"]).stdout.strip()
    assert name == _GIT_IDENTITY_NAME
    email = live_ssh_runner.run(["git", "config", "--global", "user.email"]).stdout.strip()
    assert email == _GIT_IDENTITY_EMAIL

    # --- ensure_workspace's first-dispatch path: a real clone, authenticated
    #     purely through the credential helper just configured (the clone
    #     URL itself carries no token) -------------------------------------
    ensure_workspace(live_ssh_runner, git_target.remote_url, path=WORKSPACE_PATH)

    readme = live_ssh_runner.run(["cat", f"{WORKSPACE_PATH}/README.md"]).stdout
    assert readme == "hello\n"
    log = live_ssh_runner.run(
        ["git", "-C", WORKSPACE_PATH, "log", "-1", "--format=%s"]
    ).stdout.strip()
    assert log == "seed"

    # --- a wrong token is genuinely rejected by the real server, not just
    #     accepted because the mechanism never actually checks -------------
    configure_git_credentials(live_ssh_runner, git_target.remote_url, "wrong-token")
    with pytest.raises(CommandError):
        ensure_workspace(live_ssh_runner, git_target.remote_url,
                          path="/home/debian/workspace-wrong-token")
    configure_git_credentials(live_ssh_runner, git_target.remote_url, git_target.token)

    # --- ensure_workspace's reused-sandbox path: fetches a real new
    #     upstream commit and discards a real leftover untracked file,
    #     rather than reusing whatever a previous task left behind --------
    live_ssh_runner.run(["bash", "-c", (
        f"cd {_SEED_CLONE} && echo more >> README.md && "
        "git add README.md && git commit -q -m second && git push -q origin main"
    )])
    live_ssh_runner.run(["bash", "-c", f"echo leftover > {WORKSPACE_PATH}/leftover.txt"])

    ensure_workspace(live_ssh_runner, git_target.remote_url, path=WORKSPACE_PATH)

    log2 = live_ssh_runner.run(
        ["git", "-C", WORKSPACE_PATH, "log", "-1", "--format=%s"]
    ).stdout.strip()
    assert log2 == "second", "a reused workspace should fetch and reset to the new upstream tip"
    leftover_gone = live_ssh_runner.run(
        ["test", "-e", f"{WORKSPACE_PATH}/leftover.txt"], check=False,
    ).returncode != 0
    assert leftover_gone, "ensure_workspace should discard untracked leftovers on reuse"
