// Package sysstat reads this machine's own CPU-load and memory pressure
// straight out of /proc, the same two numbers `uptime` and `free` report
// without shelling out to either -- cmd/grain/daemon.go's own
// ui.Config.HostStats (bwsalmon/agents#536's "sandbox health" pane: a
// sandbox that looks stuck is often really the host it runs on being
// starved, and that is a question about the daemon's own machine, not
// about any one sandbox -- see orchestrator.SandboxHealth's own doc comment
// on why a sandbox's usage is reported separately from this).
package sysstat

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Snapshot is one point-in-time reading Read returns.
type Snapshot struct {
	// LoadAverage1/5/15 are /proc/loadavg's own first three fields: the
	// number of runnable-or-uninterruptible processes, averaged over the
	// last 1/5/15 minutes.
	LoadAverage1, LoadAverage5, LoadAverage15 float64
	// MemTotalMB and MemUsedMB are /proc/meminfo's MemTotal and MemTotal
	// minus MemAvailable, converted from the kB /proc/meminfo itself
	// reports in. MemAvailable, not MemFree, is the kernel's own estimate
	// of memory a new process could actually get without swapping (man 5
	// proc) -- the same number `free`'s "available" column shows.
	MemTotalMB, MemUsedMB int
}

// Read returns a fresh Snapshot of this machine's own /proc/loadavg and
// /proc/meminfo. Both files are Linux-specific; a deployment on any other
// OS has no host-pressure reading to give ui.Config.HostStats, the same
// nil-means-unavailable contract every other optional pane already gives
// when its own precondition is missing.
func Read() (Snapshot, error) {
	s, err := readLoadAverage()
	if err != nil {
		return Snapshot{}, err
	}
	s.MemTotalMB, s.MemUsedMB, err = readMemory()
	if err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

func readLoadAverage() (Snapshot, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return Snapshot{}, fmt.Errorf("sysstat: reading /proc/loadavg: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return Snapshot{}, fmt.Errorf("sysstat: /proc/loadavg has too few fields: %q", data)
	}
	load1, err1 := strconv.ParseFloat(fields[0], 64)
	load5, err2 := strconv.ParseFloat(fields[1], 64)
	load15, err3 := strconv.ParseFloat(fields[2], 64)
	if err := firstOf(err1, err2, err3); err != nil {
		return Snapshot{}, fmt.Errorf("sysstat: parsing /proc/loadavg %q: %w", data, err)
	}
	return Snapshot{LoadAverage1: load1, LoadAverage5: load5, LoadAverage15: load15}, nil
}

func firstOf(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func readMemory() (totalMB, usedMB int, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("sysstat: reading /proc/meminfo: %w", err)
	}
	defer f.Close()

	var totalKB, availKB int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, convErr := strconv.Atoi(fields[1])
		if convErr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB = val
		case "MemAvailable:":
			availKB = val
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("sysstat: reading /proc/meminfo: %w", err)
	}
	if totalKB == 0 {
		return 0, 0, fmt.Errorf("sysstat: /proc/meminfo has no MemTotal line")
	}
	return totalKB / 1024, (totalKB - availKB) / 1024, nil
}
