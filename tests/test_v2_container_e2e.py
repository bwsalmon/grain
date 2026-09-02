"""End-to-end tests against the real v2 deployment container.

`test_v2_container_deploy.py` next to this file checks that the files
describing the container deployment agree with each other. This one
actually runs it: it starts the published image the way
`v2/scripts/setup.sh`'s own unit starts it -- host networking, an
unprivileged `--user`, the data and sandbox directories bind-mounted at
the paths they have on the host -- and then drives the daemon inside
through its own REST API, its CLI, and the two host-facing mechanisms
(the reboot/restart control files and the image upgrade) that only exist
because it is in a container at all.

These are the claims a container makes that no unit test can check,
because each one is about the boundary rather than about any code on
either side of it: that the store survives the container it was written
from, that files come out owned by the host account rather than by root,
that a non-root process reaches port 80 through a file capability, that
a request written into a mounted directory reaches a systemd unit
outside, and that an upgrade pulls a real tag out of a real registry and
repoints a real deployment at it.

Gated, and skipped rather than failed when the gate is closed -- the
same shape the live suites in this directory already use for `/dev/kvm`
and a `br-grain` bridge:

  GRAIN_TEST_IMAGE  the image to test, e.g. `grain-e2e:test`. Unset
                    skips the module, so `python -m pytest` on a
                    developer's laptop stays a unit run.
  docker            must answer `docker info`.

`.github/workflows/build-artifacts.yml` builds the image and runs this
against it before publishing anything, so a commit whose container does
not come up cannot become a tag a deployment might pull.
"""

from __future__ import annotations

import contextlib
import json
import os
import re
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

import pytest

IMAGE = os.environ.get("GRAIN_TEST_IMAGE", "")


def _docker_works() -> bool:
    if shutil.which("docker") is None:
        return False
    return subprocess.run(
        ["docker", "info"], capture_output=True, timeout=60
    ).returncode == 0


pytestmark = pytest.mark.skipif(
    not IMAGE or not _docker_works(),
    reason="needs GRAIN_TEST_IMAGE and a working docker daemon",
)


# --- plumbing ----------------------------------------------------------


def docker(*args: str, check: bool = True, timeout: int = 300) -> str:
    """Run a docker command, returning its stdout."""
    result = subprocess.run(
        ["docker", *args], capture_output=True, text=True, timeout=timeout
    )
    if check and result.returncode != 0:
        raise AssertionError(
            f"docker {' '.join(args)} failed ({result.returncode}):\n"
            f"{result.stdout}\n{result.stderr}"
        )
    return result.stdout


def free_port() -> int:
    """A port nothing is listening on, for a daemon on the host network."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def api(base: str, method: str, path: str, body: dict | None = None,
        timeout: int = 30) -> tuple[int, object]:
    """One REST call against a running daemon: (status, decoded body)."""
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(
        base + path, data=data, method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            return response.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as err:
        raw = err.read()
        try:
            return err.code, json.loads(raw)
        except ValueError:
            return err.code, raw.decode(errors="replace")


def wait_for(predicate, what: str, timeout: float = 60.0, interval: float = 0.5):
    """Poll until predicate returns something truthy, or fail saying what."""
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except Exception as err:  # noqa: BLE001 -- a not-up-yet daemon raises
            last = err
        time.sleep(interval)
    raise AssertionError(f"timed out after {timeout}s waiting for {what}: {last!r}")


class Daemon:
    """A `grain daemon` running in a container, the way the unit runs it.

    The argument list mirrors `v2/scripts/setup.sh`'s own
    `docker_run_args` and `write_systemd_units` rather than being the
    smallest thing that would work: host networking, `--user` with this
    process's own uid:gid (the host account, exactly as the unit passes
    $GRAIN_USER's), and the data/sandbox directories mounted at the paths
    they have out here. Testing a container started some other way would
    be testing something no deployment runs.
    """

    def __init__(self, root: Path, *, run_args=(), flags=(), bind_port=None,
                 network=("--network", "host")):
        self.name = "grain-e2e-" + uuid.uuid4().hex[:10]
        self.root = root
        self.data = root / "data"
        self.sandbox = root / "sandbox"
        # The layout setup_data_dir lays out on a real host: a HOME that
        # exists (the unit exports one, and $GRAIN_USER has no home of its
        # own) and the secrets directory the daemon's own flags name.
        (self.data / "home").mkdir(parents=True, exist_ok=True)
        (self.data / "secrets").mkdir(parents=True, exist_ok=True)
        self.sandbox.mkdir(parents=True, exist_ok=True)
        # The port to *reach* it on. bind_port differs from it only for
        # the privileged-port case, where the container binds 80 inside
        # and docker publishes that on a port out here instead.
        self.port = free_port()
        self.base = f"http://127.0.0.1:{self.port}"
        self.run_args = list(run_args)
        self.flags = list(flags)
        self.ui_addr = (f"0.0.0.0:{bind_port}" if bind_port
                        else f"127.0.0.1:{self.port}")
        self.network = list(network)
        if bind_port:
            self.network += ["--publish", f"127.0.0.1:{self.port}:{bind_port}"]

    def start(self) -> "Daemon":
        self.run()
        self.await_ready()
        return self

    def run(self) -> "Daemon":
        """Start the container without waiting for it to serve."""
        docker(
            "run", "--detach", "--name", self.name,
            *self.network,
            "--user", f"{os.getuid()}:{os.getgid()}",
            "--env", f"HOME={self.data}/home",
            "--volume", f"{self.data}:{self.data}",
            "--volume", f"{self.sandbox}:{self.sandbox}",
            *self.run_args,
            IMAGE,
            "daemon",
            "-data-dir", str(self.data),
            "-sandbox-dir", str(self.sandbox),
            "-gemini-api-key-file", str(self.data / "secrets" / "gemini-api-key"),
            "-ui-addr", self.ui_addr,
            # An hour, so the reconciler never wakes during a test. These
            # tests are about the container boundary, not about dispatch:
            # a task reaching a real agent run would need a credential
            # and a repo neither of which exists here.
            "-poll-interval", "1h",
            *self.flags,
        )
        return self

    def await_ready(self, timeout: float = 60.0):
        """Block until the daemon answers, or say why it never will.

        A container that exited is reported immediately, with its logs,
        rather than polled for the full timeout: a daemon that refuses to
        start says so in one line, and waiting a minute to print it turns
        every such failure into a slow one.
        """
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not self.running():
                raise AssertionError(
                    f"{self.name} exited before serving {self.base}:\n{self.logs()}")
            try:
                status, body = api(self.base, "GET", "/api/config", timeout=5)
                if status == 200:
                    return body
            except OSError:
                pass  # not listening yet
            time.sleep(0.25)
        raise AssertionError(
            f"{self.name} never served {self.base}:\n{self.logs()}")

    def running(self) -> bool:
        state = docker("inspect", "-f", "{{.State.Running}}", self.name,
                       check=False).strip()
        return state == "true"

    def logs(self) -> str:
        return docker("logs", self.name, check=False, timeout=60)

    def api(self, method: str, path: str, body: dict | None = None):
        return api(self.base, method, path, body)

    def stop(self):
        docker("rm", "--force", self.name, check=False, timeout=120)


@contextlib.contextmanager
def daemon(root: Path, **kwargs):
    d = Daemon(root, **kwargs)
    try:
        d.start()
        yield d
    finally:
        d.stop()


def create_task(d: Daemon, title: str) -> dict:
    """File a task through the API and return it.

    Left unapproved (the default), so it sits in `proposed` and no
    reconciler cycle ever tries to dispatch it.
    """
    status, task = d.api("POST", "/api/tasks", {
        "title": title, "description": "filed by the container e2e suite",
    })
    assert status == 201, f"creating a task: {status} {task}"
    return task


# --- the image itself --------------------------------------------------


def test_the_image_runs_the_cli_and_reports_a_schema_version():
    """`docker run <image> schema-version` is not an arbitrary smoke test.

    It is the exact command `pkg/upgrade`'s image health check runs
    against a freshly pulled image before pointing a deployment at it
    (`healthCheckImage`), and the one `setup.sh`'s
    reformat_store_if_schema_changed asks through its own wrapper. If the
    entrypoint, the binary or its libc coupling were wrong, this is where
    every one of those would fail.
    """
    out = docker("run", "--rm", IMAGE, "schema-version", timeout=120).strip()
    assert re.fullmatch(r"\d+", out), f"schema-version printed {out!r}"


def test_the_image_carries_every_binary_the_daemon_shells_out_to():
    """v2/Dockerfile's header lists these and says why each is there.

    A missing one does not fail a build or a startup -- it fails the
    first dispatch, the first kontur VM, or the first look at the Logs
    pane, which is exactly the class of failure putting them in an image
    was meant to end.
    """
    script = (
        "for b in git bash konturctl docker claude journalctl ssh curl; do "
        'command -v "$b" >/dev/null || { echo "MISSING $b"; exit 1; }; '
        "done; echo all-present"
    )
    out = docker("run", "--rm", "--entrypoint", "sh", IMAGE, "-c", script,
                 timeout=120)
    assert "all-present" in out, out


def test_the_image_runs_as_an_unprivileged_uid():
    """The unit always passes --user with the host's own grain account.

    An image that only worked as root would leave every file it wrote
    into the mounted data directory owned by root -- see
    test_the_store_comes_out_owned_by_the_host_account below for the
    consequence this is the precondition of.
    """
    out = docker("run", "--rm", "--user", "1234:1234", "--entrypoint", "id",
                 IMAGE, "-u", timeout=120).strip()
    assert out == "1234"


# --- the daemon, in the container --------------------------------------


def test_the_daemon_serves_its_api_from_the_container(tmp_path):
    with daemon(tmp_path) as d:
        status, config = d.api("GET", "/api/config")
        assert status == 200, config
        # Real content, not just a 200: the actor the daemon files work
        # as, and the capability list the UI builds its panes from.
        assert config["actor"]
        assert isinstance(config["capabilities"], list)


def test_tasks_created_through_the_api_come_back_out_of_the_store(tmp_path):
    with daemon(tmp_path) as d:
        task = create_task(d, "a task filed inside a container")
        assert task["state"] == "proposed", task

        status, listed = d.api("GET", "/api/tasks")
        assert status == 200
        assert task["id"] in [t["id"] for t in listed], listed

        status, fetched = d.api("GET", f"/api/tasks/{task['id']}")
        assert status == 200
        assert fetched["title"] == "a task filed inside a container"

        # A write that is not a create: the conversation half of a task,
        # which lands in a different table of the same store.
        status, _ = d.api("POST", f"/api/tasks/{task['id']}/comments",
                          {"body": "a comment from the e2e suite"})
        assert status in (200, 201), status
        status, fetched = d.api("GET", f"/api/tasks/{task['id']}")
        bodies = [c.get("body", "") for c in (fetched.get("comments") or [])]
        assert any("e2e suite" in b for b in bodies), fetched


def test_a_task_survives_the_container_that_created_it(tmp_path):
    """The whole reason the data directory is a bind mount.

    A container is disposable and this one is started with --rm; the
    store, the secrets database and the deployment's own configuration
    are not. Replacing the container -- which is what every deploy and
    every upgrade does -- must leave all of it exactly where it was.
    """
    with daemon(tmp_path) as first:
        task = create_task(first, "a task that outlives its container")
        first_id = first.name

    # A genuinely new container over the same directories, the way a
    # `systemctl restart grain-daemon.service` brings one up.
    with daemon(tmp_path) as second:
        assert second.name != first_id
        status, fetched = second.api("GET", f"/api/tasks/{task['id']}")
        assert status == 200, fetched
        assert fetched["title"] == "a task that outlives its container"


def test_the_store_comes_out_owned_by_the_host_account(tmp_path):
    """`docker run --user` is what keeps this true.

    Without it the daemon would run as root inside the container and
    every file it wrote into the mounted data directory would come out
    root-owned on the host -- unreadable by the account an operator's own
    `grain` CLI runs as, and a permission failure the moment anything
    outside the container touched it.
    """
    with daemon(tmp_path) as d:
        create_task(d, "a task whose store file we stat")
        store = d.data / "store"
        written = wait_for(
            lambda: [p for p in store.rglob("*") if p.is_file()],
            f"a store file under {store}",
        )
    for path in written:
        assert path.stat().st_uid == os.getuid(), f"{path} is not ours"


def test_the_cli_reaches_the_daemon_from_a_second_container(tmp_path):
    """/usr/local/bin/grain is a wrapper around this exact invocation.

    `install_cli_wrappers` writes a script that runs the deployment image
    on the host network with GRAIN_SERVER pointed at the daemon's own
    port -- so an operator's `grain list` and the daemon are the same
    build, talking over the loopback address they share.
    """
    with daemon(tmp_path) as d:
        create_task(d, "a task the CLI should list")
        out = docker(
            "run", "--rm", "--network", "host",
            "--user", f"{os.getuid()}:{os.getgid()}",
            "--env", f"GRAIN_SERVER={d.base}",
            IMAGE, "-json", "list", timeout=120,
        )
    tasks = json.loads(out)
    assert any(t["title"] == "a task the CLI should list" for t in tasks), out


# --- binding a privileged port -----------------------------------------


def test_a_privileged_port_binds_with_the_capability_and_not_without(tmp_path):
    """setup.sh's own default -ui-addr is 127.0.0.1:80.

    systemd's AmbientCapabilities used to grant that, and has no
    equivalent for a container: a non-root process gets no capability
    from --cap-add alone. v2/Dockerfile gives the grain binary the
    matching *file* capability instead, and this is the pair of runs that
    shows that is what does the work -- the same image, the same
    unprivileged uid, the same port, differing only in whether
    CAP_NET_BIND_SERVICE is in the container's bounding set for that file
    capability to be raised from.

    The failing half is asserted on what is observable -- it never
    serves -- rather than on a particular error, because the kernel has
    two ways to refuse and which one it picks is not the point: it may
    reject the bind (EACCES), or refuse the exec outright, since a
    binary carrying an effective file capability the bounding set does
    not allow is a "capability-dumb binary" the kernel declines to run at
    all rather than run under-privileged. Either way the deployment does
    not come up, which is the fact worth pinning.

    Bridged rather than host networking for both, so this needs no port
    80 on the machine running the tests -- the bind under test happens
    inside the container either way.
    """
    granted = Daemon(
        tmp_path / "granted", bind_port=80,
        network=("--network", "bridge"),
        run_args=["--cap-add", "NET_BIND_SERVICE"],
    )
    try:
        granted.start()
        status, _ = granted.api("GET", "/api/config")
        assert status == 200
    finally:
        granted.stop()

    dropped = Daemon(
        tmp_path / "dropped", bind_port=80,
        network=("--network", "bridge"),
        run_args=["--cap-drop", "ALL"],
    )
    # Not .start(): this one is expected never to serve, so what is
    # awaited is the container giving up rather than an API answering.
    dropped.run()
    try:
        logs = wait_for(
            lambda: (dropped.logs() or "<no output>") if not dropped.running() else None,
            "the capability-less container to give up on port 80",
        )
        served = None
        try:
            served, _ = api(dropped.base, "GET", "/api/config", timeout=5)
        except OSError:
            pass  # connection refused: nothing ever bound it
        assert served is None, f"it served {dropped.base} ({served}) anyway"
        # Printed, not asserted on: which refusal the kernel picked is
        # worth reading in a CI log, and is not what this pins down.
        print("refused port 80 without the capability, saying: " + logs.strip())
    finally:
        dropped.stop()


# --- reaching the host from inside the container ------------------------


def test_the_reboot_button_writes_the_control_file_on_the_host(tmp_path):
    """The container cannot reboot the machine, so it asks.

    `-reboot-cmd` (added for exactly this) points the UI's reboot-host
    button at a `touch` of a file under the mounted data directory;
    `write_control_units` installs the systemd .path unit out on the host
    that turns that into the real `systemctl reboot`. This is the half
    inside the container: pressing the button has to produce that file on
    the *host* filesystem, not just inside a container about to vanish.
    """
    control = tmp_path / "data" / "control"
    control.mkdir(parents=True, exist_ok=True)
    request = control / "reboot"
    with daemon(tmp_path, flags=[
        "-reboot-cmd", "touch", "-reboot-cmd", str(request),
    ]) as d:
        # The UI only offers the button when the daemon says it can.
        _, config = d.api("GET", "/api/config")
        assert config["rebootEnabled"] is True, config

        status, body = d.api("POST", "/api/host/reboot")
        assert status == 200, body
        wait_for(request.exists, f"{request} to appear on the host")


@pytest.mark.skipif(
    shutil.which("systemctl") is None
    or subprocess.run(["systemctl", "is-system-running"],
                      capture_output=True).returncode not in (0, 1),
    reason="needs a running systemd to install a .path unit into",
)
def test_a_path_unit_turns_that_control_file_into_a_command(tmp_path):
    """The other half, out on the host.

    `PathModified` (rather than `PathExists`) is what
    `write_control_units` watches these files with, and the reasoning
    depends on two behaviours worth pinning down rather than assuming:
    that `touch` on a file that does not exist yet triggers it, and that
    a service which *deletes* the request before acting re-arms for the
    next one instead of either looping or going deaf. A leftover request
    turning into a reboot on the next boot is the failure this shape
    exists to avoid, so it matters that it is this shape and not the
    other one.
    """
    control = tmp_path / "control"
    control.mkdir(parents=True)
    control.chmod(0o777)
    request = control / "request"
    marker = control / "acted"
    unit = "grain-e2e-control-" + uuid.uuid4().hex[:8]

    def systemd_write(name: str, body: str):
        path = Path("/tmp") / name
        path.write_text(body)
        subprocess.run(["sudo", "cp", str(path), f"/etc/systemd/system/{name}"],
                       check=True, timeout=60)

    try:
        systemd_write(f"{unit}.path", f"""[Unit]
Description=grain container e2e control channel

[Path]
PathModified={request}
Unit={unit}.service
""")
        systemd_write(f"{unit}.service", f"""[Unit]
Description=grain container e2e control channel action

[Service]
Type=oneshot
ExecStart=/bin/rm -f {request}
ExecStart=/bin/sh -c 'echo acted >> {marker}'
""")
        subprocess.run(["sudo", "systemctl", "daemon-reload"], check=True, timeout=60)
        subprocess.run(["sudo", "systemctl", "start", f"{unit}.path"],
                       check=True, timeout=60)

        # First request: the file does not exist yet, so this is a create.
        request.touch()
        wait_for(marker.exists, "the path unit to act on the first request")
        wait_for(lambda: not request.exists(), "the service to consume the request")

        # Second request, after the first was consumed -- the case that
        # matters for a deployment that reboots or restarts more than once.
        request.touch()
        wait_for(lambda: marker.read_text().count("acted") == 2,
                 "the path unit to act on a second request")
    finally:
        subprocess.run(["sudo", "systemctl", "stop", f"{unit}.path"],
                       capture_output=True, timeout=60)
        subprocess.run(
            ["sudo", "rm", "-f", f"/etc/systemd/system/{unit}.path",
             f"/etc/systemd/system/{unit}.service"], capture_output=True, timeout=60)
        subprocess.run(["sudo", "systemctl", "daemon-reload"],
                       capture_output=True, timeout=60)


# --- upgrading, against a real registry ---------------------------------


@pytest.fixture(scope="module")
def registry():
    """A throwaway OCI registry on localhost, for the upgrade tests.

    A real `docker pull` against a real registry is the point: the
    upgrade path's first step is exactly that, and stubbing it would test
    everything except the thing most likely to be wrong. `localhost` is
    also the one host docker will speak plain HTTP to without an
    insecure-registry entry, which is why the tag names below are
    localhost:<port> rather than anything prettier.
    """
    port = free_port()
    name = "grain-e2e-registry-" + uuid.uuid4().hex[:8]
    docker("run", "--detach", "--name", name,
           "--publish", f"127.0.0.1:{port}:5000", "registry:2", timeout=300)
    repo = f"localhost:{port}/grain"
    try:
        wait_for(
            lambda: urllib.request.urlopen(
                f"http://127.0.0.1:{port}/v2/", timeout=5).status == 200,
            "the local registry to answer",
        )
        # Two tags of the same image, standing in for two branches CI
        # published. Pushed once here rather than per test: the bytes are
        # identical, so the second tag costs only a manifest write.
        for tag in ("v-one", "v-two"):
            push(repo, tag)
        yield repo
    finally:
        docker("rm", "--force", name, check=False, timeout=120)


def push(repo: str, tag: str) -> str:
    ref = f"{repo}:{tag}"
    docker("tag", IMAGE, ref)
    docker("push", ref, timeout=600)
    return ref


def upgrade_to(d: Daemon, branch: str) -> dict:
    """Start an upgrade and wait for it to leave `running`."""
    status, body = d.api("POST", "/api/upgrade", {"branch": branch})
    assert status in (200, 202), f"starting an upgrade: {status} {body}"

    def settled():
        _, current = d.api("GET", "/api/upgrade")
        return current if current["phase"] in ("ok", "failed") else None

    return wait_for(settled, f"the upgrade to {branch} to finish", timeout=300)


def docker_group_args() -> list[str]:
    """What lets the container's non-root uid use the mounted socket.

    setup.sh's own docker_run_args does the same lookup, for the same
    reason: /var/run/docker.sock is root:docker 0660, so a container
    running as an ordinary uid needs the group rather than the file.
    """
    gid = subprocess.run(["getent", "group", "docker"], capture_output=True,
                         text=True, timeout=30).stdout.strip()
    return ["--group-add", gid.split(":")[2]] if gid else []


def test_an_upgrade_pulls_a_branch_tag_and_repoints_the_deployment(tmp_path, registry):
    """The container deployment's whole upgrade path, end to end.

    Pull the tag CI publishes for a branch, prove the pulled image runs,
    and write the one `GRAIN_IMAGE=` line the systemd unit reads as an
    EnvironmentFile -- after which the restart command brings the
    deployment up on it. Everything here is real: a real registry, a real
    `docker pull` through the mounted socket, a real health check run of
    the pulled image, and the real ref file.
    """
    ref_file = tmp_path / "data" / "image.env"
    control = tmp_path / "data" / "control"
    restart = control / "restart"
    (tmp_path / "data").mkdir(parents=True, exist_ok=True)
    control.mkdir(parents=True, exist_ok=True)
    ref_file.write_text(f"GRAIN_IMAGE={registry}:v-one\n")

    with daemon(
        tmp_path,
        run_args=["--volume", "/var/run/docker.sock:/var/run/docker.sock",
                  *docker_group_args()],
        flags=["-upgrade-image", registry,
               "-upgrade-image-ref-file", str(ref_file),
               "-upgrade-restart-cmd", "touch",
               "-upgrade-restart-cmd", str(restart)],
    ) as d:
        _, status = d.api("GET", "/api/upgrade")
        assert status["enabled"] is True, status

        final = upgrade_to(d, "v-two")
        assert final["phase"] == "ok", f"{final}\n{d.logs()}"
        assert f"{registry}:v-two" in final["detail"], final

        # The ref file is the deployment: the unit interpolates it into
        # its own ExecStart, so this line *is* what comes up next.
        assert ref_file.read_text().strip() == f"GRAIN_IMAGE={registry}:v-two"
        wait_for(restart.exists, "the restart request the upgrade ends with")


def test_a_branch_with_no_published_image_leaves_the_deployment_alone(tmp_path, registry):
    """A failed upgrade must be a no-op, not a broken deployment.

    The image path has no rollback and does not need one: the pull comes
    first, and the ref file -- the only thing that decides what the
    service runs -- is not touched until an image has been pulled *and*
    proved to run. A branch nobody published an image for is the ordinary
    way to reach that (a push whose build has not finished, or a typo),
    so it is worth knowing it leaves the file exactly as it was.
    """
    ref_file = tmp_path / "data" / "image.env"
    restart = tmp_path / "data" / "control" / "restart"
    (tmp_path / "data" / "control").mkdir(parents=True, exist_ok=True)
    ref_file.write_text(f"GRAIN_IMAGE={registry}:v-one\n")

    with daemon(
        tmp_path,
        run_args=["--volume", "/var/run/docker.sock:/var/run/docker.sock",
                  *docker_group_args()],
        flags=["-upgrade-image", registry,
               "-upgrade-image-ref-file", str(ref_file),
               "-upgrade-restart-cmd", "touch",
               "-upgrade-restart-cmd", str(restart)],
    ) as d:
        final = upgrade_to(d, "no-such-branch")
        assert final["phase"] == "failed", final
        assert "pull" in final["detail"], final

    assert ref_file.read_text().strip() == f"GRAIN_IMAGE={registry}:v-one"
    assert not restart.exists(), "a failed upgrade still asked for a restart"
