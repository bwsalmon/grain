// Whether runOne actually applies a task's own SandboxCPUs/SandboxMemoryMB
// override (bwsalmon/agents#534) ahead of that task's run, via a Sandboxes
// backend's optional Reshape method -- kontur_sandboxes_test.go already
// proves KonturSandboxes.Reshape itself works; this proves RunCycle calls
// it for a task that sets either field, leaves it uncalled for a task that
// sets neither, and refuses the dispatch outright when the configured
// Sandboxes backend supports no such thing.
package orchestrator_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// reshapeCall is one Reshape invocation recordingReshapeSandboxes saw.
type reshapeCall struct {
	slot           string
	cpus, memoryMB int
}

// recordingReshapeSandboxes wraps a real HostSandboxes (so a dispatched
// run still has a real directory to work with) and records every
// Reshape call, the same pattern recordingRecreateSandboxes uses for
// Recreate.
type recordingReshapeSandboxes struct {
	*orchestrator.HostSandboxes

	mu       sync.Mutex
	reshaped []reshapeCall
}

func (s *recordingReshapeSandboxes) Reshape(ctx context.Context, slot string, cpus, memoryMB int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reshaped = append(s.reshaped, reshapeCall{slot: slot, cpus: cpus, memoryMB: memoryMB})
	return nil
}

func (s *recordingReshapeSandboxes) reshapeCalls() []reshapeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reshapeCall(nil), s.reshaped...)
}

func TestRunCycleReshapesTheSandboxForATaskWithASandboxShapeOverride(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.SandboxCPUs = 4
	task.SandboxMemoryMB = 8192
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingReshapeSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	want := reshapeCall{slot: "slot-0", cpus: 4, memoryMB: 8192}
	if got := sandboxes.reshapeCalls(); len(got) != 1 || got[0] != want {
		t.Errorf("Reshape calls = %v, want exactly one %+v", got, want)
	}
}

func TestRunCycleDoesNotReshapeForATaskWithNoSandboxShapeOverride(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	sandboxes := &recordingReshapeSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if got := sandboxes.reshapeCalls(); len(got) != 0 {
		t.Errorf("Reshape calls for a task with no override = %v, want none", got)
	}
}

func TestRunCycleRefusesATaskWithASandboxShapeOverrideAgainstAnUnsupportedBackend(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.SandboxCPUs = 4
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// HostSandboxes implements no Reshape method at all -- a task asking
	// for a shape it has no VM to apply is a dispatch failure, not a
	// silent no-op, the same "refuse rather than silently do something
	// else" choice runOne already makes for a task that requests a
	// capability with no local directory to place it in.
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err == nil {
		t.Fatal("expected RunCycle to report the unsupported sandbox shape override")
	}
}
