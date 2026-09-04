package main

import (
	"testing"

	"github.com/bwsalmon/grain/pkg/staterepo"
)

// Which image the deployment writes into the CI step of its own state
// repository, and in which order the two answers win.
//
// The default is this build's own reference rather than a tag that
// follows main, because `grain state check` refuses a dump stamped with
// a schema it does not know: a deployment held at an older tag whose
// check ran main's build would fail every pull request against its own
// state for a reason that had nothing to do with the change proposed in
// it. An operator's "checkImage" still wins, because they may have an
// answer this build cannot know -- a registry mirror, a tag of their own
// -- and because it is the documented way to hold that line still.
func TestTheCheckRunsThisBuildUnlessTheOperatorSaidOtherwise(t *testing.T) {
	if got := checkImage(staterepo.Settings{}); got != defaultGrainImage {
		t.Errorf("a deployment that has said nothing does not check its state with its own build: %q", got)
	}
	const theirs = "registry.example.internal/grain:sha-abc1234"
	if got := checkImage(staterepo.Settings{CheckImage: theirs}); got != theirs {
		t.Errorf("checkImage did not win: %q", got)
	}
}

// An unstamped build -- a `make build` on a laptop, a `go test` here --
// still has to name something real, since it may be the binary an
// operator runs `grain state ci` out of. The fallback is the tag CI
// keeps pointed at main, which is the answer grain gave for this before
// anything was stamped at all.
func TestAnUnstampedBuildStillNamesARealImage(t *testing.T) {
	if defaultGrainImage != staterepo.DefaultCheckImage {
		t.Errorf("the fallback is not the published default: %q", defaultGrainImage)
	}
}
