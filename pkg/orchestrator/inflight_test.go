// What InFlight itself promises, independent of a cycle: that a run is
// live from before its goroutine starts until its work returns, that
// Wait blocks for exactly that long, and that a Wait which runs out of
// context says so rather than returning as if the drain succeeded.
package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/orchestrator"
)

func TestInFlightWaitReturnsImmediatelyWithNothingLive(t *testing.T) {
	var runs orchestrator.InFlight
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runs.Wait(ctx); err != nil {
		t.Fatalf("Wait on an empty set: %v", err)
	}
}

func TestInFlightTracksARunUntilItReturns(t *testing.T) {
	var runs orchestrator.InFlight
	started := make(chan struct{})
	release := make(chan struct{})

	runs.Go("t1-1", "t1", func() {
		close(started)
		<-release
	})
	<-started

	if got := runs.Len(); got != 1 {
		t.Fatalf("Len while the run is live = %d, want 1", got)
	}
	if got := runs.Runs(); len(got) != 1 || got[0] != "t1-1" {
		t.Fatalf("Runs while the run is live = %v, want [t1-1]", got)
	}

	waited := make(chan error, 1)
	go func() { waited <- runs.Wait(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatalf("Wait returned (%v) while a run was still live", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never returned after the run finished")
	}
	if got := runs.Len(); got != 0 {
		t.Errorf("Len after the run finished = %d, want 0", got)
	}
}

// Busy is what keeps a cycle off a task whose run this process has not
// finished with -- true for as long as the work is, and only for that
// task.
func TestInFlightBusyAnswersForTheTaskARunBelongsTo(t *testing.T) {
	var runs orchestrator.InFlight
	started := make(chan struct{})
	release := make(chan struct{})
	runs.Go("t1-1", "t1", func() {
		close(started)
		<-release
	})
	<-started

	if !runs.Busy("t1") {
		t.Error("Busy(t1) = false while t1's own run is live")
	}
	if runs.Busy("t2") {
		t.Error("Busy(t2) = true with nothing of t2's running")
	}

	close(release)
	if err := runs.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs.Busy("t1") {
		t.Error("Busy(t1) = true after its run returned")
	}
}

func TestInFlightWaitReportsARunThatOutlastsItsContext(t *testing.T) {
	var runs orchestrator.InFlight
	release := make(chan struct{})
	defer close(release)
	runs.Go("t1-1", "t1", func() { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runs.Wait(ctx); err == nil {
		t.Fatal("Wait returned nil for a run that never finished, want the context's error")
	}
}

// A Wait covers runs started while it is waiting too -- "nothing is
// live" is the condition, not "the runs I could see when I asked".
func TestInFlightWaitCoversRunsStartedWhileWaiting(t *testing.T) {
	var runs orchestrator.InFlight
	first := make(chan struct{})
	second := make(chan struct{})
	runs.Go("t1-1", "t1", func() { <-first })

	waited := make(chan error, 1)
	go func() { waited <- runs.Wait(context.Background()) }()

	runs.Go("t2-1", "t2", func() { <-second })
	close(first)
	select {
	case err := <-waited:
		t.Fatalf("Wait returned (%v) with the second run still live", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(second)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never returned after both runs finished")
	}
}
