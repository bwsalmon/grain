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
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

import grain.automation.capture as capture_module
from grain.adapter.base import VmState
from grain.adapter.libvirt import LIBVIRT_URI, LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.config import AutomationConfig
from grain.automation.dispatch import (
    WORKSPACE_PATH, SandboxTarget, UnitState, configure_git_credentials, dispatch,
    ensure_workspace, reap, start_unit, transcript_path, unit_name, unit_status,
)
from grain.automation.github import Issue, PullRequestDetail, ReviewComment
from grain.automation.history import FileSessionHistory
from grain.automation.ssh import SshRunner
from grain.automation.state import AutomationState
from grain.automation.sweeper import sweep
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
        controller_public_key_path=public_key,
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


@pytest.fixture(scope="session")
def controller_stand_in(booted_sandbox: Sandbox, ssh_runner_for_controller_setup) -> None:
    """`claude -p` now runs on the controller, not the sandbox
    (docs/roadmap.md item 8's "Update") — but this suite only ever boots one
    VM (see the module docstring). Rather than stand up a second real VM
    just to host `dispatch()`'s controller-side half, the tests below reuse
    the same sandbox as a stand-in "controller" too — legitimate for what
    they're actually proving (that the real systemd-run/SSH/mkdir/dd
    mechanism works), the same way `start_unit`'s own docstring already
    treats it as location-agnostic: "any inert stand-in command proves the
    same systemd-run/SSH mechanism `dispatch()` relies on." What it does
    *not* prove is anything about credential isolation between the
    controller and the sandbox — this VM plays both roles at once here, so
    that property is meaningless to check against it; it's covered instead
    by the design itself (no Claude credential ever reaches a sandbox,
    verified by what `provision/sandbox.sh` no longer installs) and by a
    real two-VM live run (docs/roadmap.md item 8's own verification order).

    Session-scoped: idempotent (`useradd`/`mkdir -p` both tolerate already
    existing), so paying this setup cost once for every test that needs it
    is fine.
    """
    runner = ssh_runner_for_controller_setup
    # --create-home, matching provision/controller.sh's real grain-agent
    # user: it needs one for ~/.claude and for git's own per-user config.
    runner.run(["sudo", "useradd", "--system", "--create-home",
                "--shell", "/usr/sbin/nologin", "grain-agent"], check=False)
    runner.run(["sudo", "mkdir", "-p", "/data/state/automation/units"])
    runner.run(["sudo", "chmod", "-R", "0777", "/data"])  # test-only; real
    # deployments get real ownership from provision/controller.sh -- this
    # stand-in just needs the plain, unprivileged `mkdir`/`dd` calls
    # `_start_task` issues via controller_runner (no `sudo` in front of
    # them, by design -- see dispatch.py) to succeed against a directory a
    # real controller would already own correctly.


@pytest.fixture(scope="session")
def ssh_runner_for_controller_setup(booted_sandbox: Sandbox) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=booted_sandbox.cluster.address_of(booted_sandbox.name),
        key_path=booted_sandbox.private_key,
    )


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
    # A second branch, pushed alongside `main` with distinct content — what
    # docs/roadmap.md item 9's checkout-existing-branch tests reset/clone
    # onto, standing in for a real PR's own branch (the equivalent of the
    # single `main` branch every earlier test in this suite assumed was the
    # only one that mattered).
    pr_branch: str


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

    # A second branch, standing in for an existing PR's own branch
    # (docs/roadmap.md item 9) — distinct content from main, so a test can
    # tell "landed on the PR's branch" apart from "landed on the default
    # branch" by what the checked-out file actually says.
    pr_branch = "pr-feature-x"
    _run_ok(["git", "-C", str(seed), "checkout", "-q", "-b", pr_branch])
    (seed / "README.md").write_text("hello from the PR's own branch\n")
    _run_ok(["git", "-C", str(seed), "commit", "-q", "-am", "pr work"])
    _run_ok(["git", "-C", str(seed), "push", "-q", "origin", pr_branch])
    _run_ok(["git", "-C", str(seed), "checkout", "-q", "main"])

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
            pr_branch=pr_branch,
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
    controller_stand_in: None, booted_sandbox: Sandbox,
):
    issue = Issue(
        number=7, title="a distinctive title marker",
        body="a distinctive body marker",
        html_url="https://github.com/o/r/issues/7", labels=frozenset(),
    )
    target = SandboxTarget(
        address=str(booted_sandbox.cluster.address_of("sandbox-0")),
        ssh_user=SSH_USER, ssh_key_path=str(booted_sandbox.private_key),
    )
    # ssh_runner plays both roles (sandbox and stand-in controller) — see
    # controller_stand_in's own docstring for why that's legitimate here.
    unit = dispatch(
        ssh_runner, ssh_runner, "sandbox-0", target, issue,
        remote_url=git_proxy_target.remote_url, token=git_proxy_target.token,
    )
    try:
        prompt = ssh_runner.run(["cat", f"/data/state/automation/units/{unit}/prompt.md"]).stdout
        assert "a distinctive title marker" in prompt
        assert "a distinctive body marker" in prompt
        assert "grain/issue-7" in prompt
        mcp_config = json.loads(
            ssh_runner.run(["cat", f"/data/state/automation/units/{unit}/mcp-config.json"]).stdout
        )
        server_args = mcp_config["mcpServers"]["grain-sandbox"]["args"]
        assert server_args[server_args.index("--address") + 1] == target.address
        # The workspace clone this test actually cares about really
        # happened — over the network, through the real GitProxy, before
        # the unit (expected to fail: no real Claude credential exists in
        # this environment) was even started.
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


# --- ensure_workspace(branch=...) / dispatch_pr against the real proxy
# --- (docs/roadmap.md item 9): checking out an *existing* PR branch is new
# --- territory even though ensure_workspace already existed for the
# --- fresh-clone/default-branch case (item 2) — this is what actually
# --- proves it against a real git client and a real (if throwaway) proxy,
# --- not just the FakeRunner-scripted unit tests in test_automation_dispatch.py.

def test_ensure_workspace_with_a_branch_clones_straight_onto_that_branch(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    # First-ever dispatch to this sandbox for this PR: no existing workspace
    # at all, so this exercises ensure_workspace's post-clone checkout —
    # a plain `git clone` alone would land on `main`, not the PR's branch.
    workspace = f"{WORKSPACE_PATH}-pr-first-clone-test"
    try:
        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace,
                          branch=git_proxy_target.pr_branch)
        content = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert content == "hello from the PR's own branch\n"
        branch = ssh_runner.run(
            ["git", "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD"]
        ).stdout.strip()
        assert branch == git_proxy_target.pr_branch
    finally:
        ssh_runner.run(["rm", "-rf", workspace], check=False)


def test_ensure_workspace_with_a_branch_resets_an_existing_default_branch_checkout(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
):
    # The realistic PR-dispatch scenario on a long-lived sandbox: the
    # workspace already exists from an *earlier, unrelated* dispatch on the
    # default branch (an issue task), and this dispatch must land on the
    # PR's own branch instead — not fetch-and-reset back onto origin/HEAD.
    workspace = f"{WORKSPACE_PATH}-pr-reset-test"
    try:
        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace)
        first = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert first == "hello from the live test upstream\n"

        ensure_workspace(ssh_runner, git_proxy_target.remote_url, path=workspace,
                          branch=git_proxy_target.pr_branch)
        second = ssh_runner.run(["cat", f"{workspace}/README.md"]).stdout
        assert second == "hello from the PR's own branch\n"
        branch = ssh_runner.run(
            ["git", "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD"]
        ).stdout.strip()
        assert branch == git_proxy_target.pr_branch
    finally:
        ssh_runner.run(["rm", "-rf", workspace], check=False)


def test_dispatch_pr_writes_the_real_prompt_and_checks_out_the_prs_branch(
    ssh_runner: SshRunner, git_proxy_target: GitProxyTarget, git_installed: None,
    controller_stand_in: None, booted_sandbox: Sandbox,
):
    from grain.automation.dispatch import dispatch_pr

    pr = PullRequestDetail(
        number=11, title="a distinctive PR title marker",
        body="a distinctive PR body marker",
        html_url="https://github.com/o/r/pull/11",
        head_ref=git_proxy_target.pr_branch, base_ref="main",
    )
    comments = [ReviewComment(id=1, user="reviewer", body="a distinctive comment marker",
                               path="README.md", line=1)]
    target = SandboxTarget(
        address=str(booted_sandbox.cluster.address_of("sandbox-0")),
        ssh_user=SSH_USER, ssh_key_path=str(booted_sandbox.private_key),
    )
    unit = dispatch_pr(
        ssh_runner, ssh_runner, "sandbox-0", target, pr, comments,
        remote_url=git_proxy_target.remote_url, token=git_proxy_target.token,
    )
    try:
        prompt = ssh_runner.run(["cat", f"/data/state/automation/units/{unit}/prompt.md"]).stdout
        assert "a distinctive PR title marker" in prompt
        assert "a distinctive PR body marker" in prompt
        assert "a distinctive comment marker" in prompt
        assert f"git push origin HEAD:{git_proxy_target.pr_branch}" in prompt
        # The workspace really landed on the PR's own branch, over the
        # network, through the real GitProxy — before the unit (expected to
        # fail: no real Claude credential exists in this environment) was
        # even started.
        readme = ssh_runner.run(["cat", f"{WORKSPACE_PATH}/README.md"]).stdout
        assert readme == "hello from the PR's own branch\n"
        branch = ssh_runner.run(
            ["git", "-C", WORKSPACE_PATH, "rev-parse", "--abbrev-ref", "HEAD"]
        ).stdout.strip()
        assert branch == git_proxy_target.pr_branch
    finally:
        reap(ssh_runner, unit)
        ssh_runner.run(["rm", "-rf", WORKSPACE_PATH], check=False)


# --- mcp_server.py's real SSH-to-sandbox tool round-trip (docs/roadmap.md
# --- item 8's "Update") -- proves the actual mechanism the controller-side
# --- claude -p session depends on for everything it does, independent of
# --- claude -p itself (no real Claude credential exists in this
# --- environment): a real `run_command`/`read_file`/`edit_file`/
# --- `write_file` round-trip against the real sandbox, invoked exactly the
# --- way `--mcp-config` will spawn it -- JSON-RPC over stdin/stdout, no
# --- Claude Code involved at all.

_REPO_ROOT = Path(__file__).resolve().parent.parent


def test_mcp_server_round_trips_real_tool_calls_over_real_ssh(
    booted_sandbox: Sandbox, git_installed: None, tmp_path: Path,
):
    question_path = tmp_path / "question.txt"
    requests = [
        {"jsonrpc": "2.0", "id": 1, "method": "initialize"},
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
        {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {
            "name": "run_command",
            "arguments": {"command": "echo hello-from-real-sandbox && whoami"},
        }},
        {"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {
            "name": "write_file",
            "arguments": {"file_path": f"{WORKSPACE_PATH}/mcp_test.txt",
                           "content": "line one\nline two\nline three\n"},
        }},
        {"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {
            "name": "read_file",
            "arguments": {"file_path": f"{WORKSPACE_PATH}/mcp_test.txt"},
        }},
        {"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": {
            "name": "edit_file",
            "arguments": {"file_path": f"{WORKSPACE_PATH}/mcp_test.txt",
                           "old_string": "line two", "new_string": "line TWO edited"},
        }},
        {"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": {
            "name": "run_command",
            "arguments": {"command": "sleep 5", "timeout": 1000},
        }},
        {"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": {
            "name": "ask_question",
            "arguments": {"question": "a distinctive question marker"},
        }},
    ]
    stdin_text = "\n".join(json.dumps(r) for r in requests) + "\n"

    ssh_runner = SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=booted_sandbox.cluster.address_of(booted_sandbox.name),
        key_path=booted_sandbox.private_key,
    )
    ssh_runner.run(["mkdir", "-p", WORKSPACE_PATH])
    try:
        proc = subprocess.run(
            ["python3", "-m", "grain.automation.mcp_server",
             "--address", str(booted_sandbox.cluster.address_of(booted_sandbox.name)),
             "--user", SSH_USER, "--key-path", str(booted_sandbox.private_key),
             "--workspace", WORKSPACE_PATH, "--question-path", str(question_path)],
            input=stdin_text, capture_output=True, text=True, timeout=60, cwd=_REPO_ROOT,
        )
        responses = [json.loads(line) for line in proc.stdout.splitlines() if line.strip()]
        by_id = {r["id"]: r for r in responses if "id" in r}

        assert by_id[1]["result"]["serverInfo"]["name"] == "grain-sandbox"
        tool_names = {t["name"] for t in by_id[2]["result"]["tools"]}
        assert tool_names == {
            "run_command", "read_file", "edit_file", "write_file", "ask_question",
        }

        run_text = by_id[3]["result"]["content"][0]["text"]
        assert "hello-from-real-sandbox" in run_text
        assert f"\n{SSH_USER}\n" in run_text or run_text.strip().endswith(SSH_USER)
        assert by_id[3]["result"]["isError"] is False

        assert by_id[4]["result"]["isError"] is False

        read_text = by_id[5]["result"]["content"][0]["text"]
        assert read_text == "     1\tline one\n     2\tline two\n     3\tline three"

        assert by_id[6]["result"]["isError"] is False

        # Real network-level timeout enforcement, not a mocked clock: the
        # `timeout` coreutil really killed a real `sleep 5` after 1s.
        timeout_result = by_id[7]["result"]
        assert timeout_result["isError"] is True
        assert "exit=124" in timeout_result["content"][0]["text"]

        # And the edit really landed on the real sandbox, independent of
        # the tool's own report of success.
        final = ssh_runner.run(["cat", f"{WORKSPACE_PATH}/mcp_test.txt"]).stdout
        assert final == "line one\nline TWO edited\nline three\n"

        assert by_id[8]["result"]["isError"] is False
        # ask_question never touches the sandbox at all -- the question
        # lands on the *local* filesystem this subprocess ran against, not
        # over ssh_runner.
        assert question_path.read_text() == "a distinctive question marker"
    finally:
        ssh_runner.run(["rm", "-f", f"{WORKSPACE_PATH}/mcp_test.txt"], check=False)


# --- trajectory capture against a real sandbox (docs/roadmap.md item 10) ---
#
# No real `claude -p` login exists in this environment (same constraint item
# 8 has for the agent itself — see the module docstring above), so this
# doesn't run claude for real. What it verifies live is the mechanism this
# item is actually about: a plausible, realistically-shaped trajectory file
# — in the exact JSONL, per-line `type`-tagged format `capture.py`'s
# docstring documents having confirmed against a real Claude Code session
# transcript — is written to the real path `dispatch.py`'s
# `transcript_path()` computes, on a real sandbox; `sweeper.py`'s real
# `sweep()` (not a hand-called `capture_trajectory()`) pulls it off over real
# SSH as part of releasing a real, freshly-finished systemd unit; and a real
# `FileSessionHistory` records the session with the captured content intact,
# byte for byte, before the sandbox's slot is freed for reuse.

_SIMULATED_TRAJECTORY = "\n".join([
    json.dumps({
        "type": "system", "subtype": "init", "session_id": "sim-session-1",
        "model": "claude-sonnet-5", "cwd": WORKSPACE_PATH,
    }),
    json.dumps({
        "type": "user", "session_id": "sim-session-1",
        "message": {"role": "user", "content": "You are working GitHub issue #123..."},
    }),
    json.dumps({
        "type": "assistant", "session_id": "sim-session-1",
        "message": {
            "role": "assistant",
            "content": [
                {"type": "text", "text": "I'll look at the failing test first."},
                {
                    "type": "tool_use", "id": "toolu_sim_1", "name": "Bash",
                    "input": {"command": "pytest -q", "description": "run the suite"},
                },
            ],
        },
    }),
    json.dumps({
        "type": "user", "session_id": "sim-session-1",
        "message": {
            "role": "user",
            "content": [{
                "type": "tool_result", "tool_use_id": "toolu_sim_1",
                "content": "1 failed, 41 passed", "is_error": False,
            }],
        },
    }),
    json.dumps({
        "type": "assistant", "session_id": "sim-session-1",
        "message": {
            "role": "assistant",
            "content": [{"type": "text", "text": "Fixed the off-by-one error and pushed."}],
        },
    }),
    json.dumps({
        "type": "result", "session_id": "sim-session-1", "is_error": False,
        "result": "Fixed the off-by-one error and pushed.",
        "total_cost_usd": 0.1234, "num_turns": 3, "duration_ms": 4200,
    }),
]) + "\n"


def test_sweep_captures_a_real_trajectory_file_before_freeing_the_slot(
    ssh_runner: SshRunner, booted_sandbox: Sandbox, tmp_path: Path, monkeypatch,
):
    from grain.automation.transcript import parse_transcript

    sandbox = booted_sandbox.name  # "sandbox-0"
    unit = unit_name(sandbox)
    # claude -p's transcript is a controller-local file now (docs/roadmap.md
    # item 8's "Update") -- capture_trajectory reads it directly, no SSH
    # involved, so this test's "already-written transcript" is a plain local
    # file too, not something `dd`'d onto the sandbox.
    out_path = tmp_path / f"{unit}.transcript.jsonl"
    monkeypatch.setattr(capture_module, "transcript_path", lambda u: str(out_path))
    reap(ssh_runner, unit)  # in case a previous test left it behind

    try:
        # Stand in for "claude -p --output-format stream-json --verbose
        # > out_path" having already run and written its transcript — the
        # exact redirect dispatch.py's start_unit now builds, on whichever
        # machine claude -p runs on.
        out_path.write_text(_SIMULATED_TRAJECTORY)

        # A trivial real unit standing in for the dispatched claude -p
        # process itself (same substitution test_a_real_systemd_unit_goes_
        # active_then_done_success uses) -- what sweep() actually reads to
        # decide this task is DONE_SUCCESS. Run via ssh_runner playing the
        # controller's role too (see controller_stand_in's docstring) --
        # `unit_status`/`reap` are runner-agnostic, so this genuinely proves
        # the mechanism regardless of which machine hosts the unit.
        start_unit(ssh_runner, unit, "true")
        time.sleep(2)
        assert unit_status(ssh_runner, unit) is UnitState.DONE_SUCCESS

        state = AutomationState()
        started_at = datetime.now(timezone.utc) - timedelta(minutes=1)
        state.assign(sandbox, issue=123, unit=unit, now=started_at)
        history = FileSessionHistory(tmp_path / "sessions")
        config = AutomationConfig(owner="o", repo="r")

        result = sweep(
            state, lambda name: ssh_runner, ssh_runner, config,
            datetime.now(timezone.utc), history=history,
        )

        # The slot is freed -- capture ran as part of releasing it, not on
        # some later fetch-on-demand path.
        assert sandbox not in state.assignments
        assert [o.issue for o in result.succeeded] == [123]

        records = history.for_trigger(123)
        assert len(records) == 1
        record = records[0]
        assert record.sandbox == sandbox
        assert record.unit == unit
        assert record.outcome == "succeeded"
        assert record.transcript_path is not None

        captured = history.read_transcript(record)
        assert captured == _SIMULATED_TRAJECTORY

        # And it's genuinely usable by the session browser's own parser --
        # not just bytes that happen to round-trip.
        events = parse_transcript(captured)
        assert [e.role for e in events] == [
            "system", "user", "assistant", "user", "assistant", "result",
        ]
        assert "Fixed the off-by-one error" in events[-1].summary
    finally:
        reap(ssh_runner, unit)
