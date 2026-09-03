// The sandbox image this build of grain goes with.
//
// A kontur-backed deployment runs two images, not one: this one, and the
// sandbox each task's VM runs -- a kontur image carrying grain's own
// provisioned guest, built by scripts/kontur/build-guest.sh and published
// as `guest` by ../../.github/workflows/build-artifacts.yml. They are
// built from the same commit, and a deployment running two commits' worth
// of them is exactly the drift scripts/setup.sh used to avoid by building
// on the host.
//
// It no longer builds either (bwsalmon/agents#645): it pulls this image,
// and asks *this image* which sandbox goes with it, which is what this
// file exists to answer. The reference is stamped in at build time
// (Dockerfile passes --build-arg SANDBOX_IMAGE, the Makefile turns it
// into a -ldflags -X), so a deployment needs to be told nothing at all,
// and a `grain sandbox-image` out of any image says exactly which
// sandbox that build expects -- including a rollback to an older tag,
// whose answer is its own older sandbox rather than today's.
//
// One image where there used to be two. The sandbox container and the
// guest disk inside it were separate artifacts, the disk built per host
// because it baked in a per-deployment SSH key. Since kontur generates
// that keypair per VM boot, a guest can be derived from a published
// kontur image and committed as another runnable one -- so the container
// and the guest are the same thing, with one tag to stamp and one to
// pull. See scripts/kontur/build-guest.sh.

package main

import (
	"flag"
	"fmt"
)

// defaultSandboxImage is overwritten at link time. The value here is the
// fallback for a build that does not stamp one -- a plain `make build`
// on a laptop, or a `docker build` with no --build-arg -- and names the
// tag CI keeps pointed at main rather than nothing at all, so an
// unstamped binary still resolves to a real, if less precise, image.
var defaultSandboxImage = "ghcr.io/bwsalmon/grain/guest:latest"

func sandboxImageCmd(args []string) {
	fs := flag.NewFlagSet("grain sandbox-image", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return
	}
	fmt.Println(defaultSandboxImage)
}
