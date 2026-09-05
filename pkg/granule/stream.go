package granule

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/grain"
)

// Stream is a grain's stdout: the one route out, carrying both the
// trajectory and the status snapshots a controller reads instead of
// polling.
//
// Serialised behind a mutex because three things write to it
// concurrently -- the shim's own narration, the guest's console, and the
// agent -- and a log stream is line-oriented. Two interleaved halves of
// a record are not a record a reader can skip past; they are two records
// a reader will misparse.
type Stream struct {
	mu  sync.Mutex
	w   io.Writer
	seq int64
	now func() time.Time
}

// NewStream writes records to w. now is time.Now in production and a
// fixed clock in tests.
func NewStream(w io.Writer, now func() time.Time) *Stream {
	if now == nil {
		now = time.Now
	}
	return &Stream{w: w, now: now}
}

// Seq is the sequence number of the last record emitted, which is what a
// Status reports so a controller can tell whether it is behind.
func (s *Stream) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Emit writes one record and returns its sequence number.
//
// A failure to write is returned rather than swallowed, but callers are
// expected to keep going: stdout being closed is not a reason to abandon
// a running agent, and the termination log is a second way out.
func (s *Stream) Emit(src grain.Source, kind string, data any) (int64, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return 0, fmt.Errorf("granule: marshalling a %s/%s record: %w", src, kind, err)
		}
		raw = b
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	rec := grain.Record{
		Version: grain.Version,
		Seq:     s.seq,
		T:       s.now().UTC(),
		Src:     src,
		Kind:    kind,
		Data:    raw,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return s.seq, fmt.Errorf("granule: marshalling record %d: %w", s.seq, err)
	}
	// json.Marshal escapes anything that could break a line, so the only
	// newline written is this one.
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		return s.seq, fmt.Errorf("granule: writing record %d: %w", s.seq, err)
	}
	return s.seq, nil
}

// LineWriter adapts a byte stream -- the guest's serial console, an
// agent's stdout -- into records, one per line.
//
// Wrapping rather than forwarding raw is what makes a console line
// addressable at all: a run killed by the provisioning budget quotes its
// last console lines in the failure detail, and that needs them to be
// records with sequence numbers rather than bytes that happened to share
// a file descriptor.
func (s *Stream) LineWriter(src grain.Source) io.Writer {
	return &lineWriter{stream: s, src: src}
}

type lineWriter struct {
	stream *Stream
	src    grain.Source
	buf    []byte
}

// maxLine bounds a single record's payload. A guest that prints a
// megabyte without a newline -- a progress bar, a corrupted binary --
// would otherwise buffer it all and then emit one record no reader
// wants. Flushing at the bound loses the line boundary and nothing else.
const maxLine = 16 << 10

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) >= maxLine {
				w.flush(w.buf[:maxLine])
				w.buf = w.buf[maxLine:]
				continue
			}
			break
		}
		w.flush(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return n, nil
}

// Close emits whatever is left without a trailing newline, which is how
// a console's last line before a power-off survives.
func (w *lineWriter) Close() error {
	if len(w.buf) > 0 {
		w.flush(w.buf)
		w.buf = nil
	}
	return nil
}

func (w *lineWriter) flush(line []byte) {
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	// Kind is empty for these: Data is the whole content, and the source
	// already says what it is (pkg/grain's Record.Kind).
	_, _ = w.stream.Emit(w.src, "", string(line))
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
