// Package deploy holds the cross-file content checks for the container
// deployment (bwsalmon/agents#645).
//
// grain stopped building a binary on the host it runs on: CI publishes
// one image per commit (.github/workflows/build-artifacts.yml), and
// scripts/setup.sh pulls it and runs it as grain-daemon.service. Almost
// all of that is file content -- a Dockerfile, a workflow, a generated
// systemd unit -- rather than control flow, so this holds to the bar
// v1's own provisioning tests set: `bash -n`, plus assertions pinning
// the handful of values that have to agree across files nothing else
// checks together.
//
// What is *not* here, deliberately: running any of it. The image build
// needs a container engine and a network, the unit needs systemd, and
// pkg/upgrade's own tests already cover the upgrade path's behaviour
// against stubs. These are the cross-file agreements no single package's
// tests can see, which is also why this reads the tree from disk rather
// than importing anything.
package deploy

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The one name three files have to agree on: setup.sh's default, the
// repository CI pushes to, and the daemon's own -upgrade-image.
const image = "ghcr.io/bwsalmon/grain/grain"

func TestSetupIsSyntacticallyValidBash(t *testing.T) {
	out, err := exec.Command("bash", "-n", filepath.Join(repoRoot(t), "scripts", "setup.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n scripts/setup.sh: %v\n%s", err, out)
	}
}

func TestSetupPullsAnImageAndBuildsNothing(t *testing.T) {
	code := setupCode(t)
	contains(t, code, "docker pull")
	// `make container-build` was the whole deploy once. Nothing on a
	// deployed host runs a toolchain any more, and `make` is no longer
	// even installed (see deploy.sh's install_prerequisites).
	absent(t, code, "container-build")
	absent(t, code, "ensure_make")
}

func TestSetupDefaultsToTheImageCIPublishes(t *testing.T) {
	text := setupText(t)
	contains(t, text, `GRAIN_IMAGE="${GRAIN_IMAGE:-`+image+`}"`)
	// The tag follows GRAIN_REF, with "/" replaced by "-" -- the same
	// substitution the workflow makes when it pushes and
	// pkg/upgrade.TagForBranch makes when the UI resolves a branch.
	contains(t, text, `GRAIN_IMAGE_TAG="${GRAIN_IMAGE_TAG:-${GRAIN_REF//\//-}}"`)
}

// The container is what runs unprivileged; the docker client is not.
//
// `docker run --user` is what keeps the store, the secrets database and
// every sandbox working tree owned by $GRAIN_USER exactly as they were
// before any of this was containerized -- while the unit itself has to
// start as root to reach a root-owned docker socket. A `User=` line back
// in this unit would mean the opposite of what it used to.
func TestTheUnitRunsTheImageAsTheUnprivilegedAccount(t *testing.T) {
	text := setupText(t)
	unit := between(t, text, "cat > /etc/systemd/system/grain-daemon.service", "\nUNIT\n")
	contains(t, unit, "ExecStart=$DOCKER_BIN run --name grain-daemon")
	absent(t, unit, "User=")
	// The image is read from an EnvironmentFile rather than written into
	// the unit, so an upgrade repoints the deployment by writing one line
	// (pkg/upgrade/image.go) with no unit to rewrite.
	contains(t, unit, "EnvironmentFile=${IMAGE_REF_FILE}")
	contains(t, unit, `\${GRAIN_IMAGE}`)

	args := dockerRunArgs(t, text)
	contains(t, args, `--user "${uid}:${gid}"`)
	contains(t, args, "--network host")
	// The store and the sandbox working trees, at the paths they have out
	// here -- konturctl hands docker host paths, so a mount anywhere else
	// would not resolve. GRAIN_SRC_DIR is deliberately not in this list
	// any more: the only thing in the container that read it was the
	// self-debug capability, and the image carries its own copy of the
	// source now (TestTheImageCarriesTheSourceSelfDebugReads).
	for _, mount := range []string{"GRAIN_DATA_DIR", "GRAIN_SANDBOX_DIR"} {
		if !strings.Contains(args, "${"+mount+"}:${"+mount+"}") {
			t.Errorf("%s is not mounted at its own path", mount)
		}
	}
}

// The docker socket is the one thing here that grants the container root
// on the host. kontur (konturctl, and the docker-exec sandbox transport)
// and the Upgrade button's own `docker pull` are the two features that
// need it; a deployment running neither should not be handed it.
func TestTheDockerSocketIsOnlyMountedWhenSomethingNeedsIt(t *testing.T) {
	args := dockerRunArgs(t, setupText(t))

	var socketLine string
	for _, line := range strings.Split(args, "\n") {
		if strings.Contains(line, "docker.sock") {
			socketLine = line
			break
		}
	}
	if socketLine == "" {
		t.Fatal("the docker socket is never mounted")
	}

	guard := lastLine(args[:strings.Index(args, socketLine)])
	contains(t, guard, `GRAIN_KONTUR_ENABLE" = "1"`)
	contains(t, guard, `GRAIN_ENABLE_UI_UPGRADE" = "1"`)
}

// bwsalmon/agents#645 replaced two NOPASSWD sudoers drop-ins.
//
// `systemctl` inside the container reaches no systemd that matters, so
// the daemon touches a file under $GRAIN_DATA_DIR/control and a .path
// unit out on the host turns it into the real command. PathModified, not
// PathExists: a leftover request file must not become a reboot the next
// time this host boots.
func TestTheContainerReachesTheHostThroughPathUnitsNotSudo(t *testing.T) {
	text := setupText(t)
	code := setupCode(t)

	contains(t, text, "-reboot-cmd touch -reboot-cmd")
	contains(t, text, "-upgrade-restart-cmd touch -upgrade-restart-cmd")
	for _, unit := range []string{
		"grain-reboot.path", "grain-reboot.service",
		"grain-restart.path", "grain-restart.service",
	} {
		if !strings.Contains(text, "/etc/systemd/system/"+unit) {
			t.Errorf("%s is not written", unit)
		}
	}
	contains(t, text, "PathModified=${CONTROL_DIR}/reboot")
	absent(t, code, "PathExists=")
	// No sudoers file is written any more, and a host upgraded across
	// this change has the two it used to write removed.
	absent(t, code, "visudo")
	contains(t, text, "rm -f /etc/sudoers.d/grain-daemon-reboot /etc/sudoers.d/grain-daemon-upgrade")
}

// On a host deployed before this change both names are symlinks into
// $GRAIN_DATA_DIR/bin, and `cat >` follows one -- which would write the
// wrapper over the binary at the far end and leave /usr/local/bin/grain
// still pointing at it.
func TestTheCLIWrappersReplaceASymlinkRatherThanWritingThroughIt(t *testing.T) {
	text := setupText(t)
	contains(t, text, "rm -f /usr/local/bin/grain /usr/local/bin/konturctl")
	before(t, text, "rm -f /usr/local/bin/grain", "cat > /usr/local/bin/grain",
		"the wrapper is written before the symlink is removed")
}

// The `grain` CLI is a `docker run` with the data dir bind-mounted.
//
// Docker creates a missing bind-mount source itself, as root and with a
// mode nothing asked for -- so anything invoking that CLI before
// setup_data_dir has run would hand the deployment a data directory
// docker invented rather than one this script laid out.
func TestTheDataDirectoryIsLaidOutBeforeTheCLIIsUsed(t *testing.T) {
	main := from(t, setupCode(t), "main() {")
	// ensure_kontur_images is the first step that runs the CLI (for
	// `grain sandbox-image`); reformat_store_if_schema_changed and
	// report_readiness follow it.
	before(t, main, "setup_data_dir", "ensure_kontur_images",
		"the CLI runs before the data directory is laid out")
	before(t, main, "setup_data_dir", "reformat_store_if_schema_changed",
		"the store is reformatted before the data directory is laid out")
}

// The CLI is a `docker run --user $GRAIN_USER`; this script is root.
//
// A file root writes with a 0600 umask is one that CLI cannot read --
// and `grain secrets set -value-file` failing takes the whole deploy
// down with it under `set -e`, after the image is pulled and before
// grain-daemon.service is ever written. That is not hypothetical: it is
// what a fresh VM with a minter key actually did, leaving a host with the
// sync service, the image, and no daemon.
//
// So anything staged for that CLI is created owned by $GRAIN_USER.
// `install` does it in one step, rather than a chown afterwards that
// leaves the file briefly readable by nobody but root.
func TestEveryFileHandedToTheContainerisedCLIIsReadableByIt(t *testing.T) {
	staged := body(t, setupCode(t), "seed_gcp_minter_key() {")

	if !strings.Contains(staged, `-value-file "$staged"`) {
		t.Error("the staged copy is no longer what is handed over")
	}
	if !strings.Contains(staged, `install -m0600 -o "$GRAIN_USER" -g "$GRAIN_USER"`) {
		t.Error("the minter key is staged without giving it to $GRAIN_USER")
	}
	// The shape that caused it, so it cannot come back by hand.
	absent(t, staged, "umask 077 && cat")
}

// `set -e` plus a command substitution is a trap worth pinning.
//
// Each of these assigns the output of the containerised CLI to a variable
// and then *checks* it -- but a non-zero exit inside `$( )` in an
// assignment aborts the script before the check ever runs, turning
// "report this and carry on" into "no service on this host". Every one of
// them has to tolerate the failure it is written to describe.
func TestACLIThatCannotAnswerDoesNotAbortTheDeploy(t *testing.T) {
	code := setupCode(t)

	for _, call := range []string{"grain sandbox-image", "grain schema-version"} {
		var lines []string
		for _, line := range strings.Split(code, "\n") {
			if strings.Contains(line, call) && strings.Contains(line, `="$(`) {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			t.Errorf("%s is no longer assigned from a command substitution", call)
			continue
		}
		for _, line := range lines {
			if !strings.Contains(line, "|| true") {
				t.Errorf("%s aborts the deploy instead of reporting: %s", call, strings.TrimSpace(line))
			}
		}
	}

	// agent_cli_in_image is assigned in two places; it absorbs the
	// failure itself so neither caller has to.
	contains(t, body(t, code, "agent_cli_in_image() {"), "|| true")
}

func TestTheUpgradeButtonIsWiredToTheImagePath(t *testing.T) {
	text := setupText(t)
	contains(t, text, `-upgrade-image "$GRAIN_IMAGE"`)
	contains(t, text, `-upgrade-image-ref-file "$IMAGE_REF_FILE"`)
	// The binary path's flag would ask the daemon to build on a host with
	// no toolchain at all.
	absent(t, setupCode(t), "-upgrade-install-path")
}

// The source the self-debug capability reads travels inside the image.
//
// read_grain_source answers "what is the binary I am running made of", so
// the source and the binary have to be the same commit. They were not: the
// daemon read a host checkout bind-mounted in, and that checkout tracks a
// *branch* while the image is a fixed tag -- an upgrade repointed one
// without touching the other, so the agent read the old source while the
// new binary ran, and a rollback left it reading source newer than the
// code running. Both silent.
//
// Three files have to agree for the fix to hold, and nothing else looks at
// all three: the Dockerfile has to carry the tree, cmd/grain has to look
// where it was put, and setup.sh must not reintroduce the mount or the
// flag that used to win over it.
func TestTheImageCarriesTheSourceSelfDebugReads(t *testing.T) {
	// The runtime stage takes the staged copy, not /src -- which still has
	// .git, this build's own bin/, and the node_modules `make build` just
	// created in it.
	dockerfile := read(t, "Dockerfile")
	contains(t, dockerfile, "COPY --from=build /src-export /usr/local/share/grain/src")
	contains(t, dockerfile, "rm -rf /src-export/.git")

	// cmd/grain looks exactly there.
	contains(t, read(t, "cmd", "grain", "daemon.go"),
		`const defaultSourceDir = "/usr/local/share/grain/src"`)

	// ...and the deployment no longer hands it a host checkout to prefer
	// instead, nor mounts one for it to find.
	code := setupCode(t)
	absent(t, code, "-upgrade-src-dir")
	absent(t, code, "${GRAIN_SRC_DIR}:${GRAIN_SRC_DIR}:ro")
}

func TestTheDockerfileCarriesEveryBinaryGrainShellsOutTo(t *testing.T) {
	text := read(t, "Dockerfile")
	for _, pkg := range []string{"git", "openssh-client", "ca-certificates", "systemd"} {
		if !strings.Contains(text, pkg) {
			t.Errorf("%s is not installed in the runtime image", pkg)
		}
	}
	contains(t, text, "konturctl")
	// Both agent CLIs, not just one: the framework a run uses is a live
	// per-task choice, so an image with only one of them fails every run
	// that chooses the other. agy was the one nothing installed anywhere
	// until bwsalmon/agents#645 -- an operator's manual step on every
	// host, for the *default* framework.
	contains(t, text, "claude.ai/install.sh")
	contains(t, text, "antigravity.google/cli/install.sh")
	// CAP_NET_BIND_SERVICE reaches a non-root process in a container only
	// through a file capability -- --cap-add alone grants it nothing, so
	// the default -ui-addr (port 80) would fail to bind without this.
	contains(t, text, "setcap cap_net_bind_service=+ep /usr/local/bin/grain")
	// The entrypoint is the binary, so `docker run <image> schema-version`
	// runs the CLI -- which is how setup.sh's own wrapper and pkg/upgrade's
	// image health check both invoke it.
	contains(t, text, `"/usr/local/bin/grain"]`)
}

// v1's removal promoted the Go tree from v2/ to the root.
//
// Three files encode that layout independently, and nothing else checks
// them together: the Dockerfile builds the binary, the Makefile mounts and
// builds the same tree in a container, and Dockerfile.build is the
// toolchain image both land in. A stale `v2` segment in any one of them is
// invisible to `go build` and to every Go test -- it only surfaces as a
// failed image build, which is minutes into CI and needs a docker daemon
// to reproduce.
//
// That is not hypothetical: `make -C v2 build` survived the promotion in
// the Dockerfile and broke the grain-container job, while the whole Go
// suite stayed green.
func TestEveryBuildPathAgreesTheModuleRootIsTheRepositoryRoot(t *testing.T) {
	dockerfile := read(t, "Dockerfile")
	// Built at the context root, which is the module root.
	contains(t, dockerfile, "RUN make build")
	absent(t, dockerfile, "make -C v2")
	// ...and the binary comes back from the root's own bin/, not v2/bin/.
	contains(t, dockerfile, "COPY --from=build /src/bin/grain")

	makefile := read(t, "Makefile")
	// The containerised build mounts the checkout and works at its root.
	contains(t, makefile, `-v "$(CURDIR)":/src`)
	contains(t, makefile, "-w /src $(BUILDER_IMAGE)")
	// `make image`'s context is this directory, not its parent.
	contains(t, makefile, "-f Dockerfile .")
	absent(t, makefile, "-f Dockerfile ..")

	contains(t, read(t, "Dockerfile.build"), "WORKDIR /src\n")
}

// Why a package appears in this repo's list rather than only the
// account's.
//
// GHCR attaches a container package to a repository by
// org.opencontainers.image.source and nothing else. An image without it
// still builds, still pushes and still pulls -- it just lands under the
// account's packages, unlinked, with the repo's access settings not
// governing it. That is silent in every log, which is why it went
// unnoticed until someone looked for the packages and found none.
//
// Both images need it and neither had it: the grain Dockerfile carried no
// LABEL at all, and the sandbox is built from the vendored kontur
// Dockerfile, whose label names kontur's repository instead. The
// sandbox's is overridden at build time rather than by editing
// third_party/, so an operator building into their own registry keeps the
// vendored label.
func TestBothPublishedImagesClaimThisRepository(t *testing.T) {
	if !strings.Contains(read(t, "Dockerfile"),
		`org.opencontainers.image.source="https://github.com/bwsalmon/grain"`) {
		t.Error("the grain image does not claim this repository")
	}

	oci := read(t, "scripts", "kontur", "build-oci-image.sh")
	contains(t, oci, "KONTUR_OCI_SOURCE_REPO")
	contains(t, oci, "org.opencontainers.image.source=")
	// Unset must leave the vendored label alone -- the override is for the
	// one publisher hosting this in another repository's namespace.
	contains(t, oci, `if [ -n "${KONTUR_OCI_SOURCE_REPO:-}" ]; then`)

	// ...and CI is what sets it, next to the image name it pushes.
	if !strings.Contains(jobBody(t, workflow(t), "sandbox-container:"), "KONTUR_OCI_SOURCE_REPO=") {
		t.Error("CI builds the sandbox image without pointing it at this repository")
	}
}

// The UI's Upgrade button targets a branch by name, which in a container
// deployment means pulling that branch's tag -- so a branch with no image
// published for it is a branch nobody can upgrade onto.
func TestTheWorkflowPublishesTheImageOnEveryBranch(t *testing.T) {
	text := workflow(t)
	contains(t, text, "branches: ['**']")

	job := from(t, text, "grain-container:")
	contains(t, job, `image="ghcr.io/${GITHUB_REPOSITORY,,}/grain"`)
	contains(t, job, `branch_tag="${GITHUB_REF_NAME//\//-}"`)
	contains(t, job, "sha-${GITHUB_SHA:0:7}")
	// :latest is main's alone, like the two jobs above it.
	before(t, job, `if [ "$GITHUB_REF" = "refs/heads/main" ]`,
		`docker tag "${image}:${sha_tag}" "${image}:latest"`,
		"the :latest tag is not gated on main")
}

// The e2e suite gates the push, and runs before the login.
//
// An image that does not come up should never become a tag a deployment
// might pull -- and the credential that could publish one is held for the
// shortest span that gets it pushed, so the step that runs a container
// built from the tree is not also a step holding packages:write.
func TestTheImageIsDrivenBeforeItIsPublished(t *testing.T) {
	job := from(t, workflow(t), "grain-container:")
	before(t, job, "./tests/container/...", "Log in to GHCR",
		"the e2e runs while holding a credential")
	before(t, job, "./tests/container/...", "- name: Push",
		"the e2e does not gate the push")
	contains(t, job, "GRAIN_TEST_IMAGE")
}

// A branch push must not move any `latest`.
//
// `latest` is the one name every deployment resolves. Both image jobs
// publish per-commit and per-branch tags on every branch -- a branch with
// no image is a branch nobody can deploy or upgrade onto -- and gate only
// the `latest` push.
func TestTheSharedNamesStayOnMain(t *testing.T) {
	text := workflow(t)

	for _, job := range []string{"sandbox-container:", "grain-container:"} {
		body := jobBody(t, text, job)
		latest := strings.Index(body, `:latest"`)
		if latest < 0 {
			t.Errorf("%s publishes no :latest tag at all", job)
			continue
		}
		guard := strings.LastIndex(body[:latest], `if [ "$GITHUB_REF" = "refs/heads/main" ]`)
		if guard < 0 {
			t.Errorf("%s moves :latest without gating on main", job)
		}
	}
}

// No bare binaries alongside the images.
//
// This workflow used to publish `grain` and the three kontur binaries as
// assets on a rolling `build-latest` GitHub Release. Nothing ever consumed
// them: setup.sh pulls the image and installs wrappers that exec into it
// (konturctl included), and the kontur binaries a sandbox needs live
// inside the sandbox image. A second, unversioned answer to "what is the
// release" could only drift from the image everything actually runs, so
// the images are it.
//
// Asserted on the workflow rather than left to a comment, because the
// failure mode is additive -- someone reintroducing a convenient
// `gh release upload` would not break anything that runs, and nothing else
// would notice.
func TestTheImagesAreTheOnlyPublishedRelease(t *testing.T) {
	code := stripComments(workflow(t))

	for _, forbidden := range []string{"gh release", "build-latest", "softprops/action-gh-release"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("%q is back: the images are the release, not a binaries drop", forbidden)
		}
	}

	// contents:write is what publishing a release would need, and nothing
	// left here does. Keeping it absent is the enforcement, not the intent.
	absent(t, code, "contents: write")
	if !strings.Contains(code, "packages: write") {
		t.Error("the image jobs still need to push")
	}
}

// A deployment is told nothing about its sandbox container.
//
// The grain image carries the reference of the sandbox built from its own
// commit, so `grain sandbox-image` answers it -- which is what
// scripts/setup.sh pulls and what an upgrade pulls alongside the new
// grain. It has to be the immutable sha- tag, not the branch tag: a
// rollback to an older grain must ask for its *own* older sandbox rather
// than whatever that branch points at now.
func TestTheSandboxReferenceIsStampedIntoTheGrainImage(t *testing.T) {
	job := from(t, workflow(t), "grain-container:")
	contains(t, job, `sandbox="ghcr.io/${GITHUB_REPOSITORY,,}/kontur-sandbox:sha-${GITHUB_SHA:0:7}"`)
	contains(t, job, `SANDBOX_IMAGE="$sandbox"`)
	// And the sandbox has to exist before the grain naming it is pushed.
	contains(t, upTo(t, job, "steps:"), "needs: sandbox-container")

	// The Makefile turns it into a linker stamp, and the Dockerfile
	// forwards the build arg into that.
	contains(t, read(t, "Makefile"), "-X main.defaultSandboxImage=$(SANDBOX_IMAGE)")
	contains(t, read(t, "Dockerfile"), "SANDBOX_IMAGE=${SANDBOX_IMAGE}")
}

// bwsalmon/agents#645: a deployment stopped building its sandbox.
//
// It used to run scripts/kontur/build-oci-image.sh on every host, which is
// how a deployment could end up running grain from one commit and a
// sandbox from another. What is left building locally is the guest *disk*,
// which bakes in this deployment's own SSH key and so cannot be published
// generically -- see ensure_kontur_images' own comment.
func TestTheSandboxContainerIsPulledAndNeverBuilt(t *testing.T) {
	code := setupCode(t)
	contains(t, code, "ensure_kontur_oci_image")
	if strings.Contains(code, "build-oci-image.sh") {
		t.Error("the sandbox container is still built here")
	}
	if !strings.Contains(code, "grain sandbox-image") {
		t.Error("nothing resolves the stamped-in default")
	}
	// Named explicitly to konturctl rather than relying on a local retag
	// of its default image, which is what the local build used to do.
	contains(t, code, "-kontur-create-arg -kontur-image")
	absent(t, code, "localhost:5000/kontur:latest")
	// The guest disk build stays.
	contains(t, code, "build-guest.sh")
}

// setup.sh runs on a host with docker and systemd, and nothing else.
//
// It used to want git and jq as well, and installed both itself on any
// host that lacked them -- which made "what a host has to have before it
// can deploy" a list that grew quietly, one apt package at a time, every
// time a step here wanted a tool the shell does not have. The image this
// deployment runs already carries git, curl and grain's own source, so
// every such step borrows one out of it (setup.sh's own image_run)
// instead.
//
// Asserted against the code, per tool and per command position: a `git`
// inside a container command line is the whole point, and prose about
// the git proxy or a "-kontur-git-proxy-host" flag is neither. The
// container context is opened by an image_run and runs to the end of
// that call -- the first line that neither continues onto the next nor
// leaves a quote open.
func TestTheInstallerNeedsNothingOnTheHostButDockerAndSystemd(t *testing.T) {
	// Not an exhaustive list of things a base system lacks: these are
	// the ones this script has actually reached for. `install`,
	// `useradd` and `getent` are deliberately absent from it -- those
	// come with any distribution's base install, which docker and
	// systemd do not.
	hostTool := regexp.MustCompile(`(^|[|;&(]|\$\()\s*(git|jq|curl|wget|python3|make|gsutil|gcloud|ip)\b`)

	inImage := false
	openQuote := false
	for n, line := range strings.Split(setupCode(t), "\n") {
		if !inImage && strings.Contains(line, "image_run") {
			inImage, openQuote = true, false
		}
		if inImage {
			if strings.Count(line, "'")%2 == 1 {
				openQuote = !openQuote
			}
			if !openQuote && !strings.HasSuffix(line, `\`) {
				inImage = false
			}
			continue
		}
		if m := hostTool.FindStringSubmatch(line); m != nil {
			t.Errorf("setup.sh:%d runs %s on the host, outside any image_run: %s",
				n+1, m[2], strings.TrimSpace(line))
		}
	}
}

// Nothing rewrites setup.sh underneath the process running it.
//
// sync_repo pulled a new copy of the checkout over the file this process
// was reading, which is why reexec_if_updated had to exist: every step
// after the pull was otherwise the *old* script's version of that step.
// Neither is needed once the script keeps no checkout -- and neither may
// come back without the other, so both spellings are pinned.
func TestTheInstallerNeitherClonesNorReplacesItself(t *testing.T) {
	code := setupCode(t)
	for _, gone := range []string{"sync_repo", "reexec_if_updated", "GRAIN_SRC_DIR", "GRAIN_REPO_URL"} {
		absent(t, code, gone)
	}
	// The source the kontur guest build reads comes out of the image
	// instead, so it is the same source the binary was built from.
	contains(t, code, "unpack_image_source")
	contains(t, code, `docker cp "$cid:/usr/local/share/grain/src/."`)

	// Which makes keeping the copy of setup.sh on a deployed host
	// current the job of whatever put it there: on the GCP path, the
	// deploy script that runs it.
	deploy := stripComments(read(t, "terraform", "gcp", "files", "deploy.sh"))
	contains(t, deploy, `git -C "$SRC_DIR" reset --quiet --hard "origin/$GRAIN_REF"`)
}

func TestTerraformDeployNoLongerInstallsAToolchain(t *testing.T) {
	text := read(t, "terraform", "gcp", "files", "deploy.sh")
	prerequisites := body(t, text, "install_prerequisites() {")

	contains(t, prerequisites, "for cmd in git docker jq; do")
	if makeWord.MatchString(prerequisites) {
		t.Error("make is still installed on the host")
	}
	// The image config reaches setup.sh through the same grain-config
	// metadata attribute everything else does.
	for _, v := range []string{"GRAIN_IMAGE", "GRAIN_IMAGE_TAG", "GRAIN_IMAGE_PULL_TOKEN"} {
		if !strings.Contains(text, v+"=") {
			t.Errorf("%s is not passed to setup.sh", v)
		}
	}
}

// A deployed host runs no interpreter but its own shell.
//
// `cfg`, the GCS object encoder and the metadata token reader were three
// python3 one-liners, which is why every VM apt-installed an interpreter
// -- for three lines of JSON handling and nothing else. They are jq now.
// Asserted rather than left to the prerequisites list above, because the
// failure mode is additive: a fourth one-liner would work on any host that
// happens to have python3, and only fail on the minimal image this
// deployment actually gets.
func TestNoDeployScriptShellsOutToAnInterpreter(t *testing.T) {
	for _, script := range [][]string{
		{"scripts", "setup.sh"},
		{"terraform", "gcp", "files", "deploy.sh"},
		{"terraform", "gcp", "deploy", "read-outputs.sh"},
		{"terraform", "gcp", "deploy", "terraform-apply.sh"},
		{"terraform", "gcp", "deploy", "wait-for-host.sh"},
		{"terraform", "gcp", "deploy", "write-summary.sh"},
		{"terraform", "gcp", "deploy", "push-secrets.sh"},
	} {
		for _, line := range strings.Split(stripComments(read(t, script...)), "\n") {
			if strings.Contains(line, "python") {
				t.Errorf("%s still needs an interpreter: %s",
					filepath.Join(script...), strings.TrimSpace(line))
			}
		}
	}
}

// The one place a JSON boolean's spelling is load-bearing.
//
// `cfg` was a python3 one-liner, and `print` spells True with a capital T,
// so this comparison was written against Python's spelling rather than
// JSON's. jq emits "true", so the comparison had to move with it. Getting
// that wrong is silent in a way worth guarding: no error anywhere, just a
// deployment that came up on HostSandboxes with kontur configured, because
// the string never matched and GRAIN_KONTUR_ENABLE was always 0.
func TestTheKonturToggleComparesAgainstTheSpellingCfgEmits(t *testing.T) {
	code := stripComments(read(t, "terraform", "gcp", "files", "deploy.sh"))
	contains(t, code, `[ "$ENABLE_KONTUR_SANDBOXES" = "true" ]`)
	absent(t, code, `= "True"`)
}
