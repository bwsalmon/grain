package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
)

func baseNow() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// The whole point of the type: one run's limit stops every other run in
// flight, rather than leaving each to spend an agent's worth of
// wall-clock time meeting the same refusal.
func TestBeginCancelsEveryLiveRun(t *testing.T) {
	now := baseNow()
	p := &Pause{}

	ctxA, cancelA := context.WithCancelCause(context.Background())
	defer cancelA(nil)
	ctxB, cancelB := context.WithCancelCause(context.Background())
	defer cancelB(nil)
	p.register("7-1", now, cancelA)
	p.register("8-1", now, cancelB)

	p.Begin(now, &agent.UsageLimitError{Framework: "claude", ResetAt: now.Add(time.Hour)})

	for name, ctx := range map[string]context.Context{"7-1": ctxA, "8-1": ctxB} {
		select {
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), errUsageLimit) {
				t.Errorf("run %s cause = %v, want errUsageLimit", name, context.Cause(ctx))
			}
		default:
			t.Errorf("run %s was left running through a usage-limit pause", name)
		}
	}
}

// A run that has already finished must not be cancelled by a pause its
// own limit began -- RunDispatch unregisters before it reports one, and
// this is the property that ordering exists for.
func TestUnregisteredRunsAreNotCancelled(t *testing.T) {
	now := baseNow()
	p := &Pause{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	stop := p.register("7-1", now, cancel)
	stop()
	p.Begin(now, &agent.UsageLimitError{})

	select {
	case <-ctx.Done():
		t.Errorf("an unregistered run was cancelled: %v", context.Cause(ctx))
	default:
	}
}

// Dispatch is gated for as long as the provider said, and opens again on
// its own once a later tick asks after the instant has passed.
func TestBlockedHoldsDispatchUntilTheWindowResets(t *testing.T) {
	now := baseNow()
	p := &Pause{}

	if _, _, blocked := p.Blocked(now); blocked {
		t.Fatal("a fresh Pause blocks dispatch")
	}

	until := p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(time.Hour)})
	if want := now.Add(time.Hour); !until.Equal(want) {
		t.Errorf("Begin = %s, want the provider's own reset %s", until, want)
	}
	if gotUntil, _, blocked := p.Blocked(now.Add(59 * time.Minute)); !blocked {
		t.Errorf("Blocked before the reset = %s, false; want dispatch held", gotUntil)
	}
	if _, _, blocked := p.Blocked(now.Add(time.Hour)); blocked {
		t.Error("Blocked at the reset instant still holds dispatch")
	}
	if gotUntil, _ := p.Until(); !gotUntil.IsZero() {
		t.Errorf("Until = %s after the pause expired, want it cleared", gotUntil)
	}
}

// Two runs meeting the same limit within a second of each other is the
// ordinary case -- they were in flight together -- and the second must
// not be able to shorten the first's wait.
func TestBeginKeepsTheLaterReset(t *testing.T) {
	now := baseNow()
	p := &Pause{}

	p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(3 * time.Hour)})
	got := p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(2 * time.Hour)})

	if want := now.Add(3 * time.Hour); !got.Equal(want) {
		t.Errorf("Begin = %s, want the longer wait %s kept", got, want)
	}
	if _, _, blocked := p.Blocked(now.Add(150 * time.Minute)); !blocked {
		t.Error("the shorter second limit shortened the pause")
	}
}

func TestBeginExtendsToALaterReset(t *testing.T) {
	now := baseNow()
	p := &Pause{}

	p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(time.Hour)})
	got := p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(4 * time.Hour)})

	if want := now.Add(4 * time.Hour); !got.Equal(want) {
		t.Errorf("Begin = %s, want the pause extended to %s", got, want)
	}
}

// A run dispatched by the cycle that was already in flight when the
// pause began has nowhere else to be caught.
func TestRegisterCancelsARunStartingIntoAPause(t *testing.T) {
	now := baseNow()
	p := &Pause{}
	p.Begin(now, &agent.UsageLimitError{ResetAt: now.Add(time.Hour)})

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.register("9-1", now.Add(time.Minute), cancel)

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errUsageLimit) {
			t.Errorf("cause = %v, want errUsageLimit", context.Cause(ctx))
		}
	default:
		t.Error("a run registered during a pause was left running")
	}
}

func TestResumeAtBoundsWhatTheProviderSaid(t *testing.T) {
	now := baseNow()
	for _, tc := range []struct {
		name  string
		limit *agent.UsageLimitError
		want  time.Duration
	}{
		{"no reset at all", &agent.UsageLimitError{}, defaultUsageLimitPause},
		{"nil limit", nil, defaultUsageLimitPause},
		{"a delay of seconds", &agent.UsageLimitError{RetryAfter: 5 * time.Second}, minUsageLimitPause},
		{"a reset in the past", &agent.UsageLimitError{ResetAt: now.Add(-time.Hour)}, defaultUsageLimitPause},
		{"an absurd reset", &agent.UsageLimitError{ResetAt: now.Add(4000 * time.Hour)}, maxUsageLimitPause},
		{"an honest reset", &agent.UsageLimitError{ResetAt: now.Add(2 * time.Hour)}, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resumeAt(now, tc.limit)
			if want := now.Add(tc.want); !got.Equal(want) {
				t.Errorf("resumeAt = %s, want %s", got, want)
			}
		})
	}
}

// Every method has to tolerate the nil a caller with no loop to pause
// wires (Config.Pause), since RunDispatch calls all of them unguarded.
func TestNilPauseIsUsable(t *testing.T) {
	var p *Pause
	stop := p.register("7-1", baseNow(), func(error) { t.Error("a nil Pause cancelled a run") })
	stop()
	if until := p.Begin(baseNow(), &agent.UsageLimitError{}); !until.IsZero() {
		t.Errorf("Begin on a nil Pause = %s, want the zero time", until)
	}
	if _, _, blocked := p.Blocked(baseNow()); blocked {
		t.Error("a nil Pause blocked dispatch")
	}
	if until, _ := p.Until(); !until.IsZero() {
		t.Errorf("Until on a nil Pause = %s", until)
	}
}
