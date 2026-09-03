package version

import (
	"runtime/debug"
	"testing"
	"time"
)

func stamp(settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{Settings: settings}
}

// The whole reading, out of the three settings `go build -buildvcs` puts
// there, in the form cmd/go writes them.
func TestInfoFromReadsTheVCSStamp(t *testing.T) {
	got := infoFrom(stamp(
		debug.BuildSetting{Key: "-buildmode", Value: "exe"},
		debug.BuildSetting{Key: "vcs", Value: "git"},
		debug.BuildSetting{Key: "vcs.revision", Value: "0fbfb4619f0a1c2d3e4f5a6b7c8d9e0f11223344"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-09-03T14:02:11Z"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"},
	), true)

	want := Info{
		Revision: "0fbfb4619f0a1c2d3e4f5a6b7c8d9e0f11223344",
		Time:     time.Date(2026, 9, 3, 14, 2, 11, 0, time.UTC),
	}
	if got != want {
		t.Errorf("infoFrom = %+v, want %+v", got, want)
	}
}

// vcs.time is reported in UTC whatever offset it was stamped with, so a
// caller formatting it does not have to normalise first.
func TestInfoFromNormalisesTimeToUTC(t *testing.T) {
	got := infoFrom(stamp(debug.BuildSetting{Key: "vcs.time", Value: "2026-09-03T16:02:11+02:00"}), true)

	want := time.Date(2026, 9, 3, 14, 2, 11, 0, time.UTC)
	if !got.Time.Equal(want) || got.Time.Location() != time.UTC {
		t.Errorf("infoFrom time = %v (%v), want %v (UTC)", got.Time, got.Time.Location(), want)
	}
}

func TestInfoFromReportsAModifiedTree(t *testing.T) {
	got := infoFrom(stamp(
		debug.BuildSetting{Key: "vcs.revision", Value: "abc1234"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	), true)

	if !got.Modified {
		t.Errorf("infoFrom modified = false for vcs.modified=true, want true")
	}
}

// An unstamped build -- -buildvcs=false, or any test binary -- is an
// empty Info, not an error and not a panic: every caller shows it as
// "unknown".
func TestInfoFromWithoutAStamp(t *testing.T) {
	for name, bi := range map[string]*debug.BuildInfo{
		"no build info at all": nil,
		"no vcs settings":      stamp(debug.BuildSetting{Key: "-buildmode", Value: "exe"}),
	} {
		t.Run(name, func(t *testing.T) {
			ok := bi != nil
			if got := infoFrom(bi, ok); got != (Info{}) {
				t.Errorf("infoFrom = %+v, want the zero Info", got)
			}
		})
	}
}

// A stamp whose time is unreadable still names its commit: the two are
// read independently, so a broken half does not cost the good one.
func TestInfoFromKeepsTheRevisionWhenTheTimeIsUnparseable(t *testing.T) {
	got := infoFrom(stamp(
		debug.BuildSetting{Key: "vcs.revision", Value: "abc1234"},
		debug.BuildSetting{Key: "vcs.time", Value: "not a timestamp"},
	), true)

	if got.Revision != "abc1234" || !got.Time.IsZero() {
		t.Errorf("infoFrom = %+v, want revision abc1234 and a zero time", got)
	}
}

// Get is whatever this test binary carries, which is nothing -- `go
// test` does not stamp VCS information. Worth asserting anyway: it must
// answer, repeatedly and identically, rather than panicking on the
// absence.
func TestGetIsStableAndSurvivesAnUnstampedBinary(t *testing.T) {
	if Get() != Get() {
		t.Errorf("Get returned two different answers in one process")
	}
}
