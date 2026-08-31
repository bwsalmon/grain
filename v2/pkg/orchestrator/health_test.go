package orchestrator_test

// bwsalmon/agents#536: the sandbox health pane's own backend. These cover
// HostSandboxes.Health directly and reuse kontur_sandboxes_test.go's own
// writeFakeKontur/writeFakeCrictl/writeFakeSSH helpers -- the same real
// *mcp.SSHRunner path ToolsFor and ConfigureGitCredentials already
// exercise against a fake sshd, rather than a mock of KonturSandboxes
// itself -- for KonturSandboxes.Health.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func TestHostSandboxesHealthReportsEveryCreatedSlotAsReady(t *testing.T) {
	h := orchestrator.NewHostSandboxes(t.TempDir())

	if got := h.Health(context.Background()); len(got) != 0 {
		t.Fatalf("Health before any slot exists = %v, want empty", got)
	}

	root0, err := h.RootFor("0")
	if err != nil {
		t.Fatal(err)
	}
	root1, err := h.RootFor("1")
	if err != nil {
		t.Fatal(err)
	}

	got := h.Health(context.Background())
	if len(got) != 2 {
		t.Fatalf("Health = %v, want 2 entries", got)
	}
	want := map[string]string{"0": root0, "1": root1}
	for _, entry := range got {
		if entry.Backend != "host" {
			t.Errorf("slot %s: Backend = %q, want %q", entry.Slot, entry.Backend, "host")
		}
		if !entry.Ready || entry.Error != "" {
			t.Errorf("slot %s: Ready/Error = %v/%q, want true/\"\"", entry.Slot, entry.Ready, entry.Error)
		}
		if entry.Name != want[entry.Slot] {
			t.Errorf("slot %s: Name = %q, want %q", entry.Slot, entry.Name, want[entry.Slot])
		}
	}
}

func TestKonturSandboxesHealthReportsLoadAndMemoryOverSSH(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30082)
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 0, "127.0.0.1")
	listenTCP(t, 30082)
	home := t.TempDir()
	writeFakeSSH(t, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix: "grain-test-",
		StateDir:   stateDir,
		SSHUser:    "debian",
		SSHKey:     "/key",
		Workspace:  "/workspace",
	})

	if got := k.Health(context.Background()); len(got) != 0 {
		t.Fatalf("Health before any VM exists = %v, want empty", got)
	}

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatal(err)
	}

	got := k.Health(context.Background())
	if len(got) != 1 {
		t.Fatalf("Health = %v, want 1 entry", got)
	}
	h := got[0]
	if h.Slot != "slot-0" || h.Backend != "kontur" || h.Name != "grain-test-slot-0" {
		t.Errorf("Slot/Backend/Name = %q/%q/%q, want slot-0/kontur/grain-test-slot-0", h.Slot, h.Backend, h.Name)
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

func TestKonturSandboxesHealthReportsErrorWhenThePodNeverBecomesReady(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30083)
	// readyAfter is far beyond the single call Health itself makes, so
	// the fake crictl always answers "no pod yet" -- kontur.PodIP itself
	// never succeeds here, so this never even reaches slotHealth's own
	// port-dial retry (waitForPortHealthy), exercising the "not ready
	// right now" branch deterministically rather than racing a real
	// timeout.
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 1000, "127.0.0.1")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		SSHKey:            "/key",
		Workspace:         "/workspace",
		ReadyTimeout:      20 * time.Millisecond,
		ReadyPollInterval: 5 * time.Millisecond,
	})

	// ToolsFor itself fails because the pod never becomes ready within
	// ReadyTimeout -- but ensure() has already marked the VM created by
	// the time it gives up, the same "created but not yet reachable"
	// state a real VM that is slow, or wedged, to boot leaves behind.
	if _, err := k.ToolsFor(context.Background(), "slot-0"); err == nil {
		t.Fatal("ToolsFor unexpectedly succeeded against a pod that never becomes ready")
	}

	got := k.Health(context.Background())
	if len(got) != 1 {
		t.Fatalf("Health = %v, want 1 entry", got)
	}
	if got[0].Ready {
		t.Error("Ready = true, want false for a pod that never came up")
	}
	if got[0].Error == "" {
		t.Error("Error is empty, want a reason the sandbox is not reachable")
	}
}
