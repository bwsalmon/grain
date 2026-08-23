"""Live end-to-end verification of docs/roadmap.md item 8: run the *real*
`grain.automation.core.Orchestrator.run_once` against real infrastructure —
a real sandbox VM (booted the same way `test_vm_integration.py` boots one),
a real bare git repo served over real smart-HTTP behind a real `GitProxy`
(the exact `git_proxy_target` rig `test_vm_integration.py` built for
docs/roadmap.md item 2) — with exactly two substitutions, both because the
real things genuinely don't exist in this environment:

1. **A realistic mock of the GitHub REST API** (`RealGitHubMock` below),
   wired into the real `GitHubClient` through its own `Transport` protocol —
   the identical seam `grain/automation/github.py`'s own `FakeTransport`
   uses for unit tests. So the code actually under test is the real
   `GitHubClient` (path building, pagination, status-code handling, JSON
   field extraction) and the real `Orchestrator`/`sweeper.py` decision
   logic — only the network call underneath is swapped. Critically,
   `branch_exists` is not a canned answer: it runs a real `git show-ref`
   against the same real bare repo the sandbox clones from and pushes to
   through the real `GitProxy`, so a passing check really does mean "the
   agent's push landed on the branch it was told to," the same load-bearing
   claim `core.py`'s own docstring makes about why that check exists at all.

2. **A fake `claude` binary** installed on the sandbox's `PATH`
   (`/usr/local/bin/claude`), standing in for a real Claude Code login this
   environment doesn't have (`docs/design.md`'s "Interim choice" section is
   explicit that logging in is a manual, per-sandbox step). It reads
   whatever `dispatch()` actually piped to its stdin — the real prompt,
   including the `git push origin HEAD:<branch>` instruction — parses the
   branch out of that text the way a real agent would read its own
   instructions, makes a real commit, and pushes it to the exact branch
   `dispatch()` computed. `dispatch.py`/`core.py`'s production code is not
   touched or special-cased in any way; only the sandbox-side binary is
   fake.

Everything else is the real thing: `Orchestrator.run_once` (not a
reimplementation of its decision logic), the real `SshRunner`/systemd-unit
dispatch mechanism, the real `GitProxy` and its token check, the real
`AutomationState` pool bookkeeping, the real sweeper's cleanup/health hooks.

**What this proves, and what it doesn't** — also recorded in
`docs/roadmap.md` item 8: this exercises the entire mechanical pipeline for
real — issue discovery, dispatch, a real clone through a real proxy, a real
pushed commit, sweep-time success detection against real git state, and PR
creation with the right head/base/title. It does **not** prove anything
about real GitHub's actual API behavior/quirks (rate limits, exact error
shapes, auth edge cases) or about a real Claude Code agent's actual
behavior — a real agent could push somewhere unexpected, hang, misread its
instructions, or fail in ways a scripted stand-in structurally cannot. That
gap is real and only a genuinely live run with a real credential and a real
target repo closes it.
"""

from __future__ import annotations

import json
import re
import subprocess
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from http.server import ThreadingHTTPServer
from pathlib import Path
from urllib.parse import unquote, urlsplit

import pytest

from grain.adapter.libvirt import LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork
from grain.automation.audit import RecordingAuditLog
from grain.automation.config import AutomationConfig
from grain.automation.core import Orchestrator
from grain.automation.dispatch import (
    WORKSPACE_PATH, UnitState, branch_name, unit_name, unit_status,
)
from grain.automation.github import ApiResponse, GitHubClient
from grain.automation.ssh import SshRunner
from grain.automation.state import AutomationState
from grain.inventory import GIT_PROXY_PORT, Cluster
from grain.proxy.allowlist import Allowlist
from grain.proxy.core import GitProxy
from grain.proxy.credentials import CredentialSet
from grain.proxy.server import make_handler
from grain.proxy.tokens import SandboxTokenStore, SandboxTokens
from grain.run import RealRunner

# Reused, not duplicated: booting a sandbox VM is the slow part of this
# suite, and `test_vm_integration.py` already built most of the rig for it
# (base image fetch, controller SSH keypair, the `SshRunner`-over-a-real-VM
# shape, and the git-http-backend building blocks `git_proxy_target`
# composes). `booted_sandbox`/`ssh_runner`/`git_installed` themselves are
# deliberately *not* imported — see `live_sandbox`'s own docstring below for
# why this suite boots its own, separate VM instead of sharing that one.
from test_vm_integration import (  # noqa: E402
    BOOT_TIMEOUT_S,
    SSH_USER,
    Sandbox,
    _GitBackendHandler,
    _PlainHttpForwarder,
    _host_ready,
    _run_ok,
    _wait_for_ssh,
    base_image,
    controller_key,
)

pytestmark = pytest.mark.skipif(
    not _host_ready(),
    reason="needs /dev/kvm, qemu:///system, and br-grain already up",
)

BRIDGE = "br-grain"
OWNER = "live-org"
REPO = "live-repo"


# --- a dedicated sandbox VM, at a different index from test_vm_integration's
# --- own `booted_sandbox` ---------------------------------------------------

@pytest.fixture(scope="module")
def live_sandbox(base_image, controller_key):
    """Boots this suite's own sandbox VM, at index 1 (`sandbox-1`) —
    deliberately *not* `test_vm_integration.py`'s `booted_sandbox`
    (`sandbox-0`). Found live, building this suite: this host is shared —
    another concurrent test run collided directly on `sandbox-0`/
    `10.100.0.10`/the `gr-sb0` tap (`virsh define`: "domain 'sandbox-0'
    already exists with uuid ..."; `virsh start`: "qemu unexpectedly closed
    the monitor"), because `test_vm_integration.py`'s fixtures hardcode
    that one name/address/config-dir with no per-session isolation.
    `sandbox-1` isn't a workaround bolted on after the fact: `Cluster()`'s
    own default is `sandbox_count=2`, and the host's already-applied
    firewall ruleset was confirmed live (`nft list ruleset`) to already
    carry both `gr-sb0` and `gr-sb1`'s anti-spoof and DNAT rules — this
    suite is using a resource the deployment already provisions for, just
    one `test_vm_integration.py` itself never happens to touch.
    """
    private_key, public_key = controller_key
    config_dir = Path("/var/lib/grain/instances/test-live-issue-to-pr")
    config_dir.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(sandbox_count=2, image=str(base_image), bridge=BRIDGE)
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    adapter = LibvirtAdapter(
        cluster, runner, network, config_dir=config_dir,
        controller_public_key_path=public_key,
    )
    name = "sandbox-1"
    try:
        adapter.create(cluster.spec_of(name))
        adapter.start(name)
        _wait_for_ssh(cluster.address_of(name), private_key, timeout=BOOT_TIMEOUT_S)
        yield Sandbox(adapter=adapter, cluster=cluster, name=name, private_key=private_key)
    finally:
        adapter.destroy(name)


@pytest.fixture
def live_ssh_runner(live_sandbox: Sandbox) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=live_sandbox.cluster.address_of(live_sandbox.name),
        key_path=live_sandbox.private_key,
    )


@pytest.fixture(scope="module")
def live_controller_stand_in(live_sandbox: Sandbox) -> None:
    """`claude -p` runs on the controller now, not the sandbox
    (docs/roadmap.md item 8's "Update") — but this suite, like
    `test_vm_integration.py`, only ever boots one VM. `live_sandbox` stands
    in for both roles at once here, same reasoning as
    `test_vm_integration.py`'s `controller_stand_in` (see its own
    docstring): this is testing the real dispatch/sweep/PR *mechanism*, not
    credential isolation between two machines, so one VM playing both parts
    is a legitimate simplification, not a gap in what this suite proves.

    `grain-agent` gets passwordless sudo here (a real deployment's
    `grain-agent` never has or needs sudo) purely so the fake `claude`
    script below can fix up workspace permissions itself — see that
    script's own comment for why that's needed only because this stand-in
    collapses two accounts (`debian`, who owns the cloned workspace, and
    `grain-agent`, who runs the agent process) onto one VM.
    """
    runner = SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=live_sandbox.cluster.address_of(live_sandbox.name),
        key_path=live_sandbox.private_key,
    )
    # --create-home, matching provision/controller.sh's real grain-agent
    # user: it needs one for ~/.claude and (found live, building this fake
    # script) for `git config --global` to have somewhere to write.
    runner.run(["sudo", "useradd", "--system", "--create-home",
                "--shell", "/usr/sbin/nologin", "grain-agent"], check=False)
    runner.run([
        "bash", "-c",
        "echo 'grain-agent ALL=(ALL) NOPASSWD:ALL' | "
        "sudo tee /etc/sudoers.d/99-grain-agent-test-only >/dev/null",
    ])
    runner.run(["sudo", "mkdir", "-p", "/data/state/automation/units"])
    runner.run(["sudo", "chmod", "-R", "0777", "/data"])
    runner.run(["sudo", "mkdir", "-p", "/opt/grain"])


@pytest.fixture(scope="module")
def live_git_installed(live_sandbox: Sandbox) -> None:
    """Same reasoning as `test_vm_integration.py`'s own `git_installed`:
    the bare cloud image boots with no `git` binary at all.
    """
    runner = SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=live_sandbox.cluster.address_of(live_sandbox.name),
        key_path=live_sandbox.private_key,
    )
    runner.run(["sudo", "apt-get", "update", "-qq"])
    runner.run(["sudo", "apt-get", "install", "-y", "-qq", "git"])


# --- substitution 1: a realistic mock of the GitHub REST API --------------

_ISSUES_RE = re.compile(r"^/repos/(?P<owner>[^/]+)/(?P<repo>[^/]+)/issues$")
_LABELS_POST_RE = re.compile(
    r"^/repos/(?P<owner>[^/]+)/(?P<repo>[^/]+)/issues/(?P<number>\d+)/labels$"
)
_LABEL_DELETE_RE = re.compile(
    r"^/repos/(?P<owner>[^/]+)/(?P<repo>[^/]+)/issues/(?P<number>\d+)/labels/(?P<label>[^/]+)$"
)
_BRANCH_RE = re.compile(
    r"^/repos/(?P<owner>[^/]+)/(?P<repo>[^/]+)/branches/(?P<branch>[^/]+)$"
)
_PULLS_RE = re.compile(r"^/repos/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pulls$")


def _parse_qs(query: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for part in query.split("&"):
        if not part:
            continue
        k, _, v = part.partition("=")
        out[unquote(k)] = unquote(v)
    return out


@dataclass
class FakeIssue:
    title: str
    body: str
    labels: set[str]


@dataclass
class RealGitHubMock:
    """Implements the specific endpoints `GitHubClient` calls
    (`list_issues`/`list_pull_requests` via the shared `/issues` listing,
    `add_label`, `remove_label`, `branch_exists`, `create_pull_request`),
    seeded with one fake issue carrying the trigger label. Wired in as a
    `Transport`, so `GitHubClient`'s own logic runs unmodified.

    `branch_exists` is the one endpoint that would be dishonest as a canned
    answer — see the module docstring for why it instead runs a real `git
    show-ref` against `bare_repo`.
    """

    owner: str
    repo: str
    bare_repo: Path
    issues: dict[int, FakeIssue] = field(default_factory=dict)
    pull_requests: list[dict] = field(default_factory=list)
    calls: list[tuple[str, str]] = field(default_factory=list)

    def _branch_exists(self, branch: str) -> bool:
        result = subprocess.run(
            ["git", "--git-dir", str(self.bare_repo), "show-ref", "--verify",
             "--quiet", f"refs/heads/{branch}"],
        )
        return result.returncode == 0

    def request(self, *, method: str, path: str, headers: dict[str, str],
                body: bytes | None) -> ApiResponse:
        self.calls.append((method, path))
        split = urlsplit(path)
        p = split.path
        qs = _parse_qs(split.query)

        m = _ISSUES_RE.match(p)
        if method == "GET" and m:
            assert (m["owner"], m["repo"]) == (self.owner, self.repo)
            label = qs.get("labels")
            items = []
            for number, issue in sorted(self.issues.items()):
                if label and label not in issue.labels:
                    continue
                items.append({
                    "number": number, "title": issue.title, "body": issue.body,
                    "html_url": f"https://github.example/{self.owner}/{self.repo}/issues/{number}",
                    "labels": [{"name": l} for l in sorted(issue.labels)],
                })
            return ApiResponse(200, {}, json.dumps(items).encode())

        m = _LABELS_POST_RE.match(p)
        if method == "POST" and m:
            number = int(m["number"])
            payload = json.loads(body or b"{}")
            self.issues[number].labels.update(payload["labels"])
            return ApiResponse(200, {}, b"[]")

        m = _LABEL_DELETE_RE.match(p)
        if method == "DELETE" and m:
            number = int(m["number"])
            label = unquote(m["label"])
            self.issues[number].labels.discard(label)
            return ApiResponse(200, {}, b"{}")

        m = _BRANCH_RE.match(p)
        if method == "GET" and m:
            branch = unquote(m["branch"])
            exists = self._branch_exists(branch)
            return ApiResponse(200 if exists else 404, {}, b"{}")

        m = _PULLS_RE.match(p)
        if method == "POST" and m:
            payload = json.loads(body or b"{}")
            number = 9000 + len(self.pull_requests)
            record = {
                **payload, "number": number,
                "html_url": f"https://github.example/{self.owner}/{self.repo}/pull/{number}",
            }
            self.pull_requests.append(record)
            return ApiResponse(201, {}, json.dumps(record).encode())

        raise AssertionError(f"RealGitHubMock: unhandled request {method} {path}")


# --- substitution 2: a fake `claude` binary --------------------------------
#
# Reads the real prompt dispatch() piped to its stdin, parses the branch out
# of the `git push origin HEAD:<branch>` line the same way a real agent
# would read its own instructions, and either makes a real commit and
# pushes it, or deliberately doesn't (the two failure shapes the roadmap
# item also asks to be exercised).
#
# `claude -p` runs on the controller now (docs/roadmap.md item 8's
# "Update"), started from `/opt/grain`, not from inside the workspace — a
# real agent reaches the workspace only through `mcp_server.py`'s tools
# over SSH, never a local cwd. This fake binary is intentionally *not*
# MCP-aware (that mechanism is already proven for real in
# `test_vm_integration.py`'s `test_mcp_server_round_trips_real_tool_calls_
# over_real_ssh` — re-proving it here would just be redundant); it `cd`s
# straight into the known workspace path instead, since `live_controller_
# stand_in` collapses the sandbox and controller onto one VM, so that path
# is already reachable on the local filesystem. The one thing that
# collapse actually breaks: the workspace was cloned as `debian` (over
# `sandbox_runner`, real production code, unchanged) but this script runs
# as `grain-agent` (a different, real account, matching production) — so it
# needs one `sudo` fixup line no real agent would ever need, since a real
# agent never touches the workspace as a local user at all.

_FAKE_CLAUDE_SUCCESS = f"""#!/bin/bash
set -eu
prompt="$(cat)"
branch=$(printf '%s\\n' "$prompt" | sed -n 's/.*git push origin HEAD:\\([^ ]*\\).*/\\1/p' | head -n1)
if [ -z "$branch" ]; then
  echo "fake-claude: could not find a branch to push to in the prompt" >&2
  exit 1
fi
sudo chmod -R a+rwX {WORKSPACE_PATH}
# git's own "dubious ownership" protection (checks the repo's UID, not
# permission bits -- the chmod above doesn't satisfy it) fires here for the
# same reason the chmod above is needed at all: this script runs as
# grain-agent, touching a workspace `debian` owns directly, unlike a real
# agent, which only ever reaches it via SSH-as-debian through
# mcp_server.py's tools and never hits this.
git config --global --add safe.directory {WORKSPACE_PATH}
# Same story as safe.directory above: configure_git_credentials (real
# production code, unchanged) only ever configured debian's own credential
# helper -- correct, since a real agent authenticates as debian over SSH
# through mcp_server.py, never touching git credentials as any other user.
# This script running as grain-agent instead (this test's own
# simplification) needs its own copy.
sudo cat /home/debian/.git-credentials > ~/.git-credentials
chmod 600 ~/.git-credentials
git config --global credential.helper store
cd {WORKSPACE_PATH}
git config user.email "fake-agent@grain.local"
git config user.name "grain-fake-agent"
echo "fake agent was here: $(date -u +%FT%TZ)" >> AGENT_NOTE.md
git add -A
git commit -q -m "grain: automated fix (fake agent)"
git push origin "HEAD:$branch"
echo "fake-claude: pushed to $branch"
"""

_FAKE_CLAUDE_FAIL_NONZERO = """#!/bin/bash
echo "fake-claude: simulating an agent failure, not touching the repo" >&2
exit 1
"""

_FAKE_CLAUDE_SUCCEED_NO_PUSH = """#!/bin/bash
set -eu
echo "fake-claude: exiting zero without ever pushing anything" >&2
exit 0
"""


def _install_fake_claude(ssh_runner: SshRunner, script: str) -> None:
    ssh_runner.run(["sudo", "dd", "of=/usr/local/bin/claude", "status=none"], stdin=script)
    ssh_runner.run(["sudo", "chmod", "755", "/usr/local/bin/claude"])


def _wait_for_unit(ssh_runner: SshRunner, sandbox: str, timeout: float = 30) -> UnitState:
    unit = unit_name(sandbox)
    deadline = time.monotonic() + timeout
    state = unit_status(ssh_runner, unit)
    while state is UnitState.ACTIVE and time.monotonic() < deadline:
        time.sleep(1)
        state = unit_status(ssh_runner, unit)
    return state


# --- where the fake proxy binds -------------------------------------------
#
# `Orchestrator._remote_url()` is real, unmodified production code — it
# always builds `http://{cluster.controller_ip}:{GIT_PROXY_PORT}/...`, never
# an address a test can hand it directly. An earlier version of this fixture
# gave the host a secondary IP alias at the real `Cluster.controller_ip`
# (10.100.0.2) so that address really would resolve to something this
# process could bind. Found live, on this shared host: a *different*
# concurrent test session booting a real controller VM claims that exact
# address for real (`Cluster.controller_ip` is a fixed offset, the same for
# every session), which wins the ARP race unpredictably and made the
# sandbox's connection to the alias fail outright mid-run. `host_ip`
# (10.100.0.1, the bridge's own primary address — the same one
# `test_vm_integration.py`'s `git_proxy_target` binds to) is not something
# any VM is ever assigned, on this or any other concurrent session, so it
# has no equivalent collision. `_SingleSandboxCluster.controller_ip` below
# is what redirects `_remote_url()` there — `Orchestrator`'s own code is
# still never touched, only what its `cluster` argument reports.
_PROXY_HOST = str(Cluster(sandbox_count=1).host_ip)


@dataclass
class _SingleSandboxCluster:
    """Duck-types just the `Cluster` surface `Orchestrator` actually reads
    (`sandbox_names`, `address_of`, `controller_ip` — checked directly
    against `core.py`), delegating to the real `Cluster(sandbox_count=2)`
    `live_sandbox` was booted from for correct sandbox address arithmetic,
    but exposing only `live_sandbox.name` as an assignable sandbox, and
    `_PROXY_HOST` (the host's own bridge address, not the real
    `Cluster.controller_ip`) as where the controller is reached — see that
    constant's own comment for why.

    The sandbox-name narrowing is needed because `AutomationState.
    free_sandbox` just walks `cluster.sandbox_names` in list order and
    returns the first name with no assignment — with the real two-sandbox
    `Cluster`, that is always `"sandbox-0"` first, which nothing in this
    suite ever boots (see `live_sandbox`'s docstring for why `sandbox-1` is
    the one actually running). Not a `Cluster` subclass: `Cluster` is a
    frozen dataclass with a fixed name -> index -> address scheme with no
    constructor argument that can express either override — a small
    stand-in object is less than a real subclass would be for either one.
    """

    inner: Cluster
    sandbox_names: list[str]

    def address_of(self, name: str):
        return self.inner.address_of(name)

    @property
    def controller_ip(self) -> str:
        return _PROXY_HOST


@dataclass
class LiveTarget:
    bare_repo: Path
    seed_clone: Path
    github: RealGitHubMock
    orchestrator: Orchestrator
    audit: RecordingAuditLog


@pytest.fixture
def live_target(tmp_path: Path, live_sandbox: Sandbox, live_controller_stand_in: None):
    """The real bare repo + real `GitProxy` (adapted from
    `test_vm_integration.py`'s `git_proxy_target` — same rig, seeded for a
    single default branch rather than also a PR branch, and bound to
    `_PROXY_HOST` rather than that fixture's own literal constant) plus the
    real `Orchestrator`, wired to `RealGitHubMock` instead of a real GitHub
    credential.
    """
    repos_root = tmp_path / "repos"
    bare = repos_root / OWNER / f"{REPO}.git"
    bare.mkdir(parents=True)
    _run_ok(["git", "init", "--bare", "-q", str(bare)])
    # git http-backend denies git-receive-pack (push) by default even with
    # GIT_HTTP_EXPORT_ALL=1 — that env var only covers the read side. Found
    # live debugging this fixture: the fake agent's push came back a real
    # `403` from the proxy (forwarded straight from http-backend) until this
    # was set. `test_vm_integration.py`'s own git_proxy_target never needed
    # this because nothing there pushes — docs/roadmap.md item 2 flagged
    # push as "implemented identically to fetch but not yet exercised live,
    # since that needs a real writable allow-listed repo," which is exactly
    # what this fixture now is.
    _run_ok(["git", "-C", str(bare), "config", "http.receivepack", "true"])

    seed = tmp_path / "seed"
    _run_ok(["git", "clone", "-q", str(bare), str(seed)])
    (seed / "README.md").write_text("hello from the live issue-to-PR test\n")
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

    # Pre-minted through the *real* production write side
    # (`SandboxTokenStore.ensure_token`), so the proxy's read-only
    # `SandboxTokens` (loaded once at construction, below) and the
    # Orchestrator's own later `ensure_token()` call during dispatch agree
    # on the identical token — `ensure_token` is idempotent, so the
    # Orchestrator's call is just a no-op read of what's minted here.
    tokens_path = tmp_path / "sandbox-tokens.json"
    tokens_path.write_text("{}")
    token_store = SandboxTokenStore(tokens_path)
    token_store.ensure_token(live_sandbox.name)

    allowlist_path = tmp_path / "allowlist.json"
    allowlist_path.write_text(json.dumps([f"{OWNER}/{REPO}"]))

    proxy = GitProxy(
        allowlist=Allowlist(allowlist_path),
        credentials=CredentialSet(secrets_dir),
        tokens=SandboxTokens(tokens_path),
        forwarder=_PlainHttpForwarder(host=f"127.0.0.1:{upstream_port}"),
    )
    proxy_server = ThreadingHTTPServer(
        (_PROXY_HOST, GIT_PROXY_PORT), make_handler(proxy)
    )
    proxy_thread = threading.Thread(target=proxy_server.serve_forever, daemon=True)
    proxy_thread.start()

    github_mock = RealGitHubMock(owner=OWNER, repo=REPO, bare_repo=bare)
    github_client = GitHubClient(github_mock, token=None)

    config = AutomationConfig(
        owner=OWNER, repo=REPO, ssh_user=SSH_USER,
        ssh_key_path=live_sandbox.private_key,
    )
    audit = RecordingAuditLog()
    # `sandbox_runner` plays both the sandbox role and the stand-in
    # controller role (see `live_controller_stand_in`'s docstring) — passed
    # as both `base_runner` (used unwrapped as `controller_runner`,
    # docs/roadmap.md item 8's "Update") and via `ssh_runner_factory` (which
    # bypasses `_ssh_runner_for`'s own SshRunner wrapping — without it,
    # `base_runner` being an SshRunner already would get wrapped in a
    # second one, hopping through the same VM twice for no reason).
    sandbox_runner = SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=live_sandbox.cluster.address_of(live_sandbox.name),
        key_path=live_sandbox.private_key,
    )
    orchestrator = Orchestrator(
        cluster=_SingleSandboxCluster(
            inner=live_sandbox.cluster, sandbox_names=[live_sandbox.name]
        ),
        github=github_client, config=config,
        state=AutomationState(), base_runner=sandbox_runner,
        ssh_runner_factory=lambda _sandbox: sandbox_runner,
        token_store=token_store, audit=audit,
    )

    try:
        yield LiveTarget(
            bare_repo=bare, seed_clone=seed, github=github_mock,
            orchestrator=orchestrator, audit=audit,
        )
    finally:
        upstream.shutdown()
        proxy_server.shutdown()
        upstream_thread.join(timeout=5)
        proxy_thread.join(timeout=5)


def _branch_exists_on_disk(bare_repo: Path, branch: str) -> bool:
    return subprocess.run(
        ["git", "--git-dir", str(bare_repo), "show-ref", "--verify", "--quiet",
         f"refs/heads/{branch}"],
    ).returncode == 0


def test_live_issue_to_pr_pipeline(
    live_sandbox: Sandbox, live_target: LiveTarget,
    live_ssh_runner: SshRunner, live_git_installed: None,
):
    """Runs the real `Orchestrator.run_once` three times end to end against
    the real sandbox and real bare repo above: a happy path (dispatch,
    real clone, real push, sweep-detected success, PR opened), and the two
    failure shapes the roadmap item calls out — an agent that exits
    nonzero, and one that exits zero without ever pushing.
    """
    orchestrator = live_target.orchestrator
    github = live_target.github
    sandbox = live_sandbox.name

    # Each scenario below calls `orchestrator.run_once()` (the real,
    # undecomposed entry point) to dispatch, but `orchestrator._sweep()`
    # rather than `run_once()` again to observe the finish — both are the
    # literal, unmodified halves `run_once` itself calls in sequence
    # (`run_once = self._sweep(now); self._dispatch(now)`), so this is
    # still the real orchestration code path, just with the two halves
    # separated where the test needs to inspect state between them. Calling
    # `run_once()` a second time here would be equally "real" but would
    # also immediately re-dispatch: the sweep phase frees the sandbox and
    # (on a failure/no-push outcome) puts the trigger label straight back
    # on, and that same call's own dispatch phase would then pick the
    # freshly-relabelled issue right back up before this test ever gets to
    # look at the freed, requeued state — which is itself correct
    # production behaviour, just not what these particular assertions want
    # to isolate.

    # --- 1. happy path: dispatch -> real clone -> real commit and push ----
    _install_fake_claude(live_ssh_runner, _FAKE_CLAUDE_SUCCESS)
    github.issues[101] = FakeIssue(
        title="a distinctive live-test issue title",
        body="a distinctive live-test issue body",
        labels={orchestrator.config.trigger_label},
    )

    orchestrator.run_once(datetime.now(timezone.utc))  # sweep (no-op) + dispatch

    assert sandbox in orchestrator.state.assignments, "dispatch should have claimed the sandbox"
    assert orchestrator.state.assignments[sandbox].issue == 101
    assert github.issues[101].labels == {orchestrator.config.in_progress_label}, (
        "dispatch should have moved the trigger label to in-progress"
    )

    state = _wait_for_unit(live_ssh_runner, sandbox, timeout=45)
    assert state is UnitState.DONE_SUCCESS, f"fake agent unit ended in {state}"

    branch = branch_name(101)
    orchestrator._sweep(datetime.now(timezone.utc))  # sweep: verify branch, open PR

    assert sandbox not in orchestrator.state.assignments, "sandbox should be freed"
    assert github.issues[101].labels == set(), (
        "success should remove in-progress with no trigger re-add"
    )
    assert len(github.pull_requests) == 1
    pr = github.pull_requests[0]
    assert pr["head"] == branch
    assert pr["base"] == "main"
    assert pr["title"] == "grain: fix #101"

    # The real proof: a real commit, made by the fake agent inside the real
    # sandbox, really landed on the real branch in the real bare repo —
    # this is what branch_exists (and this assertion) is actually checking
    # for, not a mock's say-so.
    assert _branch_exists_on_disk(live_target.bare_repo, branch)
    log = subprocess.run(
        ["git", "--git-dir", str(live_target.bare_repo), "log", "-1", "--format=%s", branch],
        capture_output=True, text=True,
    )
    assert log.stdout.strip() == "grain: automated fix (fake agent)"

    outcomes = [e["outcome"] for e in live_target.audit.entries]
    assert "dispatched" in outcomes
    assert any(o.startswith("opened PR #9000") for o in outcomes)

    # --- 2. failure path: the fake agent exits nonzero, never touches the
    #        repo -> requeued, not treated as done -----------------------
    _install_fake_claude(live_ssh_runner, _FAKE_CLAUDE_FAIL_NONZERO)
    github.issues[102] = FakeIssue(
        title="a second live-test issue", body="body",
        labels={orchestrator.config.trigger_label},
    )

    orchestrator.run_once(datetime.now(timezone.utc))
    assert sandbox in orchestrator.state.assignments
    state = _wait_for_unit(live_ssh_runner, sandbox, timeout=45)
    assert state is UnitState.DONE_FAILED, f"fake agent unit ended in {state}"

    orchestrator._sweep(datetime.now(timezone.utc))  # sweep: requeue

    assert sandbox not in orchestrator.state.assignments
    assert github.issues[102].labels == {orchestrator.config.trigger_label}, (
        "a failed run should go back to the trigger label, not stay in-progress"
    )
    assert len(github.pull_requests) == 1, "no new PR should have been opened"
    assert not _branch_exists_on_disk(live_target.bare_repo, branch_name(102))
    reasons_102 = [e["outcome"] for e in live_target.audit.entries if e["issue"] == 102]
    assert "failed" in reasons_102

    # Issue 102 is genuinely still queued at this point — it went back on
    # the trigger label exactly like a real requeued issue would, and the
    # *next* dispatch pass really would pick it back up first (lower
    # number). That's correct production behaviour, already covered by the
    # `run_once()` vs `_sweep()` split explained above; here it would just
    # steal the one sandbox this suite has before scenario 3 gets to run.
    # Standing in for an operator moving on (or simply not relabelling it
    # again), clear its label directly on the mock rather than through the
    # orchestrator, so scenario 3 below is isolated.
    github.issues[102].labels.clear()

    # --- 3. edge case: the fake agent exits zero but never pushes ->
    #        "succeeded" is not trusted; requeued with the branch-missing
    #        reason, exactly what core.py's docstring says this guards
    #        against -----------------------------------------------------
    _install_fake_claude(live_ssh_runner, _FAKE_CLAUDE_SUCCEED_NO_PUSH)
    github.issues[103] = FakeIssue(
        title="a third live-test issue", body="body",
        labels={orchestrator.config.trigger_label},
    )

    orchestrator.run_once(datetime.now(timezone.utc))
    assert sandbox in orchestrator.state.assignments
    state = _wait_for_unit(live_ssh_runner, sandbox, timeout=45)
    assert state is UnitState.DONE_SUCCESS, f"fake agent unit ended in {state}"

    orchestrator._sweep(datetime.now(timezone.utc))  # sweep: branch missing -> requeue

    assert sandbox not in orchestrator.state.assignments
    assert github.issues[103].labels == {orchestrator.config.trigger_label}
    assert len(github.pull_requests) == 1, "still no new PR — the unit exiting zero is not proof"
    assert not _branch_exists_on_disk(live_target.bare_repo, branch_name(103))
    reasons_103 = [e["outcome"] for e in live_target.audit.entries if e["issue"] == 103]
    assert any("does not exist" in r for r in reasons_103), reasons_103
