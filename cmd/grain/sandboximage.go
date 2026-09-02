// The sandbox container this build of grain goes with.
//
// A kontur-backed deployment runs two images, not one: this one, and the
// sandbox container each task's VM runs inside (packer/kontur/
// build-oci-image.sh's own output, published as kontur-sandbox by
// ../../.github/workflows/build-artifacts.yml). They are built from
// the same commit -- pkg/kontur here and third_party/kontur there -- and
// a deployment running two commits' worth of them is exactly the drift
// scripts/setup.sh used to avoid by building both on the host.
//
// It no longer builds either (bwsalmon/agents#645): it pulls this image,
// and asks *this image* which sandbox image goes with it, which is what
// this file exists to answer. The reference is stamped in at build time
// (Dockerfile passes --build-arg SANDBOX_IMAGE, the Makefile turns it
// into a -ldflags -X), so a deployment needs to be told nothing at all,
// and a `grain sandbox-image` out of any image says exactly which
// sandbox that build expects -- including a rollback to an older tag,
// whose answer is its own older sandbox rather than today's.

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
var defaultSandboxImage = "ghcr.io/bwsalmon/grain/kontur-sandbox:latest"

func sandboxImageCmd(args []string) {
	fs := flag.NewFlagSet("grain sandbox-image", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return
	}
	fmt.Println(defaultSandboxImage)
}
