// Whether runOne actually builds a sandbox for each dispatch and destroys
// it afterwards -- what replaced bwsalmon/agents#353's "recreate the
// sandbox after each task" once a sandbox stopped outliving the task in
// the first place. kontur_sandboxes_test.go proves KonturSandboxes'
// own Acquire/Release; this proves RunCycle calls them, on a run that
// succeeds and on one that fails, and that a task's own shape override
// reaches Acquire rather than being applied to an already-built sandbox.
package orchestrator_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// acquireCall is one Acquire recordingSandboxes saw.
type acquireCall struct {
	name  string
	shape orchestrator.Shape
}

// recordingSandboxes wraps a real HostSandboxes -- so a dispatched run
// still has a real directory to work in -- and records the lifecycle
// calls made against it, letting a test assert on them with no kontur VM
// or docker daemon anywhere nearby.
type recordingSandboxes struct {
	*orchestrator.HostSandboxes

	mu       sync.Mutex
	acquired []acquireCall
	released []string
}

func (s *recordingSandboxes) Acquire(ctx context.Context, name string, shape orchestrator.Shape) (orchestrator.Sandbox, error) {
	// The wrapped HostSandboxes refuses a non-zero shape (a directory has
	// no size of its own), which would stop this from observing what
	// runOne asked for. Record the request, then acquire the sandbox
	// itself unshaped.
	s.mu.Lock()
	s.acquired = append(s.acquired, acquireCall{name: name, shape: shape})
	s.mu.Unlock()

	inner, err := s.HostSandboxes.Acquire(ctx, name, orchestrator.Shape{})
	if err != nil {
		return nil, err
	}
	return &recordingSandbox{Sandbox: inner, owner: s}, nil
}

func (s *recordingSandboxes) calls() ([]acquireCall, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]acquireCall(nil), s.acquired...), append([]string(nil), s.released...)
}

type recordingSandbox struct {
	orchestrator.Sandbox
	owner *recordingSandboxes
}

func (s *recordingSandbox) Release(ctx context.Context) error {
	s.owner.mu.Lock()
	s.owner.released = append(s.owner.released, s.Name())
	s.owner.mu.Unlock()
	return s.Sandbox.Release(ctx)
}

func TestRunCycleReleasesTheSandboxAfterASuccessfulDispatch(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	acquired, released := sandboxes.calls()
	if len(acquired) != 1 || acquired[0].name != "t1-1" {
		t.Errorf("Acquire calls = %+v, want exactly one, named for the run", acquired)
	}
	if len(released) != 1 || released[0] != "t1-1" {
		t.Errorf("Release calls = %v, want exactly one for t1-1", released)
	}
}

func TestRunCycleReleasesTheSandboxAfterAFailedDispatch(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = []model.Grant{{Capability: "locked", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	cap := &fakeCapability{name: "locked", refuse: "not for you"}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		Config:        orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)},
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected RunCycle to report the refused capability")
	}

	if _, released := sandboxes.calls(); len(released) != 1 || released[0] != "t1-1" {
		t.Errorf("Release calls after a failed dispatch = %v, want exactly one for t1-1 -- a failed run "+
			"must not leave its sandbox running", released)
	}
}

// TestRunCycleDispatchesAGrantWithNoPlacementOntoANonRootedSandbox is a
// regression test for bwsalmon/agents#643: a grant like self-debug or
// self-repair materializes no SideSandbox placement at all, so a task
// holding one -- every Configuration agent task, since ui.Client always
// grants both -- must still dispatch cleanly onto a sandbox with no local
// directory (recordingSandbox is one; see its own doc comment). Before
// the fix, runOne refused any task with Grants at all once its sandbox
// wasn't rootedSandbox, whether or not those grants ever needed one.
func TestRunCycleDispatchesAGrantWithNoPlacementOntoANonRootedSandbox(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Interactive = true
	task.Grants = []model.Grant{{Capability: selfdebug.CapabilityName, Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		Config:        orchestrator.Config{Capabilities: model.NewCapabilityRegistry(selfdebug.New())},
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v, want a grant with no placement to dispatch fine with no local sandbox directory", err)
	}
}

// TestRunCycleFailsAGrantThatPlacesSomethingOntoANonRootedSandbox is the
// counterpart to the regression test above: a grant that does
// materialize a SideSandbox placement (bwsalmon/agents#643's gcpkey and
// geminikey, faked here) still has nowhere to write it once its sandbox
// has no local directory, and must fail with a clear reason rather than
// silently writing under the process's own working directory.
func TestRunCycleFailsAGrantThatPlacesSomethingOntoANonRootedSandbox(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = []model.Grant{{Capability: "keyed", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	cap := &fakeCapability{name: "keyed", path: "/etc/keyed/key.json", content: "secret"}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		Config:        orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)},
		MaxConcurrent: 1,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if err == nil {
		t.Fatal("expected RunCycle to report the placement with nowhere to land")
	}
	if !strings.Contains(err.Error(), "no local directory to place it in") {
		t.Errorf("RunCycle error = %v, want it to explain there was no local directory", err)
	}
}

// placedFile is one PlaceFile call a placingSandbox saw.
type placedFile struct {
	path    string
	content string
	mode    string
}

// placingSandboxes hands out sandboxes shaped like a kontur VM's:
// reachable, but with no local directory this process can write into --
// and so with orchestrator.SandboxPlacer as their only route for a
// capability placement. The wrapper embeds the Sandbox interface rather
// than the concrete host sandbox, which is what hides Root: an embedded
// interface value carries no methods outside its own method set, so
// runOne's rootedSandbox assertion fails here exactly as it does for a
// real konturSandbox.
type placingSandboxes struct {
	*orchestrator.HostSandboxes

	mu     sync.Mutex
	placed []placedFile
	// alsoRooted makes the handed-out sandbox offer Root as well as
	// PlaceFile, for the precedence test below. The root it reports is
	// the real directory HostSandboxes made, so a placement taking the
	// wrong branch lands somewhere observable rather than failing.
	alsoRooted bool
	roots      map[string]string
}

func (s *placingSandboxes) Acquire(ctx context.Context, name string, shape orchestrator.Shape) (orchestrator.Sandbox, error) {
	inner, err := s.HostSandboxes.Acquire(ctx, name, shape)
	if err != nil {
		return nil, err
	}
	sb := &placingSandbox{Sandbox: inner, owner: s}
	if !s.alsoRooted {
		return sb, nil
	}
	// The host sandbox HostSandboxes handed back is rooted; this reads
	// the directory back off it the same way runOne itself does, since
	// the wrapper above is what hides that method from runOne.
	rooted, ok := inner.(interface{ Root() (string, error) })
	if !ok {
		return nil, errors.New("HostSandboxes handed back a sandbox with no Root")
	}
	root, err := rooted.Root()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.roots[name] = root
	s.mu.Unlock()
	return &rootedPlacingSandbox{placingSandbox: sb, root: root}, nil
}

func (s *placingSandboxes) calls() []placedFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]placedFile(nil), s.placed...)
}

type placingSandbox struct {
	orchestrator.Sandbox
	owner *placingSandboxes
}

func (s *placingSandbox) PlaceFile(ctx context.Context, path, content, mode string) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.placed = append(s.owner.placed, placedFile{path: path, content: content, mode: mode})
	return nil
}

// rootedPlacingSandbox offers both routes at once -- a shape no real
// Sandbox has, built only to pin down which one applyPlacements takes.
type rootedPlacingSandbox struct {
	*placingSandbox
	root string
}

func (s *rootedPlacingSandbox) Root() (string, error) { return s.root, nil }

// TestRunCyclePlacesIntoASandboxThatCanOnlyBeReachedRemotely is the
// regression the test above left open: a kontur VM has no local
// directory, so for as long as applyPlacements had only that one route,
// EVERY task granting gcp-key (or gemini-key, or github-sandbox) on a
// deployment running -kontur-sandboxes -- which is what scripts/
// setup.sh installs for a real host -- failed during preparation, before
// the agent's first turn, and the minted credential never reached the
// sandbox at all. A sandbox that offers SandboxPlacer must have its
// placements delivered over that instead, with the placement's own path
// and mode intact.
func TestRunCyclePlacesIntoASandboxThatCanOnlyBeReachedRemotely(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = []model.Grant{{Capability: "keyed", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &placingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir()), roots: map[string]string{}}
	cap := &fakeCapability{name: "keyed", path: "/home/debian/.gcp-service-account.json", content: "minted-key"}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		Config:        orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)},
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v, want a placement onto a remotely-reachable sandbox to dispatch fine", err)
	}

	placed := sandboxes.calls()
	if len(placed) != 1 {
		t.Fatalf("PlaceFile calls = %+v, want exactly the one placement the grant materialized", placed)
	}
	want := placedFile{path: "/home/debian/.gcp-service-account.json", content: "minted-key", mode: "600"}
	if placed[0] != want {
		t.Errorf("PlaceFile call = %+v, want %+v", placed[0], want)
	}
}

// A sandbox offering both routes takes the remote one: it is the sandbox
// itself, where a local root alongside it could only be a staging copy of
// the same credential on the controller's own disk -- see
// applyPlacements' own doc comment.
func TestRunCyclePrefersARemotePlacementOverALocalRoot(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = []model.Grant{{Capability: "keyed", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &placingSandboxes{
		HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		alsoRooted:    true,
		roots:         map[string]string{},
	}
	cap := &fakeCapability{name: "keyed", path: "/home/debian/.gcp-service-account.json", content: "minted-key"}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		Config:        orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)},
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if placed := sandboxes.calls(); len(placed) != 1 {
		t.Fatalf("PlaceFile calls = %+v, want the placement to have taken the remote route", placed)
	}
	sandboxes.mu.Lock()
	roots := make([]string, 0, len(sandboxes.roots))
	for _, root := range sandboxes.roots {
		roots = append(roots, root)
	}
	sandboxes.mu.Unlock()
	if len(roots) == 0 {
		t.Fatal("no sandbox was acquired at all")
	}
	for _, root := range roots {
		staged := filepath.Join(root, "home", "debian", ".gcp-service-account.json")
		if _, err := os.Stat(staged); !os.IsNotExist(err) {
			t.Errorf("the credential was also staged on the controller's own disk at %s (err=%v)", staged, err)
		}
	}
}

func TestRunCycleAcquiresTheSandboxWithTheTasksOwnShape(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.SandboxCPUs, task.SandboxMemoryMB = 8, 16384
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	acquired, _ := sandboxes.calls()
	if len(acquired) != 1 {
		t.Fatalf("Acquire calls = %+v, want exactly one", acquired)
	}
	if want := (orchestrator.Shape{CPUs: 8, MemoryMB: 16384}); acquired[0].shape != want {
		t.Errorf("Acquire shape = %+v, want %+v -- a task's override is a create-time argument now", acquired[0].shape, want)
	}
}

func TestRunCycleAcquiresWithNoShapeForATaskThatSetsNeitherField(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	acquired, _ := sandboxes.calls()
	if len(acquired) != 1 || !acquired[0].shape.IsZero() {
		t.Errorf("Acquire shape = %+v, want the zero shape -- a deployment using no overrides must not "+
			"start asking for one", acquired)
	}
}

// A host-directory sandbox has no CPU or memory of its own, so a task
// asking for a specific shape against that backend fails its dispatch
// rather than silently getting the whole host.
func TestHostSandboxesRefusesAShapeItCannotHonour(t *testing.T) {
	h := orchestrator.NewHostSandboxes(t.TempDir())
	if _, err := h.Acquire(context.Background(), "t1-1", orchestrator.Shape{CPUs: 4}); err == nil {
		t.Fatal("expected HostSandboxes.Acquire to refuse a shape it cannot honour")
	}
	if _, err := h.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatalf("Acquire with no shape: %v", err)
	}
}

// unbuildableSandboxes is a backend whose Acquire never succeeds, for any
// name -- a kontur VM whose guest never answers inside ReadyTimeout, a
// docker daemon that is down, a host directory that cannot be made.
// isolation_test.go's own failingSandboxes refuses one named sandbox and
// builds the rest, which is a different question (does one dispatch
// failing abandon the others); this one is about the run that failed.
type unbuildableSandboxes struct{ err error }

func (s unbuildableSandboxes) Acquire(ctx context.Context, name string, shape orchestrator.Shape) (orchestrator.Sandbox, error) {
	return nil, s.err
}

// A run whose sandbox never came up has to be finished all the same.
// dispatch.Cycle already made the row durable before anything tried to
// build a sandbox for it, and RunDispatch -- the only thing that finishes
// a run -- is never reached, so without this the row stays live forever:
// task_state reads it as 'running' so the task never returns to 'queued',
// LiveRunCount keeps counting it so the deployment loses a unit of
// -max-concurrent, and retryEligible reads finished runs so the backoff
// never retries. Nothing sweeps it: MaxRunRuntime lives inside
// RunDispatch, and RecoverOrphanedRuns only runs at startup.
func TestRunCycleFinishesARunWhoseSandboxCouldNotBeAcquired(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	deps := orchestrator.Deps{
		Store: store, Client: client,
		Sandboxes:     unbuildableSandboxes{err: errors.New("guest never became reachable")},
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected RunCycle to report the failed acquisition")
	}

	live, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live runs = %d, want 0 -- a run whose sandbox failed still holds its share of "+
			"-max-concurrent, and nothing but a daemon restart would free it", live)
	}

	runs, err := store.Runs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one", runs)
	}
	if runs[0].FinishedAt == nil {
		t.Fatal("the run was left unfinished")
	}
	// The literal, not merely "not succeeded". This string is a contract
	// with the UI: DetailOverlay.jsx keys its label and badge off it by
	// name, and its own test pins the same word, because without a match
	// "setup-failed" falls through the raw-string path to a *queued* badge
	// -- a run that could not be given a sandbox rendered as one that has
	// not started yet. Asserting only "a failure" here would let a rename
	// keep this suite green while silently reintroducing that.
	if runs[0].Outcome != "setup-failed" {
		t.Errorf("outcome = %q, want %q -- no agent ever ran, and pkg/ui's DetailOverlay keys off this exact word",
			runs[0].Outcome, "setup-failed")
	}
	if !strings.Contains(runs[0].Detail, "guest never became reachable") {
		t.Errorf("detail = %q, want it to carry the acquisition error", runs[0].Detail)
	}
}

// Finishing that run is what lets the next cycle try the task again --
// the backoff dispatch already applies to any other run that ended
// without succeeding. Without it the task is not merely delayed, it is
// unreachable: task_ready only offers a 'queued' task, and a live run
// keeps it 'running'.
func TestATaskWhoseSandboxFailedIsDispatchedAgainAfterItsBackoff(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	failing := orchestrator.Deps{
		Store: store, Client: client,
		Sandboxes:     unbuildableSandboxes{err: errors.New("docker daemon is down")},
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}
	if err := orchestrator.RunCycle(ctx, failing, baseTime); err == nil {
		t.Fatal("expected the first cycle to report the failed acquisition")
	}

	// Far enough past the first attempt to be out of its backoff.
	later := baseTime.Add(24 * time.Hour)
	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	working := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}
	if err := orchestrator.RunCycle(ctx, working, later); err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}

	acquired, _ := sandboxes.calls()
	if len(acquired) != 1 || acquired[0].name != "t1-2" {
		t.Fatalf("Acquire calls on the retry = %+v, want one for t1's second attempt", acquired)
	}
}

// The same guard covers the rest of the setup path, not just Acquire: a
// token that cannot be minted is a run that never reaches RunDispatch
// either, and it has already acquired a real sandbox by then -- which
// must be released as well as the row finished.
func TestRunCycleFinishesAndReleasesWhenMintingTheSandboxTokenFails(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:        completesWithAComment(),
		MaxConcurrent:    1,
		MintSandboxToken: func(string) (string, error) { return "", errors.New("token file is unreadable") },
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected RunCycle to report the failed mint")
	}

	if _, released := sandboxes.calls(); len(released) != 1 || released[0] != "t1-1" {
		t.Errorf("Release calls = %v, want exactly one for t1-1", released)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 0 {
		t.Fatalf("live runs = %d (%v), want 0", live, err)
	}
}

// The token a run minted is revoked once its sandbox is released, so the
// file holds one entry per sandbox that still exists rather than one per
// run ever dispatched (gitproxy.SandboxTokenStore.Revoke).
func TestRunCycleRevokesTheSandboxTokenAfterReleasingTheSandbox(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	var mu sync.Mutex
	var minted, revoked []string
	sandboxes := &recordingSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
		// An absolute base, because runOne points the sandbox's git at
		// GitRemoteBase+"/placeholder/placeholder.git" as soon as a token
		// is minted, and a credential-store line needs a real URL.
		Config: orchestrator.Config{GitRemoteBase: "http://proxy.example"},
		MintSandboxToken: func(name string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			minted = append(minted, name)
			return "token-for-" + name, nil
		},
		RevokeSandboxToken: func(name string) error {
			mu.Lock()
			defer mu.Unlock()
			revoked = append(revoked, name)
			return nil
		},
	}

	// The run itself fails -- GitRemoteBase points at a host that does not
	// resolve, so RunDispatch's checkout cannot succeed -- which is beside
	// the point and useful anyway: a token has to be revoked after a run
	// that failed just as much as after one that worked, since the sandbox
	// it named is gone either way.
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected the unreachable git remote to fail this run")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(minted) != 1 || minted[0] != "t1-1" {
		t.Errorf("minted = %v, want one token for t1-1", minted)
	}
	if len(revoked) != 1 || revoked[0] != "t1-1" {
		t.Errorf("revoked = %v, want the same sandbox's token dropped once its sandbox was released", revoked)
	}
}

// A sandbox a task's shape cannot be honoured by is refused at Acquire,
// which is a setup failure like any other: the run has to be finished
// rather than left holding capacity.
func TestRunCycleFinishesARunWhoseShapeTheBackendRefuses(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.SandboxCPUs = 8
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	deps := orchestrator.Deps{
		Store: store, Client: client,
		Sandboxes:     orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected RunCycle to report the refused shape")
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 0 {
		t.Fatalf("live runs = %d (%v), want 0", live, err)
	}
}
