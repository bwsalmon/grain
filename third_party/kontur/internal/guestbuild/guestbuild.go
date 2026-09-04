// Package guestbuild customizes a kontur guest image by booting it,
// provisioning the guest from inside, and committing the result as a new
// image.
//
// This is the counterpart to the Dockerfile's own GUEST_SETUP_SCRIPT
// hook, and the two differ in what they can do rather than in what they
// produce. That hook runs in a container during the image build, on the
// build host's kernel, with no service manager -- so it can install
// packages and drop in files, and cannot start anything, load a module,
// or run a container. This boots the guest for real: the environment the
// setup script sees is the environment the guest will actually run in,
// so `systemctl start`, a warm docker image cache, and a test suite run
// against the finished image are all available.
//
// What that costs is /dev/kvm on the machine doing the build, and the
// scrub below -- a booted rootfs accumulates per-boot identity that must
// not be baked into an image every VM is then cloned from.
//
// The output is an ordinary OCI image, and specifically the same *kind*
// of image as the input: kontur, cloud-hypervisor and a bootable guest
// disk, so `docker run` on it boots a VM exactly as the base does. A
// customized guest is therefore something a consumer derives and
// publishes with the tools they already have, rather than a disk image
// they have to build, host and hand to kontur separately.
package guestbuild

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/kontur/internal/config"
	"github.com/bwsalmon/kontur/internal/dockervm"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// setupPath is where the caller's script is staged inside the guest. In
// /tmp so that it is gone by the time the image is committed even if the
// removal below is skipped -- /tmp is cleared on boot.
const setupPath = "/tmp/kontur-guest-setup.sh"

// scrub is run in the guest after the setup script and before shutdown.
//
// Everything here is per-boot identity, and every line of it is a bug if
// left in: an image is cloned into many VMs, and state that is supposed
// to be unique per machine stops being unique the moment it is baked in.
// A shared /etc/machine-id gives every VM the same journald identity and
// the same DHCP DUID; shared SSH host keys make host-key verification
// meaningless across the fleet; a shared random-seed feeds every VM's
// entropy pool the same bytes at boot.
//
// Deliberately not here: anything the setup script chose to install or
// leave behind (package caches, build trees). Those are the caller's to
// clean up if they want a smaller image -- they are size, not
// correctness, and guessing at them would silently delete things a
// caller meant to keep.
//
// Nor any authorized_keys: kontur's own guest has none, since exec no
// longer logs in (see internal/guestexec), and one on a derived guest is
// there because a setup script deliberately put it there.
//
// The SSH host keys below are scrubbed even though the base image runs
// no sshd, and that is not vestigial: a setup script is free to install
// one, and a guest image that shipped the host keys openssh-server's
// postinst generated would hand every VM cloned from it the same
// identity -- the exact failure this list exists to prevent, and one
// that would now arrive through a caller's own choice rather than
// through anything kontur installed.
const scrub = `set -eu
rm -f /etc/ssh/ssh_host_*
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
rm -f /var/lib/systemd/random-seed
rm -rf /var/lib/dhcp/* /var/lib/dhcpcd/*
if [ -d /var/log ]; then
	find /var/log -type f -exec sh -c ': > "$1"' _ {} \;
fi
`

// Options configures one Build.
type Options struct {
	// From is the base image: any image that boots a kontur guest, i.e.
	// kontur's own or one this function produced earlier.
	From string

	// Setup is the script's text (not a path), run as root inside the
	// booted guest. It reaches the guest base64-encoded on a command
	// line rather than over stdin, so nothing in it needs quoting and a
	// script with a shebang, quotes or newlines travels unchanged.
	Setup string

	// Tag is the image reference to commit the result to.
	Tag string

	// DockerBinary is the docker CLI to exec, resolved via PATH when it
	// contains no slash. Empty means "docker"; tests point it at a fake.
	DockerBinary string

	// ExtraRunArgs are passed to `docker run` before the image name.
	// This is how a build reaches a network it would not otherwise: a
	// proxy's environment, a CA bundle, a private registry's
	// credentials. The guest is a VM inside the container and inherits
	// none of the builder's own network context by itself.
	ExtraRunArgs []string

	// ReadyTimeout bounds how long to wait for the guest to accept a
	// command after the container starts. A guest that never becomes
	// reachable is the characteristic kontur failure -- see
	// deploy/guest-image/README.md's "Networking" -- so this failing is
	// informative, and its message carries the container's console log.
	ReadyTimeout time.Duration

	// ShutdownTimeout bounds the wait for the guest to power off after
	// SIGTERM. Exceeding it means docker SIGKILLs the container out from
	// under a still-running guest, leaving the filesystem in the disk
	// image dirty -- so Build fails rather than committing that.
	ShutdownTimeout time.Duration

	// KeepOnFailure leaves the container in place when something goes
	// wrong, so its console log and its guest can be inspected. The
	// error names it.
	KeepOnFailure bool

	Stdout io.Writer
	Stderr io.Writer
}

const (
	defaultReadyTimeout    = 3 * time.Minute
	defaultShutdownTimeout = 60 * time.Second
)

// Build boots From, runs Setup inside the guest, and commits the result
// as Tag.
func Build(ctx context.Context, opts Options) error {
	if opts.From == "" {
		return fmt.Errorf("a base image is required")
	}
	if opts.Tag == "" {
		return fmt.Errorf("an output tag is required")
	}
	if strings.TrimSpace(opts.Setup) == "" {
		return fmt.Errorf("a setup script is required")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = defaultReadyTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	d := &dockerCLI{binary: opts.DockerBinary}
	name := buildName()
	container := staticpod.PodName(name)

	// Booting through the docker backend rather than a bare `docker run`
	// of the image, because a guest needs a network before any of this
	// works: netshim has to create the tap, splice it onto the
	// container's own interface, and stand up the control link "kontur
	// exec" reaches the guest over -- while the guest's own address is
	// how the setup script reaches a package mirror. A lone `docker run`
	// leaves cloud-hypervisor with no --net at all and the guest
	// unreachable.
	//
	// The spec names no disk, which means the guest baked into the image
	// being customized -- the whole point here -- and DiskReadOnly=false
	// so the setup script's changes land in that disk and are what
	// `docker commit` then captures.
	spec := staticpod.Defaults()
	spec.Name = name
	spec.Backend = staticpod.BackendDocker
	spec.KonturImage = opts.From
	spec.DiskImage = ""
	// persistent, not the overlay every other VM gets: the whole point
	// here is for the setup script's changes to land in the image's own
	// disk, which is what `docker commit` then captures. Writes into a
	// per-boot overlay would be discarded with the container and the
	// committed image would be identical to its base.
	spec.DiskMode = config.DiskModePersistent
	spec.GuestUser = "root"
	spec.ShutdownTimeout = opts.ShutdownTimeout.String()
	spec.DockerRunOpts = opts.ExtraRunArgs
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("the build VM's own settings: %w", err)
	}

	dv := &dockervm.Docker{BinaryPath: opts.DockerBinary}
	fmt.Fprintf(opts.Stdout, "booting %s\n", opts.From)
	if err := dockervm.Create(ctx, dv, spec, io.Discard); err != nil {
		return fmt.Errorf("starting the guest VM: %w", err)
	}

	committed := false
	defer func() {
		if committed || opts.KeepOnFailure {
			return
		}
		// Removes the VM container and the network namespace holder
		// netshim configured; leaving either behind would leak a
		// container and a tap device per failed build.
		_ = dockervm.Delete(context.WithoutCancel(ctx), dv, name, 5, io.Discard)
	}()

	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		if opts.KeepOnFailure {
			return fmt.Errorf("%w (left running for inspection: docker logs %s; docker exec %s kontur exec)", err, container, container)
		}
		// Console first, error last. An error with 40 lines of boot log
		// appended is an error nobody reads: `tail` on the job log shows
		// the guest reaching multi-user.target and hides the sentence
		// that says what actually went wrong.
		if logs, lerr := d.output(context.WithoutCancel(ctx), "logs", "--tail", "40", container); lerr == nil && strings.TrimSpace(logs) != "" {
			return fmt.Errorf("guest console, last 40 lines:\n%s\n%w", logs, err)
		}
		return err
	}

	if err := d.waitForGuest(ctx, container, opts.ReadyTimeout); err != nil {
		return fail("waiting for the guest to become reachable: %w", err)
	}

	fmt.Fprintf(opts.Stdout, "provisioning\n")
	if err := d.guestScript(ctx, container, opts.Setup, setupPath, opts.Stdout, opts.Stderr); err != nil {
		return fail("running the setup script in the guest: %w", err)
	}

	// The scrub runs after the setup script on purpose: a setup script
	// that starts services (which is the reason to be booting at all)
	// generates logs and machine state of its own, and scrubbing before
	// it would leave exactly that behind.
	if err := d.guestExec(ctx, container, io.Discard, io.Discard, "sh", "-c", "rm -f "+setupPath); err != nil {
		return fail("removing the setup script from the guest: %w", err)
	}
	if err := d.guestExec(ctx, container, io.Discard, opts.Stderr, "sh", "-c", scrub); err != nil {
		return fail("scrubbing per-boot state from the guest: %w", err)
	}

	// SIGTERM, which "kontur run" turns into an ACPI power button press
	// and then waits out (see the top-level README's "Shutdown"). The
	// wait matters more here than anywhere else: what is being committed
	// is the guest's filesystem, and a guest killed mid-write commits a
	// dirty one that every VM cloned from this image then inherits.
	fmt.Fprintf(opts.Stdout, "shutting the guest down\n")
	secs := int(opts.ShutdownTimeout.Seconds())
	if err := d.run(ctx, io.Discard, "stop", "-t", strconv.Itoa(secs), container); err != nil {
		return fail("stopping the guest container: %w", err)
	}
	code, err := d.output(ctx, "inspect", "-f", "{{.State.ExitCode}}", container)
	if err != nil {
		return fail("reading the guest container's exit status: %w", err)
	}
	switch exit := strings.TrimSpace(code); exit {
	case "0":
	case "137":
		return fail("the guest did not power off within %s and was killed, so its filesystem may be inconsistent -- raise -shutdown-timeout, or check whether the setup script left something that blocks shutdown", opts.ShutdownTimeout)
	default:
		// Anything else: the VM supervisor itself failed on the way
		// down. Committing would capture whatever state that left.
		// Reported with the code rather than swallowed, because the
		// console above will show a guest that looks perfectly healthy
		// and the number is the only thing that says otherwise.
		return fail("the guest container exited %s rather than powering off cleanly, so its filesystem may be inconsistent", exit)
	}

	// commit carries the base image's own config forward -- entrypoint,
	// env, labels -- so the result runs exactly the way its base does
	// with nothing to restate here. The one addition records what it was
	// built from, since an image with a guest disk in it otherwise says
	// nothing about its own provenance.
	if err := d.run(ctx, opts.Stdout,
		"commit", "--change", "LABEL org.opencontainers.image.base.name="+opts.From, container, opts.Tag); err != nil {
		return fail("committing the provisioned guest: %w", err)
	}
	committed = true

	if err := dockervm.Delete(ctx, dv, name, 5, io.Discard); err != nil {
		return fmt.Errorf("removing the build VM (the image %s was committed successfully): %w", opts.Tag, err)
	}
	fmt.Fprintf(opts.Stdout, "built %s\n", opts.Tag)
	return nil
}

// buildName names the throwaway VM this build runs in. Unique per build
// so concurrent builds on one host -- a CI runner doing several guest
// variants at once -- don't collide, and short because netshim derives
// the VM's tap device name as "tap-<name>", which Linux caps at 15
// characters (VMSpec.Validate rejects anything longer).
func buildName() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return "gb-" + string(b)
}

type dockerCLI struct{ binary string }

func (d *dockerCLI) bin() string {
	if d.binary == "" {
		return "docker"
	}
	return d.binary
}

// run execs "docker <args...>", streaming stdout to w and folding stderr
// into the returned error.
func (d *dockerCLI) run(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// output execs "docker <args...>" and returns its stdout.
func (d *dockerCLI) output(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	if err := d.run(ctx, &out, args...); err != nil {
		return "", err
	}
	return out.String(), nil
}

// guestExec runs a command inside the VM guest of container name, via
// the "kontur exec" the container already carries -- the same path
// `kubectl exec ... -- kontur exec` takes, and the only way in, since
// the keypair it authenticates with was generated inside that container
// at boot and never leaves it.
func (d *dockerCLI) guestExec(ctx context.Context, name string, stdout, stderr io.Writer, args ...string) error {
	full := append([]string{"exec", name, "kontur", "exec", "--"}, args...)
	cmd := exec.CommandContext(ctx, d.bin(), full...)
	cmd.Stdout = stdout
	var errBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &errBuf)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// guestScript writes script to path inside the guest and runs it.
//
// The script travels base64-encoded on the command line rather than as
// an argument or over stdin: it is a whole shell script, with quotes and
// newlines and possibly a shebang, and base64 is the one encoding that
// survives a trip through two shells (the container's and the guest's)
// without any of it needing to be escaped. The guest is known to have
// base64 -- the Dockerfile's guest-rootfs stage asserts it, since
// kontur-authorized-key needs it too.
func (d *dockerCLI) guestScript(ctx context.Context, name, script, path string, stdout, stderr io.Writer) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	install := fmt.Sprintf("printf %%s %s | base64 -d > %s && chmod 0755 %s", encoded, path, path)
	if err := d.guestExec(ctx, name, io.Discard, stderr, "sh", "-c", install); err != nil {
		return fmt.Errorf("staging the script in the guest: %w", err)
	}
	return d.guestExec(ctx, name, stdout, stderr, path)
}

// waitForGuest polls until the guest accepts a command, giving up at
// timeout -- or sooner, if the container itself exits, which no amount
// of further waiting would fix.
func (d *dockerCLI) waitForGuest(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := d.guestExec(ctx, name, io.Discard, io.Discard, "true"); err == nil {
			return nil
		}
		running, err := d.output(ctx, "inspect", "-f", "{{.State.Running}}", name)
		if err != nil {
			return fmt.Errorf("inspecting the guest container: %w", err)
		}
		if strings.TrimSpace(running) != "true" {
			return fmt.Errorf("the guest container exited before its guest became reachable")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
