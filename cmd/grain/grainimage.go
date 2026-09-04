// The image this build of grain is published as.
//
// The sibling of sandboximage.go, and the same trick for the other half
// of the pair: that file answers "which sandbox goes with this build",
// this one answers "which build is this". Both are stamped in at link
// time (the Dockerfile passes --build-arg GRAIN_IMAGE_REF, the Makefile
// turns it into a -ldflags -X), so the answer travels inside the image
// it describes and a rollback to an older tag says its own name rather
// than today's.
//
// It exists because of one thing a deployment writes about itself into
// somewhere else: the CI step grain installs in its own state repository
// (pkg/staterepo/format.go), which is a `docker run <image> state check`
// and so has to name an image. `grain state check` refuses a dump
// stamped with a schema it does not know -- "repository is at schema N,
// this build knows M" -- so the image that check runs has to be the
// build the deployment is running, and until now it was
// staterepo.DefaultCheckImage, which is main's. Right for a deployment
// tracking main, and wrong for every other one: a deployment held at an
// older tag got a workflow that failed every pull request against its
// state for a reason that had nothing to do with the change proposed in
// it, until an operator noticed and pinned it by hand.
//
// A deployment that does not run from an image at all -- `make build` on
// a host, `go run` in a checkout -- has no tag to name, and the fallback
// below is the best available answer there for the same reason
// defaultSandboxImage's is: a moving tag that resolves to something real
// beats naming nothing.

package main

import (
	"flag"
	"fmt"

	"github.com/bwsalmon/grain/pkg/staterepo"
)

// defaultGrainImage is overwritten at link time. The value here is the
// fallback for a build that does not stamp one -- a plain `make build`
// on a laptop, or a `docker build` with no --build-arg -- and is
// staterepo.DefaultCheckImage, the tag CI keeps pointed at main, so that
// the answer an unstamped build gives is exactly the one grain gave
// before anything was stamped at all. Written as a reference to that
// constant rather than as a second copy of the string: `-X` needs a
// variable initialised to a constant expression, and this is one.
var defaultGrainImage = staterepo.DefaultCheckImage

// checkImage is the image grain writes into the CI step of its own state
// repository: what the operator asked for, or the build that is running.
//
// An operator's own answer wins because they may have one this build
// cannot know -- a mirror of the registry, a tag of their own -- and
// because "checkImage" in state-repo.json is the documented way to hold
// that line still.
func checkImage(settings staterepo.Settings) string {
	if settings.CheckImage != "" {
		return settings.CheckImage
	}
	return defaultGrainImage
}

func grainImageCmd(args []string) {
	fs := flag.NewFlagSet("grain image", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return
	}
	fmt.Println(defaultGrainImage)
}
