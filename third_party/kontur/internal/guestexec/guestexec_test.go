package guestexec

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/kontur/internal/agent"
	"github.com/bwsalmon/kontur/internal/execwire"
)

// fakeCHV stands in for cloud-hypervisor's hybrid-vsock endpoint: a unix
// socket that answers the CONNECT handshake and then hands the
// connection to the real guest-side agent.
//
// Using the real internal/agent rather than a scripted responder is the
// point. What these tests are for is the seam between the two halves --
// the handshake, the framing, which stream carries what, where the exit
// code comes from -- and a fake on the far side would only ever agree
// with whatever this side happens to do. There is no VM here, so the
// commands run on the build machine; everything else is the production
// path.
type fakeCHV struct {
	socket string

	// refuse makes the endpoint answer the handshake the way
	// cloud-hypervisor does when nothing inside the guest is listening
	// on the port yet: by closing the connection.
	refuse bool

	// hangUpMidSession closes the connection after the first output
	// frame instead of letting the session finish, standing in for a VM
	// that died with a command still running.
	hangUpMidSession bool

	// olderAgent stands in for a guest image built before Request.Dir
	// and Request.Env existed: it runs the command line and nothing
	// else, and its response names no features -- which is what an agent
	// that ignored those fields looks like from the client's side.
	olderAgent bool
}

func startFakeCHV(t *testing.T, f *fakeCHV) *fakeCHV {
	t.Helper()
	if f == nil {
		f = &fakeCHV{}
	}
	// Short path: a unix socket's address has a hard length limit
	// (~108 bytes) that a nested t.TempDir() under a long test name can
	// exceed, which fails as a confusing "invalid argument" on bind.
	dir, err := os.MkdirTemp("", "chv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f.socket = filepath.Join(dir, "vsock.sock")

	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeCHV) serve(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "CONNECT ") {
		return
	}
	if f.refuse {
		return
	}
	if _, err := fmt.Fprintf(conn, "OK %d\n", 4242); err != nil {
		return
	}

	if f.hangUpMidSession {
		f.serveThenHangUp(conn, br)
		return
	}
	if f.olderAgent {
		f.serveAsOlderAgent(conn, br)
		return
	}
	// br may hold bytes already read off the socket, so the agent has to
	// be given the buffered reader rather than the connection.
	_ = agent.Serve(context.Background(), bufConn{r: br, Conn: conn})
}

// bufConn reads through a buffer that has already consumed part of the
// stream, while writing straight to the connection.
type bufConn struct {
	r *bufio.Reader
	net.Conn
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// serveAsOlderAgent runs the command line, ignoring every optional field
// on the request, and answers with the featureless response an agent
// built before those fields would have sent.
func (f *fakeCHV) serveAsOlderAgent(conn net.Conn, br *bufio.Reader) {
	req, err := execwire.ReadRequest(br)
	if err != nil {
		return
	}
	out, err := exec.Command("/bin/sh", "-c", req.Line).Output()
	if err != nil {
		_ = execwire.WriteResponse(conn, execwire.Response{OK: false, Error: err.Error()})
		return
	}
	_ = execwire.WriteResponse(conn, execwire.Response{OK: true})
	_ = execwire.WriteFrame(conn, execwire.TypeStdout, out)
	_ = execwire.WriteFrame(conn, execwire.TypeExit, execwire.EncodeExit(0))
}

// serveThenHangUp answers the request, writes one output frame, and
// drops the connection without ever sending an exit frame.
func (f *fakeCHV) serveThenHangUp(conn net.Conn, br *bufio.Reader) {
	if _, err := execwire.ReadRequest(br); err != nil {
		return
	}
	_ = execwire.WriteResponse(conn, execwire.Response{OK: true})
	_ = execwire.WriteFrame(conn, execwire.TypeStdout, []byte("partial output"))
}

func testConfig(t *testing.T, f *fakeCHV) Config {
	t.Helper()
	return Config{Socket: f.socket, User: currentAccount(t), ConnectTimeout: 2 * time.Second}
}

// currentAccount names the account this test process runs as. The agent
// switches to whichever account a request names, and switching to a
// different one needs privileges a test runner does not have -- so a
// test can only ask for the one it already has. The production default
// (root) is what the agent gets in a guest, where it runs as root.
func currentAccount(t *testing.T) string {
	t.Helper()
	f, err := os.Open("/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	uid := strconv.Itoa(os.Getuid())
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[2] == uid {
			return fields[0]
		}
	}
	t.Fatalf("no account in /etc/passwd for uid %s", uid)
	return ""
}

func TestFromEnv_DefaultsNeedNothingSetPerVM(t *testing.T) {
	// The SSH transport required KONTUR_EXEC_ADDR, which every backend
	// had to compute and set. Needing nothing is the point of the
	// replacement, so it is worth asserting rather than assuming.
	t.Setenv(envSocket, "")
	t.Setenv(envUser, "")
	t.Setenv(envConnectTimeout, "")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv with nothing set = %v, want a usable default config", err)
	}
	if cfg.Socket != DefaultSocket {
		t.Errorf("Socket = %q, want %q", cfg.Socket, DefaultSocket)
	}
	if cfg.User != defaultUser {
		t.Errorf("User = %q, want %q", cfg.User, defaultUser)
	}
	if cfg.ConnectTimeout != defaultConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, defaultConnectTimeout)
	}
}

func TestFromEnv_Overrides(t *testing.T) {
	t.Setenv(envSocket, "/tmp/other.sock")
	t.Setenv(envUser, "debian")
	t.Setenv(envConnectTimeout, "90s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/tmp/other.sock" || cfg.User != "debian" || cfg.ConnectTimeout != 90*time.Second {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestFromEnv_InvalidConnectTimeout(t *testing.T) {
	t.Setenv(envConnectTimeout, "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv = nil, want an error naming the bad duration")
	}
}

func TestRun_ExecutesGivenCommandAndReportsExitCode(t *testing.T) {
	f := startFakeCHV(t, nil)

	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), testConfig(t, f), []string{"echo", "hello world"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr.String())
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Shell-quoted and joined, so an argument with a space stays one
	// argument rather than becoming two.
	if stdout.String() != "hello world\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello world\n")
	}
}

// A command's non-zero exit is the command's answer, not a failure of
// "kontur exec" -- the caller reads it from the code, and an error here
// would make every failing command look like a broken transport.
func TestRun_NonZeroExitIsNotAnError(t *testing.T) {
	f := startFakeCHV(t, nil)

	code, err := RunLine(context.Background(), testConfig(t, f), "exit 17", nil, nil, nil)
	if err != nil {
		t.Fatalf("RunLine returned an error for a non-zero exit: %v", err)
	}
	if code != 17 {
		t.Errorf("exit code = %d, want 17", code)
	}
}

// The two streams stay apart end to end. grain's sandbox tools read them
// separately, and merging them is exactly the corruption the guest's old
// SSH console wrapper caused.
func TestRun_KeepsStdoutAndStderrApart(t *testing.T) {
	f := startFakeCHV(t, nil)

	var stdout, stderr bytes.Buffer
	if _, err := RunLine(context.Background(), testConfig(t, f), "echo out; echo err >&2", nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if stderr.String() != "err\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunLine_SendsLineVerbatim(t *testing.T) {
	f := startFakeCHV(t, nil)

	// A line RunLine must not re-quote: the guest's shell has to see the
	// pipe and the variable, not a single literal argument.
	var stdout bytes.Buffer
	if _, err := RunLine(context.Background(), testConfig(t, f), "echo one two three | wc -w", nil, &stdout, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "3" {
		t.Fatalf("stdout = %q, want the pipeline to have run guest-side", stdout.String())
	}
}

func TestRun_ForwardsStdin(t *testing.T) {
	f := startFakeCHV(t, nil)

	var stdout bytes.Buffer
	code, err := Run(context.Background(), testConfig(t, f), []string{"cat"}, strings.NewReader("from the caller\n"), &stdout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d: cat never saw end of input", code)
	}
	if stdout.String() != "from the caller\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A guest still booting has no agent listening yet, and
// cloud-hypervisor answers by closing the connection. That is the
// ordinary case the retry loop exists for, so it must be bounded by
// ConnectTimeout rather than either giving up at once or retrying
// forever.
func TestRun_RetriesUntilTheTimeoutWhenNothingIsListeningInTheGuest(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{refuse: true})

	cfg := testConfig(t, f)
	cfg.ConnectTimeout = 750 * time.Millisecond

	start := time.Now()
	_, err := Run(context.Background(), cfg, []string{"true"}, nil, nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}
	if elapsed < cfg.ConnectTimeout {
		t.Errorf("gave up after %v, before the %v timeout: it is not retrying", elapsed, cfg.ConnectTimeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v to give up on a %v timeout", elapsed, cfg.ConnectTimeout)
	}
}

// A VM that dies mid-command must not look like a command that
// succeeded. The exit frame is the only thing that ends a session, so a
// stream that stops without one is an error rather than exit 0.
func TestRun_AGuestThatVanishesMidCommandIsAnError(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{hangUpMidSession: true})

	var stdout bytes.Buffer
	code, err := RunLine(context.Background(), testConfig(t, f), "does not matter", nil, &stdout, nil)
	if err == nil {
		t.Fatalf("Run = nil (code %d), want an error for a session with no exit status", code)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 alongside the error", code)
	}
	if !strings.Contains(err.Error(), "exit status") {
		t.Errorf("error = %v, want it to say no exit status arrived", err)
	}
}

// A socket that is not there at all is the other shape of "the VM is not
// ready", and has to be retried rather than failing on the first dial:
// cloud-hypervisor creates the socket when it starts, which is after the
// container does.
func TestRun_AMissingSocketIsRetriedAndThenReported(t *testing.T) {
	cfg := Config{Socket: filepath.Join(t.TempDir(), "never-created.sock"), ConnectTimeout: 300 * time.Millisecond}

	_, err := Run(context.Background(), cfg, []string{"true"}, nil, nil, nil)
	if err == nil {
		t.Fatal("Run = nil, want an error")
	}
	if !strings.Contains(err.Error(), "never-created.sock") {
		t.Errorf("error = %v, want it to name the socket it could not reach", err)
	}
}

// Cancelling the context ends the session rather than leaving the caller
// waiting on a command that may never finish.
func TestRun_ContextCancellationEndsTheSession(t *testing.T) {
	f := startFakeCHV(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunLine(ctx, testConfig(t, f), "sleep 30", nil, nil, nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not end the session")
	}
}

// TestReady_AnswersWhenTheGuestRunsACommand is the whole of "kontur
// ready": no output, no session, just the fact that the guest ran
// something.
func TestReady_AnswersWhenTheGuestRunsACommand(t *testing.T) {
	f := startFakeCHV(t, nil)

	if err := Ready(context.Background(), testConfig(t, f)); err != nil {
		t.Fatalf("Ready = %v, want nil against a guest that answers", err)
	}
}

// A guest still booting refuses the vsock port, which is the ordinary
// case the retry exists for: Ready has to keep asking until its timeout
// rather than reporting the first refusal as a verdict.
func TestReady_RetriesAGuestThatIsStillBooting(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{refuse: true})

	cfg := testConfig(t, f)
	cfg.ConnectTimeout = 750 * time.Millisecond

	start := time.Now()
	err := Ready(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Ready = nil, want a timeout")
	}
	if elapsed < cfg.ConnectTimeout {
		t.Errorf("gave up after %v, before the %v timeout: it is not retrying", elapsed, cfg.ConnectTimeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v to give up on a %v timeout", elapsed, cfg.ConnectTimeout)
	}
}

// A zero timeout is what a container readiness probe passes: one
// attempt, answered now, because whatever runs the probe does the
// retrying itself.
func TestReady_ZeroTimeoutIsASingleAttempt(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{refuse: true})

	cfg := testConfig(t, f)
	cfg.ConnectTimeout = 0

	start := time.Now()
	err := Ready(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Ready = nil, want the refusal reported")
	}
	if elapsed > retryInterval {
		t.Errorf("took %v for a single attempt: it retried", elapsed)
	}
}

// A guest whose agent answers but cannot actually run the command is
// not ready either -- an agent up before the account it has to switch to
// exists looks exactly like this -- so the probe has to be a command
// that ran, not merely a connection that was accepted.
func TestReady_AGuestThatCannotRunTheProbeIsNotReady(t *testing.T) {
	f := startFakeCHV(t, nil)

	cfg := testConfig(t, f)
	cfg.ConnectTimeout = 0
	cfg.User = "no-such-account-here"

	if err := Ready(context.Background(), cfg); err == nil {
		t.Fatal("Ready = nil for a guest that could not run the probe, want an error")
	}
}

func TestShellJoin(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"ls"}, "'ls'"},
		{[]string{"echo", "hello world"}, "'echo' 'hello world'"},
		{[]string{"echo", "it's"}, `'echo' 'it'\''s'`},
	}
	for _, c := range cases {
		if got := shellJoin(c.args); got != c.want {
			t.Errorf("shellJoin(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestShellCommandLine(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "no args requests interactive shell", args: nil, want: ""},
		{name: "plain -c", args: []string{"-c", "echo hi"}, want: "echo hi"},
		{name: "fused flags ending in c", args: []string{"-ec", "echo hi"}, want: "echo hi"},
		{name: "command containing shell metacharacters passed through verbatim", args: []string{"-c", "ls -la $HOME | wc -l"}, want: "ls -la $HOME | wc -l"},
		{name: "-c with no command is an error", args: []string{"-c"}, wantErr: true},
		{name: "positional args after -c's command are unsupported", args: []string{"-c", "echo hi", "extra"}, wantErr: true},
		{name: "a script file argument is unsupported", args: []string{"script.sh"}, wantErr: true},
		{name: "--login is unsupported", args: []string{"--login"}, wantErr: true},
		{name: "bare -", args: []string{"-"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ShellCommandLine(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ShellCommandLine(%q) error = nil, want an error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("ShellCommandLine(%q) error = %v", c.args, err)
			}
			if got != c.want {
				t.Errorf("ShellCommandLine(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

// Config.Dir and Config.Env are what "kontur exec -w/-e" set, and what
// they buy is a command that does not have to carry "cd ... &&" and an
// "export" of its own.
func TestRun_WorkingDirectoryAndEnvironmentReachTheCommand(t *testing.T) {
	f := startFakeCHV(t, nil)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t, f)
	cfg.Dir = dir
	cfg.Env = []string{"GOFLAGS=-mod=vendor"}

	var stdout bytes.Buffer
	if _, err := RunLine(context.Background(), cfg, `pwd; echo "$GOFLAGS"`, nil, &stdout, nil); err != nil {
		t.Fatal(err)
	}
	want := dir + "\n-mod=vendor\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// The mismatch this protocol cannot afford to be quiet about: an agent
// that predates the fields runs the command anyway, in the wrong
// directory, and says OK. The client has to notice from the features the
// response does not name, and say so.
func TestRun_AnAgentThatIgnoredTheWorkingDirectoryIsNotTakenAtItsWord(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{olderAgent: true})

	cfg := testConfig(t, f)
	cfg.Dir = t.TempDir()

	_, err := RunLine(context.Background(), cfg, "pwd", nil, nil, nil)
	if err == nil {
		t.Fatal("RunLine accepted a session that silently dropped the working directory")
	}
	if !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("error = %v, want one naming the guest agent as too old", err)
	}
}

// And a request that asked for nothing optional is still served by that
// same older agent: every added field is "omitempty" so that a client
// not using one stays compatible with a guest image that does not have
// it.
func TestRun_AnOlderAgentStillServesAPlainCommand(t *testing.T) {
	f := startFakeCHV(t, &fakeCHV{olderAgent: true})

	var stdout bytes.Buffer
	if _, err := RunLine(context.Background(), testConfig(t, f), "echo plain", nil, &stdout, nil); err != nil {
		t.Fatalf("RunLine against an older agent: %v", err)
	}
	if stdout.String() != "plain\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestParseArgs_FlagsThenCommand(t *testing.T) {
	opts, err := ParseArgs([]string{"-w", "/src", "-e", "A=1", "--env", "B=2", "--", "go", "build", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Workdir != "/src" {
		t.Errorf("workdir = %q", opts.Workdir)
	}
	if strings.Join(opts.Env, ",") != "A=1,B=2" {
		t.Errorf("env = %v, want both entries in the order given", opts.Env)
	}
	if strings.Join(opts.Command, " ") != "go build ./..." {
		t.Errorf("command = %v", opts.Command)
	}
}

// The invocation every doc, script and README line uses has to keep
// meaning exactly what it did before this mode had any flags at all.
func TestParseArgs_TheDocumentedInvocationIsUnchanged(t *testing.T) {
	opts, err := ParseArgs([]string{"--", "sh", "-c", "exit 42"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(opts.Command, "|") != "sh|-c|exit 42" {
		t.Fatalf("command = %v, want the command's own flags left alone", opts.Command)
	}
	if opts.Workdir != "" || len(opts.Env) != 0 {
		t.Fatalf("opts = %+v, want nothing set", opts)
	}

	// No "--" either: flags stop at the first thing that is not one, so
	// a command's own arguments are still its own.
	opts, err = ParseArgs([]string{"uname", "-a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(opts.Command, "|") != "uname|-a" {
		t.Fatalf("command = %v", opts.Command)
	}

	// And no arguments at all is still the interactive login shell.
	opts, err = ParseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Command) != 0 {
		t.Fatalf("command = %v, want none", opts.Command)
	}
}

func TestParseArgs_RejectsWhatWouldGoWrongLater(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		// docker exec reads a bare name from the client's environment;
		// this container's environment is not one anybody set up, so a
		// variable that quietly did not arrive is the likelier outcome.
		{"a bare variable name", []string{"-e", "GOFLAGS", "--", "true"}, "KEY=value"},
		{"an empty key", []string{"-e", "=1", "--", "true"}, "KEY=value"},
		{"a relative working directory", []string{"-w", "src", "--", "true"}, "absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseArgs(tc.args)
			if err == nil {
				t.Fatalf("ParseArgs(%q) = nil, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), Usage) {
				t.Error("the error does not carry the usage a caller needs to fix it")
			}
		})
	}
}

// "kontur exec -h" is a question, and cmd/kontur answers it with Usage
// on stdout rather than as a failure.
func TestParseArgs_HelpIsNotAnError(t *testing.T) {
	_, err := ParseArgs([]string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("ParseArgs(-h) = %v, want flag.ErrHelp", err)
	}
}
