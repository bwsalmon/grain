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
	"path/filepath"
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
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 0, "127.0.0.1")
	listenTCP(t, 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		SSHKey:            "/key",
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
