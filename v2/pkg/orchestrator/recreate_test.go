// Whether runOne actually tears a dispatched slot's sandbox down and
// rebuilds it once its own dispatch is done -- bwsalmon/agents#353's own
// ask, "recreate the sandbox after each task". kontur_sandboxes_test.go
// already proves KonturSandboxes.Recreate itself works; this proves
// RunCycle calls it, on both a run that succeeds and one that fails, and
// that HostSandboxes (which implements no such method) is left alone.
package orchestrator_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// recordingRecreateSandboxes wraps a real HostSandboxes (so a dispatched
// run still has a real directory and RootFor to work with) and records
// every slot Recreate is called for, letting a test assert on it without
// a real kontur VM or docker daemon anywhere nearby.
type recordingRecreateSandboxes struct {
	*orchestrator.HostSandboxes

	mu        sync.Mutex
	recreated []string
}

func (s *recordingRecreateSandboxes) Recreate(ctx context.Context, slot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recreated = append(s.recreated, slot)
	return nil
}

func (s *recordingRecreateSandboxes) recreatedSlots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.recreated...)
}

func TestRunCycleRecreatesTheSandboxAfterASuccessfulDispatch(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	sandboxes := &recordingRecreateSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes,
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if got := sandboxes.recreatedSlots(); len(got) != 1 || got[0] != "slot-0" {
		t.Errorf("Recreate calls = %v, want exactly one for slot-0", got)
	}
}

func TestRunCycleRecreatesTheSandboxAfterAFailedDispatch(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = []model.Grant{{Capability: "locked", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	sandboxes := &recordingRecreateSandboxes{HostSandboxes: orchestrator.NewHostSandboxes(t.TempDir())}
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

	if got := sandboxes.recreatedSlots(); len(got) != 1 || got[0] != "slot-0" {
		t.Errorf("Recreate calls after a failed dispatch = %v, want exactly one for slot-0 -- a failed run must not leave "+
			"its sandbox behind for the next one dispatched onto the same slot", got)
	}
}

func TestRunCycleDoesNotRecreateAHostSandboxesSlot(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	// HostSandboxes implements no Recreate method at all -- this is
	// really just confirming RunCycle does not panic or error trying to
	// find one, since the local-directory stand-in is deliberately left
	// long-lived (see its own doc comment).
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
}
