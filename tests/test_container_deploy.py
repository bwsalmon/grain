"""Content checks for the container deployment (bwsalmon/agents#645).

grain stopped building a binary on the host it runs on: CI publishes one
image per commit (`.github/workflows/build-artifacts.yml`), and
`scripts/setup.sh` pulls it and runs it as `grain-daemon.service`.
Almost all of that is file content -- a Dockerfile, a workflow, a
generated systemd unit -- rather than control flow, so this holds to the
same bar `test_provision_controller.py` sets for v1's own provisioning
script: `bash -n`, plus assertions pinning the handful of values that
have to agree across files nothing else checks together.

What is *not* here, deliberately: running any of it. The image build
needs a container engine and a network, the unit needs systemd, and
`pkg/upgrade`'s own Go tests already cover the upgrade path's behaviour
against stubs. These are the cross-file agreements that no single
language's test suite can see.
"""

from __future__ import annotations

import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).parent.parent
SETUP = ROOT / "scripts" / "setup.sh"
DOCKERFILE = ROOT / "Dockerfile"
WORKFLOW = ROOT / ".github" / "workflows" / "build-artifacts.yml"
DEPLOY = ROOT / "terraform" / "gcp" / "files" / "deploy.sh"

# The one name three files have to agree on: setup.sh's default, the
# repository CI pushes to, and the daemon's own -upgrade-image.
IMAGE = "ghcr.io/bwsalmon/grain/grain"


def setup_text() -> str:
    return SETUP.read_text()


def job_body(workflow: str, job: str) -> str:
    """One job out of the workflow, up to wherever the next one starts."""
    jobs = ("binaries:", "sandbox-container:", "grain-container:")
    start = workflow.index("\n  " + job) + 1
    ends = [workflow.index("\n  " + j) for j in jobs
            if j != job and workflow.index("\n  " + j) > start]
    return workflow[start:min(ends)] if ends else workflow[start:]


def setup_code() -> str:
    """setup.sh with its comment lines dropped.

    This file is more comment than code, and much of that comment is
    about what the deploy *used* to do -- so "the script no longer runs
    X" has to be asked of the code alone, or every explanation of a
    removal reads as the removal not having happened.
    """
    return "\n".join(
        line for line in SETUP.read_text().splitlines()
        if not line.lstrip().startswith("#")
    )


def test_setup_is_syntactically_valid_bash():
    result = subprocess.run(["bash", "-n", str(SETUP)], capture_output=True, text=True)
    assert result.returncode == 0, result.stderr


def test_setup_pulls_an_image_and_builds_nothing():
    code = setup_code()
    assert "docker pull" in code
    # `make container-build` was the whole deploy once. Nothing on a
    # deployed host runs a toolchain any more, and `make` is no longer
    # even installed (see deploy.sh's install_prerequisites).
    assert "container-build" not in code
    assert "ensure_make" not in code


def test_setup_defaults_to_the_image_ci_publishes():
    text = setup_text()
    assert f'GRAIN_IMAGE="${{GRAIN_IMAGE:-{IMAGE}}}"' in text
    # The tag follows GRAIN_REF, with "/" replaced by "-" -- the same
    # substitution the workflow makes when it pushes and
    # pkg/upgrade.TagForBranch makes when the UI resolves a branch.
    assert 'GRAIN_IMAGE_TAG="${GRAIN_IMAGE_TAG:-${GRAIN_REF//\\//-}}"' in text


def test_the_unit_runs_the_image_as_the_unprivileged_account():
    """The container is what runs unprivileged; the docker client is not.

    `docker run --user` is what keeps the store, the secrets database and
    every sandbox working tree owned by $GRAIN_USER exactly as they were
    before any of this was containerized -- while the unit itself has to
    start as root to reach a root-owned docker socket. A `User=` line
    back in this unit would mean the opposite of what it used to.
    """
    text = setup_text()
    unit = text[text.index("cat > /etc/systemd/system/grain-daemon.service"):]
    unit = unit[:unit.index("\nUNIT\n")]
    assert "ExecStart=$DOCKER_BIN run --name grain-daemon" in unit
    assert "User=" not in unit
    # The image is read from an EnvironmentFile rather than written into
    # the unit, so an upgrade repoints the deployment by writing one line
    # (pkg/upgrade/image.go) with no unit to rewrite.
    assert "EnvironmentFile=${IMAGE_REF_FILE}" in unit
    assert "\\${GRAIN_IMAGE}" in unit

    args = text[text.index("docker_run_args() {"):]
    args = args[:args.index("\n}\n")]
    assert '--user "${uid}:${gid}"' in args
    assert "--network host" in args
    for mount in ("GRAIN_DATA_DIR", "GRAIN_SANDBOX_DIR", "GRAIN_SRC_DIR"):
        assert f"${{{mount}}}:${{{mount}}}" in args, f"{mount} is not mounted at its own path"


def test_the_docker_socket_is_only_mounted_when_something_needs_it():
    """It is the one thing here that grants the container root on the host.

    kontur (konturctl, and the docker-exec sandbox transport) and the
    Upgrade button's own `docker pull` are the two features that need it;
    a deployment running neither should not be handed it.
    """
    text = setup_text()
    args = text[text.index("docker_run_args() {"):]
    args = args[:args.index("\n}\n")]
    socket_line = [ln for ln in args.splitlines() if "docker.sock" in ln]
    assert socket_line, "the docker socket is never mounted"
    guard = args[:args.index(socket_line[0])].splitlines()[-1]
    assert 'GRAIN_KONTUR_ENABLE" = "1"' in guard and 'GRAIN_ENABLE_UI_UPGRADE" = "1"' in guard


def test_the_container_reaches_the_host_through_path_units_not_sudo():
    """bwsalmon/agents#645 replaced two NOPASSWD sudoers drop-ins.

    `systemctl` inside the container reaches no systemd that matters, so
    the daemon touches a file under $GRAIN_DATA_DIR/control and a .path
    unit out on the host turns it into the real command. PathModified,
    not PathExists: a leftover request file must not become a reboot the
    next time this host boots.
    """
    text = setup_text()
    assert "-reboot-cmd touch -reboot-cmd" in text
    assert "-upgrade-restart-cmd touch -upgrade-restart-cmd" in text
    for unit in ("grain-reboot.path", "grain-reboot.service",
                 "grain-restart.path", "grain-restart.service"):
        assert f"/etc/systemd/system/{unit}" in text, f"{unit} is not written"
    assert "PathModified=${CONTROL_DIR}/reboot" in text
    assert "PathExists=" not in setup_code()
    # No sudoers file is written any more, and a host upgraded across
    # this change has the two it used to write removed.
    assert "visudo" not in setup_code()
    assert "rm -f /etc/sudoers.d/grain-daemon-reboot /etc/sudoers.d/grain-daemon-upgrade" in text


def test_the_cli_wrappers_replace_a_symlink_rather_than_writing_through_it():
    """On a host deployed before this change both names are symlinks into
    $GRAIN_DATA_DIR/bin, and `cat >` follows one -- which would write the
    wrapper over the binary at the far end and leave /usr/local/bin/grain
    still pointing at it."""
    text = setup_text()
    assert "rm -f /usr/local/bin/grain /usr/local/bin/konturctl" in text
    assert text.index("rm -f /usr/local/bin/grain") < text.index("cat > /usr/local/bin/grain")


def test_the_data_directory_is_laid_out_before_the_cli_is_used():
    """The `grain` CLI is a `docker run` with the data dir bind-mounted.

    Docker creates a missing bind-mount source itself, as root and with a
    mode nothing asked for -- so anything invoking that CLI before
    setup_data_dir has run would hand the deployment a data directory
    docker invented rather than one this script laid out.
    """
    code = setup_code()
    main = code[code.index("main() {"):]
    assert main.index("setup_data_dir") < main.index("ensure_kontur_images")
    # ensure_kontur_images is the first step that runs the CLI (for
    # `grain sandbox-image`); reformat_store_if_schema_changed and
    # report_readiness follow it.
    assert main.index("setup_data_dir") < main.index("reformat_store_if_schema_changed")


def test_every_file_handed_to_the_containerised_cli_is_readable_by_it():
    """The CLI is a `docker run --user $GRAIN_USER`; this script is root.

    A file root writes with a 0600 umask is one that CLI cannot read --
    and `grain secrets set -value-file` failing takes the whole deploy
    down with it under `set -e`, after the image is pulled and before
    grain-daemon.service is ever written. That is not hypothetical: it
    is what a fresh VM with a minter key actually did, leaving a host
    with the sync service, the image, and no daemon.

    So anything staged for that CLI is created owned by $GRAIN_USER.
    `install` does it in one step, rather than a chown afterwards that
    leaves the file briefly readable by nobody but root.
    """
    code = setup_code()
    staged = code[code.index("seed_gcp_minter_key() {"):]
    staged = staged[:staged.index("\n}\n")]
    assert '-value-file "$staged"' in staged, "the staged copy is no longer what is handed over"
    assert 'install -m0600 -o "$GRAIN_USER" -g "$GRAIN_USER"' in staged, (
        "the minter key is staged without giving it to $GRAIN_USER")
    # The shape that caused it, so it cannot come back by hand.
    assert "umask 077 && cat" not in staged


def test_a_cli_that_cannot_answer_does_not_abort_the_deploy():
    """`set -e` plus a command substitution is a trap worth pinning.

    Each of these assigns the output of the containerised CLI to a
    variable and then *checks* it -- but a non-zero exit inside `$( )` in
    an assignment aborts the script before the check ever runs, turning
    "report this and carry on" into "no service on this host". Every one
    of them has to tolerate the failure it is written to describe.
    """
    code = setup_code()
    for call in ("grain sandbox-image", "grain schema-version"):
        line = [ln for ln in code.splitlines() if call in ln and '="$(' in ln]
        assert line, f"{call} is no longer assigned from a command substitution"
        assert all("|| true" in ln for ln in line), (
            f"{call} aborts the deploy instead of reporting: {line}")
    # agent_cli_in_image is assigned in two places; it absorbs the
    # failure itself so neither caller has to.
    helper = code[code.index("agent_cli_in_image() {"):]
    helper = helper[:helper.index("\n}\n")]
    assert "|| true" in helper


def test_the_upgrade_button_is_wired_to_the_image_path():
    text = setup_text()
    assert '-upgrade-image "$GRAIN_IMAGE"' in text
    assert '-upgrade-image-ref-file "$IMAGE_REF_FILE"' in text
    # The binary path's flag would ask the daemon to build on a host with
    # no toolchain at all.
    assert "-upgrade-install-path" not in setup_code()


def test_the_dockerfile_carries_every_binary_grain_shells_out_to():
    text = DOCKERFILE.read_text()
    for pkg in ("git", "openssh-client", "ca-certificates", "systemd"):
        assert pkg in text, f"{pkg} is not installed in the runtime image"
    assert "konturctl" in text
    # Both agent CLIs, not just one: the framework a run uses is a live
    # per-task choice, so an image with only one of them fails every run
    # that chooses the other. agy was the one nothing installed anywhere
    # until bwsalmon/agents#645 -- an operator's manual step on every
    # host, for the *default* framework.
    assert "claude.ai/install.sh" in text
    assert "antigravity.google/cli/install.sh" in text
    # CAP_NET_BIND_SERVICE reaches a non-root process in a container only
    # through a file capability -- --cap-add alone grants it nothing, so
    # the default -ui-addr (port 80) would fail to bind without this.
    assert "setcap cap_net_bind_service=+ep /usr/local/bin/grain" in text
    # The entrypoint is the binary, so `docker run <image> schema-version`
    # runs the CLI -- which is how setup.sh's own wrapper and
    # pkg/upgrade's image health check both invoke it.
    assert '"/usr/local/bin/grain"]' in text


def test_the_workflow_publishes_the_image_on_every_branch():
    """The UI's Upgrade button targets a branch by name, which in a
    container deployment means pulling that branch's tag -- so a branch
    with no image published for it is a branch nobody can upgrade onto.
    """
    text = WORKFLOW.read_text()
    assert "branches: ['**']" in text
    job = text[text.index("grain-container:"):]
    assert 'image="ghcr.io/${GITHUB_REPOSITORY,,}/grain"' in job
    assert 'branch_tag="${GITHUB_REF_NAME//\\//-}"' in job
    assert "sha-${GITHUB_SHA:0:7}" in job
    # :latest is main's alone, like the two jobs above it.
    latest = job.index('docker tag "${image}:${sha_tag}" "${image}:latest"')
    assert 'if [ "$GITHUB_REF" = "refs/heads/main" ]' in job[:latest]


def test_the_image_is_driven_before_it_is_published():
    """The e2e suite gates the push, and runs before the login.

    An image that does not come up should never become a tag a
    deployment might pull -- and the credential that could publish one is
    held for the shortest span that gets it pushed, so the step that runs
    a container built from the tree is not also a step holding
    packages:write.
    """
    text = WORKFLOW.read_text()
    job = text[text.index("grain-container:"):]
    e2e = job.index("tests/test_container_e2e.py")
    assert e2e < job.index("Log in to GHCR"), "the e2e runs while holding a credential"
    assert e2e < job.index("- name: Push"), "the e2e does not gate the push"
    assert "GRAIN_TEST_IMAGE" in job


def test_the_shared_names_stay_on_main():
    """A branch push must not move build-latest's assets or any `latest`.

    Those are single names every deployment resolves. The binaries job is
    main's outright; the two image jobs publish per-commit and per-branch
    tags on every branch (a branch with no image is a branch nobody can
    deploy or upgrade onto) and gate only the `latest` push.
    """
    text = WORKFLOW.read_text()
    binaries = text[text.index("binaries:"):]
    assert "if: github.ref == 'refs/heads/main'" in binaries[:binaries.index("steps:")]

    for job in ("sandbox-container:", "grain-container:"):
        body = job_body(text, job)
        latest = body.index(':latest"')
        guard = body.rindex('if [ "$GITHUB_REF" = "refs/heads/main" ]', 0, latest)
        assert guard < latest, f"{job} moves :latest without gating on main"


def test_the_sandbox_reference_is_stamped_into_the_grain_image():
    """A deployment is told nothing about its sandbox container.

    The grain image carries the reference of the sandbox built from its
    own commit, so `grain sandbox-image` answers it -- which is what
    scripts/setup.sh pulls and what an upgrade pulls alongside the new
    grain. It has to be the immutable sha- tag, not the branch tag: a
    rollback to an older grain must ask for its *own* older sandbox
    rather than whatever that branch points at now.
    """
    text = WORKFLOW.read_text()
    job = text[text.index("grain-container:"):]
    assert 'sandbox="ghcr.io/${GITHUB_REPOSITORY,,}/kontur-sandbox:sha-${GITHUB_SHA:0:7}"' in job
    assert 'SANDBOX_IMAGE="$sandbox"' in job
    # And the sandbox has to exist before the grain naming it is pushed.
    assert "needs: sandbox-container" in job[:job.index("steps:")]

    # The Makefile turns it into a linker stamp, and the Dockerfile
    # forwards the build arg into that.
    makefile = (ROOT / "Makefile").read_text()
    assert "-X main.defaultSandboxImage=$(SANDBOX_IMAGE)" in makefile
    assert "SANDBOX_IMAGE=${SANDBOX_IMAGE}" in DOCKERFILE.read_text()


def test_the_sandbox_container_is_pulled_and_never_built():
    """bwsalmon/agents#645: a deployment stopped building its sandbox.

    It used to run packer/kontur/build-oci-image.sh on every host, which
    is how a deployment could end up running grain from one commit and a
    sandbox from another. What is left building locally is the guest
    *disk*, which bakes in this deployment's own SSH key and so cannot be
    published generically -- see ensure_kontur_images' own comment.
    """
    code = setup_code()
    assert "ensure_kontur_oci_image" in code
    assert "build-oci-image.sh" not in code, "the sandbox container is still built here"
    assert "grain sandbox-image" in code, "nothing resolves the stamped-in default"
    # Named explicitly to konturctl rather than relying on a local retag
    # of its default image, which is what the local build used to do.
    assert "-kontur-create-arg -kontur-image" in code
    assert "localhost:5000/kontur:latest" not in code
    # The guest disk build stays.
    assert "build-guest.sh" in code


def test_terraform_deploy_no_longer_installs_a_toolchain():
    text = DEPLOY.read_text()
    prerequisites = text[text.index("install_prerequisites() {"):]
    prerequisites = prerequisites[:prerequisites.index("\n}\n")]
    assert "for cmd in git docker python3; do" in prerequisites
    assert not re.search(r"\bmake\b", prerequisites), "make is still installed on the host"
    # The image config reaches setup.sh through the same grain-config
    # metadata attribute everything else does.
    for var in ("GRAIN_IMAGE", "GRAIN_IMAGE_TAG", "GRAIN_IMAGE_PULL_TOKEN"):
        assert f"{var}=" in text, f"{var} is not passed to setup.sh"
