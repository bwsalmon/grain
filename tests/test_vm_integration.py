"""Live integration tests: boot a real sandbox VM via libvirt and exercise
SSH plus the automation dispatch mechanism against it.

Skipped unless the host is already set up for this — KVM, `qemu:///system`
reachable, and `br-grain` already up. Bringing the host network up is
`grain host up`'s job, not a test fixture's — this suite only checks the
precondition holds, the same split `test_net_integration.py` makes for
nftables. Where it *can* run, this is the one place that verifies what the
rest of the suite fakes: that a real cloud-init seed actually lands the
controller's SSH key on the guest (`libvirt.py`'s `public-keys` support),
and that SSH from the controller genuinely reaches a sandbox — net_linux.py
says "controller may reach any sandbox"; this is what turns that rule into
a fact rather than something that merely parses.

One VM, session-scoped: boot is the slow part (cloud-init + sshd coming up,
tens of seconds), so every test in this file shares one sandbox rather than
paying that cost per test.

`claude -p` itself is not exercised: it needs a real login credential this
environment doesn't have, and docs/design.md is explicit that logging in is
"a manual first-setup step, not something [the provisioning script] can do
unattended." `dispatch()`'s systemd-run/unit_status/reap mechanism is
verified with `start_unit` and stand-in commands instead — see
`grain/automation/dispatch.py`'s `start_unit`.

**The workspace-clone tests below (docs/roadmap.md item 2) need something to
clone from.** There is no real GitHub credential in this environment, so
rather than skip that verification, `git_proxy_target` stands up the real
things on our own side of the split surface: a real bare git repo served
over real smart-HTTP by `git http-backend` (standing in for GitHub) behind
the actual `grain.proxy.core.GitProxy` (standing in for the real proxy
deployment, with only its `Forwarder` swapped from `RealForwarder`'s
hardcoded HTTPS for a plain-HTTP equivalent — the one substitution needed to
avoid provisioning a TLS cert for a throwaway test server). The sandbox
reaches this exactly the way it would reach a real controller: over the
network, through `grain/automation/dispatch.py`'s `configure_git_credentials`
and `ensure_workspace`, with the proxy's own token check
(`grain/proxy/tokens.py`) genuinely enforced. What this does *not* exercise
is a real GitHub repo or credential — that needs docs/roadmap.md item 8.
"""

from __future__ import annotations

import base64
import http.client
import json
import subprocess
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from grain.adapter.base import VmState
from grain.adapter.libvirt import LIBVIRT_URI, LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.dispatch import (
    WORKSPACE_PATH, UnitState, configure_git_credentials, dispatch,
    ensure_workspace, reap, start_unit, unit_status,
)
from grain.automation.github import Issue
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.proxy.allowlist import Allowlist
from grain.proxy.core import GitProxy
from grain.proxy.credentials import CredentialSet
from grain.proxy.forward import UpstreamResponse
from grain.proxy.server import make_handler
from grain.proxy.tokens import SandboxTokens
from grain.run import RealRunner

BRIDGE = "br-grain"
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
BASE_IMAGE_URL = (
    "https://cloud.debian.org/images/cloud/bookworm/latest/"
    "debian-12-genericcloud-amd64.qcow2"
)
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    # A default, overridable timeout: an ssh call whose connection hangs
    # (e.g. a stale SSH_AUTH_SOCK — see grain/automation/ssh.py's
    # IdentityAgent=none docstring, reproduced live in this very
    # environment) must not wedge the whole suite indefinitely. Reported as
    # an ordinary failed CompletedProcess, not an exception, so retry loops
    # like `_wait_for_ssh` keep working without a special case.
    kwargs.setdefault("timeout", 20)
    try:
        return subprocess.run(argv, capture_output=True, text=True, **kwargs)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(
            argv, returncode=124, stdout=exc.stdout or "",
            stderr=(exc.stderr or "") + f"\n[timed out after {kwargs['timeout']}s]",
        )


def _host_ready() -> bool:
    """Cheap, local, no network — safe to call at collection time on every
    `pytest` run. The base image (a one-time ~350MB fetch) is checked and,
    if needed, fetched lazily inside the `base_image` fixture instead, so
    an ordinary test run on a host without KVM never touches the network.
    """
    if not Path("/dev/kvm").exists():
        return False
    if _run(["virsh", "-c", LIBVIRT_URI, "list"]).returncode != 0:
        return False
    if _run(["ip", "link", "show", BRIDGE]).returncode != 0:
        return False
    for tool in ("qemu-img", "cloud-localds", "ssh", "ssh-keygen"):
        if _run(["which", tool]).returncode != 0:
            return False
    return True


pytestmark = pytest.mark.skipif(
    not _host_ready(),
    reason="needs /dev/kvm, qemu:///system, and br-grain already up",
)


@pytest.fixture(scope="session")
def base_image() -> Path:
    if not BASE_IMAGE.exists():
        BASE_IMAGE.parent.mkdir(parents=True, exist_ok=True)
        result = _run(
            ["curl", "-fsSL", "-o", str(BASE_IMAGE), BASE_IMAGE_URL], timeout=600
        )
        if result.returncode != 0:
            BASE_IMAGE.unlink(missing_ok=True)
            pytest.skip(f"could not fetch base image: {result.stderr}")
    return BASE_IMAGE


@pytest.fixture(scope="session")
def controller_key(tmp_path_factory) -> tuple[Path, Path]:
    key_dir = tmp_path_factory.mktemp("controller-ssh")
    private = key_dir / "id_ed25519"
    result = _run(
        ["ssh-keygen", "-t", "ed25519", "-f", str(private), "-N", "", "-q"]
    )
    assert result.returncode == 0, result.stderr
    return private, private.with_suffix(".pub")


def _wait_for_ssh(address, key_path: Path, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    last_err = ""
    while time.monotonic() < deadline:
        result = _run([
            "ssh", "-i", str(key_path), "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
            "-o", "ConnectTimeout=5",
            f"{SSH_USER}@{address}", "true",
        ])
        if result.returncode == 0:
            return
        last_err = result.stderr
        time.sleep(3)
    raise TimeoutError(f"sandbox never became reachable over SSH: {last_err}")


@dataclass
class Sandbox:
    adapter: LibvirtAdapter
    cluster: Cluster
    name: str
    private_key: Path


@pytest.fixture(scope="session")
def booted_sandbox(base_image, controller_key):
    private_key, public_key = controller_key
    # Not a pytest tmp_path: those land under /tmp mode 0700, which blocks
    # the qemu process (uid libvirt-qemu, verified via `id libvirt-qemu`)
    # from opening the disk even after dynamic_ownership chowns the file
    # itself — verified live, "Cannot access storage file ... Permission
    # denied" — dynamic_ownership relabels the leaf file, not the parent
    # directory chain. `/var/lib/grain/instances` is the real config_dir
    # default and already 0755, so it's traversable the same way a real
    # deployment's would be.
    config_dir = Path("/var/lib/grain/instances/test-live")
    config_dir.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(sandbox_count=1, image=str(base_image), bridge=BRIDGE)
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir,
        ssh_public_key_path=public_key,
    )
    name = "sandbox-0"
    try:
        adapter.create(cluster.spec_of(name))
        adapter.start(name)
        _wait_for_ssh(cluster.address_of(name), private_key, timeout=BOOT_TIMEOUT_S)
        yield Sandbox(adapter=adapter, cluster=cluster, name=name, private_key=private_key)
    finally:
        adapter.destroy(name)


@pytest.fixture
def ssh_runner(booted_sandbox: Sandbox) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=booted_sandbox.cluster.address_of(booted_sandbox.name),
        key_path=booted_sandbox.private_key,
    )


@pytest.fixture(scope="session")
def git_installed(booted_sandbox: Sandbox) -> None:
    """Found live writing this suite: the bare cloud image `booted_sandbox`
    boots — deliberately, to keep boot time down, same tradeoff its own
    docstring already makes for skipping full provisioning — has no `git`
    binary at all. Every test before docs/roadmap.md item 2 only needed
    `ssh`/`systemd`, both present out of the box; `dispatch()`'s new
    workspace-clone step is the first thing in this suite to actually need
    git. `provision/sandbox.sh` installs it in a real deployment
    (`apt-get install ... git ...`); this is the minimal equivalent for a
    suite that intentionally doesn't run the full provisioning script.
    Session-scoped so the ~10s apt cost is paid once, not once per test.
    """
    runner = SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=booted_sandbox.cluster.address_of(booted_sandbox.name),
        key_path=booted_sandbox.private_key,
    )
    runner.run(["sudo", "apt-get", "update", "-qq"])
    runner.run(["sudo", "apt-get", "install", "-y", "-qq", "git"])


# --- a real bare repo, served over real smart-HTTP, behind a real GitProxy
# --- (see the module docstring for why: no live GitHub credential exists
# --- in this environment, so this is what actually clones from) ----------

class _GitBackendHandler(BaseHTTPRequestHandler):
    """Shells out to `git http-backend` per request via the CGI contract
    git itself defines — a small, correct-enough smart-HTTP server standing
    in for GitHub as the proxy's forward target. `project_root` is bound via
    subclassing (`type(..., {"project_root": ...})`) since `http.server`
    instantiates the handler class itself, with no constructor hook.
    """

    protocol_version = "HTTP/1.1"
    project_root: str = ""

    def _cgi(self, method: str) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        path, _, query = self.path.partition("?")
        env = {
            "GIT_PROJECT_ROOT": self.project_root,
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
        sep = b"\r\n\r\n"
        head_end = proc.stdout.find(sep)
        if head_end == -1:
            sep = b"\n\n"
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

    def do_GET(self) -> None:
        self._cgi("GET")

    def do_POST(self) -> None:
        self._cgi("POST")

    def log_message(self, *args) -> None:
        pass


@dataclass
class _PlainHttpForwarder:
    """`grain.proxy.forward.RealForwarder`, with its one hardcoded
    assumption (always HTTPS, since the real upstream is always
    github.com) swapped for plain HTTP — the only change needed to point a
    real `GitProxy` at the local `_GitBackendHandler` server above instead
    of the network, without provisioning a TLS cert for a throwaway test.
    Everything else — header handling, the Basic-auth encoding GitHub's own
    HTTPS PAT convention uses — is identical to the real forwarder.
    """

    host: str

    def forward(self, *, method: str, path: str, query: str,
                headers: dict[str, str], body: bytes | None,
                token: str | None) -> UpstreamResponse:
        out_headers = {
            "Content-Type": headers.get("Content-Type", ""),
            "Accept": headers.get("Accept", "*/*"),
            "User-Agent": headers.get("User-Agent", "git/grain-proxy"),
        }
        if token:
            out_headers["Authorization"] = (
                "Basic " + base64.b64encode(f"{token}:".encode()).decode()
            )
        out_headers = {k: v for k, v in out_headers.items() if v}
        conn = http.client.HTTPConnection(self.host, timeout=30)
        try:
            full_path = f"{path}?{query}" if query else path
            conn.request(method, full_path, body=body, headers=out_headers)
            resp = conn.getresponse()
            data = resp.read()
            return UpstreamResponse(resp.status, dict(resp.getheaders()), data)
        finally:
            conn.close()


# The host's own bridge address, not `Cluster.controller_ip` — there is no
# controller VM in this suite (only one sandbox is booted), and a process
# can only bind an address actually configured on one of its interfaces.
# Reachability still matches production: net_linux.py's forward chain only
# filters traffic *routed* between bridge members (sandbox-to-sandbox,
# sandbox-to-controller-VM); traffic addressed to the bridge's own local
# address is host-local delivery through the (deliberately unmanaged, see
# net_linux.py's own docstring) INPUT chain, not FORWARD — verified against
# this exact host's live ruleset before writing this fixture (`nft list
# ruleset`: an explicit `policy drop` on the `forward` hook, and no `input`
# hook table defined for `inet grain` at all).
_PROXY_HOST = "10.100.0.1"
_PROXY_PORT = 18080
SANDBOX_TOKEN = "live-test-sandbox-token"


@dataclass
class GitProxyTarget:
    remote_url: str
    token: str
    bare_repo: Path
    # The scratch clone the fixture itself seeded the repo from — tests that
    # need to push a *second* commit (to exercise ensure_workspace's
    # fetch-and-reset path) push from here rather than through the sandbox.
    seed_clone: Path


def _run_ok(argv: list[str], **kwargs) -> None:
    result = _run(argv, **kwargs)
    assert result.returncode == 0, f"{argv}: {result.stderr}"


@pytest.fixture
def git_proxy_target(tmp_path: Path):
    """Boots a real bare repo behind a real `GitProxy`, reachable from the
    sandbox at `remote_url` with `token` — see the module docstring and the
    two classes above for what's real and what's substituted.
    """
    repos_root = tmp_path / "repos"
    bare = repos_root / "o" / "r.git"
    bare.mkdir(parents=True)
    _run_ok(["git", "init", "--bare", "-q", str(bare)])

    seed = tmp_path / "seed"
    _run_ok(["git", "clone", "-q", str(bare), str(seed)])
    (seed / "README.md").write_text("hello from the live test upstream\n")
    _run_ok(["git", "-C", str(seed), "config", "user.email", "seed@example.com"])
    _run_ok(["git", "-C", str(seed), "config", "user.name", "seed"])
    _run_ok(["git", "-C", str(seed), "add", "README.md"])
    _run_ok(["git", "-C", str(seed), "commit", "-q", "-m", "seed"])
    _run_ok(["git", "-C", str(seed), "branch", "-M", "main"])
    _run_ok(["git", "-C", str(seed), "push", "-q", "origin", "main"])
    _run_ok(["git", "-C", str(bare), "symbolic-ref", "HEAD", "refs/heads/main"])

    handler_cls = type(
        "BoundGitBackendHandler", (_GitBackendHandler,),
        {"project_root": str(repos_root)},
    )
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
    upstream_port = upstream.server_address[1]
    upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    upstream_thread.start()

    secrets_dir = tmp_path / "secrets"
    secrets_dir.mkdir()
    (secrets_dir / "credentials.json").write_text(json.dumps({"*": "anonymous"}))
    tokens_path = tmp_path / "sandbox-tokens.json"
    tokens_path.write_text(json.dumps({"sandbox-0": SANDBOX_TOKEN}))
    allowlist_path = tmp_path / "allowlist.json"
    allowlist_path.write_text(json.dumps(["o/r"]))

    proxy = GitProxy(
        allowlist=Allowlist(allowlist_path),
        credentials=CredentialSet(secrets_dir),
        tokens=SandboxTokens(tokens_path),
        forwarder=_PlainHttpForwarder(host=f"127.0.0.1:{upstream_port}"),
    )
    proxy_server = ThreadingHTTPServer((_PROXY_HOST, _PROXY_PORT), make_handler(proxy))
    proxy_thread = threading.Thread(target=proxy_server.serve_forever, daemon=True)
    proxy_thread.start()

    try:
        yield GitProxyTarget(
            remote_url=f"http://{_PROXY_HOST}:{_PROXY_PORT}/o/r.git",
            token=SANDBOX_TOKEN, bare_repo=bare, seed_clone=seed,
        )
    finally:
        upstream.shutdown()
        proxy_server.shutdown()
        upstream_thread.join(timeout=5)
        proxy_thread.join(timeout=5)


# --- boot + the new libvirt.py SSH key plumbing ---------------------------

def test_the_sandbox_reaches_running_state(booted_sandbox: Sandbox):
    assert booted_sandbox.adapter.state(booted_sandbox.name) is VmState.RUNNING


def test_the_controller_key_authenticates_at_the_assigned_address(booted_sandbox: Sandbox):
    address = booted_sandbox.cluster.address_of(booted_sandbox.name)
    result = _run([
        "ssh", "-i", str(booted_sandbox.private_key), "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentityAgent=none",
        f"{SSH_USER}@{address}", "whoami",
    ])
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == SSH_USER


# --- grain.automation.ssh.SshRunner against a real sandbox ----------------

def test_ssh_runner_executes_a_real_remote_command(ssh_runner: SshRunner):
    result = ssh_runner.run(["echo", "hello-from-sandbox"])
    assert result.returncode == 0
    assert result.stdout.strip() == "hello-from-sandbox"


def test_ssh_runner_stdin_reaches_the_remote_command(ssh_runner: SshRunner):
    result = ssh_runner.run(["cat"], stdin="round-tripped\n")
    assert result.stdout == "round-tripped\n"


# --- grain.automation.dispatch against real systemd -----------------------

def test_a_real_systemd_unit_goes_active_then_done_success(ssh_runner: SshRunner):
    unit = "grain-test-success"
    reap(ssh_runner, unit)  # in case a previous run left it behind
    start_unit(ssh_runner, unit, "sleep 5")
    try:
        assert unit_status(ssh_runner, unit) is UnitState.ACTIVE
        time.sleep(8)
        assert unit_status(ssh_runner, unit) is UnitState.DONE_SUCCESS
    finally:
        reap(ssh_runner, unit)


def test_a_real_systemd_unit_reports_failure_then_reaps_to_absent(ssh_runner: SshRunner):
    unit = "grain-test-failure"
    reap(ssh_runner, unit)
    start_unit(ssh_runner, unit, "exit 1")
    time.sleep(2)
    assert unit_status(ssh_runner, unit) is UnitState.DONE_FAILED
    reap(ssh_runner, unit)
    assert unit_status(ssh_runner, unit) is UnitState.ABSENT


# --- grain.automation.health / cleanup against a real sandbox
# --- (docs/roadmap.md item 5) ----------------------------------------------
#
# `booted_sandbox` is deliberately unprovisioned (see its own docstring) —
# no docker, no kind. That's still useful ground to verify against: it's
# the live check that `check_disk`'s `df -P /` parser and `check_systemd`'s
# `is-system-running` reading hold up against a real guest's actual output
# shape (not just a hand-written FakeRunner fixture), and that a genuinely
# missing binary is reported as a graceful failure over real SSH rather than
# a hang or a crash — the same bar docs/design.md's "verify live" history
# (ssh.py, dispatch.py) has repeatedly found reasoning-alone gets wrong.

def test_check_disk_parses_real_df_output(ssh_runner: SshRunner):
    from grain.automation.health import check_disk

    result = check_disk(ssh_runner, watermark_percent=95)
    assert result.ok, result.detail
    # A freshly booted, unprovisioned cloud image should be nowhere near a
    # sane watermark — this pins down that the percentage really was parsed
    # out of `df`'s real column layout, not a lucky "couldn't parse" all
    # counted as fine.
    assert "% used on /" in result.detail


def test_check_systemd_reports_running_on_a_fresh_boot(ssh_runner: SshRunner):
    from grain.automation.health import check_systemd

    result = check_systemd(ssh_runner)
    assert result.ok, result.detail
    assert result.detail == "state: running"


def test_check_health_against_a_real_but_unprovisioned_sandbox(ssh_runner: SshRunner):
    from grain.automation.health import HealthStatus, check_health

    report = check_health(ssh_runner, watermark_percent=95)
    # docker isn't installed on this deliberately-bare image, so this can
    # never be HEALTHY here — the live-verified claim is narrower: ssh,
    # systemd, and disk all come back genuinely healthy, and the one
    # failing check is exactly the missing-docker one, not a parsing bug
    # masquerading as an unrelated failure.
    assert report.status is HealthStatus.DEGRADED
    by_name = {c.name: c for c in report.checks}
    assert by_name["ssh"].ok
    assert by_name["systemd"].ok
    assert by_name["disk"].ok
    assert not by_name["docker"].ok


def test_cleanup_against_a_sandbox_with_no_kind_or_docker_installed(ssh_runner: SshRunner):
    from grain.automation.cleanup import cleanup

    result = cleanup(ssh_runner)  # must not raise or hang
    assert not result.ok
    by_name = {s.name: s for s in result.steps}
    assert not by_name["kind"].ok
    assert not by_name["docker"].ok
    # A real "not found" from the remote shell, not an empty/garbled
    # detail — confirms stderr genuinely round-trips over this SSH path.
    assert "not found" in by_name["kind"].detail
    assert "not found" in by_name["docker"].detail


def test_dispatch_writes_the_real_prompt_and_clones_the_real_workspace(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    issue = Issue(
        number=7, title="a distinctive title marker",
        body="a distinctive body marker",
        html_url="https://github.com/o/r/issues/7", labels=frozenset(),
    )
    unit = dispatch(
        ssh_runner, "sandbox-0", issue,
        remote_url=git_proxy_target.remote_url, token=git_proxy_target.token,
    )
    try:
        prompt = ssh_runner.run(["cat", f"/tmp/{unit}.md"]).stdout
        assert "a distinctive title marker" in prompt
        assert "a distinctive body marker" in prompt
        assert "grain/issue-7" in prompt
        # The workspace clone this test actually cares about really
        # happened — over the network, through the real GitProxy, before
        # the unit (expected to fail: no `claude` binary on the stock
        # image) was even started.
        readme = ssh_runner.run(["cat", f"{WORKSPACE_PATH}/README.md"]).stdout
        assert readme == "hello from the live test upstream\n"
    finally:
        reap(ssh_runner, unit)
        ssh_runner.run(["rm", "-rf", WORKSPACE_PATH], check=False)


# --- configure_git_credentials / ensure_workspace against the real proxy --

def test_clone_through_the_real_proxy_with_the_right_token(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    configure_git_credentials(ssh_runner, git_proxy_target.remote_url, git_proxy_target.token)
    workspace = f"{WORKSPACE_PATH}-clone-test"
    try:
        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace)
        content = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert content == "hello from the live test upstream\n"
    finally:
        ssh_runner.run(["rm", "-rf", workspace], check=False)


def test_the_real_proxy_rejects_a_wrong_sandbox_token(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    # Same mechanism dispatch() relies on, deliberately misconfigured: this
    # is what confirms the credential-helper path is actually load-bearing
    # rather than something that happens to work regardless of the token.
    configure_git_credentials(ssh_runner, git_proxy_target.remote_url, "not-the-real-token")
    workspace = f"{WORKSPACE_PATH}-bad-token-test"
    try:
        result = ssh_runner.run(
            ["git", "clone", git_proxy_target.remote_url, workspace], check=False,
        )
        assert result.returncode != 0
    finally:
        ssh_runner.run(["rm", "-rf", workspace], check=False)


def test_ensure_workspace_fetches_and_resets_an_already_cloned_workspace(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    # The scenario docs/design.md calls out explicitly: a long-lived
    # sandbox reused for a second task, not reset in between. This is what
    # confirms ensure_workspace's fetch-and-reset branch — never exercised
    # by the single-clone tests above — actually picks up a new upstream
    # commit and discards whatever the "previous task" left behind.
    configure_git_credentials(ssh_runner, git_proxy_target.remote_url, git_proxy_target.token)
    workspace = f"{WORKSPACE_PATH}-reset-test"
    try:
        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace)
        first = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert "hello from the live test upstream" in first

        # Stand in for whatever a previous task left behind.
        ssh_runner.run(["bash", "-c", f"echo leftover > {workspace}/leftover.txt"])

        # A second commit lands upstream, pushed from the fixture's own
        # seed clone — not through the sandbox.
        seed = git_proxy_target.seed_clone
        (seed / "README.md").write_text("updated after the first checkout\n")
        _run_ok(["git", "-C", str(seed), "commit", "-aqm", "second commit"])
        _run_ok(["git", "-C", str(seed), "push", "-q", "origin", "main"])

        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace)
        second = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert second == "updated after the first checkout\n"
        leftover = ssh_runner.run(["test", "-f", f"{workspace}/leftover.txt"], check=False)
        assert leftover.returncode != 0, "clean -fdx should have removed the leftover file"
    finally:
        ssh_runner.run(["rm", "-rf", workspace], check=False)
