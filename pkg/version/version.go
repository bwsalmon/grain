// Package version answers "which build of grain is this?" out of the
// binary itself: the commit `go build` stamped into it, when that commit
// was made, and whether the tree it was built from was dirty.
//
// Nothing is stamped by hand and there is no version string to bump.
// `go build -buildvcs=auto` -- what the Makefile, Dockerfile and CI all
// run (see the Makefile's own BUILDVCS comment for the one case that
// turns it off) -- records vcs.revision, vcs.time and vcs.modified in
// the binary's build info, and this reads them back. So a deployment
// says what it is running without anyone having remembered to tell it,
// and a rollback to an older image reports that older commit rather than
// whatever the release notes claim, the same reasoning
// cmd/grain/sandboximage.go gives for stamping its sandbox tag in.
//
// Absence is normal and is reported as an empty Info rather than an
// error: a `go build -buildvcs=false` binary carries no stamp, and
// neither does any test binary -- `go test` does not stamp VCS
// information at all, which is why the reading is split out into
// infoFrom below and tested there rather than through Get.
package version

import (
	"runtime/debug"
	"sync"
	"time"
)

// Info is one build's identity. The zero value means this binary carries
// no VCS stamp, which every caller has to be able to show as "unknown"
// rather than treat as a failure -- see the package comment.
type Info struct {
	// Revision is the full commit hash the binary was built from, or ""
	// when unstamped. Full rather than shortened: this is the value you
	// paste into `git show`, and a caller that wants seven characters can
	// take them, while a caller handed seven cannot get the rest back.
	Revision string
	// Time is that commit's own timestamp (vcs.time), in UTC -- not the
	// time of the build, which Go does not record. Zero when unstamped,
	// or when a stamp carried a time that would not parse.
	Time time.Time
	// Modified is true when the tree the build ran in had uncommitted
	// changes (vcs.modified), so Revision alone does not describe what is
	// running -- a laptop build against local edits, never a CI one.
	Modified bool
}

// Get reports this binary's own build stamp. Read once: the answer
// cannot change while the process lives, and GET /api/config asks for it
// on every poll of every open UI tab.
var Get = sync.OnceValue(func() Info {
	return infoFrom(debug.ReadBuildInfo())
})

// infoFrom is Get's whole body, taking debug.ReadBuildInfo's pair as
// arguments so it can be tested against a stamp -- the test binary this
// package's own tests run in has none of its own.
func infoFrom(bi *debug.BuildInfo, ok bool) Info {
	if !ok || bi == nil {
		return Info{}
	}
	var info Info
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.time":
			// RFC3339, per cmd/go. A value that does not parse leaves
			// Time zero and the revision beside it intact: half a stamp
			// is still worth reporting.
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				info.Time = t.UTC()
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}
