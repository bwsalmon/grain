package granule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/grain"
)

// Config is everything Run needs that is not the environment.
type Config struct {
	// Root is the mounted tree, grain.Root in a container.
	Root string
	// ClientBinary is the grain client to install into the guest, or ""
	// to install none.
	ClientBinary string
	// TerminationLog is where the final Result is written in addition to
	// the stream. Kubernetes surfaces it in the pod listing, which is
	// the one read that must not be missed.
	TerminationLog string
	// Heartbeat is how often a status record is emitted when nothing has
	// changed. Slow on purpose: the kubelet rotates at 10 MB across 5
	// files and status records would otherwise eat the trajectory's
	// budget.
	Heartbeat time.Duration
	// ReadyTimeout bounds waiting for the guest. It is granule's own
	// backstop and not the controller's ProvisionBudget, which is
	// enforced from outside precisely because a grain wedged here is the
	// one that cannot report being wedged.
	ReadyTimeout time.Duration
}

// DefaultConfig is a granule running in a real container.
func DefaultConfig() Config {
	return Config{
		Root:           grain.Root,
		ClientBinary:   "/usr/local/bin/grain",
		TerminationLog: grain.FileTerminationLog,
		Heartbeat:      30 * time.Second,
		ReadyTimeout:   5 * time.Minute,
	}
}

// Deps are the moving parts, injected so the lifecycle is testable
// without a VM.
type Deps struct {
	VMM   VMM
	Guest Guest
	// Agent runs the agent CLI once the sandbox is provisioned and
	// returns its Result. Nil means there is no agent to run, which is
	// how the provisioning half is exercised on its own.
	Agent func(ctx context.Context, spec grain.Spec, g Guest, out io.Writer) (grain.Result, error)
	Now   func() time.Time
}

// Run is a grain's whole life. It returns the process exit code.
//
// Every ending goes through finish(), so there is exactly one place that
// writes a terminal Status and the termination log -- which matters more
// here than usual, because a granule that dies without writing one is a
// run the controller cannot finish and a slot nothing frees.
func Run(ctx context.Context, cfg Config, deps Deps, stream *Stream) int {
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	st := &state{
		stream: stream,
		now:    now,
		status: grain.Status{
			Version: grain.Version,
			Phase:   grain.PhaseProvisioning,
			Since:   now().UTC(),
		},
	}
	st.emitLocked()

	spec, err := grain.SpecFromEnv(grain.Getenv)
	if err != nil {
		// Before anything booted, so there is nothing to shut down. A
		// wire version this build does not speak is the one failure that
		// must be distinguishable from a bad setup script, and it is
		// distinguishable by exit code (see docs/grain.md, "Exit codes").
		st.finish(cfg, grain.PhaseFailed, grain.Result{
			Outcome: grain.OutcomeSetupFailed,
			Detail:  err.Error(),
		})
		return ExitWireVersion
	}

	// MaxRuntime is the grain's own bound, enforced here rather than
	// trusted to the controller: it is defence against a controller that
	// stopped ticking, so it cannot depend on one.
	if spec.MaxRuntime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.MaxRuntime))
		defer cancel()
	}

	console := stream.LineWriter(grain.SrcConsole)
	if err := deps.VMM.Start(ctx, console); err != nil {
		st.finish(cfg, grain.PhaseFailed, grain.Result{
			Outcome: grain.OutcomeSetupFailed,
			Detail:  err.Error(),
		})
		return ExitFailed
	}
	// Shutting the VM down is the last thing that happens on every path,
	// including a panic: the container is about to die either way, but a
	// guest powered off cleanly is a disk that does not need repair on
	// the next boot from the same image.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = deps.VMM.Shutdown(sctx)
		if c, ok := console.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	st.activity("waiting for the guest")
	if err := waitReady(ctx, deps.Guest, cfg.ReadyTimeout, now); err != nil {
		st.finish(cfg, grain.PhaseFailed, grain.Result{
			Outcome: grain.OutcomeSetupFailed,
			Detail:  err.Error(),
		})
		return ExitFailed
	}

	st.activity("provisioning the sandbox")
	plan, err := PlanProvision(cfg.Root, cfg.ClientBinary)
	if err != nil {
		st.finish(cfg, grain.PhaseFailed, grain.Result{
			Outcome: grain.OutcomeSetupFailed,
			Detail:  err.Error(),
		})
		return ExitFailed
	}
	blob, err := plan.Tar()
	if err == nil {
		err = deps.Guest.Unpack(ctx, bytes.NewReader(blob))
	}
	if err != nil {
		st.finish(cfg, grain.PhaseFailed, grain.Result{
			Outcome: grain.OutcomeSetupFailed,
			Detail:  err.Error(),
		})
		return ExitFailed
	}

	if plan.Setup {
		st.activity("running setup")
		res, err := runSetup(ctx, deps.Guest)
		if err != nil {
			st.finish(cfg, grain.PhaseFailed, grain.Result{
				Outcome: grain.OutcomeSetupFailed,
				Detail:  err.Error(),
			})
			return ExitFailed
		}
		st.setup(res)
		if res.ExitCode != 0 {
			// The whole point of gating the agent on this: a failed
			// checkout costs no model tokens.
			st.finish(cfg, grain.PhaseFailed, grain.Result{
				Outcome: grain.OutcomeSetupFailed,
				Detail:  fmt.Sprintf("setup exited %d", res.ExitCode),
			})
			return ExitFailed
		}
	}

	// Straight from provisioning to running: the prompt is a file the
	// grain already has, so there is nothing to wait for in between.
	//
	// Carrying the guest's own last word across the transition rather
	// than blanking it: setup is the phase with no agent in it, so
	// whatever the sandbox last said about itself is the only account
	// there is, and a grain that reached "running" with an empty
	// activity would have thrown it away at the moment it became the
	// most recent thing known. It is also the one read that does not
	// depend on a heartbeat having ticked.
	st.enter(grain.PhaseRunning, readActivity(ctx, deps.Guest))

	stop := st.heartbeat(ctx, deps.Guest, cfg.Heartbeat)
	defer stop()

	if deps.Agent == nil {
		// No agent configured. Not an error: it is how the provisioning
		// half is run on its own, and it ends the way a grain with
		// nothing to do should -- with a Result, not by hanging.
		st.finish(cfg, grain.PhaseSucceeded, grain.Result{Outcome: grain.OutcomeNoAction})
		return ExitOK
	}

	result, err := deps.Agent(ctx, spec, deps.Guest, stream.LineWriter(grain.SrcAgent))
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// MaxRuntime. The grain ends itself rather than being destroyed
		// mid-thought, and says so.
		result.Outcome = grain.OutcomeFailed
		if result.Detail == "" {
			result.Detail = "the grain reached GRAIN_MAX_RUNTIME"
		}
		st.finish(cfg, grain.PhaseFailed, result)
		return ExitFailed
	case err != nil:
		if result.Outcome == "" {
			result.Outcome = grain.OutcomeFailed
		}
		if result.Detail == "" {
			result.Detail = err.Error()
		}
		st.finish(cfg, grain.PhaseFailed, result)
		return ExitFailed
	}
	if result.Outcome == "" {
		result.Outcome = grain.OutcomeSucceeded
	}
	phase := grain.PhaseSucceeded
	if result.Outcome != grain.OutcomeSucceeded {
		phase = grain.PhaseFailed
	}
	st.finish(cfg, phase, result)
	if phase == grain.PhaseSucceeded {
		return ExitOK
	}
	return ExitFailed
}

// Process exit codes, read where the runtime reports them rather than
// from any call into a grain (docs/grain.md, "Exit codes").
const (
	ExitOK = 0
	// ExitFailed is any ending the grain reached and described. The
	// Result on the stream and in the termination log is the detail.
	ExitFailed = 1
	// ExitWireVersion is an environment written to a wire version this
	// build does not speak, refused before anything boots. Distinct
	// because the alternative -- a generic failure -- is
	// indistinguishable from a bad setup script, and the two want
	// different responses.
	ExitWireVersion = 4
)

// waitReady polls the guest until it answers. The poll is here rather
// than in Guest because Guest.Ready is one attempt by design: the caller
// is the one narrating the wait, and a probe that waited too would only
// be racing it.
func waitReady(ctx context.Context, g Guest, budget time.Duration, now func() time.Time) error {
	deadline := now().Add(budget)
	var last error
	for {
		if err := g.Ready(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for the guest: %w", ctx.Err())
		}
		if !now().Before(deadline) {
			return fmt.Errorf("the guest did not come up within %s: %w", budget, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the guest: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// setupOutputLimit bounds what a SetupResult carries. The controller
// wrote the script and reads this to find out what went wrong, so it
// wants the end -- where the error is -- rather than the beginning.
const setupOutputLimit = 16 << 10

// runSetup runs the controller's script in the guest and reports how it
// ended, without reading it: the controller wrote that script, so it is
// the one that knows what its output means.
func runSetup(ctx context.Context, g Guest) (grain.SetupResult, error) {
	var out bytes.Buffer
	// Combined, because a setup script's diagnosis is routinely
	// interleaved across both and the reader is trying to find out what
	// went wrong.
	code, err := g.Exec(ctx, []string{GuestSetupPath}, &out, &out)
	if err != nil {
		return grain.SetupResult{}, fmt.Errorf("running the setup script: %w", err)
	}
	return grain.SetupResult{ExitCode: code, Output: tail(out.String(), setupOutputLimit)}, nil
}

// tail keeps the last n bytes, marking the cut so a reader is not left
// wondering whether a script really began mid-word.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[... truncated ...]\n" + s[len(s)-n:]
}

// state is the one place a Status is mutated, so that every emission is
// a whole snapshot of the same value rather than several writers'
// partial views.
//
// Guarded, because two goroutines write here: the lifecycle below and
// the heartbeat, which refreshes what the sandbox says about itself on
// its own tick. Without the mutex a status could be marshalled
// half-updated -- an ending with the previous phase's activity still on
// it -- which is exactly the sort of thing a controller reads once and
// acts on.
type state struct {
	stream *Stream
	now    func() time.Time

	mu     sync.Mutex
	status grain.Status
	done   bool
}

// emit writes a whole snapshot. Caller holds mu.
func (s *state) emit() {
	s.status.Seq = s.stream.Seq()
	_, _ = s.stream.Emit(grain.SrcShim, grain.KindStatus, s.status)
}

func (s *state) emitLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit()
}

func (s *state) setup(res grain.SetupResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Setup = &res
}

func (s *state) activity(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Activity == what {
		return
	}
	s.status.Activity = what
	s.emit()
}

func (s *state) enter(p grain.Phase, activity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase, s.status.Since, s.status.Activity = p, s.now().UTC(), activity
	_, _ = s.stream.Emit(grain.SrcShim, grain.KindPhase, string(p))
	s.emit()
}

// finish writes the one ending. Idempotent, because several paths can
// reach it and a grain that reported two Results would have the
// controller believe whichever it read last.
func (s *state) finish(cfg Config, p grain.Phase, r grain.Result) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.status.Phase, s.status.Since, s.status.Result = p, s.now().UTC(), &r
	s.status.Activity = ""
	_, _ = s.stream.Emit(grain.SrcShim, grain.KindPhase, string(p))
	s.emit()
	s.mu.Unlock()

	if cfg.TerminationLog == "" {
		return
	}
	body, err := json.Marshal(r)
	if err != nil {
		return
	}
	// Best effort by design: under docker nothing reads this file, and a
	// grain that could not write it has already put the same Result on
	// the stream.
	_ = os.WriteFile(cfg.TerminationLog, body, 0o644)
}

// finished reports whether the ending is already written, so the
// heartbeat can stop rather than emit a status after the terminal one.
func (s *state) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// heartbeat emits a status on a slow tick, refreshing the two things
// that come from inside the sandbox: its health, and whatever the work
// has said about itself through grain.GuestActivityFile.
//
// The activity file is read on this round trip rather than on one of its
// own, which is the whole reason it is a file: the shim is already
// asking the guest how it is, so one more path costs nothing and
// inherits a cadence that is already tuned.
func (s *state) heartbeat(ctx context.Context, g Guest, every time.Duration) func() {
	if every <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s.finished() {
					return
				}
				// Read outside the lock: it is a vsock round trip, and
				// holding the status still for its duration would block
				// the lifecycle behind the guest's responsiveness --
				// which is the one thing a heartbeat must not do.
				a := readActivity(ctx, g)
				s.mu.Lock()
				if s.done {
					s.mu.Unlock()
					return
				}
				if a != "" {
					s.status.Activity = a
				}
				s.emit()
				s.mu.Unlock()
			}
		}
	}()
	return cancel
}

// activityLimit bounds what the guest can put on a task row. It is a
// phrase for a human, and anything longer is a program having written
// its log to the wrong file.
const activityLimit = 200

func readActivity(ctx context.Context, g Guest) string {
	body, err := g.ReadFile(ctx, grain.GuestActivityFile)
	if err != nil || len(body) == 0 {
		return ""
	}
	line := string(body)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > activityLimit {
		line = line[:activityLimit]
	}
	return line
}
