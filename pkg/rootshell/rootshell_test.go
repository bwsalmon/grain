package rootshell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testTimeout is what these tests give the exchange instead of
// DefaultTimeout's two minutes: long enough that a loaded runner does
// not fail a test that would have passed, short enough that the cases
// which are *about* nothing answering finish while somebody is watching.
const testTimeout = 10 * time.Second

// responder is the stand-in for grain-shell.service: the half of the
// exchange that runs on the host, as root, and that no test can install.
// It follows the same protocol scripts/setup.sh's own responder does --
// consume the request, run it, write the output, and write the status
// last and atomically -- so what these tests exercise is the protocol
// rather than one side of it talking to itself.
func responder(t *testing.T, dir string) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			command, err := os.ReadFile(filepath.Join(dir, RequestFile))
			// An empty read is a request caught between its create and
			// its write. systemd's own PathModified never sees one (it
			// fires on the close), so waiting for the next tick is the
			// faithful behaviour rather than a workaround.
			if err != nil || len(command) == 0 {
				continue
			}
			if err := os.Remove(filepath.Join(dir, RequestFile)); err != nil {
				t.Errorf("responder: consuming the request: %v", err)
				return
			}
			run(dir, string(command))
		}
	}()
}

// run is the responder's own body, and deliberately the same three steps
// the installed script performs.
func run(dir, command string) {
	out, err := exec.Command("bash", "-lc", command).CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		out, code = []byte(err.Error()), 127
	}
	os.WriteFile(filepath.Join(dir, OutputFile), out, 0o600)
	writeStatus(dir, code)
}

// writeStatus writes the completion marker the way the installed
// responder does: into a temp file, then renamed into place, so the
// daemon side can never catch it half-written.
func writeStatus(dir string, code int) {
	tmp := filepath.Join(dir, StatusFile+".tmp")
	os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", code)), 0o600)
	os.Rename(tmp, filepath.Join(dir, StatusFile))
}

func runner(t *testing.T, dir string) *Runner {
	t.Helper()
	return New(dir).WithTimeout(testTimeout)
}

// The point of the whole package: a command an operator typed into the
// UI runs somewhere they cannot otherwise reach, and what it printed
// comes back.
func TestRunReturnsWhatTheCommandPrinted(t *testing.T) {
	dir := t.TempDir()
	responder(t, dir)

	got, err := runner(t, dir).Run(context.Background(), "echo hello from the host")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "hello from the host\n"; got.Output != want {
		t.Errorf("output is %q, want %q", got.Output, want)
	}
	if got.ExitCode != 0 {
		t.Errorf("exit code is %d, want 0", got.ExitCode)
	}
}

// stderr comes back too, interleaved with stdout rather than reported
// separately -- a debugging pane that dropped the stream the errors are
// on would be showing an empty box for every command that failed.
func TestRunReturnsWhatTheCommandPrintedOnStderr(t *testing.T) {
	dir := t.TempDir()
	responder(t, dir)

	got, err := runner(t, dir).Run(context.Background(), "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"out", "err"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("output %q does not carry %q", got.Output, want)
		}
	}
}

// A command that fails is an answer, not a failure of the exchange:
// `grep` finding nothing, a unit that is not running, a path that does
// not exist are all things this pane is opened to find out.
func TestAFailingCommandIsAResultRatherThanAnError(t *testing.T) {
	dir := t.TempDir()
	responder(t, dir)

	got, err := runner(t, dir).Run(context.Background(), "echo nope >&2; exit 3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.ExitCode != 3 {
		t.Errorf("exit code is %d, want 3", got.ExitCode)
	}
	if !strings.Contains(got.Output, "nope") {
		t.Errorf("output %q does not carry what the command printed", got.Output)
	}
}

// The failure that would look like an answer, which is why clear() runs
// before every request rather than only after a successful read: an
// answer to a command whose operator gave up on it two minutes ago is
// still sitting in the directory, and must not be handed to the next
// command as its own.
func TestAnAnswerLeftBehindIsNeverReadAsThisCommandsOwn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, OutputFile), []byte("output of a command nobody is waiting for"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStatus(dir, 0)

	// No responder: with the stale answer left in place this would
	// return it instantly, so a timeout is the assertion.
	_, err := New(dir).WithTimeout(200*time.Millisecond).Run(context.Background(), "true")
	if err == nil {
		t.Fatal("Run answered with a command's output that nothing produced this time")
	}
	if !strings.Contains(err.Error(), "no answer") {
		t.Errorf("error is %q, want the one about nothing answering", err)
	}
}

// What a deployment that never installed the responder is told. This is
// the error most likely to be read by somebody who has just clicked the
// tab for the first time, on a host deployed before it existed, so it
// has to name the thing to install rather than only that nothing
// happened.
func TestNothingAnsweringSaysWhatIsMissing(t *testing.T) {
	dir := t.TempDir()

	_, err := New(dir).WithTimeout(200*time.Millisecond).Run(context.Background(), "echo hello")
	if err == nil {
		t.Fatal("Run returned no error with no responder installed")
	}
	for _, want := range []string{"grain-shell.service", "setup.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// And the request does not stay behind to be run whenever a
	// responder is next installed.
	if _, err := os.Stat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the abandoned request is still in the control directory (%v)", err)
	}
}

// A caller giving up -- the browser closing the pane's request -- ends
// the wait rather than holding a goroutine for the full timeout.
func TestACancelledContextEndsTheWait(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := runner(t, dir).Run(ctx, "echo hello")
	if err == nil {
		t.Fatal("Run returned no error after its context was cancelled")
	}
	if elapsed := time.Since(start); elapsed > testTimeout/2 {
		t.Errorf("Run waited %s after being cancelled", elapsed)
	}
}

// Nothing is left in the control directory afterwards: the next command
// starts from the same empty directory this one did, and an operator
// looking in there sees no copy of whatever the last one printed.
func TestRunLeavesTheControlDirectoryEmpty(t *testing.T) {
	dir := t.TempDir()
	responder(t, dir)

	if _, err := runner(t, dir).Run(context.Background(), "echo something"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), RequestFile) {
			t.Errorf("%s is still in the control directory", e.Name())
		}
	}
}

// The three file names are fixed, so two commands at once would read
// each other's answers. They queue instead -- which is what a terminal
// does anyway, and this is a UI with one Root shell tab.
func TestConcurrentCommandsDoNotReadEachOthersAnswers(t *testing.T) {
	dir := t.TempDir()
	responder(t, dir)
	r := runner(t, dir)

	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("answer-%d", i)
			got, err := r.Run(context.Background(), "echo "+want)
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			if strings.TrimSpace(got.Output) != want {
				t.Errorf("command for %q got back %q", want, got.Output)
			}
		}()
	}
	wg.Wait()
}

// An empty command is refused here rather than sent, so that a stray
// Enter in the pane does not wake a root shell on the host at all.
func TestAnEmptyCommandIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{"", "   ", "\n\t"} {
		if _, err := New(dir).Run(context.Background(), command); err == nil {
			t.Errorf("Run(%q) was accepted", command)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("an empty command still wrote something to the control directory (%v, %v)", entries, err)
	}
}

// A responder writing something that is not an exit status is a broken
// deployment, and is reported as one instead of being rounded to 0 --
// "the command succeeded" is the one thing this must not say when it
// does not know.
func TestAnAnswerThatIsNotAnExitStatusIsReported(t *testing.T) {
	dir := t.TempDir()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, RequestFile)); err == nil {
				os.WriteFile(filepath.Join(dir, StatusFile), []byte("who knows"), 0o600)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	_, err := runner(t, dir).Run(context.Background(), "echo hello")
	if err == nil {
		t.Fatal("Run accepted an answer with no exit status in it")
	}
	if !strings.Contains(err.Error(), "not an exit status") {
		t.Errorf("error is %q, want the one about the status making no sense", err)
	}
}
