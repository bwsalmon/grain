package hosttop_test

// The real `top`, not a fake standing in for it -- the whole of this
// package is the flags it passes and what it keeps of the output, and a
// stub would assert only that the stub was written to match. A machine
// without procps skips, the same gate pkg/procgroup's own suite puts on
// bash.

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/hosttop"
)

func requireTop(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("top"); err != nil {
		t.Skip("top is not installed (procps)")
	}
}

func TestReadReturnsOneSampleOfRealTopOutput(t *testing.T) {
	requireTop(t)

	lines, err := hosttop.Read(context.Background(), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("Read returned no lines")
	}
	// One sample, not both: the summary header opens each iteration, and
	// keeping the second one alone is what makes the %CPU column mean
	// "now" (Read's own doc comment).
	if !strings.HasPrefix(lines[0], "top - ") {
		t.Errorf("first line = %q, want top's own summary header", lines[0])
	}
	headers := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "top - ") {
			headers++
		}
	}
	if headers != 1 {
		t.Errorf("output carries %d summary headers, want exactly 1:\n%s", headers, strings.Join(lines, "\n"))
	}
	// The process table itself, which is the point of the pane: top's
	// column header, and at least one row under it.
	table := -1
	for i, line := range lines {
		if strings.Contains(line, "PID") && strings.Contains(line, "COMMAND") {
			table = i
			break
		}
	}
	if table < 0 {
		t.Fatalf("no process table header in:\n%s", strings.Join(lines, "\n"))
	}
	if table == len(lines)-1 {
		t.Error("the process table header is the last line, so no process rows came back")
	}
}

func TestReadCapsTheLinesItReturns(t *testing.T) {
	requireTop(t)

	lines, err := hosttop.Read(context.Background(), 8)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 8 {
		t.Fatalf("len(lines) = %d, want 8:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// A caller that gives up -- the browser closing the pane's own request --
// takes top down with it rather than leaving it sampling for the rest of
// its interval.
func TestReadFailsOnACancelledContext(t *testing.T) {
	requireTop(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := hosttop.Read(ctx, 0); err == nil {
		t.Fatal("Read on a cancelled context returned no error")
	}
}
