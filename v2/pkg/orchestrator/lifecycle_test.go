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
	"sync"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
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
	if len(acquired) != 1 || acquired[0].name != "t1-r1" {
		t.Errorf("Acquire calls = %+v, want exactly one, named for the run", acquired)
	}
	if len(released) != 1 || released[0] != "t1-r1" {
		t.Errorf("Release calls = %v, want exactly one for t1-r1", released)
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

	if _, released := sandboxes.calls(); len(released) != 1 || released[0] != "t1-r1" {
		t.Errorf("Release calls after a failed dispatch = %v, want exactly one for t1-r1 -- a failed run "+
			"must not leave its sandbox running", released)
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
	if _, err := h.Acquire(context.Background(), "t1-r1", orchestrator.Shape{CPUs: 4}); err == nil {
		t.Fatal("expected HostSandboxes.Acquire to refuse a shape it cannot honour")
	}
	if _, err := h.Acquire(context.Background(), "t1-r1", orchestrator.Shape{}); err != nil {
		t.Fatalf("Acquire with no shape: %v", err)
	}
}
