// Package rootshell runs one shell command as root on the machine the
// grain daemon runs on -- the UI's last-resort debug hatch, behind the
// Debug overlay's Root shell tab (pkg/ui.Config.RootShell).
//
// It does not run anything itself. The daemon is a container process
// running unprivileged as $GRAIN_USER, with no sudo in the image and no
// route across the container boundary (scripts/setup.sh's own
// docker_run_args); a `bash -c` from in there is neither root nor on the
// host, which is exactly the pair of things an operator opening this
// pane wants. So this asks instead of acting, over the same control
// channel the reboot button and the Upgrade button's restart already use
// (setup.sh's "the control channel: acting on the host from inside the
// container"): the daemon writes the command to a file in a directory it
// has mounted, a systemd .path unit out on the host notices, and
// grain-shell.service runs it as root and writes back what it printed.
//
// The exchange is three files in that one directory:
//
//	shell         the command, written by the daemon. Writing it is what
//	              the .path unit's PathModified watches for.
//	shell.out     everything the command printed, stdout and stderr
//	              interleaved, written by the responder.
//	shell.status  its exit status, written by the responder *last* and
//	              atomically (a temp file and a rename). That ordering is
//	              the whole completion protocol: shell.status existing
//	              means shell.out is finished, so this side never has to
//	              guess whether it is reading a half-written answer.
//
// Which makes the responder, not this, the thing holding root: what a
// deployment grants by installing that unit is "run an arbitrary command
// as root", where the two units beside it grant one fixed command each.
// That is the point of the pane and it is not a small grant -- see the
// README section "A root shell, from the UI" for what it is worth and
// what it costs, and GRAIN_ROOT_SHELL=0 for the deployment that would
// rather not have it at all.
package rootshell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The three file names of the exchange above. Exported because the
// responder scripts/setup.sh installs has to spell the same three, and a
// test standing in for that responder should be able to name them
// without repeating string literals that could drift apart.
const (
	RequestFile = "shell"
	OutputFile  = "shell.out"
	StatusFile  = "shell.status"
)

// DefaultTimeout is how long Run waits for a responder to answer before
// giving up on it. Long enough for the kind of command this pane exists
// for -- a `systemctl restart`, a `docker logs`, a `du` over a data
// directory -- and short enough that a deployment with no responder
// installed at all learns so while the operator is still watching.
//
// It is not a limit on the command: nothing here can stop a command the
// host has already started, and grain-shell.service's own
// TimeoutStartSec is what bounds that end. A run that overruns this
// leaves its answer in the control directory, where the next Run's own
// clear() discards it.
const DefaultTimeout = 2 * time.Minute

// pollInterval is how often Run looks for the responder's answer. The
// commands this pane runs are usually over in milliseconds, and a human
// is watching a spinner for the rest of them, so this is short enough
// that a quick command feels immediate -- it costs one stat of one file.
const pollInterval = 50 * time.Millisecond

// Result is what one command left behind: what it printed, and what it
// exited with.
type Result struct {
	// Output is stdout and stderr interleaved, in the order the command
	// wrote them -- which is what an operator reading a terminal sees,
	// and the only ordering the responder's own single redirect can
	// preserve.
	Output string
	// ExitCode is the command's own status as the responder's shell saw
	// it, so 127 for a command that does not exist and 130 for one the
	// service's timeout killed. A non-zero code is not an error here:
	// `grep` finding nothing is a perfectly good answer to show.
	ExitCode int
}

// Runner is the daemon's end of the exchange, over one control
// directory.
type Runner struct {
	dir     string
	timeout time.Duration
	// mu serialises the exchange. The three file names above are fixed,
	// so two commands in flight at once would read each other's answers;
	// this is a single-operator UI with one Root shell tab, so queueing
	// the second one behind the first is both cheap and what a terminal
	// would do anyway.
	mu sync.Mutex
}

// New builds a Runner over dir -- $GRAIN_DATA_DIR/control in a real
// deployment, the same directory the reboot and restart requests are
// written to.
func New(dir string) *Runner { return &Runner{dir: dir, timeout: DefaultTimeout} }

// WithTimeout returns a Runner that waits d for an answer instead of
// DefaultTimeout. It is a fresh Runner rather than a setter so that the
// mutex above is never copied out from under a command in flight.
func (r *Runner) WithTimeout(d time.Duration) *Runner {
	return &Runner{dir: r.dir, timeout: d}
}

// Run asks the host to run command as root and waits for what it
// printed.
//
// A command that fails is a Result, not an error: the error return is
// for the exchange itself failing -- an unwritable control directory, a
// responder that never answered, an answer that made no sense. Those are
// the cases where the pane should say something about the deployment
// rather than about the command.
//
// ctx bounds the wait from the other end (the browser closing the pane's
// request), and whichever of ctx and the timeout fires first ends it.
func (r *Runner) Run(ctx context.Context, command string) (Result, error) {
	if strings.TrimSpace(command) == "" {
		return Result{}, errors.New("no command given")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	timeout := r.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Any answer still lying here is from a command whose operator has
	// already given up on it (see DefaultTimeout), and leaving it would
	// be handed back as this command's own output -- the worst failure
	// this exchange has available to it, since it is the one that looks
	// like an answer.
	if err := r.clear(); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(r.path(RequestFile), []byte(command), 0o600); err != nil {
		return Result{}, fmt.Errorf("asking the host to run a command as root: %w", err)
	}
	result, err := r.await(ctx, timeout)
	if err != nil {
		// Best effort, and racy by nature: a responder may be reading the
		// file this instant. It is still worth doing -- an unconsumed
		// request left behind is one the responder runs whenever it next
		// starts, long after the operator stopped watching for it.
		os.Remove(r.path(RequestFile))
		return Result{}, err
	}
	return result, nil
}

func (r *Runner) path(name string) string { return filepath.Join(r.dir, name) }

// clear removes whatever the last exchange left in the directory. A
// file that is simply not there is the normal case, not an error; a file
// that will not go is the deployment being misconfigured (a control
// directory this process cannot write), which every later step would
// fail on anyway and less clearly.
func (r *Runner) clear() error {
	for _, name := range []string{OutputFile, StatusFile} {
		if err := os.Remove(r.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clearing the last root shell answer out of %s: %w", r.dir, err)
		}
	}
	return nil
}

// await waits for shell.status to appear, then reads the answer out of
// the directory and takes both files away with it.
func (r *Runner) await(ctx context.Context, timeout time.Duration) (Result, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		status, err := os.ReadFile(r.path(StatusFile))
		switch {
		case err == nil:
			return r.read(status)
		case !errors.Is(err, os.ErrNotExist):
			return Result{}, fmt.Errorf("reading the root shell's exit status: %w", err)
		}
		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("no answer from this host's root shell responder after %s: "+
				"grain-shell.path and grain-shell.service are what watch %s and run the command as root, "+
				"and a host deployed before they existed has neither -- `sudo ./scripts/setup.sh` installs them: %w",
				timeout, r.path(RequestFile), ctx.Err())
		case <-ticker.C:
		}
	}
}

// read turns the pair of files into a Result and removes them. The
// output is read after the status, which is safe in exactly one
// direction: the responder writes the status last, so anything it has
// to say is already on disk by the time this side sees one at all.
func (r *Runner) read(status []byte) (Result, error) {
	code, err := strconv.Atoi(strings.TrimSpace(string(status)))
	if err != nil {
		return Result{}, fmt.Errorf("this host's root shell responder wrote %q to %s, which is not an exit status",
			strings.TrimSpace(string(status)), r.path(StatusFile))
	}
	// A command that printed nothing writes no output file on some
	// shells and an empty one on others; both mean the same thing, so a
	// missing file here is empty output rather than a failure.
	out, err := os.ReadFile(r.path(OutputFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("reading what the root shell printed: %w", err)
	}
	if cerr := r.clear(); cerr != nil {
		return Result{}, cerr
	}
	return Result{Output: string(out), ExitCode: code}, nil
}
