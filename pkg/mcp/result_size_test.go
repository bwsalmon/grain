package mcp

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// shrinkMaxToolResultBytes caps results at something a test can produce
// in a line or two, rather than generating 16 KB to prove the cap works.
func shrinkMaxToolResultBytes(t *testing.T, n int) {
	t.Helper()
	old := maxToolResultBytes
	maxToolResultBytes = n
	t.Cleanup(func() { maxToolResultBytes = old })
}

func TestElideMiddleKeepsHeadAndTailAndSaysWhatWentMissing(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("line of output\n")
	}
	text := b.String()

	// The limit bounds the output kept, not the notice grain adds on
	// top of it -- which is a few hundred bytes, once.
	got := elideMiddle(text, 1000, elisionAdviceCommandOutput)
	if len(got) > 1000+len(elisionAdviceCommandOutput)+200 {
		t.Errorf("elided text is %d bytes, want near the 1000-byte limit", len(got))
	}
	if !strings.HasPrefix(got, "line of output\n") {
		t.Error("elided text lost the head of the output")
	}
	if !strings.HasSuffix(got, "line of output\n") {
		t.Error("elided text lost the tail of the output")
	}
	if !strings.Contains(got, "elided here by grain, not by the command") {
		t.Errorf("elided text does not say grain cut it:\n%s", got)
	}
	if !strings.Contains(got, "read that back with read_file's") {
		t.Errorf("elided text does not say how to get the rest:\n%s", got)
	}
	// Neither half may end mid-line: a half-written line reads as
	// output the command produced rather than as a cut.
	head, _, _ := strings.Cut(got, "[grain]")
	if !strings.HasSuffix(head, "\n") {
		t.Errorf("head does not end at a line boundary: %q", head[max(0, len(head)-40):])
	}
}

func TestElideMiddleLeavesTextThatFitsAlone(t *testing.T) {
	text := "small enough\n"
	if got := elideMiddle(text, 1000, elisionAdviceCommandOutput); got != text {
		t.Errorf("got %q, want it untouched", got)
	}
}

// TestElideMiddleHandlesOneOverlongLine is the degenerate case: minified
// JSON, a base64 blob, a `curl` of a whole page -- there is no line
// boundary to snap a tail to, and the notice still has to arrive rather
// than the whole line coming through uncut.
func TestElideMiddleHandlesOneOverlongLine(t *testing.T) {
	text := strings.Repeat("x", 5000)
	got := elideMiddle(text, 1000, elisionAdviceCommandOutput)
	if len(got) > 1400 {
		t.Errorf("one long line elided to %d bytes, want near the 1000-byte limit", len(got))
	}
	if !strings.Contains(got, "[grain]") {
		t.Errorf("no elision notice for a single overlong line:\n%s", got)
	}
}

func TestSplitResultBudgetLendsTheUnusedHalfToTheOtherStream(t *testing.T) {
	// Both fit: neither is touched.
	if a, b := splitResultBudget(10, 20, 100); a != 10 || b != 20 {
		t.Errorf("got (%d, %d), want (10, 20)", a, b)
	}
	// A small stderr next to a huge stdout leaves stdout the rest,
	// rather than halving stdout and wasting the other half.
	if a, b := splitResultBudget(1000, 10, 100); a != 90 || b != 10 {
		t.Errorf("got (%d, %d), want (90, 10)", a, b)
	}
	if a, b := splitResultBudget(10, 1000, 100); a != 10 || b != 90 {
		t.Errorf("got (%d, %d), want (10, 90)", a, b)
	}
	// Both oversized: half each, and never more than the budget
	// between them.
	a, b := splitResultBudget(1000, 1000, 100)
	if a+b != 100 {
		t.Errorf("got (%d, %d), want them to sum to the 100-byte budget", a, b)
	}
}

// TestRunCommandCapsItsOutput is docs/agent-ergonomics.md's finding 4 for
// run_command: one `go test ./...` on a failing suite, one `git log -p`
// or one over-broad `grep -r` used to come back whole and spend a large
// part of the run's context in a single turn.
func TestRunCommandCapsItsOutput(t *testing.T) {
	if _, err := exec.LookPath("seq"); err != nil {
		t.Skip("seq not installed")
	}
	shrinkMaxToolResultBytes(t, 2000)

	client := newTestClient(t, t.TempDir())
	res, err := client.CallTool(context.Background(), "run_command", map[string]any{
		"command": "seq 1 20000",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Text()
	if len(text) > 3000 {
		t.Errorf("run_command answer is %d bytes against a 2000-byte cap", len(text))
	}
	if !strings.Contains(text, "elided here by grain") {
		t.Errorf("capped run_command answer does not say it was cut:\n%s", text)
	}
	// The head and the tail are what a run reads: the command it ran
	// and how it ended.
	if !strings.Contains(text, "\n1\n") {
		t.Error("capped run_command answer lost the head of stdout")
	}
	if !strings.Contains(text, "\n20000\n") {
		t.Error("capped run_command answer lost the tail of stdout")
	}
}

// TestReadFileCapsAWholeFileAndPointsAtOffsetAndLimit is finding 4 for
// read_file, where the way back to the missing part is already in the
// answer: the cat -n numbering either side of the cut is the "offset" the
// next call needs.
func TestReadFileCapsAWholeFileAndPointsAtOffsetAndLimit(t *testing.T) {
	shrinkMaxToolResultBytes(t, 2000)

	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	var content strings.Builder
	for i := 1; i <= 2000; i++ {
		content.WriteString("a line of a rather long file\n")
	}
	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "big.txt", "content": content.String(),
	}); err != nil {
		t.Fatal(err)
	}

	res, err := client.CallTool(ctx, "read_file", map[string]any{"file_path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Text()
	if len(text) > 3000 {
		t.Errorf("read_file answer is %d bytes against a 2000-byte cap", len(text))
	}
	if !strings.Contains(text, "read the missing range with read_file's \"offset\"") {
		t.Errorf("capped read_file answer does not say how to read the rest:\n%s", text)
	}
	if !strings.Contains(text, "     1\ta line") {
		t.Error("capped read_file answer lost the first line")
	}
	if !strings.Contains(text, "  2000\ta line") {
		t.Error("capped read_file answer lost the last line")
	}
}

// TestReadFileLeavesASmallFileExactlyAsItWas: the cap is a backstop for
// the answers that were eating a run's context, not a change to what
// read_file returns for the files it is normally pointed at.
func TestReadFileLeavesASmallFileExactlyAsItWas(t *testing.T) {
	client := newTestClient(t, t.TempDir())
	ctx := context.Background()
	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "f.txt", "content": "a\nb\nc\n",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := client.CallTool(ctx, "read_file", map[string]any{"file_path": "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := "     1\ta\n     2\tb\n     3\tc"
	if res.Text() != want {
		t.Errorf("got %q, want %q", res.Text(), want)
	}
}
