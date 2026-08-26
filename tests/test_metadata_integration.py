"""Live integration test for the metadata anycast path: a real sandbox VM,
a real nftables DNAT rule, and a real (stand-in) per-sandbox metadata
listener, wired together exactly the way `docs/design.md` step 7 and
`grain/adapter/net_linux.py`'s DNAT rule describe -- the specific gap
docs/roadmap.md item 4 calls out as unverified ("not exercised ... is
`MetadataLauncher.start()` actually running against that real user/binary
pair on a real controller").

Skipped unless the host has KVM/libvirt *and* is root: unlike
`test_vm_integration.py`, this suite also has to apply a real nftables
ruleset (the DNAT rule under test lives there), which -- same as
`test_net_integration.py` -- needs a real, reachable netfilter and root.

A dedicated bridge and subnet, not the `br-grain`/10.100.0.0/24 that
`test_vm_integration.py` expects an operator to have already brought up:
found live, writing this suite, that a sandboxed *test* environment can
itself be a grain sandbox whose own `eth0` already carries an address in
that exact default range (10.100.0.10, the same address `Cluster()`'s
default assigns to `sandbox-0`) -- the kernel then treats the address as
local and answers real SSH probes to it as *that* machine's own sshd, not
a nested VM, which reads exactly like a rejected key (clean handshake,
`Permission denied (publickey)`) despite the guest itself being configured
correctly (confirmed live by comparing a from-the-guest self-check against
the same connection attempt from the host: the guest's own `authorized_keys`
and `sshd -T` were fine, and a packet capture on `br-grain` during the
"failed" attempt showed zero packets -- the connection never left the host
at all). A distinct subnet sidesteps that collision entirely rather than
depending on the test host having no address of its own in 10.100.0.0/24.

No real `gce_metadata_server` binary exists in this environment (`which
gce_metadata_server` finds nothing) -- `MetadataConfig.binary` is exactly
the injection point for that, so `_stand_in_metadata_server` below is a
few lines of `http.server` standing in for it, launched through the real,
unmodified `MetadataLauncher.start()`/`systemd-run` mechanism the same way
`test_vm_integration.py`'s `git_proxy_target` stands in a real bare repo
behind a real `GitProxy` rather than a real GitHub. What this *doesn't*
exercise is a real token mint (needs a real GCP project and IAM bindings,
same caveat docs/roadmap.md item 4 already gives) -- what it does exercise
is everything docs/design.md calls "authenticated by network position":
the DNAT rule really rewrites a sandbox's request to 169.254.169.254 to
this specific sandbox's own controller-side port, and `MetadataLauncher`
really starts and stops a real systemd unit that answers there.
"""

from __future__ import annotations

import ipaddress
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

from grain.adapter.base import EgressMode, VmState
from grain.adapter.libvirt import LIBVIRT_URI, LibvirtAdapter
from grain.adapter.net_linux import LinuxNetwork, NAT_TABLE, TABLE
from grain.automation.ssh import SshRunner
from grain.inventory import Cluster
from grain.metadata.config import MetadataConfig
from grain.metadata.launcher import MetadataLauncher
from grain.automation.dispatch import UnitState
from grain.run import RealRunner

# A short suffix keeps this under Linux's 15-character interface-name limit
# while still standing out as this agent's own scratch infrastructure.
_AGENT_SUFFIX = "86cb5a"
BRIDGE = f"br-md-{_AGENT_SUFFIX}"
SUBNET = ipaddress.IPv4Network("10.201.0.0/24")
BASE_IMAGE = Path("/var/lib/grain/images/debian-12.qcow2")
SSH_USER = "debian"
BOOT_TIMEOUT_S = 180
STAND_IN_MARKER = "grain-metadata-stand-in"


def _run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    kwargs.setdefault("timeout", 20)
    try:
        return subprocess.run(argv, capture_output=True, text=True, **kwargs)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(
            argv, 124, exc.stdout or "",
            (exc.stderr or "") + "\n[timed out]",
        )
    except FileNotFoundError as exc:
        return subprocess.CompletedProcess(argv, 127, "", f"{exc}\n")


def _host_ready() -> bool:
    import os
    if os.geteuid() != 0:
        return False
    if not Path("/dev/kvm").exists():
        return False
    if _run(["virsh", "-c", LIBVIRT_URI, "list"]).returncode != 0:
        return False
    if _run(["nft", "list", "ruleset"]).returncode != 0:
        return False
    for tool in ("qemu-img", "cloud-localds", "ssh", "ssh-keygen"):
        if _run(["which", tool]).returncode != 0:
            return False
    return not BASE_IMAGE_MISSING


BASE_IMAGE_MISSING = not BASE_IMAGE.exists()

pytestmark = pytest.mark.skipif(
    not _host_ready(),
    reason="needs root, /dev/kvm, qemu:///system, and the debian-12 base image",
)


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
class Env:
    cluster: Cluster
    adapter: LibvirtAdapter
    network: LinuxNetwork
    private_key: Path
    sandbox: str


@pytest.fixture(scope="module")
def env(tmp_path_factory):
    """Real bridge, real nftables (this cluster's own subnet, so it cannot
    collide with whatever `test_vm_integration.py` already applied to
    `br-grain`), a real sandbox VM, and the *host* also carrying this
    cluster's `controller_ip` -- the same "this suite only boots one VM,
    so it plays both sandbox and controller" simplification
    `test_vm_integration.py`'s own `controller_stand_in` fixture already
    uses, extended one step further: a real deployment's controller has
    its own address on its own VM, so the DNAT rule below forwards to it
    across the bridge; here the "controller" is this host itself, so its
    own address has to be assigned to the same bridge the DNAT rule routes
    through, or the rewritten packet has nowhere to land.
    """
    config_dir = Path(f"/var/lib/grain/instances/metadata-test-{_AGENT_SUFFIX}")
    config_dir.mkdir(parents=True, exist_ok=True)
    key_dir = tmp_path_factory.mktemp("metadata-ssh")
    private_key = key_dir / "id_ed25519"
    result = _run(["ssh-keygen", "-t", "ed25519", "-f", str(private_key), "-N", "", "-q"])
    assert result.returncode == 0, result.stderr
    public_key = private_key.with_suffix(".pub")

    cluster = Cluster(sandbox_count=1, subnet=SUBNET, bridge=BRIDGE, image=str(BASE_IMAGE))
    runner = RealRunner()
    network = LinuxNetwork(cluster, runner)
    network.up(EgressMode.OPEN)
    # See the fixture's own docstring: the host stands in for the
    # controller VM too, so it needs the controller's address as well as
    # its own.
    runner.run(["ip", "addr", "replace",
                f"{cluster.controller_ip}/{cluster.subnet.prefixlen}", "dev", BRIDGE])

    adapter = LibvirtAdapter(cluster, runner, network, config_dir=config_dir,
                              controller_public_key_path=public_key)
    name = "sandbox-0"
    if adapter.state(name) is not VmState.ABSENT:
        adapter.destroy(name)
    adapter.create(cluster.spec_of(name))
    adapter.start(name)
    try:
        _wait_for_ssh(cluster.address_of(name), private_key, timeout=BOOT_TIMEOUT_S)
        yield Env(cluster=cluster, adapter=adapter, network=network,
                   private_key=private_key, sandbox=name)
    finally:
        adapter.destroy(name)
        runner.run(["nft", "delete", "table", "inet", TABLE], check=False)
        runner.run(["nft", "delete", "table", "ip", NAT_TABLE], check=False)
        runner.run(["ip", "link", "delete", BRIDGE], check=False)


@pytest.fixture
def sandbox_ssh(env: Env) -> SshRunner:
    return SshRunner(
        inner=RealRunner(), user=SSH_USER,
        address=env.cluster.address_of(env.sandbox),
        key_path=env.private_key,
    )


def _stand_in_metadata_server(marker: str) -> str:
    """A raw shebang script standing in for `gce_metadata_server` -- the
    real binary isn't installed in this environment (module docstring).
    Accepts and ignores every flag `metadata/config.py`'s `build_argv`
    passes except `-interface`/`-port`, which is all a bind-and-answer
    stand-in needs; the response body names the bound port, so the test
    can confirm *which* per-sandbox instance actually answered rather than
    just that something did.
    """
    return f"""#!/usr/bin/env python3
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

interface = "0.0.0.0"
port = 80
for arg in sys.argv[1:]:
    if arg.startswith("-interface="):
        interface = arg.split("=", 1)[1]
    elif arg.startswith("-port="):
        port = int(arg.split("=", 1)[1].lstrip(":"))

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"{marker} port={{port}} path={{self.path}}\\n".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass

HTTPServer((interface, port), Handler).serve_forever()
"""


@pytest.fixture
def metadata_launcher(env: Env):
    # Not a pytest tmp_path: those land under a 0700 `pytest-of-root`
    # ancestor when this suite runs as root (required for the nftables
    # calls above), which blocks the unprivileged uid `systemd-run --uid=`
    # drops to below from even traversing to the script -- the same
    # "confinement dir must be somewhere world-traversable" lesson
    # `test_vm_integration.py`'s own `booted_sandbox` fixture already
    # documents for the VM config dir, applying here to the stand-in
    # script instead.
    script_dir = Path(f"/var/lib/grain/instances/metadata-test-{_AGENT_SUFFIX}/standin")
    script_dir.mkdir(parents=True, exist_ok=True)
    script_dir.chmod(0o755)
    script_path = script_dir / "gce_metadata_server_standin.py"
    script_path.write_text(_stand_in_metadata_server(STAND_IN_MARKER))
    script_path.chmod(0o755)

    data_dir = Path(f"/var/lib/grain/instances/metadata-test-{_AGENT_SUFFIX}/data")
    config = MetadataConfig(
        service_account_email="stand-in@example.iam.gserviceaccount.com",
        project_id="stand-in-project",
        binary=str(script_path),
        metadata_user=SSH_USER,  # a real, already-existing unprivileged user
    )
    launcher = MetadataLauncher(cluster=env.cluster, config=config,
                                 runner=RealRunner(), data_dir=data_dir)
    launcher.stop(env.sandbox)  # in case a previous run left it behind
    launcher.start(env.sandbox)
    try:
        yield launcher
    finally:
        launcher.stop(env.sandbox)


def test_the_launcher_reports_the_real_systemd_unit_as_active(
    env: Env, metadata_launcher: MetadataLauncher,
):
    assert metadata_launcher.status(env.sandbox) is UnitState.ACTIVE


def test_a_sandboxs_request_to_the_anycast_address_reaches_its_own_instance(
    env: Env, sandbox_ssh: SshRunner, metadata_launcher: MetadataLauncher,
):
    result = sandbox_ssh.run(["curl", "-s", "-m", "5", "http://169.254.169.254/computeMetadata/v1/"])
    assert result.returncode == 0, result.stderr
    assert STAND_IN_MARKER in result.stdout
    assert f"port={env.cluster.metadata_port(env.sandbox)}" in result.stdout
    assert "path=/computeMetadata/v1/" in result.stdout


def test_a_sandbox_cannot_reach_the_controllers_other_services_directly(
    env: Env, sandbox_ssh: SshRunner,
):
    """Rule 4 in `net_linux.py`'s forward chain: a sandbox may reach the
    controller on exactly the git proxy port and its own metadata port --
    not arbitrary controller ports. 9999 stands in for "anything else";
    nothing listens there, so a *timeout* (the drop policy) is what proves
    the rule, as opposed to an immediate connection-refused a listening-but
    -unauthorized port would give.
    """
    result = sandbox_ssh.run(
        ["curl", "-s", "-m", "5", f"http://{env.cluster.controller_ip}:9999/"],
        check=False,
    )
    assert result.returncode != 0
