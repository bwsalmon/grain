package orchestrator_test

// HostSandboxes and KonturSandboxes each hand out one sandbox per slot,
// created lazily on first use and reused after that -- both guard their
// bookkeeping (roots, created, gitCreds) with a single mutex. Nothing
// today calls ToolsFor for more than one slot at a time (reconcileDispatch
// runs each dispatch's whole RunDispatch in one goroutine, sequentially --
// cycle.go's own doc comment), but that is a scheduling choice this
// package could reasonably drop in a later change without either type
// changing shape, since both were already written to hand out sandboxes
// concurrently. These tests hold that promise to its word: drive both
// with many goroutines, mixing repeat calls for the same slot with calls
// for many distinct slots, and run with -race.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func TestHostSandboxesRootForIsSafeForConcurrentCallers(t *testing.T) {
	h := orchestrator.NewHostSandboxes(t.TempDir())

	const (
		slots        = 8
		callsPerSlot = 16
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	roots := make([][]string, slots)
	var mu sync.Mutex
	errs := make(chan error, slots*callsPerSlot)

	wg.Add(slots * callsPerSlot)
	for s := 0; s < slots; s++ {
		slot := fmt.Sprintf("slot-%d", s)
		for c := 0; c < callsPerSlot; c++ {
			go func(s int, slot string) {
				defer wg.Done()
				<-start
				root, err := h.RootFor(slot)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				roots[s] = append(roots[s], root)
				mu.Unlock()
			}(s, slot)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for s := 0; s < slots; s++ {
		want := roots[s][0]
		for _, got := range roots[s] {
			if got != want {
				t.Errorf("slot-%d: RootFor returned %q and %q across concurrent calls, want the same directory every time", s, got, want)
			}
		}
	}
}

func TestKonturSandboxesToolsForIsSafeForManyConcurrentDistinctSlots(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	const (
		slots        = 12
		callsPerSlot = 8
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, slots*callsPerSlot)

	wg.Add(slots * callsPerSlot)
	for s := 0; s < slots; s++ {
		slot := fmt.Sprintf("slot-%d", s)
		for c := 0; c < callsPerSlot; c++ {
			go func(slot string) {
				defer wg.Done()
				<-start
				if _, err := k.ToolsFor(context.Background(), slot); err != nil {
					errs <- fmt.Errorf("%s: %w", slot, err)
				}
			}(slot)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for s := 0; s < slots; s++ {
		if got, want := k.VMNameFor(fmt.Sprintf("slot-%d", s)), fmt.Sprintf("grain-test-slot-%d", s); got != want {
			t.Errorf("VMNameFor(slot-%d) = %q, want %q", s, got, want)
		}
	}
}

// writeFakeKonturWithDelay is writeFakeKontur plus an artificial pause
// inside "vm create" (delay), long enough that two concurrent creates
// only look non-serialized if they were genuinely run in parallel rather
// than one after another. It appends "<name> <start-ns> <end-ns>" to
// timesLog for every "vm create" call, straddling that pause, which
// TestKonturSandboxesCreatesDistinctSlotsVMsConcurrentlyNotSerially reads
// back to check for overlap.
func writeFakeKonturWithDelay(t *testing.T, argvLog, timesLog string, port int, delay time.Duration) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "vm" ] && { [ "$2" = "create" ] || [ "$2" = "delete" ]; }; then
  action="$2"
  name="$3"
  statedir=""
  shift 3
  while [ $# -gt 0 ]; do
    if [ "$1" = "-state-dir" ]; then
      statedir="$2"
    fi
    shift
  done
  if [ "$action" = "create" ]; then
    start=$(date +%%s%%N)
    sleep %f
    end=$(date +%%s%%N)
    echo "$name $start $end" >> %q
    echo "{\"port\": %d}" > "$statedir/$name.json"
  else
    rm -f "$statedir/$name.json"
  fi
fi
`, argvLog, delay.Seconds(), timesLog, port)
	path := filepath.Join(dir, "konturctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestKonturSandboxesCreatesDistinctSlotsVMsConcurrentlyNotSerially holds
// cycle.go's reconcileDispatch doc comment to its word: "HostSandboxes and
// KonturSandboxes both guard their own per-slot state with a mutex keyed
// by slot ... so nothing here needs to hold a lock of its own" -- which is
// exactly what makes -max-concurrent/GRAIN_MAX_CONCURRENT a real
// concurrency knob rather than just a scheduling one (that same comment,
// bwsalmon/agents#435). If KonturSandboxes' locking were actually keyed
// by the whole map instead of by slot, two dispatches landing on distinct,
// freshly-unseen slots at the same time would still create their VMs one
// at a time -- indistinguishable from -max-concurrent=1 for exactly the
// case (a cold sandbox after Recreate, or first use) where creation is
// slow enough to matter. This drives ToolsFor for several distinct slots
// at once, each behind a fake konturctl that sleeps for delay inside "vm
// create", and asserts that at least one pair of those sleeps overlapped
// in wall-clock time -- something full serialization could never produce.
func TestKonturSandboxesCreatesDistinctSlotsVMsConcurrentlyNotSerially(t *testing.T) {
	stateDir := t.TempDir()
	timesLog := filepath.Join(t.TempDir(), "times.log")
	writeFakeKonturWithDelay(t, filepath.Join(t.TempDir(), "kontur-argv.log"), timesLog, 30081, 300*time.Millisecond)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	const slots = 5
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, slots)
	wg.Add(slots)
	for s := 0; s < slots; s++ {
		slot := fmt.Sprintf("slot-%d", s)
		go func(slot string) {
			defer wg.Done()
			<-start
			if _, err := k.ToolsFor(context.Background(), slot); err != nil {
				errs <- fmt.Errorf("%s: %w", slot, err)
			}
		}(slot)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	type interval struct{ start, end int64 }
	intervals := map[string]interval{}
	data, err := os.ReadFile(timesLog)
	if err != nil {
		t.Fatalf("reading %s: %v", timesLog, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("malformed times.log line %q", line)
		}
		s, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parsing start in %q: %v", line, err)
		}
		e, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("parsing end in %q: %v", line, err)
		}
		intervals[fields[0]] = interval{s, e}
	}
	if len(intervals) != slots {
		t.Fatalf("times.log recorded %d distinct VM creates, want %d", len(intervals), slots)
	}

	overlap := false
	names := make([]string, 0, len(intervals))
	for name := range intervals {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			a, b := intervals[names[i]], intervals[names[j]]
			if a.start < b.end && b.start < a.end {
				overlap = true
			}
		}
	}
	if !overlap {
		t.Error("no two of the concurrent slots' \"vm create\" calls overlapped in time -- KonturSandboxes is serializing distinct slots' VM creation behind one lock instead of guarding per-slot state the way cycle.go's reconcileDispatch doc comment says it does")
	}
}
