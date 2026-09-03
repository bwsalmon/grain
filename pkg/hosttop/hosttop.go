// Package hosttop reads a `top` snapshot of the processes running
// alongside this deployment's own daemon -- cmd/grain/daemon.go's
// ui.Config.HostTop, behind the Debug overlay's Top tab.
//
// It is the per-process half of what pkg/sysstat already reports in
// aggregate. Sandbox health answers "is this machine starved" with a load
// average and a memory figure; the question that always follows is "by
// what", and neither /proc/loadavg nor /proc/meminfo can say. `top` can,
// and it is already the first thing an operator with an SSH session
// reaches for -- so this shells out to the same tool rather than
// reimplementing its accounting over /proc, the same argument
// pkg/systemlog.Journalctl makes for shelling out to journalctl instead
// of vendoring a journal client. procps is in the deployment image for
// exactly this (Dockerfile's own header, tests/container's
// TestTheImageCarriesEveryBinaryTheDaemonShellsOutTo).
package hosttop

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DefaultLines is how many lines Read keeps when a caller passes 0 or
// less -- the summary block plus enough process rows to cover anything
// actually using the machine. `top` sorts by %CPU, so the rows dropped
// off the end are the idle ones.
const DefaultLines = 60

// iterations is how many samples `top` is asked for, and why it is not
// one: on its first iteration top has no previous sample to difference
// against, so every %CPU it prints is that process's share of CPU time
// since it *started*, which for a long-lived daemon is a number about
// the past week rather than about now. The second iteration is a real
// delta over the interval below, and is the one Read keeps.
const iterations = "2"

// interval is the delay between those two samples, in seconds as top's
// own -d spells it. It is what every %CPU in the output is measured
// over, and it is paid on every request -- so it is short enough not to
// be felt next to the pane's own refresh cadence, and long enough to
// average over more than a scheduler tick.
const interval = "0.5"

// width is top's -w: in batch mode top otherwise truncates each row to 80
// columns, which cuts the COMMAND column down to a few characters and
// loses exactly the part an operator is reading the pane for. The UI
// scrolls the block horizontally rather than wrapping it (.logs-view).
const width = "512"

// Read runs `top` once and returns at most lines lines of its output,
// summary block first, oldest-to-newest in top's own order.
//
// Errors carry top's own stderr: "top: command not found" on an image
// without procps, or a permissions failure reading /proc, are both things
// the pane should show the operator rather than swallow into an empty
// block.
//
// ctx bounds the whole run, interval included -- a caller that gives up
// (the browser closing the pane's request) kills top with it rather than
// leaving it sampling.
func Read(ctx context.Context, lines int) ([]string, error) {
	if lines <= 0 {
		lines = DefaultLines
	}
	// -b is batch mode: no cursor addressing, no tty required, plain
	// lines on stdout. -o %CPU asks for the sort explicitly rather than
	// relying on it being the default a given procps build (or a stray
	// ~/.toprc) happens to have.
	cmd := exec.CommandContext(ctx, "top", "-b", "-n", iterations, "-d", interval, "-w", width, "-o", "%CPU")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("top: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return trim(lastSample(splitLines(stdout.String())), lines), nil
}

// lastSample is the iterations-th iteration of top's output and nothing
// before it. Each iteration begins with top's own "top - <time> up ..."
// header line, so the last one of those starts the sample worth keeping;
// output with no such line at all (a procps build that words its header
// differently) is returned whole rather than emptied, since some output
// is a better answer for a debugging pane than none.
func lastSample(all []string) []string {
	for i := len(all) - 1; i >= 0; i-- {
		if strings.HasPrefix(all[i], "top - ") {
			return all[i:]
		}
	}
	return all
}

// trim caps the sample at n lines. It cuts from the end, where top's
// %CPU sort has already put the processes doing nothing.
func trim(sample []string, n int) []string {
	if len(sample) <= n {
		return sample
	}
	return sample[:n]
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
