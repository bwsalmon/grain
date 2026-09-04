package execwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The whole session shares one byte stream: two JSON lines, then frames.
// This is the property that decides how the lines are read -- a
// json.Decoder buffers past the value it decodes and would eat the first
// frames -- so it is worth pinning rather than leaving to the
// implementation to remember.
func TestTheFrameStreamSurvivesReadingTheOpeningLines(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, Request{Line: "echo hi", TTY: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := WriteResponse(&buf, Response{OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TypeStdout, []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TypeExit, EncodeExit(0)); err != nil {
		t.Fatal(err)
	}

	r := bufio.NewReader(&buf)
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.Line != "echo hi" || !req.TTY || req.Cols != 80 || req.Rows != 24 {
		t.Fatalf("request round trip = %+v", req)
	}
	resp, err := ReadResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("response round trip = %+v", resp)
	}

	typ, payload, err := ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeStdout || string(payload) != "hi\n" {
		t.Fatalf("first frame after the lines = %d %q, want the stdout frame", typ, payload)
	}
	typ, payload, err = ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	code, err := DecodeExit(payload)
	if err != nil || typ != TypeExit || code != 0 {
		t.Fatalf("second frame = %d %q (code %d, err %v)", typ, payload, code, err)
	}
}

// An empty payload is a frame in its own right: TypeStdinClose is
// nothing else, so "no payload" must not read as "no frame".
func TestAnEmptyPayloadIsStillAFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeStdinClose, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TypeStdout, []byte("after")); err != nil {
		t.Fatal(err)
	}

	typ, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeStdinClose || len(payload) != 0 {
		t.Fatalf("= %d %q, want an empty stdin-close frame", typ, payload)
	}
	if typ, payload, err = ReadFrame(&buf); err != nil || typ != TypeStdout || string(payload) != "after" {
		t.Fatalf("the frame after an empty one = %d %q (%v)", typ, payload, err)
	}
}

// The length prefix decides an allocation, so a hostile or corrupt one
// has to be refused by size rather than attempted and then discovered.
func TestAnOversizedLengthIsRefusedRatherThanAllocated(t *testing.T) {
	var hdr [5]byte
	hdr[0] = TypeStdout
	binary.BigEndian.PutUint32(hdr[1:], MaxPayload+1)

	if _, _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("reading an oversized length = %v, want ErrPayloadTooLarge", err)
	}
	if err := WriteFrame(io.Discard, TypeStdout, make([]byte, MaxPayload+1)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("writing an oversized payload = %v, want ErrPayloadTooLarge", err)
	}
	// The boundary itself is allowed, so the limit is "too large" rather
	// than "as large".
	if err := WriteFrame(io.Discard, TypeStdout, make([]byte, MaxPayload)); err != nil {
		t.Fatalf("writing exactly MaxPayload = %v, want it accepted", err)
	}
}

// The end of a session and a session cut off mid-frame are different
// facts: one is the agent having finished, the other is the VM having
// gone away with a command still running. A caller that cannot tell them
// apart reports a killed guest as a clean exit.
func TestACleanEndAndATruncatedFrameAreDifferentErrors(t *testing.T) {
	if _, _, err := ReadFrame(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Fatalf("reading at a frame boundary = %v, want io.EOF", err)
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeStdout, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	truncated := buf.Bytes()[:len(buf.Bytes())-4]
	_, _, err := ReadFrame(bytes.NewReader(truncated))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("reading a frame cut short = %v, want an error that is not io.EOF", err)
	}

	// Cut inside the header rather than the payload: also truncation,
	// and also not a clean end.
	if _, _, err := ReadFrame(bytes.NewReader([]byte{TypeStdout, 0, 0})); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("reading a partial header = %v, want an error that is not io.EOF", err)
	}
}

func TestWinsizeAndExitRoundTrip(t *testing.T) {
	cols, rows, err := DecodeWinsize(EncodeWinsize(120, 40))
	if err != nil || cols != 120 || rows != 40 {
		t.Fatalf("winsize round trip = %d %d (%v)", cols, rows, err)
	}
	// Exit codes are carried as a signed value: a caller that reports
	// -1 for "killed by a signal" must not read it back as 4294967295.
	for _, want := range []int{0, 1, 127, 255, -1} {
		got, err := DecodeExit(EncodeExit(want))
		if err != nil || got != want {
			t.Fatalf("exit round trip of %d = %d (%v)", want, got, err)
		}
	}
	if _, _, err := DecodeWinsize([]byte{1, 2}); err == nil {
		t.Error("a short winsize payload decoded without error")
	}
	if _, err := DecodeExit([]byte{1, 2}); err == nil {
		t.Error("a short exit payload decoded without error")
	}
}

func TestSignalRoundTrip(t *testing.T) {
	for _, want := range []int{1, 2, 9, 15, MaxSignal} {
		got, err := DecodeSignal(EncodeSignal(want))
		if err != nil || got != want {
			t.Fatalf("signal round trip of %d = %d (%v)", want, got, err)
		}
	}
	// Signal 0 is kill(2)'s "does this process exist?", which nothing
	// here has any business asking, and a number past SIGRTMAX is not a
	// signal at all -- both are refused at the decode rather than left
	// for the agent to notice.
	for _, sig := range []int{0, MaxSignal + 1} {
		if _, err := DecodeSignal(EncodeSignal(sig)); err == nil {
			t.Errorf("signal %d decoded without error", sig)
		}
	}
	if _, err := DecodeSignal([]byte{1, 2}); err == nil {
		t.Error("a short signal payload decoded without error")
	}
}

// A frame type an older agent would step over in silence is announced by
// name like a field is, and reaches a client through the same response
// it already reads. Nothing refuses a session over it: the point is that
// a client which wants to signal can tell whether the frame will be
// acted on, and drop the connection instead when it will not.
func TestTheSignalFrameIsAnnouncedAsAFeature(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResponse(&buf, Response{OK: true, Features: Features}); err != nil {
		t.Fatal(err)
	}
	resp, err := ReadResponse(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Supports(FeatureSignal) {
		t.Errorf("an agent from this commit does not report %s", FeatureSignal)
	}

	// An agent that predates the frame names nothing, which is the case
	// a client has to be able to recognise. It is not a MissingFeatures
	// answer, because a request cannot ask for the frame in advance --
	// the client checks for itself and falls back.
	older := Response{OK: true}
	if older.Supports(FeatureSignal) {
		t.Errorf("a response with no features reported %s", FeatureSignal)
	}
	if missing := older.MissingFeatures(Request{Line: "true"}); len(missing) != 0 {
		t.Errorf("MissingFeatures on a plain request = %v, want the signal frame left out of it", missing)
	}
}

// Dir and Env are the two fields a caller sets to move a command out of
// the account's home directory and away from the fixed login
// environment, so they have to survive the trip verbatim -- an Env entry
// with an "=" in its value included, since that is what a command line
// or a PATH looks like.
func TestTheWorkingDirectoryAndEnvironmentRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Request{
		Line: "go build ./...",
		Dir:  "/src",
		Env:  []string{"GOFLAGS=-mod=vendor", "PATH=/opt/go/bin:/usr/bin"},
	}
	if err := WriteRequest(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != want.Dir || strings.Join(got.Env, "\x00") != strings.Join(want.Env, "\x00") {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// A request that sets neither must stay on the wire exactly as it was
// before those fields existed: that is what lets a client built from
// this commit still talk to an agent built before it, which is the only
// compatibility this protocol offers at all.
func TestAnUnsetFieldIsNotSentAtAll(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, Request{Line: "echo hi"}); err != nil {
		t.Fatal(err)
	}
	if line := buf.String(); strings.Contains(line, "dir") || strings.Contains(line, "env") {
		t.Fatalf("request line = %s, want no mention of the fields it did not set", line)
	}
}

// The agent's half of the mismatch answer: a field this build has never
// heard of is refused, and the error names the field rather than
// arriving as a generic decode failure -- see the package comment.
func TestAFieldThisBuildDoesNotKnowIsRefused(t *testing.T) {
	line := `{"line":"echo hi","teleport":"/src"}` + "\n"
	_, err := ReadRequest(bufio.NewReader(strings.NewReader(line)))
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("ReadRequest on a request with an unknown field = %v, want ErrUnknownField", err)
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("error %q does not name the field that caused it", err)
	}
}

// Every field Request actually has must pass that check, which is only
// true while the known-field set is derived from the struct. A field
// added with a json tag and forgotten here would otherwise make the
// agent refuse the client that ships with it.
func TestEveryFieldRequestHasIsAccepted(t *testing.T) {
	var buf bytes.Buffer
	req := Request{Line: "echo hi", User: "root", TTY: true, Cols: 80, Rows: 24, Dir: "/src", Env: []string{"A=1"}}
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(bufio.NewReader(&buf)); err != nil {
		t.Fatalf("ReadRequest on a fully populated request: %v", err)
	}
	if len(requestFields) != len(jsonFieldNames(reflect.TypeOf(Request{}))) {
		t.Fatal("the known-field set is not the struct's own")
	}
}

// The client's half: what it asked for and what the agent said it
// implements are compared by name, so a response from an agent that
// predates the field reads as "missing" rather than as agreement.
func TestMissingFeaturesNamesWhatTheAgentDidNotHonour(t *testing.T) {
	req := Request{Line: "true", Dir: "/src"}
	if missing := (Response{OK: true, Features: Features}).MissingFeatures(req); len(missing) != 0 {
		t.Fatalf("an agent from this commit reported %v as missing", missing)
	}
	// An agent from before the field: OK, and no features at all.
	missing := (Response{OK: true}).MissingFeatures(req)
	if len(missing) != 1 || missing[0] != FeatureDirEnv {
		t.Fatalf("MissingFeatures against an older agent = %v, want [%s]", missing, FeatureDirEnv)
	}
	// A request that asked for nothing optional is served by any agent.
	if missing := (Response{OK: true}).MissingFeatures(Request{Line: "true"}); len(missing) != 0 {
		t.Fatalf("a plain request reported %v as missing", missing)
	}
}
