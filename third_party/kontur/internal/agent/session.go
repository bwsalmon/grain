// Package agent is the guest-side half of "kontur exec": it runs one
// command per connection and speaks internal/execwire back over it.
//
// It is deliberately separate from cmd/kontur-agent, which is only the
// vsock listener around it, so that everything below can be tested
// against an ordinary net.Pipe on a build machine -- there is no VM in
// `go test`, and a session handler that could only be exercised inside
// one would be a session handler nobody exercises.
//
// # Reproducing what sshd did
//
// This replaces an sshd, and the environment a command runs in is part
// of what it replaces. sshd runs a non-interactive command as
// `<shell> -c <line>` with no profile read at all, handing it a fixed
// default PATH; the guest images built on this rely on that in ways that
// look nothing like a shell setting when they break. grain's own guest
// puts the Go and Node toolchains in /usr/local/bin specifically because
// that directory is in sshd's default PATH and nothing it ships reads a
// profile (see grain's scripts/kontur/guest-setup.sh). So loginEnv below
// reproduces that environment rather than inventing a cleaner one, and
// an interactive session (an empty command line) gets a login shell,
// which is the other thing sshd did.
//
// A request can move away from both of those, and that is the point of
// having them be a base rather than the whole answer: Request.Dir picks
// the working directory (the account's home otherwise, as before) and
// Request.Env is overlaid on loginEnv key by key. So a caller that wants
// its own toolchain on PATH says so per command instead of installing
// into /usr/local/bin to land inside the fixed one, and the parts of the
// environment that describe the *account* stay true unless that caller
// deliberately overrides them.
//
// Supplementary groups are part of the same debt. A guest account is
// routinely in a group that grants something -- `docker`, most of all --
// and a session that set only uid and gid would leave every `docker`
// command failing on the socket's permissions, with nothing about the
// error naming the group it lost.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/bwsalmon/kontur/internal/execwire"
)

// defaultPATH is sshd's own default for a session with no profile read,
// and so what a command run here has always seen. Changing it is a
// guest-visible change, not a tidy-up.
const defaultPATH = "/usr/local/bin:/usr/bin:/bin:/usr/games"

// Serve handles exactly one exec session on conn and returns when the
// command has finished and its exit frame has been written.
//
// A failure to *start* the command is reported in the opening Response
// and returns nil: the session was served correctly, it is the command
// that could not run. Only a broken connection or a malformed request is
// an error here.
func Serve(ctx context.Context, conn io.ReadWriter) error {
	br := bufio.NewReader(conn)

	req, err := execwire.ReadRequest(br)
	if err != nil {
		if errors.Is(err, execwire.ErrUnknownField) {
			// A request this build cannot honour in full, rather than a
			// connection that broke: the client is owed the reason, and
			// gets it the same way a command that could not start does.
			_ = execwire.WriteResponse(conn, execwire.Response{OK: false, Error: err.Error(), Features: execwire.Features})
			return nil
		}
		return fmt.Errorf("reading the request: %w", err)
	}

	sess, err := start(ctx, req)
	if err != nil {
		// Best effort: if the connection is gone too, the caller has
		// already lost the answer anyway.
		_ = execwire.WriteResponse(conn, execwire.Response{OK: false, Error: err.Error(), Features: execwire.Features})
		return nil
	}
	// Every response carries the feature list, refusal or not: it is how
	// a client that asked for Dir or Env finds out whether this agent is
	// new enough to have honoured them (see execwire's package comment).
	if err := execwire.WriteResponse(conn, execwire.Response{OK: true, Features: execwire.Features}); err != nil {
		sess.kill()
		return fmt.Errorf("writing the response: %w", err)
	}

	return sess.pump(br, conn)
}

// session is one running command and the file descriptors wired to it.
type session struct {
	cmd *exec.Cmd

	// stdin is what the client's stdin frames are written to, and
	// stdout/stderr what is read back. With a pty all three are the same
	// file: a terminal has one stream, and closing it once is what ends
	// the session.
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	pty   *os.File // nil unless the request asked for one
	slave *os.File // the child's end, closed once the child holds it

	// mu guards writes to the connection: stdout and stderr are pumped
	// by separate goroutines into one stream of frames.
	mu sync.Mutex
}

func start(ctx context.Context, req execwire.Request) (*session, error) {
	acct, err := lookupAccount(req.User)
	if err != nil {
		return nil, err
	}

	argv := []string{acct.Shell, "-c", req.Line}
	if strings.TrimSpace(req.Line) == "" {
		// No command: an interactive login shell, the same fallback
		// sshd makes for a session with no command.
		argv = []string{acct.Shell, "-l"}
	}

	dir, err := workingDir(req.Dir, acct)
	if err != nil {
		return nil, err
	}
	env, err := sessionEnv(acct, req.Env)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if cred := acct.credential(); cred != nil {
		cmd.SysProcAttr.Credential = cred
	}

	s := &session{cmd: cmd}
	if req.TTY {
		if err := s.attachPTY(req.Cols, req.Rows); err != nil {
			return nil, err
		}
	} else if err := s.attachPipes(); err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		s.closeFiles()
		if req.Dir != "" && errors.Is(err, fs.ErrPermission) {
			// The chdir happens in the forked child, after it has
			// dropped to the account's credentials, and the errno that
			// comes back names the binary rather than the directory --
			// "fork/exec /bin/sh: permission denied" for a directory
			// that root can stat and this account cannot enter. Say
			// which of the two it probably was.
			return nil, fmt.Errorf("starting %s in %s: %w (is %s reachable by %s? the agent can see it, the session runs as the account)", argv[0], cmd.Dir, err, cmd.Dir, acct.Name)
		}
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	if s.pty != nil {
		// The child holds the slave side now. Closing this end here is
		// what makes reads on the master return EOF when the child
		// exits, rather than hanging on a descriptor this process still
		// keeps open.
		s.closeSlave()
	}
	return s, nil
}

func (s *session) attachPipes() error {
	var err error
	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		return fmt.Errorf("wiring stdin: %w", err)
	}
	if s.stdout, err = s.cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("wiring stdout: %w", err)
	}
	if s.stderr, err = s.cmd.StderrPipe(); err != nil {
		return fmt.Errorf("wiring stderr: %w", err)
	}
	return nil
}

// pump moves bytes in both directions until the command exits, then
// writes the exit frame.
func (s *session) pump(br *bufio.Reader, w io.Writer) error {
	var wg sync.WaitGroup

	// Output. With a pty, stderr is nil: everything is on the one
	// stream, exactly as it is over SSH with a pty.
	wg.Add(1)
	go func() { defer wg.Done(); s.copyOut(w, s.stdout, execwire.TypeStdout) }()
	if s.stderr != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.copyOut(w, s.stderr, execwire.TypeStderr) }()
	}

	// Input. This one is not waited on: a client that never closes
	// stdin (an interactive session, or any caller that leaves the pipe
	// open) would otherwise hold the session open past the command's own
	// exit. It ends on its own when the connection closes.
	go s.copyIn(br)

	// Drain before reaping, not after. os/exec closes the pipes StdoutPipe
	// and StderrPipe returned as part of Wait, so a Wait that ran first
	// would pull them out from under the goroutines above and truncate
	// whatever the command wrote just before exiting -- its error message,
	// most of the time. Waiting on the output first is also what orders
	// the exit frame behind the output it concludes. With a pty there is
	// no such race, but there is no harm either: reads on the master end
	// fail once the child is gone.
	wg.Wait()
	err := s.cmd.Wait()

	return execwire.WriteFrame(w, execwire.TypeExit, execwire.EncodeExit(exitCode(err)))
}

func (s *session) copyOut(w io.Writer, r io.Reader, typ byte) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.mu.Lock()
			werr := execwire.WriteFrame(w, typ, buf[:n])
			s.mu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *session) copyIn(br *bufio.Reader) {
	for {
		typ, payload, err := execwire.ReadFrame(br)
		if err != nil {
			// The client is gone, so nothing is going to read this
			// command's output or its exit code. Close this end
			// unconditionally -- including the pty, where endStdin
			// deliberately would not -- so a command blocked on a read
			// ends rather than waiting on a client that has left.
			s.closeStdin()
			return
		}
		switch typ {
		case execwire.TypeStdin:
			if _, err := s.stdin.Write(payload); err != nil {
				return
			}
		case execwire.TypeStdinClose:
			s.endStdin()
		case execwire.TypeWinsize:
			cols, rows, err := execwire.DecodeWinsize(payload)
			if err == nil {
				s.resize(cols, rows)
			}
		}
	}
}

// endStdin handles the client saying "no more input".
//
// Without a pty that is a close, and a command blocked on a read sees
// EOF. With one it must not be: stdin, stdout and stderr are the same
// terminal, so closing it would end the session rather than the input,
// and take the command's own output with it. A terminal signals
// end-of-input in band instead, which is the client's business to send
// (^D) and not something to synthesize here.
func (s *session) endStdin() {
	if s.pty != nil {
		return
	}
	s.closeStdin()
}

func (s *session) closeStdin() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
}

func (s *session) closeFiles() {
	s.closeStdin()
	if s.stdout != nil {
		_ = s.stdout.Close()
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
	}
}

func (s *session) kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// exitCode turns Wait's error into the number to report. A command
// killed by a signal has no exit status of its own, and -1 is what
// os/exec already reports for it.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	// Wait failed for a reason that is not the command's own exit --
	// 127 is the shell's own "could not run it", which is the closest
	// true thing to say.
	return 127
}

// account is the parts of a guest account a session needs. os/user
// answers most of it, but not the login shell, and not the groups --
// hence the two small readers below rather than a straight
// user.Lookup.
type account struct {
	Name   string
	UID    int
	GID    int
	Home   string
	Shell  string
	Groups []uint32

	// self is set when the request names the account this process is
	// already running as, in which case no credential is applied: the
	// agent runs as root in the guest, so this is only ever true in
	// tests and when a caller asks for root explicitly.
	self bool
}

func (a *account) credential() *syscall.Credential {
	if a.self {
		return nil
	}
	return &syscall.Credential{Uid: uint32(a.UID), Gid: uint32(a.GID), Groups: a.Groups}
}

func loginEnv(a *account) []string {
	return []string{
		"PATH=" + defaultPATH,
		"HOME=" + a.Home,
		"USER=" + a.Name,
		"LOGNAME=" + a.Name,
		"SHELL=" + a.Shell,
		// A command run over this transport has no terminal type of its
		// own unless the client asked for a pty, and a bare "dumb" is
		// what stops curses programs from trying to draw.
		"TERM=" + envOr("TERM", "dumb"),
	}
}

// sessionEnv overlays a request's own "KEY=value" entries onto the login
// environment, rather than replacing it.
//
// Overlay and not replacement because the parts of loginEnv that come
// from the account -- HOME, USER, LOGNAME, SHELL -- have to keep
// agreeing with the account the command actually runs as. A caller
// asking for GOFLAGS is not asking to be moved out of its home
// directory, and a caller that really does want a different HOME can say
// so, since a later entry wins over the base.
//
// The keys are applied in the order given, and a repeated key takes its
// last value, which is what a shell's own "A=1 A=2 cmd" does.
func sessionEnv(a *account, extra []string) ([]string, error) {
	env := loginEnv(a)
	for _, e := range extra {
		key, _, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			// Not something to pass through and let the command puzzle
			// over: an entry with no "=" is a caller mistake, and the
			// only place that can name it is here.
			return nil, fmt.Errorf("environment entry %q is not in KEY=value form", e)
		}
		env = setEnv(env, key, e)
	}
	return env, nil
}

// setEnv replaces the entry for key in env with entry, or appends it.
func setEnv(env []string, key, entry string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = entry
			return env
		}
	}
	return append(env, entry)
}

// workingDir resolves the directory a session starts in: the request's
// own, when it names one, and otherwise the account's home the way every
// session before this field existed started.
//
// A directory that is not there is refused before the command runs,
// because the alternative is worse than it looks: the chdir happens in
// the forked child, so the failure comes back from Start as
// "fork/exec /bin/sh: no such file or directory" -- which reads as a
// guest with no shell rather than as a caller who asked for a directory
// the guest does not have.
//
// This stat runs as the agent (root), so it answers "is it there", not
// "can this account get into it". The second question is only truly
// answered by the chdir itself, which is why Start's error is annotated
// too rather than trusted to be self-explanatory.
func workingDir(dir string, a *account) (string, error) {
	if dir == "" {
		return a.Home, nil
	}
	if !filepath.IsAbs(dir) {
		// A relative path would be resolved against the agent's own
		// working directory -- whatever init left it at -- which is not
		// something a caller can reason about, and not what "relative"
		// would suggest either.
		return "", fmt.Errorf("working directory %q must be an absolute path", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("working directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("working directory %s is not a directory", dir)
	}
	return dir, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
