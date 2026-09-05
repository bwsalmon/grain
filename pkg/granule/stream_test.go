package granule_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/granule"
)

// A log stream is line-oriented and a reader may join it anywhere, so a
// record that spans lines is not one a reader skips -- it is two records
// a reader misparses. This is the invariant every other read of the
// stream depends on.
func TestNoRecordEverSpansALine(t *testing.T) {
	var out bytes.Buffer
	s := granule.NewStream(&out, func() time.Time { return time.Unix(0, 0) })

	console := s.LineWriter(granule.SrcConsole)
	// Everything a guest console can plausibly emit: embedded newlines
	// arriving mid-write, carriage returns, quotes and control bytes.
	console.Write([]byte("[    0.0] boot\r\nsystemd[1]: \"quoted\"\tand\ttabs\n"))
	console.Write([]byte("half a line"))
	console.Write([]byte(" and the rest\n"))
	if _, err := s.Emit(granule.SrcShim, granule.KindStatus, granule.Status{Activity: "line\nbreak\nin a phrase"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i, line := range lines {
		var r granule.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %d is not one whole record: %q: %v", i, line, err)
		}
	}
	if len(lines) != 4 {
		t.Errorf("got %d records, want 4:\n%s", len(lines), out.String())
	}
}

// Seq is the cursor a controller pages the trajectory by, so it has to
// be monotonic no matter who is writing -- the shim, the console and the
// agent all share this stream.
func TestSeqIsMonotonicUnderConcurrentWriters(t *testing.T) {
	var out bytes.Buffer
	s := granule.NewStream(&out, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.LineWriter(granule.SrcAgent)
			for j := 0; j < 25; j++ {
				w.Write([]byte("a line\n"))
				_, _ = s.Emit(granule.SrcShim, granule.KindPhase, "running")
			}
		}()
	}
	wg.Wait()

	seen := map[int64]bool{}
	var last int64
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var r granule.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("interleaved writers produced an unparseable line: %q", line)
		}
		if seen[r.Seq] {
			t.Fatalf("sequence number %d was used twice", r.Seq)
		}
		seen[r.Seq] = true
		if r.Seq > last {
			last = r.Seq
		}
	}
	if int(last) != len(seen) {
		t.Errorf("highest seq %d over %d records: the sequence has gaps", last, len(seen))
	}
}

// A guest that prints without ever ending a line must not buffer
// unboundedly, and must not stall the stream either.
func TestAnUnterminatedLineIsFlushedAtTheBound(t *testing.T) {
	var out bytes.Buffer
	s := granule.NewStream(&out, nil)
	w := s.LineWriter(granule.SrcConsole)

	w.Write(bytes.Repeat([]byte("x"), 40<<10))
	if out.Len() == 0 {
		t.Fatal("40KiB with no newline produced no records at all")
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var r granule.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a flushed chunk is not a record: %v", err)
		}
	}
}
