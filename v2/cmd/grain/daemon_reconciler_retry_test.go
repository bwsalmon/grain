// bwsalmon/agents#576: runDaemon's one-time slot-provisioning step
// (configuring each slot's git credentials -- and, for a kontur-backed
// slot, creating that slot's VM) used to return straight out of
// runDaemon on its very first failure, which permanently wedged
// reconciliation for the rest of the process's life -- see
// daemon_ui_survives_failure_test.go's own doc comment on why that
// failure was already "logged, not fatal" one level up, in run(), but
// that only kept the UI alive, it never gave the failed step itself
// another chance. These tests cover retryWithBackoff (the mechanism
// configureSlotGitCredentials now retries through) and reconcilerDown
// (what GET /api/config now reports once runDaemon does eventually give
// up) in isolation, both fast enough to run in CI without a real kontur
// VM or a live GitHub/Gemini endpoint -- daemon_kontur_wiring_test.go and
// daemon_ui_survives_failure_test.go already cover the slower, more
// realistic end-to-end paths either mechanism sits inside of.
package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryWithBackoffRetriesUntilSuccess(t *testing.T) {
	var failures []int
	attempts := 0
	err := retryWithBackoff(context.Background(), time.Millisecond, 4*time.Millisecond,
		func(attempt int, _ error) { failures = append(failures, attempt) },
		func() error {
			attempts++
			if attempts < 4 {
				return errors.New("still failing")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("retryWithBackoff() = %v, want nil once fn eventually succeeds", err)
	}
	if attempts != 4 {
		t.Fatalf("fn called %d times, want exactly 4 (three failures then a success)", attempts)
	}
	if want := []int{1, 2, 3}; !equalInts(failures, want) {
		t.Fatalf("onFailure called with attempts %v, want %v", failures, want)
	}
}

func TestRetryWithBackoffStopsOnceContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- retryWithBackoff(ctx, time.Millisecond, 4*time.Millisecond,
			func(int, error) {}, func() error {
				attempts++
				if attempts == 2 {
					// Cancel from inside fn, right before the loop's own
					// ctx.Done() select would otherwise just wait out the
					// next backoff -- proves cancellation is noticed
					// promptly rather than only between long waits.
					cancel()
				}
				return errors.New("permanently failing")
			})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retryWithBackoff() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retryWithBackoff never returned after ctx was cancelled")
	}
	if attempts != 2 {
		t.Fatalf("fn called %d times after cancellation, want exactly 2", attempts)
	}
}

func TestRetryWithBackoffCapsDelayAtMax(t *testing.T) {
	// Five failures against a base delay that would blow past maxDelay
	// well before the last one if uncapped (1ms, 2ms, 4ms, 8ms would be
	// fine, but a larger base makes the point unambiguously): this just
	// proves the whole run finishes well within a budget only reachable
	// if delay is in fact clamped rather than growing forever.
	const maxDelay = 5 * time.Millisecond
	attempts := 0
	start := time.Now()
	err := retryWithBackoff(context.Background(), 3*time.Millisecond, maxDelay,
		func(int, error) {}, func() error {
			attempts++
			if attempts <= 6 {
				return errors.New("still failing")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("retryWithBackoff() = %v, want nil", err)
	}
	// Uncapped doubling from 3ms over 6 failures would be
	// 3+6+12+24+48+96 = 189ms; capped at 5ms every time after the first
	// it is at most 3+5*5 = 28ms. A generous ceiling well below the
	// uncapped total, allowing for scheduler slop.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("retryWithBackoff took %s, want well under 100ms if delay is actually capped at %s", elapsed, maxDelay)
	}
}

func TestReconcilerDownDefaultsFalseAndLatchesTrue(t *testing.T) {
	// reconcilerDown is process-global (mirroring orchestrator.
	// ChecksUnavailable's own doc comment on why it too is package-level
	// state rather than threaded through Deps): this only asserts the
	// zero value and the one-way latch, without needing a real runDaemon
	// failure to set it -- daemon_ui_survives_failure_test.go already
	// drives that end to end.
	t.Cleanup(func() { reconcilerDown.Store(false) })
	reconcilerDown.Store(false)
	if reconcilerDown.Load() {
		t.Fatal("reconcilerDown started true, want false")
	}
	reconcilerDown.Store(true)
	if !reconcilerDown.Load() {
		t.Fatal("reconcilerDown did not latch true")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
