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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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

// The third unit on that same channel: the UI's root shell
// (grain/task-13, pkg/rootshell). Unlike the two above it runs a command
// nothing in this script names, which is the whole point of a debug
// hatch and also why the gate that installs it, the flag that turns it
// on in the daemon, and the responder's own protocol all have to agree
// across three files nothing else checks together.
func TestSetupInstallsTheRootShellResponder(t *testing.T) {
	text := setupText(t)
	code := setupCode(t)

	for _, unit := range []string{"grain-shell.path", "grain-shell.service"} {
		if !strings.Contains(text, "/etc/systemd/system/"+unit) {
			t.Errorf("%s is not written", unit)
		}
	}
	contains(t, text, "PathModified=${CONTROL_DIR}/shell")
	// The daemon end and the host end have to name the same directory,
	// and the flag is what makes the route exist at all: without it the
	// UI reports the pane unavailable no matter what is installed here.
	contains(t, text, `-root-shell-control-dir "$CONTROL_DIR"`)
	// Watching, not merely installed -- a .path unit that is not running
	// answers every command with pkg/rootshell's own timeout.
	contains(t, code, "systemctl enable --now grain-shell.path")

	// The completion protocol pkg/rootshell.Runner reads from the other
	// side: the status file is written last and renamed into place, so
	// that seeing one means the output beside it is finished.
	before(t, text, `>"$out" 2>&1`, `>"$status.tmp"`,
		"the responder must write the command's output before its exit status")
	before(t, text, `>"$status.tmp"`, `mv "$status.tmp" "$status"`,
		"the status file must be renamed into place rather than written in place")
	// And the request is consumed before it is run, so a command that
	// reboots this host is not run again on the way back up.
	before(t, text, `rm -f "$req"`, `bash -lc "$command"`,
		"the responder must consume the request before running it")

	// GRAIN_ROOT_SHELL=0 is a deployment that wants none of this, and a
	// re-run with it takes back what an earlier run granted rather than
	// leaving a unit installed that nothing passes the flag for.
	contains(t, text, `GRAIN_ROOT_SHELL="${GRAIN_ROOT_SHELL:-1}"`)
	contains(t, code, "systemctl disable --now grain-shell.path")
	contains(t, code, "rm -f /etc/systemd/system/grain-shell.path /etc/systemd/system/grain-shell.service")
}

// A deployed host does not suspend under the work it was given.
//
// The failure is silent and looks like nothing else: the daemon's clock
// stops mid-task, the UI stops answering, and whatever the agent was
// doing -- a clone, a push, a run of CI it was waiting on -- resumes
// minutes or hours later against timed-out connections, with nothing in
// any log saying the machine was asleep. Nothing else in this suite
// reads the deploy's power policy.
//
// Every route in has to be closed, which is why this asserts the whole
// set rather than "suspend is mentioned": masking sleep.target alone
// still leaves a host that hibernates, and masked targets alone leave a
// laptop failing a suspend every time its lid shuts.
func TestTheDeployedHostDoesNotSuspendUnderARunningTask(t *testing.T) {
	code := setupCode(t)

	for _, target := range []string{
		"sleep.target", "suspend.target", "hibernate.target", "hybrid-sleep.target",
	} {
		if !strings.Contains(code, target) {
			t.Errorf("%s is neither masked nor unmasked, so a host can still reach it", target)
		}
	}
	contains(t, code, `systemctl mask "${SLEEP_TARGETS[@]}"`)
	// logind's own events, so a closed lid or a sleep key is not an
	// endlessly retried, endlessly failing suspend.
	contains(t, code, "HandleLidSwitch=ignore")
	contains(t, code, "HandleSuspendKey=ignore")
	contains(t, code, "IdleAction=ignore")

	// Reversible from the same script that did it. A masked unit and a
	// drop-in outlive the deployment that wrote them, so an operator who
	// turns this off has to be able to turn it off by re-running the
	// installer rather than by reverse-engineering what it masked.
	suspend := body(t, code, "disable_host_suspend() {")
	contains(t, suspend, `systemctl unmask "${SLEEP_TARGETS[@]}"`)
	contains(t, suspend, `rm -f "$LOGIND_DROPIN"`)

	// And the daemon is not started onto a host that might still sleep.
	before(t, code, "disable_host_suspend\n", "enable_services\n",
		"the daemon is started before this host is told not to suspend")
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

// The CLI has to be told the two things about this deployment it cannot
// work out for itself: where the daemon listens, and where its files are.
//
// The second was missing (grain/task-303). `grain state` and `grain
// secrets` edit the data directory directly and take a -data-dir; the
// wrapper already bind-mounts that directory at its own path, but passed
// nothing naming it -- so the `grain state status` this script's own
// closing report prints failed, typed exactly as printed, with
// "-data-dir is required".
//
// Both halves are asserted: the wrapper's container, which is where a
// `grain` invocation on this host actually runs, and the profile script,
// which is what a shell (and anything reading $GRAIN_DATA_DIR out here,
// like pkg/capability/bootstrap's playbooks) gets.
func TestTheCLIIsToldWhereThisDeploymentsStateLives(t *testing.T) {
	text := setupText(t)

	wrapper := between(t, text, "cat > /usr/local/lib/grain/run-image.sh", "\nWRAPPER\n")
	contains(t, wrapper, "--env GRAIN_DATA_DIR=${GRAIN_DATA_DIR}")
	// Baked, not passed through from the caller the way GRAIN_SERVER is:
	// the only data directory inside that container is the one mounted
	// on the next line.
	contains(t, wrapper, "--volume ${GRAIN_DATA_DIR}:${GRAIN_DATA_DIR}")

	profile := body(t, text, "write_cli_profile() {")
	contains(t, profile, `export GRAIN_DATA_DIR="${GRAIN_DATA_DIR}"`)
	contains(t, profile, `export GRAIN_SERVER=`)
	// The data directory is exported even when GRAIN_UI_ADDR carries no
	// port to build a GRAIN_SERVER out of: the two are unrelated, and
	// that branch used to write no profile script at all.
	absent(t, profile, "not writing /etc/profile.d/grain.sh")
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

// The minter key is the one secret this deployment rotates on its own,
// and for a while the two halves of that disagreed.
//
// deploy/push-secrets.sh mints a fresh minter key on every run and then
// deletes every key on the account beyond the newest two, while
// seed_gcp_minter_key returned early whenever the host already had a
// gcp-key-minter entry -- so a host kept authenticating with the key
// from its first deploy until the third push-secrets.sh run deleted it,
// and every gcp-key mint after that failed with Google's `invalid_grant`
// on a deployment whose Capabilities tab still read Ready.
//
// So the seed converges: a key handed to this script is the key the
// daemon ends up holding. The early return that is left is the one for
// a deploy carrying no key at all, which must still leave an operator's
// own `grain secrets set` alone.
func TestTheMinterKeyIsReseededOnEveryRunSoRotationReachesTheDaemon(t *testing.T) {
	seed := body(t, setupCode(t), "seed_gcp_minter_key() {")

	if strings.Contains(seed, `grep -q '^gcp-key-minter:'`) {
		t.Error("seed_gcp_minter_key still skips a run whose key it has already seen, " +
			"so a rotated minter key never reaches the daemon")
	}
	if !strings.Contains(seed, `if [ -z "$GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE" ]; then`) {
		t.Error("seed_gcp_minter_key no longer leaves an existing credential alone when " +
			"this deploy carries no key of its own")
	}
	// pkg/secrets.Store.Resolve answers the bare "gcp-key-minter" name
	// only while the secret holds exactly one key, so writing under a
	// fixed name would break a secret first written from Settings (which
	// uses `value`) in a second way -- the resolve would fail as
	// ambiguous rather than hand back either key.
	if !strings.Contains(seed, "minter_secret_key") {
		t.Error("the key is written under a fixed name rather than the one the secret " +
			"already holds, which can leave gcp-key-minter with two keys and no way " +
			"to resolve the bare name")
	}
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
	// Every agent CLI, not just one: the framework a run uses is a live
	// per-task choice, so an image with only some of them fails every run
	// that chooses another. agy was the one nothing installed anywhere
	// until bwsalmon/agents#645 -- an operator's manual step on every
	// host, for the *default* framework.
	contains(t, text, "claude.ai/install.sh")
	contains(t, text, "antigravity.google/cli/install.sh")
	contains(t, text, "openai/codex/releases/download")
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
// LABEL at all, and the sandbox inherits kontur's, which names kontur's
// repository. The sandbox's is overridden at build time rather than by
// editing third_party/, so an operator building into their own registry
// keeps the inherited label.
func TestBothPublishedImagesClaimThisRepository(t *testing.T) {
	if !strings.Contains(read(t, "Dockerfile"),
		`org.opencontainers.image.source="https://github.com/bwsalmon/grain"`) {
		t.Error("the grain image does not claim this repository")
	}

	// The guest is committed from a kontur image, and `docker commit`
	// carries the base's config forward -- including this label, which on
	// that base names kontur's repository. GHCR uses it alone to decide
	// which repository a package belongs to, so without the override the
	// build and the push both succeed and the package lands in someone
	// else's namespace.
	guest := read(t, "scripts", "kontur", "build-guest.sh")
	contains(t, guest, "GUEST_SOURCE_REPO")
	contains(t, guest, "org.opencontainers.image.source=")
	// Unset must leave the inherited label alone -- the override is for the
	// one publisher hosting this in another repository's namespace.
	contains(t, guest, `if [ -n "${GUEST_SOURCE_REPO:-}" ]; then`)

	// ...and CI is what sets it, next to the image name it pushes.
	if !strings.Contains(jobBody(t, workflow(t), "sandbox-guest:"), "GUEST_SOURCE_REPO=") {
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

	for _, job := range []string{"sandbox-guest:", "grain-container:"} {
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

// The model credential lives in exactly one workflow, and that workflow
// is one no branch can trigger.
//
// live-agent.yml runs tests/e2e's live agy test nightly, which means CI
// now holds a GEMINI_API_KEY somewhere -- and where it is held is the
// whole safety argument. The other two workflows must stay free of it:
// tests.yml runs code from an unreviewed PR branch, and
// build-artifacts.yml runs on `branches: ['**']`, which in this
// repository means every branch grain's own agents push. Adding
// `${{ secrets.* }}` to either would hand a model credential to code no
// human has read, and nothing else in the tree would notice.
//
// The trigger is asserted for the same reason: a `push:` added to
// live-agent.yml would put the key back on every branch, since a
// repository's secrets are readable by any workflow on any branch push
// regardless of which file declares them. What keeps the key to the
// default branch is the `environment:` -- GitHub refuses the secret to a
// job on a ref the environment's branch policy does not name -- so the
// environment has to be there too.
func TestOnlyTheScheduledLiveJobHoldsAModelCredential(t *testing.T) {
	for name, text := range map[string]string{
		"tests.yml":           testsWorkflow(t),
		"build-artifacts.yml": workflow(t),
	} {
		if strings.Contains(stripComments(text), "secrets.") {
			t.Errorf("%s reads a secret: it is triggered by branches nobody has reviewed, and is only safe to trigger that way while it holds nothing worth stealing", name)
		}
	}

	live := stripComments(liveAgentWorkflow(t))
	contains(t, live, "schedule:")
	contains(t, live, "workflow_dispatch:")
	contains(t, live, "environment: live-agent")
	contains(t, live, "GRAIN_LIVE_AGENT_TEST")
	// The whole of tests/e2e's live set, by prefix rather than by name:
	// there are two of them now (the run that proves a live agy reaches
	// its sandbox, and the one that proves it honours grain's tool
	// denial), and a job naming one test is a job that silently stops
	// running the next one somebody adds.
	contains(t, live, "-run 'TestLive'")
	// -count=1: a cached "ok" from a run where the test skipped must
	// never stand in for a live one, the same reason the container and
	// real-VM suites pass it.
	contains(t, live, "-count=1")
	for _, forbidden := range []string{"push:", "pull_request:"} {
		if strings.Contains(upTo(t, live, "jobs:"), forbidden) {
			t.Errorf("live-agent.yml triggers on %s: the credential it holds would then be readable by any branch, which is what the environment exists to prevent", forbidden)
		}
	}
}

// The GCP credential lives in exactly one workflow too, and it is one no
// branch can trigger.
//
// gcp-smoke.yml stands a real VM and a real GKE cluster up nightly
// (scripts/gce-vm-smoke.sh, scripts/gke-cluster-smoke.sh), which means CI
// holds a minter for the deployment's own agent service account. Every
// part of live-agent.yml's argument applies, and the stakes are higher:
// this credential can create machines and bill for them.
//
// Three things are asserted because they are what make the arrangement
// safe rather than merely intended. The trigger: no `push` and no
// `pull_request`, since a repository secret is readable by any workflow
// on any branch push whichever file declares it. The `environment:`,
// which is the only part GitHub itself enforces, through that
// environment's branch policy. And the cleanup: a run that leaks is a run
// that bills, so the minted key is revoked and the leftovers deleted on
// every path out, `if: always()`.
func TestOnlyTheScheduledSmokeJobHoldsAGCPCredential(t *testing.T) {
	smoke := stripComments(gcpSmokeWorkflow(t))

	contains(t, smoke, "schedule:")
	contains(t, smoke, "workflow_dispatch:")
	contains(t, smoke, "environment: gcp-smoke")
	for _, forbidden := range []string{"push:", "pull_request:"} {
		if strings.Contains(upTo(t, smoke, "jobs:"), forbidden) {
			t.Errorf("gcp-smoke.yml triggers on %s: the minter it holds would then be readable by any branch, which is what the environment exists to prevent", forbidden)
		}
	}

	// Minted per run rather than parked in a secret -- which is also the
	// only way this job notices that minting itself has stopped working,
	// the failure that breaks every gcp-key grant at once.
	contains(t, smoke, "service-accounts keys create")

	sweep := between(t, smoke, "Delete anything left behind", "- name: Revoke")
	contains(t, sweep, "if: always()")
	contains(t, sweep, "instances delete")
	contains(t, sweep, "clusters delete")

	revoke := from(t, smoke, "Revoke the minted key")
	contains(t, revoke, "if: always()")
	contains(t, revoke, "service-accounts keys delete")
}

// The nightly job runs the scripts that exist, with flags they take.
//
// Nothing else would catch a rename or a dropped flag: this workflow
// fires from a schedule on the default branch, so the first report of a
// broken command line is a red run at night, days after the commit that
// caused it. Same reasoning as the `--- PASS` assertions the real-VM job
// makes about its own suite.
func TestTheSmokeWorkflowRunsTheScriptsAsTheyAre(t *testing.T) {
	smoke := stripComments(gcpSmokeWorkflow(t))
	gce := read(t, "scripts", "gce-vm-smoke.sh")
	gke := read(t, "scripts", "gke-cluster-smoke.sh")

	for _, script := range []string{"gce-vm-smoke.sh", "gke-cluster-smoke.sh"} {
		contains(t, smoke, "./scripts/"+script)
		executable(t, "scripts", script)
		out, err := exec.Command("bash", "-n", filepath.Join(repoRoot(t), "scripts", script)).CombinedOutput()
		if err != nil {
			t.Errorf("bash -n scripts/%s: %v\n%s", script, err, out)
		}
	}

	// The one shape decision the workflow exposes: `default` asks for the
	// production-shaped identity -- the project's own default compute
	// account, which needs roles/iam.serviceAccountUser on it -- rather
	// than the self-acting default both scripts fall back to. Each script
	// spells it differently because each attaches it to a different thing.
	contains(t, smoke, "--service-account=default")
	contains(t, gce, "--service-account)")
	contains(t, smoke, "--node-service-account=default")
	contains(t, gke, "--node-service-account)")

	// The zone reaches both scripts through the environment rather than
	// through a flag, which is the only reason one `env:` at the top of
	// the workflow governs both legs.
	contains(t, smoke, "ZONE: ${{ inputs.zone || 'us-central1-a' }}")
	for _, script := range []string{gce, gke} {
		contains(t, script, `ZONE="${ZONE:-`)
	}
}

// The GCE smoke script can tag the VM it creates, and names the tag this
// module's own networks ask for.
//
// --iap is the leg worth checking, since terraform/gcp gives the host no
// external IP at all -- and network.tf admits IAP's 35.235.240.0/20 to
// port 22 for a *target tag* only: agent_iap_ssh covers
// `<name_prefix>-agent-vm`, the ssh rule covers `<name_prefix>-host`, and
// an instance carrying neither matches no rule and is unreachable however
// good the credential. IAP reports that drop as
// [4003: 'failed to connect to backend'], which is also what it says
// about a VM that has not finished booting, so the error itself sends
// people to look at the key. Two things keep the script out of that hole,
// and neither is visible from the script alone: --tags has to reach the
// create, and the tag its own guidance names has to still be the one
// network.tf creates. A rename there would otherwise leave the script
// advising a tag no rule mentions -- the same dead end, reached by
// following the instructions.
func TestTheGCESmokeScriptCanTagTheVMItCreates(t *testing.T) {
	gce := read(t, "scripts", "gce-vm-smoke.sh")
	code := stripComments(gce)

	contains(t, code, "--tags)")
	contains(t, code, `CREATE_ARGS+=(--tags="$TAGS")`)
	contains(t, code, "[--tags T]")

	contains(t, read(t, "terraform", "gcp", "network.tf"), `agent_vm_tag = "${var.name_prefix}-agent-vm"`)
	if !strings.Contains(gce, "-agent-vm") {
		t.Error("gce-vm-smoke.sh does not name the `<name_prefix>-agent-vm` tag network.tf's agent_iap_ssh rule is scoped to; without it, --iap tells the operator to pass a tag but not which one")
	}

	// The failure path says which tags the network actually requires,
	// rather than leaving the operator with IAP's own opaque error.
	hint := body(t, gce, "iap_firewall_hint() {")
	contains(t, hint, "firewall-rules list")
	contains(t, hint, "targetTags")
	contains(t, hint, "35.235.240.0/20")
}

// The introspection job is dispatch-only, holds nothing, and keeps the
// token that can write a branch out of the job that runs agy.
//
// agy-surface.yml exists because a grain sandbox cannot answer questions
// about agy -- no network beyond the git proxy, no agy in the image -- and
// a runner can (docs/agy-surface.md, scripts/agy-surface.sh). Three things
// about its shape are load-bearing rather than incidental.
//
// The trigger. A schedule would open a pull request a day whether or not
// agy moved, and a `push` would re-answer a question about the *installed*
// agy on every commit; dispatching, meanwhile, takes the same write access
// that pushing this job's branch does, which is what makes `contents:
// write` here no wider than the person who clicked it already has.
//
// The credential, of which there is none. live-agent.yml's header explains
// at length why a repository secret is readable by any workflow on any
// branch push whichever file declares it; nothing here needs one, and the
// way to keep that true is to assert it.
//
// And the split. The job that runs a 200MB binary nobody in this
// repository built has `contents: read` and hands its output on as an
// artifact; the job that holds the write token never runs agy at all --
// the same "hold the credential for the shortest span that gets the work
// published" build-artifacts.yml applies to its GHCR login.
func TestTheAgySurfaceJobIsDispatchOnlyAndHoldsNoCredential(t *testing.T) {
	surface := stripComments(agySurfaceWorkflow(t))

	triggers := upTo(t, surface, "jobs:")
	contains(t, triggers, "workflow_dispatch:")
	for _, forbidden := range []string{"push:", "pull_request:", "schedule:"} {
		if strings.Contains(triggers, forbidden) {
			t.Errorf("agy-surface.yml triggers on %s: it publishes a branch with the repository's own token, and dispatch is what keeps that behind somebody who already has write access", forbidden)
		}
	}
	if strings.Contains(surface, "secrets.") || strings.Contains(surface, "environment:") {
		t.Error("agy-surface.yml reads a credential: it installs and runs a third-party binary, and holding nothing worth stealing is the whole of its safety argument")
	}

	// By their indented job headers, not by bare names: `publish` is also
	// the dispatch input that decides whether the branch is written at
	// all, and it appears above both jobs.
	capture := between(t, surface, "\n  capture:", "\n  publish:")
	contains(t, capture, "contents: read")
	contains(t, capture, "./scripts/agy-surface.sh")
	if strings.Contains(capture, "contents: write") {
		t.Error("the job that runs agy holds a write token; that token belongs to the publish job, which touches nothing but the artifact and git")
	}

	publish := from(t, surface, "\n  publish:")
	contains(t, publish, "contents: write")
	contains(t, publish, "needs: capture")
	if strings.Contains(publish, "agy") && strings.Contains(publish, "install.sh") {
		t.Error("the publish job installs agy: the point of the split is that the token is never in the job that runs it")
	}

	// The branch, not the ref it was dispatched from: a capture becomes a
	// pull request somebody merges, never a commit that appears on main
	// because a button was clicked.
	contains(t, publish, "HEAD:refs/heads/agy-surface")
	if strings.Contains(publish, "HEAD:refs/heads/main") || strings.Contains(publish, "push origin main") {
		t.Error("agy-surface.yml pushes to main")
	}
}

// The introspection job installs the agy an image would carry, and runs
// the script that is in the tree.
//
// Unpinned in three places on purpose -- the Dockerfile, the nightly live
// run and this one: what each is asking about is the agy a freshly built
// image would have, not one a workflow file froze months ago. That only
// holds while all three fetch the same installer, and a URL that moves in
// one of them is invisible to everything else here.
//
// The script gets the same treatment gcp-smoke.yml's two get, and for the
// same reason: this workflow fires only when somebody dispatches it, so a
// renamed script or a lost +x bit would first be reported by the person
// who dispatched it, at the moment they wanted an answer.
func TestTheAgySurfaceJobInstallsTheAgyAnImageWouldAndRunsTheScriptAsItIs(t *testing.T) {
	const installer = "https://antigravity.google/cli/install.sh"
	surface := stripComments(agySurfaceWorkflow(t))

	contains(t, read(t, "Dockerfile"), installer)
	contains(t, stripComments(liveAgentWorkflow(t)), installer)
	contains(t, surface, installer)
	// Found, not assumed: agy's install path is documented only by
	// Google, so a layout change on their side should move the symlink
	// rather than fail this job for a binary that is actually there.
	contains(t, surface, `-name agy -perm -u+x`)
	contains(t, surface, "agy --version")

	contains(t, surface, "./scripts/agy-surface.sh")
	executable(t, "scripts", "agy-surface.sh")
	out, err := exec.Command("bash", "-n", filepath.Join(repoRoot(t), "scripts", "agy-surface.sh")).CombinedOutput()
	if err != nil {
		t.Errorf("bash -n scripts/agy-surface.sh: %v\n%s", err, out)
	}

	// One path, agreed on by the script's invocation, the commit the
	// publish job makes, and the file in the tree that a sandboxed agent
	// reads instead of asking. The capture is worth committing only
	// because it lands somewhere findable.
	contains(t, surface, "docs/agy-surface.md")
	if _, err := os.Stat(filepath.Join(repoRoot(t), "docs", "agy-surface.md")); err != nil {
		t.Errorf("docs/agy-surface.md is missing: %v -- the workflow publishes over it, and README points at it", err)
	}
}

// A deployment is told nothing about its sandbox image.
//
// One image, not two: the guest a task runs and the container it runs in
// are the same artifact since the guest stopped being built per host, so
// there is a single reference to stamp and a single one to pull.
//
// The grain image carries the reference of the sandbox built from its own
// commit, so `grain sandbox-image` answers it -- which is what
// scripts/setup.sh pulls and what an upgrade pulls alongside the new
// grain. It has to be the immutable sha- tag, not the branch tag: a
// rollback to an older grain must ask for its *own* older sandbox rather
// than whatever that branch points at now.
func TestTheSandboxReferenceIsStampedIntoTheGrainImage(t *testing.T) {
	job := from(t, workflow(t), "grain-container:")
	contains(t, job, `sandbox="ghcr.io/${GITHUB_REPOSITORY,,}/guest:sha-${GITHUB_SHA:0:7}"`)
	contains(t, job, `SANDBOX_IMAGE="$sandbox"`)
	// And the sandbox has to exist before the grain naming it is pushed.
	contains(t, upTo(t, job, "steps:"), "needs: sandbox-guest")

	// The Makefile turns it into a linker stamp, and the Dockerfile
	// forwards the build arg into that.
	contains(t, read(t, "Makefile"), "-X main.defaultSandboxImage=$(SANDBOX_IMAGE)")
	contains(t, read(t, "Dockerfile"), "SANDBOX_IMAGE=${SANDBOX_IMAGE}")
}

// The other half of the same trick: the grain image is told what it is
// called, not only what sandbox goes with it.
//
// A deployment writes that reference into the CI step it installs in its
// own state repository, whose `grain state check` refuses a dump stamped
// with a schema it does not know -- so the check has to run the build the
// deployment runs, and nothing but the deployment knows which that is.
// The sha- tag again, never the branch tag: a deployment held at an older
// one has to name itself rather than main, which is the whole point.
func TestTheGrainImageIsStampedWithItsOwnReference(t *testing.T) {
	job := from(t, workflow(t), "grain-container:")
	contains(t, job, `GRAIN_IMAGE_REF="${image}:sha-${GITHUB_SHA:0:7}"`)

	contains(t, read(t, "Makefile"), "-X main.defaultGrainImage=$(GRAIN_IMAGE_REF)")
	contains(t, read(t, "Dockerfile"), "GRAIN_IMAGE_REF=${GRAIN_IMAGE_REF}")
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

	// Nothing is built here any more, guest included. The guest disk
	// used to be, because guest-setup.sh baked this deployment's own SSH
	// key into it; kontur generates that keypair per VM boot now, so the
	// guest is derived from a published image at build time and pulled
	// here like anything else. A host that starts building one again is
	// a host paying a debootstrap on every deploy generation.
	absent(t, code, "build-guest.sh")
	absent(t, code, "ensure_kontur_guest_build")
	absent(t, code, "ensure_kontur_guest_fetch")

	// The guest travels inside the sandbox image, so konturctl is given
	// no disk, kernel or initramfs, and no host directories to resolve
	// them against. It does need a writable root: konturctl's own
	// default is read-only, which a dispatched task cannot use.
	absent(t, code, "-images-hostpath")
	absent(t, code, "-disk-hostpath")
	contains(t, code, "-disk-mode=overlay")
}

// Every sandbox guest gets a nameserver it can actually reach, and a
// deployment can name a different one.
//
// The guest used to resolve through whatever /etc/resolv.conf the machine
// that built the guest image happened to have, which the kontur base
// inherited from debootstrap -- routinely an address that exists only in
// that host's own network namespace and is unroutable from a VM on a tap.
// Sandboxes came up with completely open IP egress and no working DNS at
// all, and nothing noticed for a long time because the addresses a run
// depends on (the git proxy, the UI) are literal IPs: the failure showed
// up as `apt`/`npm`/`gcloud` timing out, which reads as a blocked
// network. The resolver is konturctl's own setting now, defaulting to a
// public one; this asserts grain leaves that default alone unless the
// deployment named something, rather than hard-coding a resolver of its
// own.
func TestASandboxGuestGetsAResolverAndADeploymentCanChooseIt(t *testing.T) {
	code := setupCode(t)
	contains(t, code, "GRAIN_KONTUR_DNS=\"${GRAIN_KONTUR_DNS:-}\"")
	contains(t, code, "-kontur-create-arg -dns -kontur-create-arg \"$GRAIN_KONTUR_DNS\"")

	// Passed only when set: an empty -dns means "leave the guest's own
	// /etc/resolv.conf alone" to konturctl, which is the opposite of
	// what an unset deployment setting is asking for.
	if !strings.Contains(code, "if [ -n \"$GRAIN_KONTUR_DNS\" ]; then") {
		t.Error("setup.sh passes -dns unconditionally: an unset GRAIN_KONTUR_DNS would reach konturctl as the empty string, which means \"no nameserver at all\" rather than \"your default\"")
	}

	// The setting is only usable if it is documented where an operator
	// reads the list of them.
	contains(t, setupText(t), "GRAIN_KONTUR_DNS           nameserver")

	// And the default it falls through to is the vendored kontur's,
	// which has to be a resolver reachable from inside a guest rather
	// than this host's own.
	contains(t, read(t, "third_party", "kontur", "internal", "netshim", "config.go"), "DefaultDNS = \"8.8.8.8\"")
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

// The tailnet option publishes the UI without moving it.
//
// "Put it behind Tailscale" has been this deployment's own suggested
// answer to reachability since the UI was first bound to loopback
// (setup.sh's header, README.md's "The UI"), and GRAIN_TAILSCALE_ENABLE
// is the script finally doing it. The whole of what it may change is the
// host: tailscaled proxies the tailnet to the port the daemon already
// listens on, so the daemon's own configuration -- what it binds, what
// its container is given -- has to come out of this identical to a
// deployment that never asked for it. A version that widened -ui-addr
// instead would reach the same browser and open the port to everything
// else on the network too.
func TestTheTailnetOptionServesTheUIWithoutMovingIt(t *testing.T) {
	code := setupCode(t)

	// Off unless asked for: turning it on publishes an API with no auth
	// of its own to a whole tailnet, which is an operator's decision.
	contains(t, code, `GRAIN_TAILSCALE_ENABLE="${GRAIN_TAILSCALE_ENABLE:-0}"`)
	// And the daemon still binds loopback either way.
	contains(t, code, `GRAIN_UI_ADDR="${GRAIN_UI_ADDR:-127.0.0.1:80}"`)

	// The served target is whatever -ui-addr's port is, rather than a
	// literal 80: a deployment that moved the UI must still be the thing
	// published, not a port nothing is listening on.
	serve := body(t, code, "tailscale_serve_ui() {")
	contains(t, serve, `http://127.0.0.1:${GRAIN_UI_ADDR##*:}`)
	contains(t, serve, `args+=("--http=$GRAIN_TAILSCALE_SERVE_PORT")`)

	// tailscaled is a host service, not something the daemon's container
	// gets: the tailnet address belongs to the machine and has to
	// outlive any one `docker run`.
	absent(t, dockerRunArgs(t, code), "tailscale")
	contains(t, body(t, code, "setup_tailscale() {"), "systemctl enable --now tailscaled")

	// After the daemon is up, so that a `tailscale serve` failure is
	// about tailscale rather than about a UI that had not started yet.
	before(t, body(t, code, "main() {"), "enable_services", "setup_tailscale",
		"the tailnet is served before the daemon it points at is running")

	// The auth key authenticates this *host* to a tailnet; it is
	// tailscaled's to keep, and nothing here may copy it in among the
	// daemon's own credentials.
	for n, line := range strings.Split(code, "\n") {
		if strings.Contains(line, "GRAIN_TAILSCALE_AUTH_KEY") && strings.Contains(line, "GRAIN_DATA_DIR") {
			t.Errorf("setup.sh:%d writes the tailnet auth key into the deployment's own data directory: %s",
				n+1, strings.TrimSpace(line))
		}
	}
}

// Asking for a tailnet cannot cost you the deployment.
//
// Reachability is not what makes a deployment work, so it must not be
// what makes one fail: an unsupported distribution, a missing auth key,
// a tailnet without HTTPS certificates enabled. Every one of those ends
// the same way the kontur prerequisites above do -- the feature off, the
// reason logged, the rest of the run finished -- and report_readiness is
// what keeps that from passing for success.
func TestTheTailnetOptionConvergesRatherThanFailingTheDeploy(t *testing.T) {
	code := setupCode(t)

	setup := body(t, code, "setup_tailscale() {")
	for _, step := range []string{"install_tailscale", "tailscale_login", "tailscale_serve_ui"} {
		if !strings.Contains(setup, "if ! "+step+"; then") {
			t.Errorf("setup_tailscale does not let %s give up: a step that cannot converge fails the whole deploy", step)
		}
	}
	if strings.Contains(setup, "exit ") {
		t.Error("setup_tailscale exits: a host that cannot reach its tailnet still has a working loopback deployment")
	}
	contains(t, setup, "GRAIN_TAILSCALE_ENABLE=0")

	// Which is only honest if the closing report says so -- the same
	// requested-but-not-achieved distinction GRAIN_KONTUR_REQUESTED
	// exists to draw.
	contains(t, code, `GRAIN_TAILSCALE_REQUESTED="$GRAIN_TAILSCALE_ENABLE"`)
	contains(t, body(t, code, "report_readiness() {"),
		`[ "$GRAIN_TAILSCALE_REQUESTED" = "1" ] && [ "$GRAIN_TAILSCALE_ENABLE" != "1" ]`)
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
	// Nothing here reads the source at all any more. The one thing that
	// did -- the kontur guest build -- unpacked it out of the image so
	// that it matched the binary; there is no guest build here now, so
	// there is nothing to keep in step and no copy to unpack.
	absent(t, code, "unpack_image_source")

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

// The apply's own output is the last place a secret can escape, and the
// only test here that runs a script rather than reading one.
//
// An apply that *replaces* the host renders the instance's prior state in
// full, push-secrets.sh's metadata included, because
// lifecycle.ignore_changes governs in-place diffs and not the state a
// destroy is printed from (instance.tf says the same thing at the block
// itself). That is not hypothetical: raising boot_disk_gb forces
// replacement, and doing so printed the minter account's private key into
// a deploy log (bwsalmon/agents#653). GitHub Actions masks the values a
// workflow handed it; the minter key is minted mid-run, so nothing
// upstream of this filter can mask it.
//
// So terraform-apply.sh's redact runs here for real, over a diff shaped
// like the one that leaked: content checks cannot tell a filter that works
// from a filter with a typo in its regex, and a filter with a typo in its
// regex is indistinguishable from no filter at all until the day it
// matters.
func TestTheApplyRedactsSecretsOutOfItsOwnOutput(t *testing.T) {
	// body stops at the closing brace rather than including it, so the
	// function has to be closed again before bash will take it.
	fn := body(t, read(t, "terraform", "gcp", "deploy", "terraform-apply.sh"), "redact() {") + "\n}"

	// Every value here is fake, and every one of them is shaped like the
	// real thing it stands in for -- a jsonencode'd minter key with a
	// heredoc PEM, a one-line token, and a PEM rendered inline with
	// escaped newlines, which is the other way the provider prints one.
	const diff = `      ~ metadata                   = {
          - "grain-gcp-minter-key"     = jsonencode(
                {
                  - client_email                = "grain-main-gcp-key-minter@example.iam.gserviceaccount.com"
                  - private_key                 = <<-EOT
                        -----BEGIN PRIVATE KEY-----
                        MIIEvQIBADANBgkqhkiG9w0BAQEFAASCsentinelHEREDOC
                        -----END PRIVATE KEY-----
                    EOT
                  - private_key_id              = "cbc69cd7d294521700f8744cc2a835c1"
                }
            ) -> null
          - "grain-claude-oauth-token" = "sk-ant-oat-sentinelTOKEN" -> null
          - "grain-github-token"       = "github_pat_sentinelPAT" -> null
          - "grain-github-app-private-key" = "-----BEGIN RSA PRIVATE KEY-----\nsentinelINLINE\n-----END RSA PRIVATE KEY-----"
        }
      ~ initialize_params {
          ~ size                        = 50 -> 100 # forces replacement
        }
`

	cmd := exec.Command("bash", "-c", fn+"\nredact")
	cmd.Stdin = strings.NewReader(diff)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running redact: %v\n%s", err, out)
	}
	got := string(out)

	for _, secret := range []string{
		"sentinelHEREDOC", "sentinelTOKEN", "sentinelPAT", "sentinelINLINE",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("redact let %s through:\n%s", secret, got)
		}
	}

	// Redacted, not silenced. The plan has to stay worth reading: this log
	// is the first authenticated plan a deployment ever gets, since a
	// config repo's plan.yml deliberately runs without credentials. And
	// private_key_id is what names the key to revoke when one did leak.
	for _, kept := range []string{
		`"grain-gcp-minter-key"`,
		`"grain-claude-oauth-token"`,
		`"grain-github-app-private-key"`,
		"cbc69cd7d294521700f8744cc2a835c1",
		"size                        = 50 -> 100 # forces replacement",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("redact dropped %s, which is not a secret:\n%s", kept, got)
		}
	}
}

// The filter above is keyed on metadata names, so it can only cover the
// keys push-secrets.sh actually writes -- and instance.tf's own
// ignore_changes is the list of those, kept in one place. A secret added
// to one and not the other is silently unredacted, which is exactly the
// failure this whole change exists to stop.
func TestEverySecretMetadataKeyIsBothIgnoredAndRedacted(t *testing.T) {
	// Cut at the closing bracket on its own line: every entry in the list
	// ends in a "]" of its own.
	ignored := between(t,
		read(t, "terraform", "gcp", "instance.tf"), "ignore_changes = [", "\n    ]")
	redactor := body(t,
		read(t, "terraform", "gcp", "deploy", "terraform-apply.sh"), "redact() {")

	keys := regexp.MustCompile(`metadata\["([^"]+)"\]`).FindAllStringSubmatch(ignored, -1)
	if len(keys) == 0 {
		t.Fatal("instance.tf's ignore_changes names no metadata keys")
	}
	for _, k := range keys {
		if !strings.Contains(redactor, k[1]) {
			t.Errorf("%s is pushed onto the instance but terraform-apply.sh's redact does not cover it", k[1])
		}
	}
}

// The formatting gate lives in one place, and CI runs that place rather
// than a second copy of it.
//
// Unformatted Go builds, vets and tests exactly like formatted Go, so
// until the go job ran this nothing in CI could see the drift, and three
// files sat unformatted on main long enough that `gofmt -l` was no use
// as a local signal: it always had something to say about files the
// change in hand had not touched, and an editor that formats on save
// turned that into diff noise. `make fmt` rather than a gofmt line of
// the workflow's own is what keeps the two from drifting apart -- a
// contributor reproducing a red build locally has to be able to run the
// same check.
func TestTheGoJobRunsTheFormattingGate(t *testing.T) {
	goJob := between(t, stripComments(testsWorkflow(t)), "go-test:", "\n  ui-e2e:")
	contains(t, goJob, "run: make fmt")

	// And the target it names has to be a check that fails, not one that
	// lists offenders and exits 0 -- which is how this went unnoticed
	// before there was a job running it at all.
	contains(t, between(t, read(t, "Makefile"), "\nfmt:", "\n\n"), "gofmt -l")
	contains(t, between(t, read(t, "Makefile"), "\nfmt:", "\n\n"), "exit 1")
}

// The frontend's half of it, which has three moving parts in three files
// -- the workflow step, the Makefile target it names and the npm script
// that target names -- and one ordering constraint that is invisible in
// any of them: prettier lives in ui/node_modules, so the step only works
// where `make frontend` has already run `npm ci`. Moved up beside the Go
// `Format` step, where it looks like it belongs, it would fail with
// "prettier: not found" on a tree that is perfectly formatted.
func TestTheGoJobRunsTheFrontendFormattingGateAfterInstallingIt(t *testing.T) {
	goJob := between(t, stripComments(testsWorkflow(t)), "go-test:", "\n  ui-e2e:")
	contains(t, goJob, "run: make fmt-frontend")

	before(t, goJob, "run: make frontend", "run: make fmt-frontend",
		"the go job would check frontend formatting before `make frontend` installs prettier, and fail on a formatted tree")

	// The target, and the script it hands off to. `--check` rather than
	// `--write`: a gate that silently rewrote the tree would pass on
	// every commit and gate nothing.
	target := between(t, read(t, "Makefile"), "\nfmt-frontend:", "\n\n")
	contains(t, target, "npm run format:check")
	pkg := read(t, "ui", "package.json")
	contains(t, pkg, `"format:check": "prettier --check .`)

	// And prettier has to be a locked devDependency, so that `npm ci`
	// installs it and a new prettier release cannot redden a commit that
	// touched nothing -- unlike gofmt, prettier's output does change
	// between versions.
	contains(t, between(t, pkg, `"devDependencies"`, "\n  }"), `"prettier"`)
	contains(t, read(t, "ui", "package-lock.json"), `"node_modules/prettier"`)

	// The config file is what pins the gate to prettier's defaults, and
	// what stops prettier walking up out of the repository into whatever
	// ~/.prettierrc the contributor happens to have.
	read(t, "ui", ".prettierrc")
}

// The Go suite is one command, and the two files that run it run the
// same one.
//
// The Makefile's own header promises it "mirrors the steps
// .github/workflows/tests.yml's go-test job runs", and for that promise's
// whole life it did not: `make test` ran `go test -race ./...` while the
// workflow ran `go test ./...`, so the race detector -- the one check
// that can see a data race between the reconcile loop, a goroutine per
// dispatch, the addenda pollers, an atomic ForbiddenSet swapped under a
// serving proxy and a stateManager lock shared by a UI handler and a
// timer -- had never run in CI at all. Neither file can see the other,
// and both read like the whole story on their own, which is exactly the
// kind of drift this package exists to catch.
//
// Compared as one string rather than by asserting -race twice: any
// divergence at all -- a flag, a package pattern, a timeout on one side
// only -- means a red build locally and a green one in CI, or the
// reverse, and either way `make test` stops being how a contributor
// reproduces this job.
func TestTheGoJobRunsTheSameSuiteCommandTheMakefileDoes(t *testing.T) {
	// The recipe's `go test` line, without the leading tab make needs.
	var recipe string
	for _, line := range strings.Split(between(t, read(t, "Makefile"), "\ntest: frontend", "\n\n"), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "go test") {
			recipe = trimmed
			break
		}
	}
	if recipe == "" {
		t.Fatal("the Makefile's test target runs no `go test` line")
	}

	// The workflow's, which is the "Test" step and not the "Test frontend"
	// one below it -- hence the newline in the marker.
	goJob := between(t, stripComments(testsWorkflow(t)), "go-test:", "\n  ui-e2e:")
	step := strings.TrimSpace(between(t, goJob, "- name: Test\n", "\n\n"))
	_, command, ok := strings.Cut(step, "run:")
	if !ok {
		t.Fatalf("the go job's Test step runs nothing: %q", step)
	}
	command = strings.TrimSpace(command)

	if command != recipe {
		t.Errorf("CI runs %q and `make test` runs %q; the Makefile says it mirrors CI, so they have to be the same command", command, recipe)
	}
	// Naming the flag as well, so that dropping it from *both* files --
	// which the comparison above would happily accept -- is still a
	// failure someone has to argue with rather than a silent loss of the
	// only race coverage this repository has.
	contains(t, recipe, "-race")

	// The detector is a cgo library. Disabling cgo for this job would not
	// quietly drop the coverage -- `go test -race` refuses to build
	// without it -- but it would turn the whole suite red for a reason
	// that reads like a toolchain fault, so keep the two apart: only the
	// binary that ships is built with CGO_ENABLED=0.
	if regexp.MustCompile(`CGO_ENABLED[=:]\s*["']?0`).MatchString(goJob) {
		t.Error("the go job disables cgo somewhere, and `go test -race` needs it")
	}
}

// The sandbox guest image's toolchain, which three files have to agree
// on and none of them can see the others.
//
// A dispatched task -- most sharply a merge-queue fix task, which exists
// only to make a red build green -- can only check its own work if the
// guest carries the Go the module asks for and the Node CI runs, with
// caches warm enough to build offline. Getting either version from
// somewhere other than the file that already pins it is how a guest ends
// up failing a task's `go build` for a reason that has nothing to do
// with the task.
func TestTheSandboxGuestBuildsWithTheToolchainsTheRepositoryPins(t *testing.T) {
	build := read(t, "scripts", "kontur", "build-guest.sh")
	setup := read(t, "scripts", "kontur", "guest-setup.sh")

	// Go: one version, read out of go.mod, with the same expression the
	// Makefile already reads it with for Dockerfile.build.
	const readsGoMod = `sed -n 's/^go[[:space:]]\{1,\}//p'`
	contains(t, read(t, "Makefile"), readsGoMod+" go.mod")
	contains(t, build, readsGoMod+" ../../go.mod")
	// ...and nothing in the guest script writes a second copy of it down.
	if regexp.MustCompile(`GO_VERSION=["']?[0-9]`).MatchString(stripComments(setup)) {
		t.Error("guest-setup.sh pins a Go version of its own; build-guest.sh reads it out of go.mod")
	}

	// Node: the guest needs a full version (it fetches a tarball), the
	// workflow pins a major, and the two have to be the same major -- an
	// agent reproducing a red `npm test` on a different major is
	// reproducing a different thing.
	guestNode := regexp.MustCompile(`NODE_VERSION="(\d+)\.[^"]*"`).FindStringSubmatch(setup)
	if guestNode == nil {
		t.Fatal("guest-setup.sh pins no NODE_VERSION")
	}
	ciNode := regexp.MustCompile(`node-version: (\d+)`).FindStringSubmatch(testsWorkflow(t))
	if ciNode == nil {
		t.Fatal("tests.yml pins no node-version")
	}
	if guestNode[1] != ciNode[1] {
		t.Errorf("the guest carries Node %s but CI runs the suite on Node %s", guestNode[1], ciNode[1])
	}
}

// The caches are what make those toolchains usable, and they are warmed
// from the same four manifests CI installs from: a sandbox has no route
// to proxy.golang.org or registry.npmjs.org, so anything the image did
// not resolve at build time cannot be resolved at dispatch time either.
func TestTheSandboxGuestWarmsItsCachesFromTheLockedManifests(t *testing.T) {
	build := read(t, "scripts", "kontur", "build-guest.sh")
	// The one command that decides what the guest's caches know about.
	packed := between(t, build, `manifests="$(tar`, "\n")
	for _, manifest := range []string{"go.mod", "go.sum", "ui/package.json", "ui/package-lock.json"} {
		contains(t, packed, manifest)
	}
	setup := stripComments(read(t, "scripts", "kontur", "guest-setup.sh"))
	contains(t, setup, "go mod download")
	contains(t, setup, "npm ci")

	// Playwright's browsers are the one thing deliberately left out, and
	// leaving them out is not free: @playwright/test's install script
	// fetches them, so `npm ci` in ui/ only works on this guest because
	// npm is wrapped to skip that download.
	absent(t, setup, "playwright install")
	contains(t, setup, "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD")
}

func TestTheGuestBuildScriptsAreSyntacticallyValid(t *testing.T) {
	for _, script := range []string{"build-guest.sh", "guest-setup.sh"} {
		path := filepath.Join(repoRoot(t), "scripts", "kontur", script)
		// guest-setup.sh is /bin/sh (it runs in kontur's own guest stage,
		// which is not guaranteed a bash), so it is checked by the shell
		// it declares rather than by bash.
		shell := "bash"
		if script == "guest-setup.sh" {
			shell = "sh"
		}
		if out, err := exec.Command(shell, "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s -n scripts/kontur/%s: %v\n%s", shell, script, err, out)
		}
	}
}

// --- the sandbox volume -------------------------------------------------
//
// terraform/gcp gives sandboxes a disk of their own: docker's data root
// (the sandbox image, and every kontur VM's writable root, which is a
// qcow2 overlay inside that VM's own container) and HostSandboxes'
// per-run checkouts, so a task that fills a disk costs the runs in flight
// rather than the OS, the store, config-sync and the UI. Three files have
// to agree for that to be true, and none of them can see the others:
// instance.tf attaches the disk, files/startup.sh mounts it and points
// docker at it, and scripts/setup.sh decides where the daemon looks for
// its checkouts.

func startupScript(t *testing.T) string {
	return read(t, "terraform", "gcp", "files", "startup.sh")
}

// startupPaths reads every `readonly NAME="..."` out of startup.sh with
// references to earlier ones expanded, so an assertion can be about the
// paths the script actually uses rather than the text it spells them
// with.
func startupPaths(t *testing.T, text string) map[string]string {
	t.Helper()
	vals := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^readonly ([A-Z_]+)="([^"]*)"`).FindAllStringSubmatch(text, -1) {
		value := m[2]
		// Longest name first: "$DOCKER_DROPIN_DIR" also starts with
		// "$DOCKER_DROPIN", and substituting the shorter one into it
		// would produce a path that appears nowhere on the host.
		names := make([]string, 0, len(vals))
		for name := range vals {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		for _, name := range names {
			value = strings.ReplaceAll(value, "$"+name, vals[name])
		}
		vals[m[1]] = value
	}
	// A second pass, for a value that names one declared below it -- the
	// expansion above only knows about the ones it has already read.
	for name, value := range vals {
		for other, known := range vals {
			if other != name {
				value = strings.ReplaceAll(value, "$"+other, known)
			}
		}
		vals[name] = value
	}
	return vals
}

// under reports whether path is dir itself or anything inside it.
func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

func TestTheSandboxDiskTerraformAttachesIsTheOneTheHostMounts(t *testing.T) {
	instance := read(t, "terraform", "gcp", "instance.tf")
	startup := startupScript(t)
	paths := startupPaths(t, startup)

	// The by-id link is derived from attached_disk's device_name, and is
	// the only stable way to tell the two disks apart inside the VM.
	for device, mount := range map[string]string{"grain-data": "DATA_MNT", "grain-sandbox": "SANDBOX_MNT"} {
		contains(t, instance, `device_name = "`+device+`"`)
		if paths[mount] == "" {
			t.Errorf("startup.sh declares no %s to mount %s at", mount, device)
		}
		contains(t, startup, "/dev/disk/by-id/google-"+device)
	}

	// Whether there is a second disk at all is metadata, not a guess: a
	// declared disk that failed to attach has to be a warning rather than
	// a host quietly filling its boot disk again.
	contains(t, instance, "grain-sandbox-disk = var.sandbox_disk_gb > 0")
	contains(t, startup, "instance/attributes/grain-sandbox-disk")

	// dockerd must not start before the volume it stores everything on is
	// mounted: without this it wins the race often enough to create its
	// data root on the boot disk and have the mount hide it. Written
	// either as the variable or as the path it holds.
	if !strings.Contains(startup, "RequiresMountsFor=$SANDBOX_MNT") &&
		!strings.Contains(startup, "RequiresMountsFor="+paths["SANDBOX_MNT"]) {
		t.Errorf("nothing makes docker.service wait for %s, so dockerd can start before the volume is mounted and put its data root on the boot disk", paths["SANDBOX_MNT"])
	}
}

func TestTheSandboxVolumeHoldsDockersStoreAndTheCheckoutsButNeverOneInsideTheOther(t *testing.T) {
	startup := startupScript(t)
	paths := startupPaths(t, startup)

	docker, work, mnt := paths["SANDBOX_DOCKER_DIR"], paths["SANDBOX_WORK_DIR"], paths["SANDBOX_MNT"]
	if !under(docker, mnt) {
		t.Errorf("docker's data root (%s) is not on the sandbox volume (%s), so the sandbox image and every VM's overlay stay on the boot disk", docker, mnt)
	}
	if !under(work, mnt) {
		t.Errorf("the per-run checkouts (%s) are not on the sandbox volume (%s)", work, mnt)
	}
	// The one arrangement that looks tidy and destroys the deployment:
	// orchestrator.HostSandboxes.ReapOrphans removes *everything* under
	// its base directory at startup, docker's whole store included.
	if under(docker, work) {
		t.Errorf("docker's data root (%s) is inside the directory HostSandboxes.ReapOrphans empties (%s)", docker, work)
	}

	// The bind mount is what makes the checkouts land there with no
	// -sandbox-dir override anywhere: it has to be the path setup.sh
	// already defaults to.
	sandboxDir := regexp.MustCompile(`GRAIN_SANDBOX_DIR="\$\{GRAIN_SANDBOX_DIR:-([^}]*)\}"`).FindStringSubmatch(setupText(t))
	if sandboxDir == nil {
		t.Fatal("scripts/setup.sh no longer defaults GRAIN_SANDBOX_DIR to anything")
	}
	if paths["HOST_SANDBOX_MNT"] != sandboxDir[1] {
		t.Errorf("startup.sh bind-mounts the sandbox volume onto %s but setup.sh points the daemon at %s", paths["HOST_SANDBOX_MNT"], sandboxDir[1])
	}
	contains(t, stripComments(startup), `"data-root": "%s"`)
}

// startup.sh grew real branching with the sandbox volume (a mount that is
// allowed to fail, and a configuration to undo when the volume is turned
// off), and it is the one script here that only ever runs on a real GCE
// boot -- a syntax error in it is a host that never installs config-sync
// and so never deploys at all.
func TestTheHostBootScriptsAreSyntacticallyValidBash(t *testing.T) {
	for _, script := range []string{"startup.sh", "config-sync.sh", "deploy.sh"} {
		path := filepath.Join(repoRoot(t), "terraform", "gcp", "files", script)
		if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("bash -n terraform/gcp/files/%s: %v\n%s", script, err, out)
		}
	}
}
