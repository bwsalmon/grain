package mcp

// result_size.go bounds how much text one tool answer may carry back.
//
// Nothing used to. run_command returned the whole of stdout and stderr,
// read_file with no "limit" returned the whole file line-numbered, and
// one `go test ./...` on a failing suite, one `git log -p`, one `grep -r`
// that matched more than expected or one 250 KB README spent a large
// fraction of a run's context in a single turn -- context the run cannot
// get back, on output it did not ask for and mostly will not read.
//
// Past that point the behaviour was whichever CLI happens to be driving
// the run: claude caps an MCP tool's output itself and rejects a result
// that exceeds the cap, so an oversized answer costs the run the wall
// clock the command took and buys it nothing at all; antigravity is a
// different CLI with its own answer. So the size at which a tool result
// stopped working was a per-framework default grain had not chosen, did
// not know, and could not explain to an agent when it was hit. Capping
// here fixes it once for every framework, and the server is the only
// place that can produce the thing that makes a cut answer usable: a
// statement of what was dropped and how to get it back.
//
// Head and tail are kept, never just the head. A command's verdict is
// usually in the last lines (the failure summary, the final error) and
// its invocation in the first, and the middle is what nobody needed.

import (
	"fmt"
	"strings"
)

// maxToolResultBytes is the most output text a single run_command or
// read_file answer carries. 16 KB, which is two things at once: what the
// window below says keeps nearly every answer whole, and antigravity's
// own per-result default -- and agy is what grain dispatches with unless
// a deployment says otherwise (model.AgentFrameworkAntigravity is
// -agent-framework's default). Past a framework's own limit a result is
// not a bigger answer, it is one the CLI truncates or refuses on its own
// terms, with none of the notice below saying what went and how to get
// it back, so there is nothing to be gained by capping above it.
//
// It replaces 64 KB, which this file called "a starting guess, not a
// measurement" and which turned out to be too high to ever fire: over
// the 90 days to 2026-09-04, across the 23 runs of this deployment that
// recorded a census, no run_command answer reached 43 KB and no
// read_file answer 45 KB, so the cap cut nothing in 1,254 calls. The
// same window is what sizes the replacement (`grain metrics -window
// 90d`, quoted in full in README.md's "No single answer may eat the
// run's context"):
//
//	tool          calls  mean bytes    p50      p95      p99    max
//	run_command    1162        2163  <=1023   <=8191  <=32767  43460
//	read_file        92        6736  <=4095  <=32767  <=65535  44860
//
// The percentiles are base-2 bucket bounds, not measured bytes
// (metrics.Sizes says why). Read against 16 KB: at least 95% of
// run_command answers pass whole and half of them are under 1 KB;
// read_file is the heavier of the two distributions -- its p95 sits in
// the 16-32 KB octave, so a few in twenty are cut -- and it is also the
// tool whose elision notice recovers exactly, since the line numbers
// either side of the cut are the offset and limit that fetch the missing
// range. One number rather than one per tool despite that difference:
// the framework limit this now matches is per result, so a 32 KB
// read_file answer grain let through is one agy would cut instead,
// without saying so.
//
// It bounds the command's own output, not the whole result: the exit
// line, grain's own notices and the deadline notice are added on top and
// are a few hundred bytes between them.
//
// A var, not a const. A test shrinks it rather than generating 16 KB of
// output to prove the cap works, and one deployment's distribution is
// not yet a reason to make it a stored per-deployment setting -- when a
// second deployment's numbers disagree with these, or runs a framework
// whose own limit is higher, that is the moment for a setting rather
// than now.
var maxToolResultBytes = 16 << 10

// elisionAdviceCommandOutput and elisionAdviceFileLines are the "how to
// get the rest" half of an elision notice, which is the half that decides
// whether a cut answer is usable. Each names the specific next call for
// its tool rather than saying "output was truncated" and leaving the run
// to invent one -- re-running the same command verbatim to see the middle
// is exactly the move the notice exists to prevent.
const elisionAdviceCommandOutput = "re-run it narrowed to what you need " +
	"(grep, head, tail, a quieter flag), or redirect the output to a file and " +
	"read that back with read_file's \"offset\" and \"limit\""

const elisionAdviceFileLines = "read the missing range with read_file's \"offset\" " +
	"and \"limit\" -- the line numbers either side of this notice are where it starts and ends"

// elideMiddle returns text unchanged when it fits in limit bytes, and
// otherwise its head and its tail with one notice between them saying how
// much was dropped and how to get it.
//
// The cut snaps to line boundaries where there are any, so neither half
// ends mid-line: a half-line of JSON or a half-written line number is
// worse than no line at all, and read_file's numbering only survives as a
// way of naming the gap if the numbers at the cut are whole.
//
// limit <= 0 disables the cap, which is what a caller with nothing left
// in its budget for this stream means -- splitResultBudget never hands
// one out, but a test shrinking maxToolResultBytes to zero would.
func elideMiddle(text string, limit int, advice string) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	headEnd := lineBoundaryBefore(text, limit/2)
	tailStart := lineBoundaryAfter(text, len(text)-(limit-limit/2))
	if tailStart <= headEnd {
		// One line longer than the whole budget (minified JSON, a
		// base64 blob): there is no boundary to snap the tail to
		// above the head, so keep the head and say the rest went.
		tailStart = len(text)
	}

	var b strings.Builder
	b.WriteString(text[:headEnd])
	if headEnd > 0 && !strings.HasSuffix(text[:headEnd], "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "[grain] %s of %s elided here by grain, not by the command -- "+
		"a tool result is capped so one answer cannot eat the run's context. To see it: %s.\n",
		humanBytes(tailStart-headEnd), humanBytes(len(text)), advice)
	b.WriteString(text[tailStart:])
	return b.String()
}

// lineBoundaryBefore returns the offset just past the last newline at or
// before pos, or pos itself when there is no newline to snap to.
func lineBoundaryBefore(text string, pos int) int {
	if i := strings.LastIndexByte(text[:pos], '\n'); i >= 0 {
		return i + 1
	}
	return pos
}

// lineBoundaryAfter returns the offset just past the first newline at or
// after pos, or pos itself when there is no newline to snap to.
func lineBoundaryAfter(text string, pos int) int {
	if pos < 0 {
		pos = 0
	}
	if i := strings.IndexByte(text[pos:], '\n'); i >= 0 {
		return pos + i + 1
	}
	return pos
}

// splitResultBudget divides budget between two streams that share one
// answer -- run_command's stdout and stderr -- so that the pair of them
// is bounded rather than each being bounded separately and the answer
// being twice the cap.
//
// A stream that fits in its half keeps all of it and lends the remainder
// to the other, which is the common shape by far: a build that fails
// prints kilobytes of stderr and megabytes of stdout, or the reverse, and
// halving the big one while the small one leaves half the budget unspent
// would throw away output there was room for.
func splitResultBudget(a, b, budget int) (int, int) {
	if budget <= 0 || a+b <= budget {
		return a, b
	}
	half := budget / 2
	switch {
	case a <= half:
		return a, budget - a
	case b <= half:
		return budget - b, b
	}
	return half, budget - half
}

// humanBytes renders a size the way somebody deciding whether they need
// the missing part reads it: two significant figures, no more.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
