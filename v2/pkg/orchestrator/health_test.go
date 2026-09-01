package orchestrator_test

// bwsalmon/agents#536: the sandbox health pane's own backend. These cover
// HostSandboxes.Health directly and reuse kontur_sandboxes_test.go's own
// writeFakeKontur/writeFakeCrictl/writeFakeSSH helpers -- the same real
// *mcp.SSHRunner path Acquire and ConfigureGitCredentials already
// exercise against a fake sshd, rather than a mock of KonturSandboxes
// itself -- for KonturSandboxes.Health.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func TestHostSandboxesHealthReportsEveryLiveSandboxAsReady(t *testing.T) {
	ctx := context.Background()
	h := orchestrator.NewHostSandboxes(t.TempDir())

	if got := h.Health(ctx); len(got) != 0 {
		t.Fatalf("Health with nothing running = %v, want empty", got)
	}

	first, err := h.Acquire(ctx, "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Acquire(ctx, "t2-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}

	got := h.Health(ctx)
	if len(got) != 2 {
		t.Fatalf("Health = %v, want 2 entries", got)
	}
	for _, entry := range got {
		if entry.Backend != "host" {
			t.Errorf("%s: Backend = %q, want %q", entry.Sandbox, entry.Backend, "host")
		}
		if !entry.Ready || entry.Error != "" {
			t.Errorf("%s: Ready/Error = %v/%q, want true/\"\"", entry.Sandbox, entry.Ready, entry.Error)
		}
		if entry.Name == "" {
			t.Errorf("%s: Name is empty, want the sandbox's own directory", entry.Sandbox)
		}
	}

	// Releasing one drops it from the pane: what this reports is what is
	// running, not a fixed set of slots that are sometimes idle.
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	got = h.Health(ctx)
	if len(got) != 1 || got[0].Sandbox != "t2-1" {
		t.Fatalf("Health after releasing one = %v, want only t2-1", got)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.Health(ctx); len(got) != 0 {
		t.Fatalf("Health after releasing both = %v, want empty", got)
	}
}

func TestKonturSandboxesHealthReportsLoadAndMemoryOverSSH(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30082)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if got := k.Health(context.Background()); len(got) != 0 {
		t.Fatalf("Health before any VM exists = %v, want empty", got)
	}

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	got := k.Health(context.Background())
	if len(got) != 1 {
		t.Fatalf("Health = %v, want 1 entry", got)
	}
	h := got[0]
	if h.Sandbox != "t1-1" || h.Backend != "kontur" || h.Name != "g-t1-1" {
		t.Errorf("Sandbox/Backend/Name = %q/%q/%q, want t1-1/kontur/g-t1-1", h.Sandbox, h.Backend, h.Name)
	}
	if h.Error != "" {
		t.Errorf("Error = %q, want none", h.Error)
	}
	if !h.Ready {
		t.Error("Ready = false, want true")
	}
	// The fake ssh script (writeFakeSSH) really execs the given command
	// locally, so this is genuinely this test process's own
	// /proc/loadavg and /proc/meminfo, not a canned value.
	if h.LoadAverage == "" {
		t.Error("LoadAverage is empty, want a real /proc/loadavg reading")
	}
	if h.MemoryTotalMB == 0 {
		t.Error("MemoryTotalMB = 0, want a real /proc/meminfo reading")
	}
}

// A sandbox that was acquired fine and has since stopped answering is
// exactly what this pane exists to surface (bwsalmon/agents#536): it is
// reported with an Error rather than dropped, or waited on for the whole
// boot-length ReadyTimeout.
//
// The previous version of this covered a VM that never became reachable
// at all, which was a state a slot's sandbox could sit in indefinitely --
// ensure() marked it created the moment `konturctl vm create` returned,
// whether or not the guest ever came up. A failed Acquire now deletes the
// VM and registers nothing, so that state no longer exists; a VM that
// dies under a live run does.
func TestKonturSandboxesHealthReportsErrorWhenAVMStopsAnswering(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30083)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyTimeout:      20 * time.Millisecond,
		ReadyPollInterval: 5 * time.Millisecond,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	// Shadow the fake docker with one that never answers, so the guest
	// this run is holding stops responding without the handle knowing.
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter2"), 1000, "")

	got := k.Health(context.Background())
	if len(got) != 1 {
		t.Fatalf("Health = %v, want 1 entry -- an unreachable sandbox is still one this run holds", got)
	}
	if got[0].Ready {
		t.Error("Ready = true, want false for a guest that stopped answering")
	}
	if got[0].Error == "" {
		t.Error("Error is empty, want a reason the sandbox is not reachable")
	}
}
