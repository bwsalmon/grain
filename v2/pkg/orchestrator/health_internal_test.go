package orchestrator

import (
	"testing"
)

func TestParseProcStatsReadsLoadAverageAndMemory(t *testing.T) {
	output := "0.10 0.05 0.01 1/234 5678\n" +
		"MemTotal:        1024000 kB\n" +
		"MemFree:          200000 kB\n" +
		"MemAvailable:     400000 kB\n"

	loadAverage, usedMB, totalMB := parseProcStats(output)

	if loadAverage != "0.10 0.05 0.01" {
		t.Errorf("loadAverage = %q, want %q", loadAverage, "0.10 0.05 0.01")
	}
	if totalMB != 1000 {
		t.Errorf("totalMB = %d, want 1000", totalMB)
	}
	// usedMB is MemTotal minus MemAvailable, not minus MemFree -- see
	// parseProcStats' own doc comment on why.
	if usedMB != 609 {
		t.Errorf("usedMB = %d, want 609", usedMB)
	}
}

func TestParseProcStatsToleratesMissingMeminfo(t *testing.T) {
	loadAverage, usedMB, totalMB := parseProcStats("0.50 0.40 0.30 2/10 99\n")

	if loadAverage != "0.50 0.40 0.30" {
		t.Errorf("loadAverage = %q, want %q", loadAverage, "0.50 0.40 0.30")
	}
	if usedMB != 0 || totalMB != 0 {
		t.Errorf("usedMB/totalMB = %d/%d, want 0/0 with no MemTotal line", usedMB, totalMB)
	}
}
