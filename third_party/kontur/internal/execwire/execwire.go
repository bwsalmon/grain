// Package execwire is the wire format "kontur exec" and the guest's
// kontur-agent speak to each other over a virtio-vsock connection.
//
// It exists because the transport underneath it carries one byte stream
// and an exec session needs several: stdin in, stdout and stderr back
// separately, terminal resizes while the command runs, and an exit code
// at the end that has to be distinguishable from the command's own
// output. SSH gave all of that for free, which is most of why it was the
// transport to begin with; this is the part that has to be rebuilt to
// stop paying for the rest of SSH (a daemon, host keys, a keypair, an
// account to log into) on every guest.
//
// The shape is deliberately the smallest thing that carries a session:
// one JSON request line, one JSON response line, then length-prefixed
// frames until the exit frame. There is no version negotiation and no
// keepalive, because both ends are built from one commit and shipped in
// one image -- kontur-agent comes out of the same tree as the konturctl
// that talks to it, the same way the guest image and konturctl already
// have to agree (see the Dockerfile's guest-rootfs stages). A mismatched
// pair is a build mistake, not a runtime case to negotiate around.
//
// # Two ends, one commit -- and what happens when they aren't
//
// That doctrine holds right up until an optional field is added, because
// JSON's own default is to ignore what it does not recognize. An agent
// that predates Request.Dir does not fail on a request that sets it: it
// starts the command in the account's home directory, reports OK, and
// the caller who asked for /src gets a clean run of the wrong thing.
// That is the one failure this protocol cannot afford to make quiet, so
// it is handled from both sides:
//
//   - The agent lists what it implements. Response.Features names the
//     optional Request fields the agent honours, and a client that set
//     one checks for it (Response.MissingFeatures) rather than assuming.
//     This is the half that catches today's mismatch -- a guest image
//     older than the client -- because it needs nothing of the old
//     agent but the response it already sends. It cannot catch it before
//     the command starts, since the response is what reports the start;
//     it turns a wrong answer into a loud error, not into a no-op.
//   - The agent rejects fields it does not know. ReadRequest decodes
//     strictly, so a request carrying a field this agent has never heard
//     of is refused before anything runs, naming the field. That is the
//     half that catches the mismatch the other way round -- a client
//     newer than the guest image -- and it starts working for the next
//     field added, not this one, since an agent from before this comment
//     ignores unknown fields by construction. Every optional field is
//     "omitempty" for exactly this reason: a newer client that isn't
//     using a field doesn't send it, and so still talks to an older
//     agent.
//
// Neither is version negotiation, and neither makes a mismatched pair
// supported. They make one say so.
//
// # Frame types need the same line
//
// A frame type added later has the same problem and only the first half
// of the answer, because the frame stream cannot refuse what it does not
// know: the reader has the length prefix, so it can always step over an
// unrecognized frame. Skipping it is the right behaviour for a frame
// whose absence is cosmetic -- a terminal resize nobody applied -- and
// exactly the wrong one for a frame whose absence changes what happens.
// TypeSignal asks an agent to interrupt a command, and an agent too old
// to know the type answers by leaving it running.
//
// So a frame type whose absence matters gets a feature name too
// (FeatureSignal), and the client looks for it in Response.Features
// before relying on the frame. Unlike the optional request fields, this
// is not something MissingFeatures should fail a session over: a client
// that cannot signal has a worse fallback rather than no fallback --
// dropping the connection, which is all it could do before the frame
// existed -- so it checks for itself and takes that path.
//
// # Trust
//
// There is no authentication here, and that is a deliberate consequence
// of what vsock is rather than an omission. A virtio-vsock connection
// can only be opened from the VM's own hypervisor process -- for
// cloud-hypervisor's hybrid vsock, by connecting to a unix socket inside
// the VM's container -- so reaching this protocol at all already means
// having the run of that container. That is the same reach the SSH
// transport granted, since the private key sat in the same container on
// the same filesystem; what changes is that the guest no longer has to
// run a network service, hold an authorized key, or have an account to
// log into for kontur to get in.
package execwire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Port is the vsock port kontur-agent listens on and "kontur exec"
// connects to. Above the 1024 privileged range and otherwise arbitrary;
// it is fixed rather than configurable because both ends ship together.
const Port = 1024

// MaxPayload bounds a single frame's payload, and so how much a
// malformed or hostile length prefix can make the other end allocate.
// 64KiB is comfortably above the pipe buffer sizes either side reads
// into, so it never costs an extra frame in practice.
const MaxPayload = 64 << 10

// Request is the first line a client sends: what to run and how.
type Request struct {
	// Line is the command line, already shell-quoted and joined by the
	// caller, to be run by the login shell of User. Empty asks for an
	// interactive login shell instead, which is what "kontur exec" with
	// no command means.
	Line string `json:"line"`

	// User is the guest account to run as. Empty means root.
	User string `json:"user,omitempty"`

	// TTY asks the agent to allocate a pseudo-terminal for the session.
	// When it is set, stderr is not separated: a terminal has one output
	// stream, so everything arrives as Stdout frames, exactly as it does
	// over SSH with a pty.
	TTY bool `json:"tty,omitempty"`

	// Cols and Rows are the initial terminal size, meaningful only with
	// TTY. Zero leaves the pty at its own default rather than sizing it
	// to nothing.
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`

	// Dir is the working directory to run the command in. Empty leaves
	// it at the home directory of User, which is what a session has
	// always started in. It must be an absolute path: a relative one
	// would be resolved against the agent's own working directory, which
	// is not anything a caller can reason about.
	Dir string `json:"dir,omitempty"`

	// Env are "KEY=value" entries overlaid on the environment the agent
	// builds for User -- not a replacement for it. A key that is already
	// in that environment is overridden, and everything else stays, so
	// HOME/USER/LOGNAME keep agreeing with the account the command runs
	// as unless a caller deliberately says otherwise.
	Env []string `json:"env,omitempty"`
}

// FeatureDirEnv is the feature name an agent reports in Response.Features
// when it honours Request.Dir and Request.Env. See RequiredFeatures for
// why a client has to look.
const FeatureDirEnv = "dir-env"

// FeatureSignal is the feature name an agent reports when it acts on a
// TypeSignal frame. It is not a Request field, so RequiredFeatures never
// names it and MissingFeatures never fails a session over it: a client
// looks for it at the point it wants to signal, and falls back to
// dropping the connection when it is absent. See "Frame types need the
// same line" above.
const FeatureSignal = "signal"

// Features is every feature name an agent built from this commit
// implements, and so what it reports in its Response.
var Features = []string{FeatureDirEnv, FeatureSignal}

// RequiredFeatures names the features an agent must implement for this
// request to mean what it says -- the optional fields it actually sets,
// as opposed to the ones it leaves at their zero value.
func (r Request) RequiredFeatures() []string {
	var need []string
	if r.Dir != "" || len(r.Env) > 0 {
		need = append(need, FeatureDirEnv)
	}
	return need
}

// Response is the first line the agent sends back, before any frame.
// It reports whether the command could be *started*; a command that
// starts and then fails reports that through its exit frame instead, so
// that "no such file" from the agent and a non-zero exit from the
// command never get confused for each other.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// Features names the optional parts of the protocol this agent
	// understands -- request fields and frame types both (see Features
	// and Supports). It is the one capability line in the protocol, and
	// it exists because an agent that predates a field ignores it
	// silently, and one that predates a frame type steps over it -- see
	// "Two ends, one commit" above.
	Features []string `json:"features,omitempty"`
}

// Supports reports whether the agent that sent this response named
// feature as one it implements.
func (r Response) Supports(feature string) bool {
	for _, f := range r.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// MissingFeatures returns the features req needs that this response does
// not report, in the order req needs them. An empty result means the
// agent understood everything the request asked for.
func (r Response) MissingFeatures(req Request) []string {
	var missing []string
	for _, f := range req.RequiredFeatures() {
		if !r.Supports(f) {
			missing = append(missing, f)
		}
	}
	return missing
}

// Frame types. Client-to-server and server-to-client types share one
// number space so that a frame arriving on the wrong side is a decode
// error rather than a plausible frame of another kind.
const (
	TypeStdin      byte = 1 // client -> agent
	TypeStdinClose byte = 2 // client -> agent, empty payload
	TypeWinsize    byte = 3 // client -> agent, 4 bytes: cols, rows
	TypeStdout     byte = 4 // agent -> client
	TypeStderr     byte = 5 // agent -> client
	TypeExit       byte = 6 // agent -> client, 4 bytes: exit code
	TypeSignal     byte = 7 // client -> agent, 4 bytes: signal number
)

// MaxSignal is the highest signal number a TypeSignal frame may carry:
// Linux's real-time signals end at SIGRTMAX, 64. Naming a ceiling here
// keeps a decoded number safe to hand straight to kill(2) rather than
// something the agent has to sanity-check for itself.
const MaxSignal = 64

// ErrPayloadTooLarge is returned by ReadFrame for a length prefix past
// MaxPayload, rather than attempting the allocation it asks for.
var ErrPayloadTooLarge = errors.New("execwire: frame payload exceeds the maximum")

// WriteRequest sends r as the session's opening line.
func WriteRequest(w io.Writer, r Request) error {
	return writeJSONLine(w, r)
}

// ErrUnknownField is what ReadRequest reports for a request naming a
// field this build does not have, so the agent can tell it apart from a
// broken connection and answer with a Response instead of hanging up.
var ErrUnknownField = errors.New("execwire: the request asks for something this agent does not understand")

// requestFields is every JSON name Request has, taken from the struct
// itself so the check below cannot drift from the type it guards.
var requestFields = jsonFieldNames(reflect.TypeOf(Request{}))

// ReadRequest reads the opening line written by WriteRequest.
//
// A field this build has never heard of is an error rather than
// something to skip past: it is a client asking for behaviour this agent
// does not have, and serving the request as if the field were not there
// is the one mistake this protocol must not make quietly. See "Two ends,
// one commit" above.
func ReadRequest(r *bufio.Reader) (Request, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return Request{}, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return Request{}, err
	}
	var unknown []string
	for name := range raw {
		if _, ok := requestFields[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Request{}, fmt.Errorf("%w: %s -- the guest's kontur-agent is older than the client that dialled it, and the two ship from one commit", ErrUnknownField, strings.Join(unknown, ", "))
	}

	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// jsonFieldNames returns the JSON names of t's fields.
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	names := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names[name] = struct{}{}
		}
	}
	return names
}

// WriteResponse sends resp as the reply to a Request.
func WriteResponse(w io.Writer, resp Response) error {
	return writeJSONLine(w, resp)
}

// ReadResponse reads the line written by WriteResponse.
func ReadResponse(r *bufio.Reader) (Response, error) {
	var resp Response
	err := readJSONLine(r, &resp)
	return resp, err
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func readJSONLine(r *bufio.Reader, v any) error {
	// ReadString rather than a json.Decoder: a Decoder buffers ahead of
	// the value it decodes, which would swallow the head of the frame
	// stream that follows this line on the same connection.
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(line), v)
}

// WriteFrame writes one typed frame. A zero-length payload is a valid
// frame -- TypeStdinClose is nothing else.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame written by WriteFrame. At a clean end of
// stream it returns io.EOF.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		// A frame that ends mid-header is a truncated stream, but an EOF
		// exactly on the boundary is the ordinary end of one, so only the
		// first is worth renaming.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("execwire: truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, n)
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("execwire: truncated frame payload: %w", err)
	}
	return hdr[0], payload, nil
}

// EncodeWinsize renders a terminal size as a TypeWinsize payload.
func EncodeWinsize(cols, rows uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b[:]
}

// DecodeWinsize reads a payload written by EncodeWinsize.
func DecodeWinsize(payload []byte) (cols, rows uint16, err error) {
	if len(payload) != 4 {
		return 0, 0, fmt.Errorf("execwire: winsize payload is %d bytes, want 4", len(payload))
	}
	return binary.BigEndian.Uint16(payload[0:]), binary.BigEndian.Uint16(payload[2:]), nil
}

// EncodeSignal renders a signal number as a TypeSignal payload.
func EncodeSignal(sig int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(sig))
	return b[:]
}

// DecodeSignal reads a payload written by EncodeSignal. Signal 0 is
// rejected along with everything past MaxSignal: kill(2) reads it as
// "does this process exist?", which is not something a session has any
// business asking and not something a client can have meant.
func DecodeSignal(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("execwire: signal payload is %d bytes, want 4", len(payload))
	}
	sig := binary.BigEndian.Uint32(payload)
	if sig == 0 || sig > MaxSignal {
		return 0, fmt.Errorf("execwire: signal %d is outside 1..%d", sig, MaxSignal)
	}
	return int(sig), nil
}

// EncodeExit renders an exit code as a TypeExit payload.
func EncodeExit(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}

// DecodeExit reads a payload written by EncodeExit.
func DecodeExit(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("execwire: exit payload is %d bytes, want 4", len(payload))
	}
	return int(int32(binary.BigEndian.Uint32(payload))), nil
}
