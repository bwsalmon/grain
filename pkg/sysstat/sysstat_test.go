package sysstat_test

// Real /proc/loadavg and /proc/meminfo, not fakes -- both are Linux
// kernel interfaces every CI runner already has, the same "nothing here
// is a fake standing in for the real thing" discipline model/simulate_test.go's
// own doc comment holds this codebase to elsewhere.

import (
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
