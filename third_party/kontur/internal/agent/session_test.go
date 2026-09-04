package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/kontur/internal/execwire"
)

// result is what a client sees for one session.
type result struct {
	resp   execwire.Response
	stdout string
	stderr string
	code   int
}

// currentAccount is the name of the account this test process is running
// as, read the same way the agent reads it.
//
// Tests ask for this account rather than for root, because switching to
// a different one needs privileges a test runner does not have: asking
// for root as an ordinary user makes the agent try to setuid(0) and fail
// with EPERM before the command ever runs. The production agent runs as
// root in the guest, where every account is reachable; a test can only
// exercise the account it already has.
func currentAccount(t *testing.T) string {
	t.Helper()
	f, err := os.Open(passwdPath)
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
	t.Fatalf("no account in %s for uid %s", passwdPath, uid)
	return ""
}

// run drives one Serve over an in-memory connection, the way "kontur
// exec" drives one over vsock. stdinFrames are sent before reading any
// output, which is enough for every command here.
func run(t *testing.T, req execwire.Request, stdin string) result {
	t.Helper()
	if req.User == "" {
		req.User = currentAccount(t)
	}

	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- Serve(context.Background(), server)
	}()
	defer client.Close()

	if err := execwire.WriteRequest(client, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	resp, err := execwire.ReadResponse(br)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		// Reported here rather than left to whatever the caller was
		// asserting about output: a refused start otherwise reads as a
		// command that ran and printed nothing, which is what made the
		// first CI failure on this package look like six unrelated ones.
		if !expectRefusal(req) {
			t.Fatalf("the agent refused to start the command: %s", resp.Error)
		}
		return result{resp: resp}
	}

	if stdin != "" {
		if err := execwire.WriteFrame(client, execwire.TypeStdin, []byte(stdin)); err != nil {
			t.Fatal(err)
		}
	}
	if err := execwire.WriteFrame(client, execwire.TypeStdinClose, nil); err != nil {
		t.Fatal(err)
	}

	res := result{resp: resp}
	var out, errb bytes.Buffer
	for {
		typ, payload, err := execwire.ReadFrame(br)
		if err != nil {
			t.Fatalf("reading frames: %v (stdout so far %q)", err, out.String())
		}
		switch typ {
		case execwire.TypeStdout:
			out.Write(payload)
		case execwire.TypeStderr:
			errb.Write(payload)
		case execwire.TypeExit:
			res.code, err = execwire.DecodeExit(payload)
			if err != nil {
				t.Fatal(err)
			}
			res.stdout, res.stderr = out.String(), errb.String()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Serve: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return after the exit frame")
			}
			return res
		}
	}
}

// expectRefusal reports whether a request is one of the few that are
// meant to be refused before anything runs.
func expectRefusal(req execwire.Request) bool {
	return req.User == "nosuchaccount"
}

func TestACommandsOutputAndExitCodeComeBack(t *testing.T) {
	res := run(t, execwire.Request{Line: "echo hello"}, "")
	if res.stdout != "hello\n" || res.code != 0 {
		t.Fatalf("stdout %q, exit %d", res.stdout, res.code)
	}

	res = run(t, execwire.Request{Line: "exit 42"}, "")
	if res.code != 42 {
		t.Fatalf("exit %d, want 42", res.code)
	}
}

// The two output streams stay apart. This is the whole reason the
// protocol has two frame types rather than one, and grain's sandbox
// tools read them separately -- a merged stream is what the SSH console
// wrapper used to do wrong, and what GUEST_CONSOLE_WRAP=0 exists to
// avoid.
func TestStdoutAndStderrStaySeparate(t *testing.T) {
	res := run(t, execwire.Request{Line: "echo out; echo err >&2"}, "")
	if res.stdout != "out\n" {
		t.Errorf("stdout = %q, want just the stdout line", res.stdout)
	}
	if res.stderr != "err\n" {
		t.Errorf("stderr = %q, want just the stderr line", res.stderr)
	}
}

// A command's own output must survive its exit. os/exec closes the pipes
// as part of Wait, so an implementation that reaped before draining
// loses whatever was written just before exiting -- which is exactly
// where a failing command puts its error message.
func TestOutputWrittenJustBeforeExitIsNotLost(t *testing.T) {
	res := run(t, execwire.Request{Line: "echo the-last-thing-it-said >&2; exit 3"}, "")
	if res.code != 3 {
		t.Fatalf("exit %d, want 3", res.code)
	}
	if !strings.Contains(res.stderr, "the-last-thing-it-said") {
		t.Fatalf("stderr = %q, want the message written immediately before exit", res.stderr)
	}
}

func TestStdinReachesTheCommand(t *testing.T) {
	res := run(t, execwire.Request{Line: "cat"}, "round trip\n")
	if res.stdout != "round trip\n" {
		t.Fatalf("stdout = %q", res.stdout)
	}
	if res.code != 0 {
		t.Fatalf("exit %d: a command reading stdin never saw EOF", res.code)
	}
}

// Failing to start and failing once started are different answers.
// Collapsing them would report a guest with no such account as a command
// that ran and exited non-zero.
func TestAnAccountThatDoesNotExistFailsBeforeTheCommandRuns(t *testing.T) {
	res := run(t, execwire.Request{Line: "echo unreachable", User: "nosuchaccount"}, "")
	if res.resp.OK {
		t.Fatal("a request naming an unknown account was accepted")
	}
	if !strings.Contains(res.resp.Error, "nosuchaccount") {
		t.Errorf("error %q does not name the account", res.resp.Error)
	}
}

// With a pty the session has one stream, and it is a terminal. Both
// halves matter: interactive tools test isatty, and a caller that got
// stderr frames from a pty session would be seeing something SSH never
// produced either.
func TestATTYSessionIsATerminalOnOneStream(t *testing.T) {
	res := run(t, execwire.Request{Line: "test -t 1 && echo is-a-tty; echo to-stderr >&2", TTY: true, Cols: 80, Rows: 24}, "")
	if !strings.Contains(res.stdout, "is-a-tty") {
		t.Fatalf("stdout = %q, want the command to have seen a terminal", res.stdout)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want everything on the one stream a terminal has", res.stderr)
	}
	if !strings.Contains(res.stdout, "to-stderr") {
		t.Errorf("stdout = %q, want the stderr line merged into it", res.stdout)
	}
}

func TestTheRequestedTerminalSizeIsApplied(t *testing.T) {
	res := run(t, execwire.Request{Line: "stty size", TTY: true, Cols: 120, Rows: 40}, "")
	// stty prints "rows cols". Skipped rather than failed where stty is
	// absent: the size is set by ioctl either way, and this asserts it
	// only where something can read it back.
	if strings.TrimSpace(res.stdout) == "" {
		t.Skip("no stty on this machine to read the size back with")
	}
	if got := strings.TrimSpace(res.stdout); got != "40 120" {
		t.Fatalf("stty size = %q, want \"40 120\"", got)
	}
}

// The environment is sshd's, not this process's, because guest images
// built on kontur depend on it -- grain puts its Go and Node toolchains
// in /usr/local/bin precisely because that is on this PATH and no
// profile is read.
func TestTheCommandGetsTheSameEnvironmentSSHDGaveIt(t *testing.T) {
	res := run(t, execwire.Request{Line: "echo \"$PATH\"; echo \"$HOME\"; echo \"$USER\""}, "")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q", res.stdout)
	}
	if lines[0] != defaultPATH {
		t.Errorf("PATH = %q, want sshd's own default %q", lines[0], defaultPATH)
	}
	if lines[2] == "" {
		t.Error("USER is empty")
	}
}

// An empty command line is an interactive login shell, the same fallback
// sshd makes. Asserted by the shell reading its startup files, which a
// `-c` invocation does not do.
func TestAnEmptyCommandLineAsksForALoginShell(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_ = Serve(context.Background(), server)
	}()
	defer client.Close()

	if err := execwire.WriteRequest(client, execwire.Request{User: currentAccount(t)}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	resp, err := execwire.ReadResponse(br)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("an empty command line was refused: %s", resp.Error)
	}
	// A login shell with stdin closed exits on its own; all that is
	// asserted here is that it started and ended rather than erroring.
	if err := execwire.WriteFrame(client, execwire.TypeStdinClose, nil); err != nil {
		t.Fatal(err)
	}
	for {
		typ, payload, err := execwire.ReadFrame(br)
		if err != nil {
			t.Fatalf("the login shell never reported an exit: %v", err)
		}
		if typ == execwire.TypeExit {
			if _, err := execwire.DecodeExit(payload); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

// The account files are the guest's, and the fields that matter are the
// two os/user does not answer: the login shell and the supplementary
// groups.
func TestAccountLookupReadsTheShellAndTheSupplementaryGroups(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	// Any uid but this process's own: lookupAccount reads no groups for
	// the account it is already running as (see account.self), so a
	// fixture that happened to name the uid running the tests -- 1000,
	// the first account on a Debian host, is the obvious one to write
	// here -- would assert nothing on that machine and fail there alone.
	uid := 1000
	if os.Getuid() == uid {
		uid = 1001
	}
	if err := os.WriteFile(passwd, []byte(fmt.Sprintf(
		"root:x:0:0:root:/root:/bin/bash\n"+
			"debian:x:%d:%d:,,,:/home/debian:/bin/bash\n", uid, uid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte(
		"docker:x:998:debian\n"+
			"sudo:x:27:debian\n"+
			"nobody:x:65534:\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldPasswd, oldGroup := passwdPath, groupPath
	passwdPath, groupPath = passwd, group
	defer func() { passwdPath, groupPath = oldPasswd, oldGroup }()

	a, err := lookupAccount("debian")
	if err != nil {
		t.Fatal(err)
	}
	if a.UID != uid || a.GID != uid || a.Shell != "/bin/bash" {
		t.Fatalf("account = %+v", a)
	}
	// The primary gid first, then every group naming it. Losing the
	// docker group is the failure this guards: every `docker` command in
	// the guest would fail on the socket's permissions, saying nothing
	// about a group.
	want := map[uint32]bool{uint32(uid): true, 998: true, 27: true}
	if len(a.Groups) != len(want) {
		t.Fatalf("groups = %v, want the primary plus docker and sudo", a.Groups)
	}
	for _, g := range a.Groups {
		if !want[g] {
			t.Errorf("unexpected group %d in %v", g, a.Groups)
		}
	}

	if _, err := lookupAccount("ghost"); err == nil {
		t.Error("an account not in the file was resolved anyway")
	}
}

// The credential is the production path no test can actually exercise
// -- switching to another account needs privileges a test runner does
// not have -- so what is built for one is asserted directly instead.
// The groups are the part worth pinning: a session that set only uid and
// gid would leave every `docker` command in the guest failing on the
// socket's permissions, with nothing in the error naming the group it
// lost.
func TestAnotherAccountGetsACredentialCarryingItsGroups(t *testing.T) {
	a := &account{Name: "debian", UID: 1000, GID: 1000, Groups: []uint32{1000, 998}}
	cred := a.credential()
	if cred == nil {
		t.Fatal("no credential for an account this process is not running as")
	}
	if cred.Uid != 1000 || cred.Gid != 1000 {
		t.Errorf("credential = %+v, want uid/gid 1000", cred)
	}
	if len(cred.Groups) != 2 || cred.Groups[0] != 1000 || cred.Groups[1] != 998 {
		t.Errorf("credential groups = %v, want the account's own", cred.Groups)
	}
}

// The account the request names is routinely the one the agent is
// already running as: root, in the guest, where the agent runs as root.
// No credential is applied for it -- a setuid to the uid the process
// already has -- and that is also what lets every session test above run
// a real command without privileges. Asserted here because it is the
// case the test above has to steer around: without it, a group lookup
// that had been skipped and one that was broken would look the same.
func TestTheAccountTheAgentAlreadyRunsAsGetsNoCredential(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte(
		fmt.Sprintf("itself:x:%d:%d::%s:/bin/sh\n", os.Getuid(), os.Getgid(), dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPasswd := passwdPath
	passwdPath = passwd
	defer func() { passwdPath = oldPasswd }()

	a, err := lookupAccount("itself")
	if err != nil {
		t.Fatal(err)
	}
	if !a.self {
		t.Fatalf("account %+v was not recognized as this process's own", a)
	}
	if cred := a.credential(); cred != nil {
		t.Errorf("credential = %+v for the account the agent already runs as; a setuid to the uid it has would fail for an unprivileged agent", cred)
	}
}

// A home directory named in /etc/passwd but absent from the filesystem
// would fail every command on chdir, which reads as the command being
// broken rather than the account being half-created.
func TestAMissingHomeDirectoryFallsBackRatherThanFailingEveryCommand(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte("ghosthome:x:0:0::/home/does-not-exist:/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPasswd := passwdPath
	passwdPath = passwd
	defer func() { passwdPath = oldPasswd }()

	a, err := lookupAccount("ghosthome")
	if err != nil {
		t.Fatal(err)
	}
	if a.Home != "/" {
		t.Fatalf("home = %q, want / when the named directory is not there", a.Home)
	}
}

// serveOne drives one Serve and returns the opening response, for the
// requests that are meant to be refused before anything runs. run above
// treats a refusal as a test failure, which is right for every request
// that should have started.
func serveOne(t *testing.T, req execwire.Request) execwire.Response {
	t.Helper()
	if req.User == "" {
		req.User = currentAccount(t)
	}

	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- Serve(context.Background(), server)
	}()
	defer client.Close()

	if err := execwire.WriteRequest(client, req); err != nil {
		t.Fatal(err)
	}
	resp, err := execwire.ReadResponse(bufio.NewReader(client))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The point of the field: a caller says where to run, instead of writing
// "cd /src && ..." into the command line and quoting it correctly.
func TestARequestedWorkingDirectoryIsWhereTheCommandRuns(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, execwire.Request{Line: "pwd", Dir: dir}, "")
	if got := strings.TrimSpace(res.stdout); got != dir {
		t.Fatalf("pwd = %q, want the requested %q", got, dir)
	}
}

// And without one, nothing moves: a session still starts in the
// account's home directory, which is where every session started before
// this field existed.
func TestWithoutOneTheSessionStillStartsInTheAccountsHome(t *testing.T) {
	acct, err := lookupAccount(currentAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(acct.Home)
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, execwire.Request{Line: "pwd"}, "")
	if got := strings.TrimSpace(res.stdout); got != want {
		t.Fatalf("pwd = %q, want the account's home %q", got, want)
	}
}

// A directory that is not there has to be refused with a message naming
// it. Left to the chdir in the forked child, the same mistake comes back
// from Start as "fork/exec /bin/sh: no such file or directory", which
// reads as a guest with no shell.
func TestAWorkingDirectoryThatIsNotThereIsRefusedByName(t *testing.T) {
	resp := serveOne(t, execwire.Request{Line: "echo unreachable", Dir: "/no/such/place"})
	if resp.OK {
		t.Fatal("a request naming a directory that does not exist was accepted")
	}
	if !strings.Contains(resp.Error, "/no/such/place") {
		t.Errorf("error %q does not name the directory", resp.Error)
	}
	if strings.Contains(resp.Error, "/bin/sh") {
		t.Errorf("error %q blames the shell for the caller's directory", resp.Error)
	}
}

func TestAWorkingDirectoryThatIsNotADirectoryIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := serveOne(t, execwire.Request{Line: "echo unreachable", Dir: file})
	if resp.OK {
		t.Fatal("a request naming a regular file as its working directory was accepted")
	}
	if !strings.Contains(resp.Error, file) {
		t.Errorf("error %q does not name the path", resp.Error)
	}
}

// A relative path would be resolved against whatever the agent's own
// working directory happens to be, which is not something a caller can
// reason about -- so it is refused rather than resolved.
func TestARelativeWorkingDirectoryIsRefused(t *testing.T) {
	resp := serveOne(t, execwire.Request{Line: "pwd", Dir: "src"})
	if resp.OK {
		t.Fatal("a relative working directory was accepted")
	}
	if !strings.Contains(resp.Error, "absolute") {
		t.Errorf("error %q does not say what is wrong with it", resp.Error)
	}
}

// Overlaid, not substituted: the entries a caller sends are added to the
// login environment, and the rest of it -- the PATH guest images are
// built against, and the HOME/USER that describe the account -- is still
// there.
func TestARequestedEnvironmentIsOverlaidOnTheLoginEnvironment(t *testing.T) {
	res := run(t, execwire.Request{
		Line: `echo "$GOFLAGS"; echo "$PATH"; echo "$HOME"`,
		Env:  []string{"GOFLAGS=-mod=vendor"},
	}, "")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q", res.stdout)
	}
	if lines[0] != "-mod=vendor" {
		t.Errorf("GOFLAGS = %q, want the value the request set", lines[0])
	}
	if lines[1] != defaultPATH {
		t.Errorf("PATH = %q, want the login default %q left alone", lines[1], defaultPATH)
	}
	if lines[2] == "" {
		t.Error("HOME is empty: the login environment was replaced rather than added to")
	}
}

// Deliberately overriding one of those defaults is allowed, and the last
// entry for a key wins, the way "A=1 A=2 cmd" does in a shell.
func TestAnEntryCanOverrideTheLoginDefaultAndTheLastOneWins(t *testing.T) {
	res := run(t, execwire.Request{
		Line: `echo "$PATH"; echo "$HOME"`,
		Env:  []string{"PATH=/first/bin", "HOME=/somewhere/else", "PATH=/second/bin"},
	}, "")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q", res.stdout)
	}
	if lines[0] != "/second/bin" {
		t.Errorf("PATH = %q, want the last value given", lines[0])
	}
	if lines[1] != "/somewhere/else" {
		t.Errorf("HOME = %q, want the override the request asked for", lines[1])
	}
}

// An entry with no "=" is a caller mistake, and this is the only place
// that can say so: passed through, it would reach the command as
// nothing at all.
func TestAnEnvironmentEntryThatIsNotKeyValueIsRefused(t *testing.T) {
	resp := serveOne(t, execwire.Request{Line: "echo unreachable", Env: []string{"GOFLAGS"}})
	if resp.OK {
		t.Fatal("an environment entry with no value was accepted")
	}
	if !strings.Contains(resp.Error, "GOFLAGS") {
		t.Errorf("error %q does not name the entry", resp.Error)
	}
}

// The capability line: every response says what this agent implements,
// refusal or not, because that is how a client finds out whether the
// Dir/Env it sent were honoured or silently dropped by an older guest.
func TestEveryResponseNamesTheFeaturesThisAgentImplements(t *testing.T) {
	res := run(t, execwire.Request{Line: "true"}, "")
	if !res.resp.Supports(execwire.FeatureDirEnv) {
		t.Errorf("a successful response reported features %v", res.resp.Features)
	}
	resp := serveOne(t, execwire.Request{Line: "echo unreachable", Dir: "/no/such/place"})
	if !resp.Supports(execwire.FeatureDirEnv) {
		t.Errorf("a refusal reported features %v; a client cannot tell a refusal from an old agent", resp.Features)
	}
}

// The other direction of the same problem: a client newer than this
// agent gets an answer naming the field, rather than a connection that
// closes without a word.
func TestARequestNamingAFieldThisAgentDoesNotKnowIsAnswered(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- Serve(context.Background(), server)
	}()
	defer client.Close()

	if _, err := client.Write([]byte(`{"line":"echo hi","teleport":"/src"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := execwire.ReadResponse(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("the agent hung up on a request it could not honour: %v", err)
	}
	if resp.OK {
		t.Fatal("a request carrying an unknown field was accepted")
	}
	if !strings.Contains(resp.Error, "teleport") {
		t.Errorf("error %q does not name the field", resp.Error)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after refusing the request")
	}
}
