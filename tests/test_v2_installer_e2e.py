"""End-to-end tests that actually run `v2/scripts/setup.sh`.

`test_v2_container_e2e.py` drives the *image*; this drives the
*installer*. The distinction cost a deployment: a fresh VM came up with
the config-sync service running, the grain image pulled, and no
`grain-daemon.service` at all, because `seed_gcp_minter_key` staged a
credential as root and then handed it to a `grain` that runs as
$GRAIN_USER inside a container. Nothing in CI ran setup.sh, so nothing
could have caught it -- every test either drove the image (which was
fine) or read the script's text (which looked fine).

So this runs the script, as root, on the machine running the tests, and
then asks systemd whether the deployment it was supposed to produce is
actually there and serving. What it deliberately does *not* do is
sandbox the script: it creates a real system user, writes real units into
/etc/systemd/system, and starts a real service, because a fake of any of
those is a fake of the thing that broke.

That makes it destructive to the host it runs on, which is why the gate
below is as narrow as it is: a throwaway CI runner, never a developer's
laptop, and never without being asked explicitly.

  GRAIN_TEST_IMAGE      the image to deploy, e.g. `grain-e2e:test`.
  GRAIN_INSTALLER_E2E=1 explicit opt-in. Without it this module skips
                        even where everything else is available, because
                        "the tests took over /usr/local/bin/grain" is not
                        a surprise anyone should get from `pytest`.
  root                  via passwordless sudo; setup.sh refuses otherwise.
  systemd + docker      the two things the deployment is made of.

Kontur sandboxing stays off (GRAIN_KONTUR_ENABLE=0). It is the one part
of a deploy that needs nested virtualisation and a multi-minute
debootstrap, and the failures this file exists to catch are all in the
path every deployment takes.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

import pytest

ROOT = Path(__file__).parent.parent
SETUP = ROOT / "v2" / "scripts" / "setup.sh"
IMAGE = os.environ.get("GRAIN_TEST_IMAGE", "")


def _can_sudo() -> bool:
    if os.geteuid() == 0:
        return True
    if shutil.which("sudo") is None:
        return False
    return subprocess.run(
        ["sudo", "-n", "true"], capture_output=True, timeout=30
    ).returncode == 0


def _systemd_running() -> bool:
    if shutil.which("systemctl") is None:
        return False
    # 0 is "running", 1 covers "degraded"/"maintenance" -- both are a
    # systemd that accepts units, which is all this needs.
    return subprocess.run(
        ["systemctl", "is-system-running"], capture_output=True, timeout=30
    ).returncode in (0, 1)


def _docker_works() -> bool:
    if shutil.which("docker") is None:
        return False
    return subprocess.run(
        ["docker", "info"], capture_output=True, timeout=60
    ).returncode == 0


pytestmark = pytest.mark.skipif(
    not IMAGE
    or os.environ.get("GRAIN_INSTALLER_E2E") != "1"
    or not _can_sudo()
    or not _systemd_running()
    or not _docker_works(),
    reason="needs GRAIN_INSTALLER_E2E=1, GRAIN_TEST_IMAGE, root, systemd and docker",
)


def run(*argv: str, check: bool = True, timeout: int = 600, **kwargs) -> subprocess.CompletedProcess:
    result = subprocess.run(
        argv, capture_output=True, text=True, timeout=timeout, **kwargs
    )
    if check and result.returncode != 0:
        raise AssertionError(
            f"{' '.join(argv)} failed ({result.returncode}):\n"
            f"--- stdout ---\n{result.stdout}\n--- stderr ---\n{result.stderr}"
        )
    return result


def sudo(*argv: str, **kwargs) -> subprocess.CompletedProcess:
    prefix = () if os.geteuid() == 0 else ("sudo", "-n")
    return run(*prefix, *argv, **kwargs)


def free_port() -> int:
    import socket

    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for(predicate, what: str, timeout: float = 90.0, interval: float = 1.0):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except Exception as err:  # noqa: BLE001
            last = err
        time.sleep(interval)
    raise AssertionError(f"timed out after {timeout}s waiting for {what}: {last!r}")


@pytest.fixture(scope="module")
def deployment(tmp_path_factory):
    """One real `setup.sh` run, and everything it left behind.

    Module-scoped: the run is the expensive part and every test here
    asks a different question of the same result, the way an operator
    would look over one finished deploy rather than doing five.
    """
    tmp = tmp_path_factory.mktemp("installer")
    user = "grain-e2e"
    port = free_port()
    branch = "installer-e2e-" + uuid.uuid4().hex[:8]

    # setup.sh's own sync_repo updates $GRAIN_SRC_DIR from a remote at a
    # ref. Pointing that at this working tree, on a branch pinned to
    # whatever is checked out, is what makes the script under test the
    # one in this checkout rather than whatever main happens to be --
    # and still exercises sync_repo for real rather than stubbing it.
    run("git", "-C", str(ROOT), "branch", "--force", branch, "HEAD")
    src = tmp / "src"
    # Cloned here rather than left to sync_repo's own clone branch,
    # mirroring terraform/gcp-v2/files/deploy.sh: the script has to
    # already be on disk to be run at all, so a real deploy always takes
    # sync_repo's *update* path. That is the one worth testing.
    run("git", "clone", "--quiet", "--branch", branch, str(ROOT), str(src))

    data = tmp / "data"
    sandbox = tmp / "sandbox"
    # A stand-in for the GCP minter credential push-secrets.sh pushes.
    # Its *contents* never matter -- nothing here authenticates to GCP --
    # but its presence is what makes setup.sh stage it for the
    # containerised CLI, which is the step that broke.
    minter = tmp / "minter-key.json"
    minter.write_text(json.dumps({"type": "service_account", "project_id": "fake"}))

    repo, _, tag = IMAGE.rpartition(":")
    env = {
        "GRAIN_REPO_URL": str(ROOT),
        "GRAIN_REF": branch,
        "GRAIN_SRC_DIR": str(src),
        "GRAIN_DATA_DIR": str(data),
        "GRAIN_SANDBOX_DIR": str(sandbox),
        "GRAIN_USER": user,
        "GRAIN_IMAGE": repo,
        "GRAIN_IMAGE_TAG": tag,
        "GRAIN_UI_ADDR": f"127.0.0.1:{port}",
        # Off: the only part of a deploy needing nested virtualisation
        # and a debootstrap, and not where this file's failures live.
        "GRAIN_KONTUR_ENABLE": "0",
        # On: it is the default, and it is what mounts the docker socket
        # and wires the control units.
        "GRAIN_ENABLE_UI_UPGRADE": "1",
        "GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE": str(minter),
        # Deliberately empty: a project set would send
        # mint_gemini_operating_key at the real GCP API.
        "GRAIN_GCP_PROJECT": "",
        "GRAIN_GITHUB_TOKEN": "ghp_fake_token_for_the_installer_e2e",
        # Empty: format_target_repo_if_empty would otherwise `git
        # ls-remote` against GitHub with that fake token.
        "GRAIN_TARGET_REPO": "",
        "PATH": os.environ.get("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
    }

    try:
        result = sudo(
            "env", *(f"{k}={v}" for k, v in env.items()),
            "bash", str(src / "v2" / "scripts" / "setup.sh"),
            check=False, timeout=900,
        )
        yield {
            "result": result, "data": data, "sandbox": sandbox, "src": src,
            "user": user, "port": port, "image": IMAGE, "tmp": tmp,
        }
    finally:
        for unit in ("grain-daemon.service", "grain-reboot.path",
                     "grain-restart.path", "grain-reboot.service",
                     "grain-restart.service"):
            sudo("systemctl", "disable", "--now", unit, check=False, timeout=120)
            sudo("rm", "-f", f"/etc/systemd/system/{unit}", check=False, timeout=60)
        sudo("systemctl", "daemon-reload", check=False, timeout=120)
        sudo("docker", "rm", "--force", "grain-daemon", check=False, timeout=120)
        sudo("rm", "-rf", "/usr/local/lib/grain", "/usr/local/bin/grain",
             "/usr/local/bin/konturctl", "/etc/profile.d/grain.sh",
             check=False, timeout=60)
        sudo("userdel", user, check=False, timeout=60)
        run("git", "-C", str(ROOT), "branch", "--delete", "--force", branch,
            check=False, timeout=60)


def test_setup_runs_to_completion(deployment):
    """The whole point: it has to *finish*.

    Every failure this file exists for looked like a partial deploy --
    the script exiting mid-way under `set -e`, leaving a host with some
    of a deployment on it and no service. The exit code is the assertion;
    the output is what makes a failure readable without a re-run.
    """
    result = deployment["result"]
    assert result.returncode == 0, (
        f"setup.sh exited {result.returncode}\n"
        f"--- last 4000 chars of stdout ---\n{result.stdout[-4000:]}\n"
        f"--- last 2000 chars of stderr ---\n{result.stderr[-2000:]}"
    )


def test_the_daemon_service_exists_and_is_running(deployment):
    """`systemctl is-active grain-daemon.service` -- the missing piece.

    This is the observation that started it: a host with the config-sync
    service, the image, and no daemon unit. Asked of systemd rather than
    of the filesystem, because a unit file that exists and will not start
    is the same outcome for an operator.
    """
    state = sudo("systemctl", "is-active", "grain-daemon.service",
                 check=False, timeout=60).stdout.strip()
    if state != "active":
        journal = sudo("journalctl", "-u", "grain-daemon.service", "-n", "50",
                       "--no-pager", check=False, timeout=60).stdout
        raise AssertionError(f"grain-daemon.service is {state!r}:\n{journal}")


def test_the_deployment_serves_its_api(deployment):
    """A running unit is not the same as a working deployment.

    The container has to have come up with mounts it can actually use --
    the data directory it writes its store into, a HOME that exists --
    and to be reachable on the address the unit gave it.
    """
    base = f"http://127.0.0.1:{deployment['port']}"

    def config():
        with urllib.request.urlopen(base + "/api/config", timeout=5) as r:
            return json.loads(r.read())

    cfg = wait_for(config, f"the deployment to serve {base}/api/config")
    assert cfg["actor"], cfg


def test_the_minter_credential_reached_the_secrets_database(deployment):
    """The exact failure, asserted on its result rather than its shape.

    setup.sh stages this credential under the data directory and hands
    the path to `grain secrets set`, which runs as $GRAIN_USER inside a
    container. Staged as root at 0600 -- the obvious thing for a script
    running as root to do -- that CLI cannot read it, the command fails,
    and `set -e` ends the deploy before the unit is ever written.

    `grain secrets list` naming it is the proof the whole path worked:
    staged readably, read inside the container, and written into a store
    that is itself owned correctly.
    """
    listed = sudo("/usr/local/bin/grain", "secrets",
                  "-data-dir", str(deployment["data"]), "list",
                  check=False, timeout=180)
    assert "gcp-key-minter" in listed.stdout, (
        f"the minter credential never landed:\n{listed.stdout}\n{listed.stderr}")
    # And the staging copy does not outlive the command that needed it.
    assert not (deployment["data"] / "secrets" / ".minter-key.staged.json").exists()


def test_what_the_deployment_writes_is_owned_by_the_service_account(deployment):
    """Root ran the installer; the daemon is not root.

    Every path the container writes through has to come out owned by
    $GRAIN_USER, or the next thing to touch it -- the daemon itself, an
    operator's `grain` -- fails on permissions. This is the general form
    of the bug above.
    """
    import pwd

    uid = pwd.getpwnam(deployment["user"]).pw_uid
    data = deployment["data"]
    for path in (data, data / "secrets", data / "home", data / "image.env"):
        assert path.exists(), f"{path} was never created"
        assert path.stat().st_uid == uid, (
            f"{path} is owned by uid {path.stat().st_uid}, not {deployment['user']} ({uid})")


def test_the_unit_runs_the_image_this_deploy_was_given(deployment):
    """The image ref file is the indirection an upgrade rewrites.

    The unit reads it as an EnvironmentFile, so what it names *is* what
    the deployment runs -- and setup.sh writing it is what makes a
    re-run with a different GRAIN_IMAGE_TAG a rollback.
    """
    ref = (deployment["data"] / "image.env").read_text().strip()
    assert ref == f"GRAIN_IMAGE={deployment['image']}", ref

    unit = Path("/etc/systemd/system/grain-daemon.service").read_text()
    assert "docker" in unit and "run --name grain-daemon" in unit
    assert f"EnvironmentFile={deployment['data']}/image.env" in unit


def test_the_host_control_units_are_installed_and_watching(deployment):
    """The daemon's only way to reach the host it runs on.

    The reboot button and the Upgrade button's restart are both a touch
    of a file under the data directory; these are the units that turn
    that into a command. Enabled *and* active: a .path unit that exists
    but is not running is watching nothing.
    """
    for unit in ("grain-reboot.path", "grain-restart.path"):
        state = sudo("systemctl", "is-active", unit, check=False, timeout=60).stdout.strip()
        assert state == "active", f"{unit} is {state!r}, so nothing is watching for requests"
    control = deployment["data"] / "control"
    assert control.is_dir(), f"{control} was never created"


def test_the_cli_wrapper_talks_to_the_deployment_it_installed(deployment):
    """`grain list` on the host, against the daemon on the host.

    The wrapper bakes in GRAIN_SERVER from the unit's own -ui-addr, so
    this exercises the one thing an operator does first -- and would
    catch a wrapper pointed at cmd/grain's 8420 default instead.
    """
    listed = sudo("/usr/local/bin/grain", "-json", "list", check=False, timeout=180)
    assert listed.returncode == 0, f"{listed.stdout}\n{listed.stderr}"
    assert json.loads(listed.stdout or "[]") == []
