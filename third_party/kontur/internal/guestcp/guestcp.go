// Package guestcp implements "kontur cp": copying a file or a directory
// between the caller's own stdin/stdout and the VM guest's filesystem,
// over exactly the same vsock session internal/guestexec already uses to
// run a command inside the guest.
//
// # Why this exists at all
//
// `docker cp` and `kubectl cp` reach the *container*, which for a kontur
// VM is a scratch image holding two static binaries and none of the
// guest's filesystem -- and `kubectl cp` fails outright besides, since it
// shells out to a `tar` that image does not carry. A copy that lands in
// the workload has to go through the same door `kontur exec` does, and
// until this existed that meant every caller hand-building a `cat` or
// `tar` pipeline through `kontur exec` and getting the encoding right
// themselves.
//
// Nothing here is new guest-side machinery: this is one ordinary exec
// session running one ordinary shell pipeline, so it works on any guest
// `kontur exec` already works on, needs no new credential, and adds
// nothing to the agent (cmd/kontur-agent) or the wire format
// (internal/execwire).
//
// # Why one side is always "-"
//
// The two filesystems within reach are the guest's and whatever the
// caller's own shell redirects into this container's stdin/stdout -- the
// container's own filesystem holds two binaries and nothing anybody wants
// to copy. So a copy is always "stream in" or "stream out", and "-" is
// which end of it the caller's `docker exec -i`/`kubectl exec -i` is
// wired to.
//
// # Why the bytes are base64 on the wire by default
//
// Because the caller should not have to know which guest they have. The
// transport itself is byte-transparent -- stdin, stdout and stderr are
// separate frames, and there is no pty unless a session asks for one --
// but the guest gets a say too: a guest that puts a pty back in the
// middle of every session (an sshd with a ForceCommand of its own, a
// console wrapper like the reference guest image used to ship) turns
// every LF into CRLF, folds stderr into stdout, and mangles binary in
// both directions. That is what made copying out of a stock guest unsafe
// for anything but text, and what the old README recipes worked around
// with a `base64 | tr -d '\r' | base64 -d` dance the caller had to know
// to type.
//
// Encoding on the wire moves that knowledge in here: base64 survives a
// pty (it is 7-bit, has no control characters, and the decode side
// discards the whitespace a pty adds), so the same command copies a
// binary correctly whether or not something is wrapping the session. The
// price is a third more bytes and a `base64` process at each end, which
// is why "auto" probes the guest once rather than assuming: a guest
// without a `base64` binary falls back to a raw stream, and a guest that
// is *both* pty-wrapped and without one is told so rather than handed a
// corrupt copy. -encode picks explicitly for callers who would rather
// skip the probe -- which also skips what that probe notices about the
// session itself, so a copy *into* a guest that wraps its sessions in a
// pty needs -encode auto to know to end the stream the way a terminal
// requires (see eot).
package guestcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bwsalmon/kontur/internal/guestexec"
)

// The values Options.Encoding takes; see the package comment for what
// the choice costs either way.
const (
	// EncodeAuto probes the guest once and picks between the two below.
	EncodeAuto = "auto"
	// EncodeBase64 encodes the payload on the wire, which survives a
	// guest that runs the session under a pty.
	EncodeBase64 = "base64"
	// EncodeRaw streams the bytes as they are, which is correct only
	// while nothing in the session translates them.
	EncodeRaw = "raw"
)

// Usage is the synopsis ParseArgs quotes back at a caller who got the
// arguments wrong, and cmd/kontur's own -h for this mode.
const Usage = `usage: kontur cp [-tar] [-encode auto|base64|raw] <src> <dst>

Exactly one of <src>/<dst> is "-", this container's own stdin/stdout; the
other is a path inside the guest:

  docker exec -i kontur-vm-web kontur cp - /srv/app.tar < app.tar
  docker exec    kontur-vm-web kontur cp /srv/app.tar - > app.tar
  docker exec -i kontur-vm-web kontur cp -tar - /srv/app < app.tar
  docker exec    kontur-vm-web kontur cp -tar /srv/app - > app.tar`

// Direction says which way a copy goes.
type Direction int

const (
	// ToGuest reads the stream and writes it into the guest.
	ToGuest Direction = iota + 1
	// FromGuest reads from the guest and writes the stream.
	FromGuest
)

// Options is one copy, as ParseArgs understands the command line.
type Options struct {
	// Direction is which way the bytes move.
	Direction Direction

	// Path is the guest-side path: the file to write or read, or -- with
	// Archive -- the directory to unpack into or pack up.
	Path string

	// Archive makes the stream a tar archive and Path a directory, which
	// is what copying anything but a single file takes. Both ends of the
	// archive are the caller's: what goes in is what comes out, so a
	// directory copied out and back in round-trips.
	Archive bool

	// Encoding is one of EncodeAuto (the default), EncodeBase64 or
	// EncodeRaw.
	Encoding string
}

// ParseArgs turns the arguments after "kontur cp" into an Options.
func ParseArgs(args []string) (Options, error) {
	fs := flag.NewFlagSet("kontur cp", flag.ContinueOnError)
	// The flag package's own usage dump is a list of flags with no word
	// about the "-" that makes the whole thing work, so every error here
	// carries Usage instead.
	fs.SetOutput(io.Discard)
	archive := fs.Bool("tar", false, "the stream is a tar archive and the guest path is a directory")
	encoding := fs.String("encode", EncodeAuto, "how to carry the payload over the session: auto, base64 or raw")
	if err := fs.Parse(args); err != nil {
		return Options{}, fmt.Errorf("%w\n\n%s", err, Usage)
	}

	opts := Options{Archive: *archive, Encoding: *encoding}
	switch opts.Encoding {
	case EncodeAuto, EncodeBase64, EncodeRaw:
	default:
		return Options{}, fmt.Errorf("-encode %q: want %q, %q or %q", opts.Encoding, EncodeAuto, EncodeBase64, EncodeRaw)
	}

	rest := fs.Args()
	if len(rest) != 2 {
		return Options{}, fmt.Errorf("kontur cp takes exactly two paths, got %d\n\n%s", len(rest), Usage)
	}
	src, dst := rest[0], rest[1]
	switch {
	case src == "-" && dst == "-":
		return Options{}, fmt.Errorf("both paths are \"-\", so neither of them names anything in the guest\n\n%s", Usage)
	case src == "-":
		opts.Direction, opts.Path = ToGuest, dst
	case dst == "-":
		opts.Direction, opts.Path = FromGuest, src
	default:
		// Not an oversight worth guessing around: a copy between two
		// guest paths is `kontur exec -- cp`, and a copy involving the
		// container's own filesystem is a copy into a scratch image.
		return Options{}, fmt.Errorf("neither %q nor %q is \"-\": kontur cp streams one side through this container's own stdin/stdout, so one of the two has to be \"-\"\n\n%s", src, dst, Usage)
	}
	if opts.Path == "" {
		return Options{}, fmt.Errorf("the guest-side path is empty\n\n%s", Usage)
	}
	return opts, nil
}

// Copy performs one copy, returning once the guest side has finished and
// reported its exit status.
//
// stdin is read for a ToGuest copy and stdout written for a FromGuest
// one; the other is left alone apart from any output the guest-side
// command produces itself. stderr always carries the guest's own stderr,
// so a `tar` complaint or a "No such file or directory" reaches the
// caller as the message it was rather than as an exit code to interpret.
//
// An error means the copy did not (or may not have) completed. Since
// both directions stream, a failure partway through can leave a partial
// file behind at the far end; there is no atomic rename to hide that
// behind, and inventing one would put a second full-sized copy in a
// guest whose disk headroom is fixed at image build time.
func Copy(ctx context.Context, cfg guestexec.Config, opts Options, stdin io.Reader, stdout, stderr io.Writer) error {
	if opts.Direction != ToGuest && opts.Direction != FromGuest {
		return errors.New("guestcp: no copy direction set")
	}
	if opts.Path == "" {
		return errors.New("guestcp: no guest-side path set")
	}

	encoding, wrapped := opts.Encoding, false
	if encoding == "" || encoding == EncodeAuto {
		var err error
		if encoding, wrapped, err = chooseEncoding(ctx, cfg); err != nil {
			return err
		}
	}

	line := commandLine(opts, encoding)
	if opts.Direction == ToGuest {
		// A pty never reports end-of-input to the command reading it
		// just because the far end stopped writing -- that is what the
		// EOT character is for, and only the encoded stream can carry
		// one safely (it ends on a line boundary, and an EOT is not part
		// of the base64 alphabet, so a guest whose session is *not* a
		// terminal would take it for data). See sendEOT.
		return copyToGuest(ctx, cfg, line, encoding, wrapped && encoding == EncodeBase64, stdin, stdout, stderr)
	}
	return copyFromGuest(ctx, cfg, line, encoding, stdout, stderr)
}

// copyToGuest feeds the local stream into the guest-side command.
//
// The stream is fed through a pipe rather than handed to guestexec
// directly, even when nothing has to be encoded, because that is what
// makes a local read failure visible: guestexec's stdin pump treats any
// read error as "no more input" and closes the guest's stdin, which is
// indistinguishable at the guest end from a file that really did end
// there -- the copy would truncate and still exit 0.
func copyToGuest(ctx context.Context, cfg guestexec.Config, line, encoding string, sendEOT bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdin == nil {
		stdin = strings.NewReader("")
	}

	pr, pw := io.Pipe()
	srcErr := make(chan error, 1)
	go func() {
		var err error
		if encoding == EncodeBase64 {
			if err = encodeStream(pw, stdin); err == nil && sendEOT {
				// On its own line, which encodeStream has just ended:
				// a terminal only turns an EOT into end-of-input when
				// nothing else is pending on the current line.
				_, err = pw.Write([]byte{eot})
			}
		} else {
			_, err = io.Copy(pw, stdin)
		}
		// Closing with the error is what stops the guest-side command
		// reading a truncated stream as a complete one: guestexec's pump
		// sees the read fail and ends the session's stdin, and the check
		// below reports what happened.
		pw.CloseWithError(err)
		srcErr <- err
	}()

	code, err := guestexec.RunLine(ctx, cfg, line, pr, stdout, stderr)
	// If the guest went away first, the feeder is still blocked writing
	// into the pipe; this is what unblocks it so the receive below can
	// complete.
	pr.Close()
	feedErr := <-srcErr

	if err != nil {
		return err
	}
	if feedErr != nil && !errors.Is(feedErr, io.ErrClosedPipe) {
		return fmt.Errorf("reading the stream to copy into the guest: %w", feedErr)
	}
	if code != 0 {
		return fmt.Errorf("the guest could not store the copy: its shell exited with status %d", code)
	}
	return nil
}

// copyFromGuest streams the guest-side command's stdout back out,
// decoding it on the way if the session is carrying base64.
func copyFromGuest(ctx context.Context, cfg guestexec.Config, line, encoding string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}

	out := stdout
	var pw *io.PipeWriter
	decErr := make(chan error, 1)
	if encoding == EncodeBase64 {
		var pr *io.PipeReader
		pr, pw = io.Pipe()
		out = pw
		go func() {
			_, err := io.Copy(stdout, base64.NewDecoder(base64.StdEncoding, &whitespaceStripper{r: pr}))
			// A decode that failed must not leave the session writing
			// into a pipe nobody reads any more.
			pr.CloseWithError(err)
			decErr <- err
		}()
	}

	code, err := guestexec.RunLine(ctx, cfg, line, nil, out, stderr)
	var derr error
	if pw != nil {
		// The session is over, so the decoder's input has ended: without
		// this it would block forever waiting for the rest of a stream
		// that is already complete.
		pw.Close()
		derr = <-decErr
	}

	// Order matters, because these three describe the same failure from
	// three distances. A dead session breaks the decode too, so the
	// transport's own error comes first -- except when it is only the
	// pipe the decoder itself closed, which is the decode error wearing
	// a disguise. A guest command that failed (no such file, say) is the
	// next most specific thing, and only then is a stream that would not
	// decode worth blaming on the encoding.
	switch {
	case err != nil && !(derr != nil && errors.Is(err, io.ErrClosedPipe)):
		return err
	case code != 0:
		return fmt.Errorf("the guest could not read the copy: its shell exited with status %d", code)
	case derr != nil:
		return fmt.Errorf("the guest's output was not the base64 stream it was asked for (%w) -- something in the session is rewriting bytes; -encode raw copies without the encoding", derr)
	}
	return nil
}

// commandLine renders the shell command line the guest runs for one
// copy. It is one line for a shell rather than an argv because that is
// what the exec session carries (see internal/execwire's Request), and
// because the tar cases are pipelines anyway.
func commandLine(opts Options, encoding string) string {
	p := shellQuote(opts.Path)
	b64 := encoding == EncodeBase64

	switch {
	case opts.Direction == ToGuest && opts.Archive:
		// mkdir -p, so unpacking into a directory that does not exist
		// yet is a copy rather than an error the caller has to go and
		// fix with a separate exec.
		// No pipeline dance on this side, unlike the other one: `tar` is
		// the last stage here, so its status is already the line's, and
		// a `base64 -d` that fails partway hands it a truncated archive
		// it refuses anyway.
		unpack := "tar -xf - -C " + p
		if b64 {
			unpack = "base64 -d | " + unpack
		}
		return "mkdir -p " + p + " && " + unpack

	case opts.Direction == ToGuest:
		write := "exec cat > " + p
		if b64 {
			write = "exec base64 -d > " + p
		}
		return dirGuard(opts.Path, "is a directory: name the file to write, or pass -tar to unpack an archive into it") + " " + write

	case opts.Archive:
		// "." rather than the directory itself, so the archive holds the
		// directory's contents at its root and unpacking it elsewhere
		// does not bury them under the source's own name.
		pack := "tar -cf - -C " + p + " ."
		if b64 {
			return pipeline(pack, "base64")
		}
		return pack

	default:
		read := "exec cat " + p
		if b64 {
			read = "exec base64 " + p
		}
		return dirGuard(opts.Path, "is a directory: pass -tar to copy it as an archive") + " " + read
	}
}

// pipeline renders "first | second" as a command line whose exit status
// is the *first* stage's whenever that one failed, rather than the
// shell's usual answer of the last stage's alone.
//
// This is what stops a failed copy out reporting success. The pipeline
// above ends in the base64 half, which succeeds happily on the empty
// input a `tar` that could not open its directory leaves behind: "kontur
// cp -tar /nowhere -" wrote an empty archive, exited 0, and said nothing
// -- the guest's own "Cannot open" reached stderr and was the only sign
// anything had gone wrong, which is no use at all to the script that
// went on to treat the empty archive as a backup.
//
// $PIPESTATUS is bash's alone and the guest shell is whichever one
// /etc/passwd names (busybox ash on the Alpine guest), so the first
// stage's status is carried out on a descriptor of its own instead: fd 3
// for the status, captured by the command substitution, and fd 4 holding
// the real stdout that the second stage still has to write the payload
// to. The `exec 4>&1` is separate on purpose -- a redirection written on
// the assignment itself is applied after the substitution has already
// run in bash, and so leaves the second stage writing to a descriptor
// that is not open yet.
func pipeline(first, second string) string {
	return "exec 4>&1; " +
		"st=$( { { " + first + "; echo $? >&3; } | " + second + " >&4; } 3>&1 ); rest=$?; " +
		`if [ "$st" -ne 0 ]; then exit "$st"; fi; exit "$rest"`
}

// dirGuard is a shell fragment that refuses a directory where a file was
// meant, naming the flag that would have copied it. Without it the
// failure is whatever `cat` or `base64` says about a directory, which on
// the way in is nothing at all: `cat > /some/dir` fails, but `tar` never
// gets mentioned.
func dirGuard(path, complaint string) string {
	msg := shellQuote("kontur cp: " + path + " " + complaint)
	return "if [ -d " + shellQuote(path) + " ]; then echo " + msg + " >&2; exit 2; fi;"
}

// eot is what a terminal takes as end-of-input (^D): the byte that makes
// a guest-side `base64 -d` reading a pty see EOF, which nothing else
// does. A pty is stdin, stdout and stderr at once, so the client closing
// its end would end the whole session rather than the input (see
// internal/agent's endStdin) -- end-of-input on a terminal is in band,
// and this is the band.
const eot = 0x04

// chooseEncoding asks the guest what it can do before committing to a
// wire format, which is the whole of what -encode auto means.
//
// Two facts come back from one probe. Whether the guest has a `base64`
// binary decides whether encoding is available at all -- both reference
// guests do (coreutils on Debian, busybox on Alpine), but a guest of
// somebody else's making need not. And whether the probe's own trailing
// newline arrives as CRLF says whether something in the session is
// rewriting bytes, which is exactly the condition raw copies get wrong
// -- and, on the way in, the condition that needs an EOT to end the
// stream at all.
func chooseEncoding(ctx context.Context, cfg guestexec.Config) (encoding string, wrapped bool, err error) {
	hasBase64, wrapped, err := probe(ctx, cfg)
	if err != nil {
		return "", false, err
	}
	switch {
	case hasBase64:
		return EncodeBase64, wrapped, nil
	case wrapped:
		return "", wrapped, errors.New("this guest runs the session under a pty (its output comes back with CRLF line endings) and has no base64 binary to encode around it, so a copy would be corrupted rather than merely slow: install base64 (coreutils or busybox) in the guest, or pass -encode raw to copy anyway")
	default:
		return EncodeRaw, wrapped, nil
	}
}

// probeMarker prefixes the probe's one line of output so it can be
// picked out of anything else the guest's shell decides to say -- a
// wrapped session folds stderr into stdout, and a login banner would
// otherwise be indistinguishable from the answer.
const probeMarker = "kontur-cp-probe"

// probeLine only uses what a POSIX shell has built in: a guest missing
// `base64` is the case being detected, so the detection cannot need
// anything the guest might equally be missing.
const probeLine = "if command -v base64 >/dev/null 2>&1; then b=1; else b=0; fi; printf '" + probeMarker + " base64=%s\\n' \"$b\""

func probe(ctx context.Context, cfg guestexec.Config) (hasBase64, wrapped bool, err error) {
	var out bytes.Buffer
	code, err := guestexec.RunLine(ctx, cfg, probeLine, nil, &out, io.Discard)
	if err != nil {
		return false, false, fmt.Errorf("asking the guest how to encode the copy: %w", err)
	}
	if code != 0 {
		return false, false, fmt.Errorf("asking the guest how to encode the copy: its shell exited with status %d", code)
	}

	// LF is what was sent; CRLF back means a pty translated it, and so
	// that nothing on this session is safe to send unencoded.
	wrapped = bytes.Contains(out.Bytes(), []byte("\r\n"))

	for _, field := range strings.Fields(out.String()) {
		if field == "base64=1" {
			return true, wrapped, nil
		}
		if field == "base64=0" {
			return false, wrapped, nil
		}
	}
	return false, false, fmt.Errorf("the guest's shell did not answer the encoding probe (it said %q): pass -encode base64 or -encode raw to copy without asking", strings.TrimSpace(out.String()))
}

// b64LineWidth is where the encoder breaks lines on the way in.
//
// Not cosmetic: a pty in canonical mode has a fixed line buffer (4096
// bytes on Linux) and quietly drops whatever a line holds past it, so
// one unbroken base64 line is a copy that fails on exactly the guests
// this encoding exists to survive. 76 is the width every base64 tool
// already wraps at, and every decoder ignores newlines.
const b64LineWidth = 76

// encodeStream writes src to w as wrapped base64, ending with a newline.
func encodeStream(w io.Writer, src io.Reader) error {
	lines := &lineBreaker{w: w, width: b64LineWidth}
	enc := base64.NewEncoder(base64.StdEncoding, lines)
	if _, err := io.Copy(enc, src); err != nil {
		return err
	}
	// Close flushes the last partial group and its padding; without it a
	// copy loses up to two bytes off the end.
	if err := enc.Close(); err != nil {
		return err
	}
	return lines.Close()
}

// lineBreaker inserts a newline every width bytes written through it.
type lineBreaker struct {
	w     io.Writer
	width int
	col   int
}

func (l *lineBreaker) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		room := l.width - l.col
		chunk := p
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := l.w.Write(chunk)
		written += n
		l.col += n
		if err != nil {
			return written, err
		}
		p = p[n:]
		if l.col >= l.width {
			if _, err := l.w.Write([]byte{'\n'}); err != nil {
				return written, err
			}
			l.col = 0
		}
	}
	return written, nil
}

// Close ends the final line, so the guest's decoder sees a whole line
// rather than one waiting to be finished.
func (l *lineBreaker) Close() error {
	if l.col == 0 {
		return nil
	}
	l.col = 0
	_, err := l.w.Write([]byte{'\n'})
	return err
}

// whitespaceStripper drops the whitespace out of a base64 stream on the
// way back: the newlines the guest's own `base64` writes, and the
// carriage returns a pty adds to them. Go's decoder ignores \r and \n
// itself, but not spaces or tabs, and a wrapped session is exactly where
// something unexpected turns up in the gaps.
type whitespaceStripper struct {
	r   io.Reader
	err error
}

func (s *whitespaceStripper) Read(p []byte) (int, error) {
	for {
		if s.err != nil {
			return 0, s.err
		}
		n, err := s.r.Read(p)
		s.err = err

		kept := 0
		for _, c := range p[:n] {
			switch c {
			case ' ', '\t', '\r', '\n', '\v', '\f':
			default:
				p[kept] = c
				kept++
			}
		}
		// Never return (0, nil): a read that was all whitespace has to
		// keep going rather than look like a stalled stream.
		if kept > 0 {
			return kept, nil
		}
		if s.err != nil {
			return 0, s.err
		}
	}
}

// shellQuote renders s as a single POSIX shell word, the same way
// guestexec quotes an argv for the same shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
