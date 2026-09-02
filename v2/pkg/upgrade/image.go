package upgrade

// The container-deployment half of this package (bwsalmon/agents#645).
//
// A deployment installed by v2/scripts/setup.sh no longer builds a
// binary on the host it runs on: CI publishes one image per commit
// (../../.github/workflows/build-artifacts.yml), and the host pulls it.
// That leaves the checkout/build/install pipeline in upgrade.go with
// nothing to do on such a host -- there is no checkout to fetch into, no
// toolchain to build with, and the binary at InstallPath is not what the
// service runs -- so Config.Image switches Start onto the three steps
// that mean the same thing for an image:
//
//	pull the tag CI published for that branch
//	run it once, as a health check, before anything cuts over to it
//	point the deployment's image ref file at it, and restart
//
// It is strictly simpler than the binary path, and one difference is
// worth naming: there is no rollback. The binary path installs over the
// running binary and so has to be able to put the old one back if the
// new one fails its health check; here the health check runs against the
// pulled image *before* the ref file is touched at all, so a failure
// leaves the deployment pointing exactly where it already pointed, with
// nothing to undo.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// ImageConfig, set on Config.Image, makes an Upgrader upgrade a
// container deployment: pull, health-check and record an image tag
// instead of fetching, building and installing a binary. SrcDir,
// BuildCmd, BuiltBinary and InstallPath are all unused when it is set;
// HealthCheckArgs, Timeout, RestartCmd and StatusFile mean exactly what
// they do for the binary path.
type ImageConfig struct {
	// Repository is the image CI publishes a tag per branch to, with no
	// tag of its own -- "ghcr.io/bwsalmon/grain/grain". Required.
	Repository string
	// RefFile is the file naming the image the deployment's service
	// actually runs, which a successful upgrade rewrites: one
	// `GRAIN_IMAGE=<ref>` line, so that grain-daemon.service can read it
	// as an EnvironmentFile and interpolate it straight into its
	// ExecStart (v2/scripts/setup.sh's write_systemd_units). The
	// indirection is the whole mechanism -- the unit names a *file*
	// rather than a tag, so an upgrade is a file write plus a restart
	// and never a re-write of the unit itself. Required.
	RefFile string
	// PullCmd fetches an image ref appended as its last argument;
	// {"docker", "pull"} when left nil. Overridable so a test can swap
	// in something that needs no docker daemon.
	PullCmd []string
	// RunCmd runs an image, with the ref and then HealthCheckArgs
	// appended; {"docker", "run", "--rm"} when left nil. The image's
	// own ENTRYPOINT is `grain` (v2/Dockerfile), so appending
	// HealthCheckArgs -- {"schema-version"} on a real deployment -- runs
	// exactly the same subcommand the binary path's health check runs,
	// against the image about to be cut over to.
	RunCmd []string
	// SandboxImageArgs, when non-empty, asks the newly pulled image
	// which *sandbox* container it expects -- {"sandbox-image"} on a
	// real deployment, cmd/grain/sandboximage.go's own subcommand -- and
	// pulls that too, before anything cuts over.
	//
	// A kontur deployment runs two images from the same commit: grain,
	// and the sandbox container each task's VM runs inside. Upgrading
	// one without the other would leave a deployment whose next
	// dispatched task reaches for an image nothing ever fetched, so the
	// two move together or not at all -- a failure here stops the
	// upgrade with the ref file untouched, exactly like a failed health
	// check.
	//
	// Left nil by a deployment that dispatches into host directories:
	// there is no sandbox container in that shape to keep in step, and
	// pulling one it will never run would be a slow no-op with a real
	// failure mode of its own.
	SandboxImageArgs []string
}

// imageRefEnvKey is the variable name written into ImageConfig.RefFile,
// and the one grain-daemon.service's ExecStart interpolates.
const imageRefEnvKey = "GRAIN_IMAGE"

// imageHealthCheckTimeout bounds healthCheckImage, and matches the bound
// healthCheck puts on the binary path's equivalent run -- see that
// function's own comment for why it is this short.
const imageHealthCheckTimeout = 10 * time.Second

// maxTagLen is the registry limit on a tag: 128 characters. A branch
// long enough to exceed it has no image published for it at all (CI
// would have failed to push the tag), so resolving one is reported here
// rather than left to a pull that would 404 with a less obvious reason.
const maxTagLen = 128

// TagForBranch is the tag CI publishes for a branch: the branch name
// with '/' replaced by '-', since a docker tag may not contain a slash
// and grain's own branches routinely do (grain/issue-642,
// claude/some-task). ../../.github/workflows/build-artifacts.yml makes
// the identical substitution when it pushes; the two are written down
// separately on purpose, since a workflow cannot call into this package.
func TagForBranch(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

// imageRef is the fully qualified image an upgrade onto branch pulls.
func (u *Upgrader) imageRef(branch string) (string, error) {
	if u.cfg.Image.Repository == "" {
		return "", fmt.Errorf("no image repository configured")
	}
	tag := TagForBranch(branch)
	if len(tag) > maxTagLen {
		return "", fmt.Errorf("branch %q makes a %d-character image tag, over the %d-character limit -- no image is published for it", branch, len(tag), maxTagLen)
	}
	return u.cfg.Image.Repository + ":" + tag, nil
}

func (u *Upgrader) pullImage(ctx context.Context, ref string) error {
	argv := u.cfg.Image.PullCmd
	if len(argv) == 0 {
		argv = []string{"docker", "pull"}
	}
	argv = append(append([]string{}, argv...), ref)
	cmd := newCommand(ctx, "", argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// healthCheckImage runs the just-pulled image with HealthCheckArgs, the
// image-mode counterpart of healthCheck's run of the just-installed
// binary -- and, unlike that one, it runs *before* anything cuts over,
// so a failure needs no rollback (see this file's own header).
//
// It shares healthCheck's 10-second bound and for the same reason:
// "schema-version" touches no store and needs no config, so the timeout
// is there to catch a binary that hangs instead of exiting, not to
// allow for real work. The pull it follows is what pays for the network,
// and that runs under the upgrade's own Timeout.
func (u *Upgrader) healthCheckImage(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, imageHealthCheckTimeout)
	defer cancel()
	argv := u.cfg.Image.RunCmd
	if len(argv) == 0 {
		argv = []string{"docker", "run", "--rm"}
	}
	argv = append(append([]string{}, argv...), ref)
	argv = append(argv, u.cfg.HealthCheckArgs...)
	cmd := newCommand(ctx, "", argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sandboxImage asks a pulled image which sandbox container it expects,
// by running the subcommand SandboxImageArgs names inside it. An image
// that prints nothing is treated as having no sandbox to keep in step,
// not as an error: an older grain, pulled by a rollback, predates the
// subcommand and answers with a usage error on stderr rather than a ref
// -- which is exactly the "nothing to pull" case, and not worth failing
// an otherwise good rollback over.
func (u *Upgrader) sandboxImage(ctx context.Context, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, imageHealthCheckTimeout)
	defer cancel()
	argv := u.cfg.Image.RunCmd
	if len(argv) == 0 {
		argv = []string{"docker", "run", "--rm"}
	}
	argv = append(append([]string{}, argv...), ref)
	argv = append(argv, u.cfg.Image.SandboxImageArgs...)
	cmd := newCommand(ctx, "", argv[0], argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	// One line, whatever else it printed: the subcommand prints exactly
	// the ref, and a stray trailing newline is not part of it.
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// writeImageRef points the deployment at ref. Atomic for the same reason
// install and writeStatus are: grain-daemon.service reads this file on
// every start, and a start that raced a half-written one would come up
// with no image name at all.
func (u *Upgrader) writeImageRef(ref string) error {
	line := fmt.Sprintf("%s=%s\n", imageRefEnvKey, ref)
	tmp := u.cfg.Image.RefFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, u.cfg.Image.RefFile)
}
