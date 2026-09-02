// loadtest_test.go is bwsalmon/agents#416's v2 equivalent of v1's own
// tests/loadtest.py (docs/roadmap.md item 6, "actually run, not just
// built"): drive orchestrator.RunCycle under sustained concurrent load --
// many tasks, across many repos, many slots dispatching at once, while
// several independent goroutines write to the very same real, on-disk
// SQLite store the way pkg/ui's own concurrently served HTTP handlers
// really would while RunCycle is, at that same moment, ticking against
// it -- to catch scheduling starvation, sqlite contention
// (pkg/model/sqlite's own doc comment: "the single writer for the whole
// store"), and a capability provider leaking a resource it minted. None
// of the ordinary suite's fixed, small scenarios exercise any of these
// three at real scale: e2e_test.go and simulate_test.go use three slots
// and a couple dozen tasks at most, and even random_test.go's own long
// mode (TestRandomizedClusterLong) drives one store writer at a time,
// taking turns with a CLI subprocess by design (withStore's own doc
// comment) rather than ever letting the store see genuinely concurrent
// callers the way this file's runLoadWriter does.
//
// v1's own version of this test booted real VMs, because the question it
// answered (docs/design.md open question 2) was a host-sizing one: does
// a 4-vCPU host hold a controller plus two fully provisioned sandbox VMs
// under real concurrent `kind create cluster` and build load. v2 has no
// equivalent question to answer by booting anything -- its default
// deployment dispatches onto plain host directories, not VMs
// (terraform/gcp-v2/variables.tf's own machine_type comment: "this VM
// does not run nested virtualization"), and its target host shape is a
// single e2-standard-2 (2 vCPU, 8 GB) instance running one grain daemon
// against a handful of repos. What that deployment actually risks at
// scale is the orchestrator/store layer itself, which is what this file
// is a load test of -- a redesign for what v2 is actually built to run,
// not a port of v1's VM-boot mechanics onto a shape v2 does not have.
//
// This harness never has a dispatch push a real branch. That is a
// deliberate scope cut, not an oversight: the git/pull-request path is
// already exercised at real scale by e2e_test.go, random_test.go and
// pkg/orchestrator/live_test.go, and pushing a branch touches none of
// the three things bwsalmon/agents#416 actually asks this file to catch.
// Every dispatch here ends by closing the task with a comment, failing
// (to exercise dispatch.Cycle's own retry backoff), or asking a question
// (to exercise the awaiting_reply/reply cycle) -- see loadGenerator in
// loadtest_helpers_test.go. All three run through exactly the same
// dispatch.Cycle, RunDispatch, capability materialize/revoke and
// FinishRun code a push would.
//
// Skipped unless GRAIN_LOAD_TEST is set, the same convention
// random_test.go's TestRandomizedClusterLong already established in this
// exact package for "a test worth running by hand on a sized host, never
// in CI" -- go-test.yml's own `go test ./...` step still compiles this
// file on every pull request, so a change that breaks it to build is
// still caught there; only actually running it is opt in. Run it:
//
//	GRAIN_LOAD_TEST=1 go test ./e2e/... -run TestLoadSustainedConcurrency -v -timeout 15m
//
// or `make loadtest` (Makefile). This file's own defaults finish in a
// couple of minutes on a modern laptop; every dimension is sized
// up independently through an env var (loadConfigFromEnv, below) for an
// actual run against v2's own target deployment shape -- see v2/README.md's
// own pointer to this file for suggested sizing on an e2-standard-2.
package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

// loadConfig is every dimension v1's own CLI flags (--sandboxes,
// --interval, ...) let a caller size to the host it actually ran
// on -- an env var apiece here instead of flags, since `go test` is how
// this file is invoked either way.
type loadConfig struct {
	slots         int // the concurrency limit; still named for the env var that sets it
	repos         int
	writers       int
	initialTasks  int
	duration      time.Duration
	drainTimeout  time.Duration
	revokeFailPct float64
	seed          uint64
}

func loadConfigFromEnv(t *testing.T) loadConfig {
	t.Helper()
	cfg := loadConfig{
		slots: 12, repos: 6, writers: 4, initialTasks: 48,
		duration: 30 * time.Second, drainTimeout: 120 * time.Second,
		revokeFailPct: 0.1, seed: uint64(time.Now().UnixNano()),
	}
	envInt(t, "GRAIN_LOAD_TEST_SLOTS", &cfg.slots)
	envInt(t, "GRAIN_LOAD_TEST_REPOS", &cfg.repos)
	envInt(t, "GRAIN_LOAD_TEST_WRITERS", &cfg.writers)
	envInt(t, "GRAIN_LOAD_TEST_INITIAL_TASKS", &cfg.initialTasks)
	envDuration(t, "GRAIN_LOAD_TEST_DURATION", &cfg.duration)
	envDuration(t, "GRAIN_LOAD_TEST_DRAIN", &cfg.drainTimeout)
	if v := os.Getenv("GRAIN_LOAD_TEST_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("GRAIN_LOAD_TEST_SEED: %v", err)
		}
		cfg.seed = n
	}
	return cfg
}

func envInt(t *testing.T, name string, dst *int) {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	*dst = n
}

func envDuration(t *testing.T, name string, dst *time.Duration) {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	*dst = d
}

func TestLoadSustainedConcurrency(t *testing.T) {
	if os.Getenv("GRAIN_LOAD_TEST") == "" {
		t.Skip("set GRAIN_LOAD_TEST=1 to run the sustained concurrent-load harness (see this file's own doc comment)")
	}
	cfg := loadConfigFromEnv(t)
	t.Logf("load config: %+v", cfg)

	dataDir := t.TempDir()
	db, err := sqlite.Open(sqlite.DefaultConfig(dataDir))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	repos := make([]model.RepoRef, cfg.repos)
	for i := range repos {
		repos[i] = model.RepoRef{Owner: "acme", Name: fmt.Sprintf("repo-%d", i)}
	}

	gh := newLoadGitHub()
	capability := newLoadCapability(cfg.revokeFailPct, 2*time.Second)
	registry := model.NewCapabilityRegistry(capability)
	metrics := newLoadMetrics()

	tickerRNG := rand.New(rand.NewPCG(cfg.seed, cfg.seed^0x9e3779b97f4a7c15))
	var tickerRNGMu sync.Mutex
	var taskSeq atomic.Int64

	deps := orchestrator.Deps{
		Store: store, Client: gh, Sandboxes: sandboxes,
		Framework: func() agent.Framework {
			return antigravity.NewForTest(newLoadGenerator(&tickerRNGMu, tickerRNG, metrics))
		},
		Config:        orchestrator.Config{Capabilities: registry},
		MaxConcurrent: cfg.slots,
	}

	// Seed a backlog before the sustained phase starts, so every slot has
	// something to do from the very first tick rather than ramping up
	// from zero.
	for i := 0; i < cfg.initialTasks; i++ {
		createLoadTask(t, ctx, store, &taskSeq, repos, tickerRNG, metrics)
	}

	var creating atomic.Bool
	creating.Store(true)
	stopWriters := make(chan struct{})
	var writers sync.WaitGroup
	for i := 0; i < cfg.writers; i++ {
		writers.Add(1)
		go func(id int) {
			defer writers.Done()
			runLoadWriter(t, id, store, repos, &taskSeq, metrics, &creating, stopWriters)
		}(i)
	}

	// The sustained-load phase: tick RunCycle back to back (no
	// poll-interval delay -- this is a stress test, not a realistic
	// cadence) for cfg.duration while cfg.writers goroutines create and
	// mutate tasks concurrently through their own store connections.
	deadline := time.Now().Add(cfg.duration)
	for time.Now().Before(deadline) {
		runOneTick(t, ctx, store, deps, metrics)
	}

	// Drain: stop creating new tasks, but let writers keep approving,
	// replying and closing so a parked or proposed task still has a way
	// to resolve, and keep ticking until the queue empties or
	// cfg.drainTimeout runs out.
	creating.Store(false)
	drainDeadline := time.Now().Add(cfg.drainTimeout)
	for time.Now().Before(drainDeadline) {
		if storeIsQuiet(t, ctx, store) {
			break
		}
		runOneTick(t, ctx, store, deps, metrics)
	}

	// Only now do writers actually stop -- closing and waiting on them
	// before this loop's own last check would leave a race exactly like
	// the one this harness caught while this file was still being
	// written: a writer approving or reopening a task in the gap between
	// that check and stopWriters closing leaves a freshly ready task no
	// further tick would ever see. A short, writer-free tail below mops
	// up anything created in that last window instead.
	close(stopWriters)
	writers.Wait()
	for i := 0; i < 5 && !storeIsQuiet(t, ctx, store); i++ {
		runOneTick(t, ctx, store, deps, metrics)
	}

	assertDrainedCleanly(t, ctx, store)
	reportConcurrency(t, metrics)
	reportCapability(t, ctx, capability)
	reportMetrics(t, metrics)
}

// storeIsQuiet reports whether nothing is ready and no slot is occupied
// -- draining's own stopping condition, used both while writers may still
// be creating work and, briefly, once they cannot be anymore.
func storeIsQuiet(t *testing.T, ctx context.Context, store *model.Store) bool {
	t.Helper()
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatalf("LiveRunCount: %v", err)
	}
	return len(ready) == 0 && occupied == 0
}

func runOneTick(t *testing.T, ctx context.Context, store *model.Store, deps orchestrator.Deps, metrics *loadMetrics) {
	t.Helper()
	tickCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	if err := orchestrator.RunCycle(tickCtx, deps, start.UTC()); err != nil {
		// A writer's own "close" action deliberately avoids a task it
		// last saw running (doLoadWriterAction's own comment), but the
		// read-then-act gap between that check and the close landing is
		// real, so this race still happens occasionally: RunDispatch's
		// own cancel-on-close (run.go's errTaskClosed) firing exactly
		// when a task is closed out from under a run genuinely still
		// live. That is expected, already-tested behavior here, not a
		// failure this file exists to catch -- see reconcileDispatch's
		// own doc comment on why one dispatch's error never fails the
		// cycle either.
		if !strings.Contains(err.Error(), "task closed while its run was still live") {
			t.Fatalf("RunCycle: %v", err)
		}
		t.Logf("RunCycle (tolerated -- close/cancel race): %v", err)
	}
	metrics.recordTick(time.Since(start))
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	metrics.observeReadyTasks(ready, time.Now())
}

// createLoadTask files one task directly (bypassing the slower ui.Client
// round trip -- fine for the initial backlog and for a writer's own
// "create" action, since ui.Client.CreateTask's extra bookkeeping,
// dependsOnLinks/grantsFor, has nothing to resolve for a task with no
// dependencies and a capability granted directly). 80% land already
// approved (ready at once); the rest land proposed, giving writers'
// "approve" action something to do. 30% carry the fake capability grant,
// enough to make an outstanding-resource leak or a successful reap
// visible without every single dispatch paying for one.
func createLoadTask(t *testing.T, ctx context.Context, store *model.Store, seq *atomic.Int64,
	repos []model.RepoRef, rng *rand.Rand, metrics *loadMetrics) {
	t.Helper()
	id := fmt.Sprintf("load-%07d", seq.Add(1))
	repo := repos[rng.IntN(len(repos))]
	approved := rng.Float64() < 0.8
	withCapability := rng.Float64() < 0.3
	now := time.Now().UTC()
	task := newLoadTask(id, repo, approved, withCapability, now)
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing %s: %v", id, err)
	}
	metrics.taskFiled()
	if approved {
		metrics.taskBecameReady(id, now)
	}
}

func newLoadTask(id string, repo model.RepoRef, approved, withCapability bool, now time.Time) model.Task {
	var actor model.Principal
	var reason model.OriginReason
	if approved {
		// model.LandsQueued: a human actor lands queued outright.
		actor = model.Principal{Kind: model.PrincipalHuman, ID: "load-operator"}
		reason = model.ReasonDirect
	} else {
		actor = model.Principal{Kind: model.PrincipalAutomation, ID: "load-generator"}
		reason = model.ReasonProposal
	}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "load task " + id,
		Origin:    model.Origin{Attribution: model.Attribution{Actor: actor}, Reason: reason},
		Binding:   model.BindingDirective,
		Target:    &repo,
		CreatedAt: &now,
	}
	if approved {
		task.Approval = &model.Attribution{Actor: actor}
	}
	if withCapability {
		task.Grants = []model.Grant{{Capability: loadCapabilityName, Via: model.GrantByGrain}}
	}
	return task
}

// runLoadWriter is this harness's stand-in for pkg/ui's own concurrently
// served HTTP handlers (pkg/ui's own doc comment: "that process still
// serves concurrent requests ... safe to run concurrently") -- several
// goroutines calling into the very same *model.Store the RunCycle loop
// above is also, at that same moment, ticking against. That is v2's
// actual concurrency shape today: one process, one *model.Store, the UI's
// own concurrent request goroutines and the reconcile loop all sharing
// it (cmd/grain/daemon.go's own doc comment -- the CLI is a REST client
// of that same UI now, not a second direct writer of the file, so this
// harness does not open a second *sql.DB to stand in for it). What still
// makes this a real stress on pkg/model/sqlite's "single writer" design
// is that database/sql opens more than one underlying connection to the
// same file when called from more than one goroutine at once, so
// SQLite's own file lock (_txlock=immediate, busy_timeout) is fully in
// play regardless of every caller sharing one *model.Store Go value --
// nothing in the ordinary suite ever drives that store concurrently with
// a live RunCycle tick the way this file does (withStore's own doc
// comment: existing tests take turns by design).
func runLoadWriter(t *testing.T, id int, store *model.Store, repos []model.RepoRef, seq *atomic.Int64,
	metrics *loadMetrics, creating *atomic.Bool, stop <-chan struct{}) {
	t.Helper()
	seed := uint64(id)*0x9e3779b97f4a7c15 + 1
	rng := rand.New(rand.NewPCG(seed, seed^0xff51afd7ed558ccd))

	for {
		select {
		case <-stop:
			return
		default:
		}
		doLoadWriterAction(context.Background(), store, rng, repos, seq, metrics, creating.Load())
		time.Sleep(time.Duration(50+rng.IntN(100)) * time.Millisecond)
	}
}

func doLoadWriterAction(ctx context.Context, store *model.Store, rng *rand.Rand, repos []model.RepoRef,
	seq *atomic.Int64, metrics *loadMetrics, creating bool) {

	states, err := store.States(ctx)
	if err != nil {
		return // a transient read failure costs this one action, nothing more
	}
	client := ui.NewClient(ui.Config{Actor: ui.DefaultActor("load-writer")}, store)

	roll := rng.Float64()
	switch {
	case creating && roll < 0.2:
		id := fmt.Sprintf("load-%07d", seq.Add(1))
		repo := repos[rng.IntN(len(repos))]
		approved := rng.Float64() < 0.8
		withCapability := rng.Float64() < 0.3
		now := time.Now().UTC()
		start := time.Now()
		err := store.PutTask(ctx, newLoadTask(id, repo, approved, withCapability, now))
		metrics.recordWrite(time.Since(start), err)
		if err == nil {
			metrics.taskFiled()
			if approved {
				metrics.taskBecameReady(id, now)
			}
		}
	case roll < 0.45:
		if id, ok := loadPickState(states, model.StateProposed, rng); ok {
			start := time.Now()
			err := client.Approve(ctx, id)
			metrics.recordWrite(time.Since(start), benignOrErr(err))
			if err == nil {
				metrics.taskBecameReady(id, time.Now())
			}
		}
	case roll < 0.65:
		if id, ok := loadPickState(states, model.StateAwaitingReply, rng); ok {
			start := time.Now()
			err := client.AddComment(ctx, id, "here is some direction", nil)
			metrics.recordWrite(time.Since(start), benignOrErr(err))
			if err == nil {
				metrics.taskBecameReady(id, time.Now())
			}
		}
	case roll < 0.85:
		// Never a currently running task: cancel-on-close
		// (RunDispatch's own watchForTaskClosed) is a real, already
		// separately tested code path, not one of the three things this
		// file exists to catch, and racing it in here would only turn
		// RunCycle's own reported error into noise this test would
		// otherwise have to specifically parse out again.
		if id, ok := loadPickWhere(states, rng, func(s model.State) bool {
			return s != model.StateClosed && s != model.StateRunning
		}); ok {
			start := time.Now()
			err := client.Close(ctx, id)
			metrics.recordWrite(time.Since(start), benignOrErr(err))
		}
	default:
		if id, ok := loadPickState(states, model.StateClosed, rng); ok {
			start := time.Now()
			err := client.Reopen(ctx, id)
			metrics.recordWrite(time.Since(start), benignOrErr(err))
			if err == nil {
				metrics.taskBecameReady(id, time.Now())
			}
		}
	}
}

// benignOrErr drops a *ui.NotFoundError or *ui.ValidationError to nil --
// both are an ordinary, expected outcome of several goroutines picking
// among the same tasks at random (one writer closes a task between
// another reading states and acting on it), not a sign of sqlite
// contention, which is what metrics.recordWrite's own error count exists
// to catch.
func benignOrErr(err error) error {
	switch err.(type) {
	case *ui.NotFoundError, *ui.ValidationError:
		return nil
	default:
		return err
	}
}

func loadPickState(states map[string]model.State, want model.State, rng *rand.Rand) (string, bool) {
	return loadPickWhere(states, rng, func(s model.State) bool { return s == want })
}

func loadPickWhere(states map[string]model.State, rng *rand.Rand, ok func(model.State) bool) (string, bool) {
	var candidates []string
	for id, s := range states {
		if ok(s) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[rng.IntN(len(candidates))], true
}

// loadRetryBackoff mirrors pkg/dispatch's own unexported retryBackoff
// (baseRetryBackoff 30s, doubling, capped at 30m) -- duplicated rather
// than exported, the same "small helpers are duplicated per test" choice
// harness_test.go's own package doc comment already makes for this
// package, so assertDrainedCleanly can tell a task legitimately still
// backing off after a failed run apart from one dispatch.Cycle should
// have picked up again by now but did not.
func loadRetryBackoff(streak int) time.Duration {
	if streak <= 0 {
		return 0
	}
	backoff := 30 * time.Second * time.Duration(uint64(1)<<uint(streak-1))
	if backoff > 30*time.Minute || backoff <= 0 {
		return 30 * time.Minute
	}
	return backoff
}

// assertDrainedCleanly is this file's scheduling-starvation check: no
// slot may still be occupied (a dispatched run that never finished blocks
// its slot forever, exactly the "cluster ... blocks" failure mode
// random_test.go's own doc comment names), and no ready task may still be
// sitting in the queue unless it is legitimately still backing off after
// a recent failure -- dispatch.Cycle's own retryEligible check, mirrored
// here since task_ready itself (Store.Ready) has no notion of time
// (dispatch.go's own doc comment) and so lists a backing-off task too.
func assertDrainedCleanly(t *testing.T, ctx context.Context, store *model.Store) {
	t.Helper()
	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatalf("LiveRunCount: %v", err)
	}
	if occupied != 0 {
		t.Errorf("scheduling: %d slot(s) still occupied after draining -- a dispatched run never finished: %v", occupied, occupied)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var stuck []string
	for _, id := range ready {
		streak, err := store.FailureStreak(ctx, id)
		if err != nil {
			t.Fatalf("FailureStreak(%s): %v", id, err)
		}
		if streak == nil || streak.Count == 0 {
			stuck = append(stuck, id)
			continue
		}
		if time.Since(streak.LastFinishedAt) >= loadRetryBackoff(streak.Count) {
			stuck = append(stuck, id)
		}
	}
	if len(stuck) != 0 {
		t.Errorf("scheduling starvation: %d task(s) still ready with no active retry backoff after draining -- dispatch.Cycle should have picked them up: %v", len(stuck), stuck)
	} else if len(ready) != 0 {
		t.Logf("%d task(s) still ready after draining, all legitimately backing off after a recent failure", len(ready))
	}
}

// reportConcurrency logs whether dispatched runs actually overlapped in
// wall-clock time -- see loadMetrics.concurrencyRatio's own doc comment.
// A ratio at or near 1.0x despite cfg.slots > 1 free slots and several
// tasks ready means every dispatch this run made actually executed one
// at a time, regardless of how many slots were configured.
func reportConcurrency(t *testing.T, metrics *loadMetrics) {
	t.Helper()
	ratio, n := metrics.concurrencyRatio()
	t.Logf("concurrency: %d dispatch(es) measured, overlap ratio %.2fx "+
		"(1.0x means dispatches never actually overlapped, regardless of how many slots were free)", n, ratio)
}

// reportCapability calls Reap the same way cmd/grain/daemon.go's own
// reapCapabilities does on its hourly timer -- here, once, now that
// assertDrainedCleanly has already confirmed no run is still live, so
// every capability this run ever materialized has already had its own
// chance at a normal Revoke by the time this looks for what that missed.
func reportCapability(t *testing.T, ctx context.Context, cap *loadCapability) {
	t.Helper()
	if _, err := cap.Reap(ctx, nil, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	stats := cap.Stats()
	t.Logf("capability: minted=%d revoked=%d revoke_failures=%d reaped=%d still_outstanding=%d",
		stats.Minted, stats.Revoked, stats.RevokeFails, stats.Reaped, stats.StillOutstanding)
	if stats.StillOutstanding != 0 {
		t.Errorf("capability leak: %d resource(s) still outstanding after Reap", stats.StillOutstanding)
	}
	if stats.RevokeFails > 0 && stats.Reaped == 0 {
		t.Errorf("capability: %d revoke failure(s) were injected but Reap never reclaimed anything -- the leak-recovery path was never actually exercised", stats.RevokeFails)
	}
}

func reportMetrics(t *testing.T, metrics *loadMetrics) {
	t.Helper()
	s := metrics.snapshot()
	t.Logf("tasks: filed=%d max_ready_depth=%d unresolved_at_end=%d outcomes=%v",
		s.filed, s.maxReady, s.stillUnresolved, s.outcomes)
	t.Logf("ready-to-resolved latency: n=%d min=%v p50=%v p95=%v max=%v",
		s.readyWait.N, s.readyWait.Min, s.readyWait.P50, s.readyWait.P95, s.readyWait.Max)
	t.Logf("sqlite writes: n=%d errors=%d min=%v p50=%v p95=%v max=%v",
		s.writeOps, s.writeErrs, s.write.Min, s.write.P50, s.write.P95, s.write.Max)
	t.Logf("RunCycle tick duration: n=%d min=%v p50=%v p95=%v max=%v",
		s.tick.N, s.tick.Min, s.tick.P50, s.tick.P95, s.tick.Max)

	if s.writeOps > 0 && s.writeErrs*20 > s.writeOps { // more than 5% failed outright
		t.Errorf("sqlite contention: %d/%d concurrent store write(s) failed even after Store.write's own retry loop", s.writeErrs, s.writeOps)
	}
}
