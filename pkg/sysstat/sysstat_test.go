package sysstat_test

// Real /proc/loadavg, /proc/meminfo and statfs, not fakes -- all three
// are Linux kernel interfaces every CI runner already has, the same
// "nothing here is a fake standing in for the real thing" discipline
// model/simulate_test.go's own doc comment holds this codebase to
// elsewhere.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bwsalmon/grain/pkg/sysstat"
)

func TestReadReturnsARealSnapshotOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.Read only ever works on Linux")
	}

	snap, err := sysstat.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.LoadAverage1 < 0 {
		t.Errorf("LoadAverage1 = %v, want >= 0", snap.LoadAverage1)
	}
	if snap.MemTotalMB <= 0 {
		t.Errorf("MemTotalMB = %d, want > 0", snap.MemTotalMB)
	}
	if snap.MemUsedMB < 0 || snap.MemUsedMB > snap.MemTotalMB {
		t.Errorf("MemUsedMB = %d, want between 0 and MemTotalMB (%d)", snap.MemUsedMB, snap.MemTotalMB)
	}
}

// DiskUsage against a real directory, for the same reason Read is
// exercised against real /proc: statfs is the interface, and a fake of it
// would only test the arithmetic.
func TestDiskUsageReportsARealFilesystemOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.DiskUsage only ever works on Linux")
	}

	totalMB, usedMB, err := sysstat.DiskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if totalMB <= 0 {
		t.Errorf("totalMB = %d, want > 0", totalMB)
	}
	if usedMB < 0 || usedMB > totalMB {
		t.Errorf("usedMB = %d, want between 0 and totalMB (%d)", usedMB, totalMB)
	}
}

// A path that does not exist is an error rather than a silent 0/0: the
// caller (cmd/grain's hostStats) is the one that decides an unavailable
// reading is not worth failing the whole host-pressure call over, and it
// can only decide that if it is told.
func TestDiskUsageReportsAMissingPathAsAnError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.DiskUsage only ever works on Linux")
	}

	if _, _, err := sysstat.DiskUsage(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Error("DiskUsage of a missing path returned no error")
	}
}

// The property cmd/grain's own hostStats relies on: two paths that are
// the same filesystem answer the same, and the answer survives the paths
// looking nothing alike. A subdirectory is the only two-paths-one-disk
// case a test can create without root (a bind mount needs CAP_SYS_ADMIN),
// but it is the same st_dev comparison the bind-mounted sandbox volume
// this exists for is decided by.
func TestFilesystemIDIsTheSameForOneFilesystemOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.FilesystemID only ever works on Linux")
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	root, err := sysstat.FilesystemID(dir)
	if err != nil {
		t.Fatalf("FilesystemID(%s): %v", dir, err)
	}
	nested, err := sysstat.FilesystemID(sub)
	if err != nil {
		t.Fatalf("FilesystemID(%s): %v", sub, err)
	}
	if root != nested {
		t.Errorf("FilesystemID = %d for %s and %d for %s, want one filesystem to answer once", root, dir, nested, sub)
	}
}

// Missing paths are errors here for the same reason they are for
// DiskUsage above: the caller decides what an unavailable reading costs,
// and folding two disk figures together on a 0 it was never given would
// hide one of them.
func TestFilesystemIDReportsAMissingPathAsAnError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.FilesystemID only ever works on Linux")
	}

	if _, err := sysstat.FilesystemID(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Error("FilesystemID of a missing path returned no error")
	}
}
