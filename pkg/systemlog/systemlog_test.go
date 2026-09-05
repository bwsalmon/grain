package systemlog_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/systemlog"
)

// The real journalctl against this machine's real kernel journal, not a
// fake standing in for it -- the whole of Dmesg is the flags it passes,
// and a stub would assert only that the stub was written to match (the
// same call pkg/hosttop's suite makes about `top`). A machine with no
// journalctl, or one whose journal this test cannot read, skips: both
// are properties of the runner rather than of the code under test.
func requireKernelJournal(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("journalctl is not installed (systemd)")
	}
	if err := exec.Command("journalctl", "--dmesg", "--lines", "1", "--no-pager").Run(); err != nil {
		t.Skipf("this machine's kernel journal is not readable: %v", err)
	}
}

func TestDmesgTailReadsTheKernelJournal(t *testing.T) {
	requireKernelJournal(t)

	lines, err := (systemlog.Dmesg{}).Tail(context.Background(), 5)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("Tail returned no lines, but the journal has kernel entries")
	}
	// journalctl's --lines is a cap on entries, and short-iso prints one
	// line per entry -- so asking for 5 is asking for at most 5, which is
	// what keeps GET /api/logs/dmesg?lines= a bound rather than a hint.
	if len(lines) > 5 {
		t.Errorf("Tail(5) returned %d lines: %v", len(lines), lines)
	}
}

// A caller that gives up -- the browser closing the Logs pane's own
// request -- takes journalctl down with it rather than leaving it
// reading.
func TestDmesgTailFailsOnACancelledContext(t *testing.T) {
	requireKernelJournal(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (systemlog.Dmesg{}).Tail(ctx, 5); err == nil {
		t.Fatal("Tail on a cancelled context returned no error")
	}
}

// The error an unreadable source produces names the read that failed --
// "journalctl -u ..." or "journalctl --dmesg" -- rather than a bare exit
// status, since that name is the whole of what the pane can show an
// operator about it.
func TestJournalctlErrorsNameTheReadThatFailed(t *testing.T) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("journalctl is not installed (systemd)")
	}

	// A negative count fails on any host, whatever its journal holds:
	// journalctl reads the "-1" as a flag of its own and refuses it. The
	// UI's own handler never sends one (handleGetLogLines rejects a
	// non-positive ?lines= before it gets here) -- this is about what a
	// failed read reports, not about the count.
	_, err := (systemlog.Journalctl{Unit: "grain-daemon.service"}).Tail(context.Background(), -1)
	if err == nil {
		t.Fatal("Journalctl.Tail(-1) returned no error")
	}
	if !strings.Contains(err.Error(), "journalctl -u grain-daemon.service") {
		t.Errorf("error = %q, want it to name the unit read", err)
	}
	_, err = (systemlog.Dmesg{}).Tail(context.Background(), -1)
	if err == nil {
		t.Fatal("Dmesg.Tail(-1) returned no error")
	}
	if !strings.Contains(err.Error(), "journalctl --dmesg") {
		t.Errorf("error = %q, want it to name the kernel read", err)
	}
}

func TestFileTailReturnsWholeFileWhenShorterThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}

func TestFileTailReturnsOnlyTheLastNLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"three", "four"}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
}

func TestFileTailOfMissingFileIsEmptyNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.log")
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %v, want empty", lines)
	}
}
