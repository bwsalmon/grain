// Package version reports the version grain's UI shows (bwsalmon/agents#397):
// the store schema this build expects (model.SchemaVersion, bumped only
// for a database change an existing store cannot simply be reconciled
// into -- see that constant's own doc comment) joined with the git
// commit the running binary was built from. Neither half means much
// alone: two builds can share a schema version yet differ in everything
// else the commit brought, and a commit hash alone says nothing about
// whether a database an older build wrote is safe for this one to open.
package version

import (
	"fmt"
	"runtime/debug"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// revisionLen matches `git rev-parse --short`'s own default -- long
// enough that a collision within one checkout's history is not a
// practical concern, short enough to read at a glance in a UI footer.
const revisionLen = 7

// Revision returns the git commit this binary was built from, shortened
// to revisionLen characters and suffixed "-dirty" if the working tree
// that built it carried uncommitted changes, or "" when neither is
// knowable: -buildvcs=false (Makefile's own doc comment on BUILDVCS), or
// a build with no .git available to stamp at all.
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if len(revision) > revisionLen {
		revision = revision[:revisionLen]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}

// String is what the UI and `grain version` both print: the schema
// version, then the revision Revision reports -- "unknown" in its place
// when a build carries none -- so a database's breaking-change gate and
// the exact commit behind it are never seen apart from one another.
func String() string {
	revision := Revision()
	if revision == "" {
		revision = "unknown"
	}
	return fmt.Sprintf("%d+%s", model.SchemaVersion, revision)
}
