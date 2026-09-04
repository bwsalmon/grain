package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bwsalmon/kontur/internal/dockervm"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// exitStatusError carries a guest command's own exit status out to Run,
// which turns it into konturctl's exit status.
//
// A command that ran in the guest and exited non-zero is not a konturctl
// failure: the whole point of "vm exec" is to be transparent, so the
// status has to come back to the caller's shell as the guest's own rather
// than as konturctl's blanket 1, and nothing extra gets printed -- the
// command's own stderr has already been passed through.
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string {
	return fmt.Sprintf("the command exited with status %d", e.code)
}

// statusError returns nil for a successful command and an exitStatusError
// for any other status, so callers can "return statusError(code)".
func statusError(code int) error {
	if code == 0 {
		return nil
	}
	return &exitStatusError{code: code}
}

// execSettings is what "vm exec", "vm shell" and "vm run" all need once
// their own flags have been parsed: which VM, how to reach the guest, and
// what to run in it.
type execSettings struct {
	stateDir string
	tty      bool
	user     string
	// connectTimeout, when set, becomes KONTUR_EXEC_CONNECT_TIMEOUT for
	// this one session -- how long "kontur exec" keeps retrying the dial
	// into a guest that may still be booting (30s by default).
	connectTimeout time.Duration
	// workdir and env belong to the guest command rather than to the
	// session that carries it, so they are passed through as "kontur
	// exec"'s own -w/-e (see dockervm.ExecOptions) rather than as
	// environment on the docker exec.
	workdir string
	env     repeatedFlag
	command []string
}

// registerExecFlags registers the flags "vm exec" and "vm shell" share.
func registerExecFlags(fs *flag.FlagSet, s *execSettings, ttyDefault bool) {
	fs.StringVar(&s.stateDir, "state-dir", defaultStateDir, "directory kontur stores VM state in")
	fs.BoolVar(&s.tty, "it", ttyDefault, "allocate a terminal for the session, the way \"docker exec -it\" does; needs a terminal on konturctl's own stdin")
	fs.StringVar(&s.user, "user", "", "guest account to run as, overriding the VM's own -guest-user for this one command (empty means root)")
	fs.DurationVar(&s.connectTimeout, "connect-timeout", 0, "how long to keep retrying the connection into a guest that may still be booting; 0 leaves the guest side's own default (30s)")
	// -w and -e, spelled the way "docker exec" spells them, since that
	// is where someone reaching for them has just come from. The long
	// forms are registered too: Go's flag package treats -workdir and
	// --workdir as the same flag.
	fs.StringVar(&s.workdir, "w", "", "absolute guest directory to run the command in (empty means the account's home directory)")
	fs.StringVar(&s.workdir, "workdir", "", "absolute guest directory to run the command in (empty means the account's home directory)")
	fs.Var(&s.env, "e", "environment variable for the guest command, KEY=value, repeatable; overlaid on the session's own environment rather than replacing it")
	fs.Var(&s.env, "env", "environment variable for the guest command, KEY=value, repeatable; overlaid on the session's own environment rather than replacing it")
}

// checkGuestEnv rejects a -e that is not KEY=value here, rather than
// letting it travel to the guest and come back as a refused request with
// a container name in the message.
//
// A bare "-e NAME" is refused too, even though "docker exec -e NAME"
// takes the value from the client's own environment: the value would
// have to cross into the container and then into the guest, and a
// variable that quietly did not make that trip is worse than one that
// was never accepted. "-e NAME=$NAME" says the same thing out loud.
func (s execSettings) checkGuestEnv() error {
	for _, e := range s.env {
		key, _, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			return fmt.Errorf("-e %q: expected KEY=value (a bare variable name is not taken from konturctl's own environment -- write -e %s=\"$%s\" if that is what you meant)", e, e, e)
		}
	}
	return nil
}

// execEnv turns the settings that belong to the guest side into the
// environment "kontur exec" reads them from.
func (s execSettings) execEnv() []string {
	var env []string
	if s.user != "" {
		env = append(env, "KONTUR_EXEC_USER="+s.user)
	}
	if s.connectTimeout > 0 {
		env = append(env, "KONTUR_EXEC_CONNECT_TIMEOUT="+s.connectTimeout.String())
	}
	return env
}

// isTerminal reports whether r is a terminal, which is what decides
// whether a session can ask docker for a tty at all. A non-*os.File
// reader -- a pipe, a test's buffer -- never is.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too, and "< /dev/null" is an
	// ordinary way to run something with no input -- but docker refuses
	// -t on it ("the input device is not a TTY"), so it must not count as
	// a terminal here.
	if devNull, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, devNull) {
		return false
	}
	return true
}

// runVMExec runs one command inside a VM's guest, or opens an interactive
// session in it when given no command.
func runVMExec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm exec")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm exec "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var s execSettings
	registerExecFlags(fs, &s, false)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// Everything after the flags -- with or without a "--" in front of it,
	// since Go's flag package stops at the first non-flag argument either
	// way -- is the guest command.
	s.command = fs.Args()

	code, err := execInVM(ctx, name, s, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	return statusError(code)
}

// runVMShell opens an interactive login shell in a VM's guest: "vm exec"
// with a terminal asked for by default and no command to run.
func runVMShell(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm shell")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm shell "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var s execSettings
	// A terminal by default, but only if there is one: "konturctl vm
	// shell web < script.sh" is a perfectly good way to feed a guest
	// shell a script, and asking docker for a tty there fails outright.
	registerExecFlags(fs, &s, isTerminal(stdin))
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return fmt.Errorf("vm shell takes no command (got %q); use \"konturctl vm exec %s -- %s\"", extra[0], name, extra[0])
	}

	code, err := execInVM(ctx, name, s, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	return statusError(code)
}

// execInVM resolves a VM's backend from its saved state and runs a
// command in its guest, returning the command's own exit status.
func execInVM(ctx context.Context, name string, s execSettings, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	spec, err := staticpod.Load(s.stateDir, name)
	if err != nil {
		return 0, fmt.Errorf("VM %q not found in %s (\"konturctl vm list -state-dir %s\" shows what is): %w", name, s.stateDir, s.stateDir, err)
	}
	if backend := spec.BackendOrDefault(); backend != staticpod.BackendDocker {
		return 0, errNoExecForBackend(name, backend)
	}
	if s.tty && !isTerminal(stdin) {
		return 0, fmt.Errorf("-it needs a terminal on stdin, and konturctl's stdin is not one; drop -it (stdin is still passed through to the guest command either way)")
	}
	if err := s.checkGuestEnv(); err != nil {
		return 0, err
	}

	return dockervm.Exec(ctx, &dockervm.Docker{}, name, dockervm.ExecOptions{
		Command:  s.command,
		TTY:      s.tty,
		Env:      s.execEnv(),
		Workdir:  s.workdir,
		GuestEnv: s.env,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
	})
}

// errNoExecForBackend explains that konturctl can only exec into
// docker-backed VMs, and hands over the commands that do the same thing
// by hand for the one it can't.
//
// The static-pod backend's VMs run under the standalone kubelet
// (deploy/static-kubelet/README.md), where the way into a container is
// crictl against containerd's socket -- and crictl is not something
// konturctl installs or can assume is there, so this says what to run
// rather than shelling out to something that probably isn't present.
func errNoExecForBackend(name, backend string) error {
	const endpoint = "unix:///run/containerd/containerd.sock"
	return fmt.Errorf(`exec is only implemented for -backend %s, and VM %q runs under -backend %s.
Reach it by hand through crictl instead (see deploy/static-kubelet/README.md):

  crictl --runtime-endpoint %s ps -q --name %s --state Running
  crictl --runtime-endpoint %s exec -it <id> kontur exec -- <command>`,
		staticpod.BackendDocker, name, backend, endpoint, name, endpoint)
}

// runVMRun is the whole of the top-level README's Flow 1 in one command:
// create a VM, wait for its guest, run one command in it, report that
// command's exit status, and delete the VM again.
//
// It takes every flag "vm create" does, since the VM it makes is an
// ordinary one, plus the two this shape needs of its own: how long to
// wait for the guest, and whether to keep the VM when something goes
// wrong on the way to running the command.
func runVMRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm run")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm run "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var s execSettings
	registerExecFlags(fs, &s, false)
	// -backend docker, not "vm create"'s static-pod default: a one-off VM
	// is only useful if its command can be run in it, and exec is the
	// docker backend's alone (see errNoExecForBackend).
	backend := fs.String("backend", staticpod.BackendDocker, `how to run this VM; only "docker" can be exec'd into, so only "docker" works here`)
	readyTimeout := fs.Duration("ready-timeout", defaultReadyTimeout, "how long to wait for the guest to become reachable after the VM starts")
	keepOnFailure := fs.Bool("keep-on-failure", false, "leave the VM in place if it never becomes reachable, for inspection (a command that merely exits non-zero still deletes it -- that is a result, not a failure)")
	// Every VM flag "vm create" takes, on the same defaults -- for the
	// docker backend, since that is the only one this command can use
	// (so -kontur-image defaults to the locally built image rather than
	// to the static-pod registry).
	v := registerVMFlags(fs, staticpod.DefaultsForBackend(staticpod.BackendDocker))
	if err := fs.Parse(rest); err != nil {
		return err
	}
	s.command = fs.Args()
	if len(s.command) == 0 {
		return fmt.Errorf("vm run: a command is required (konturctl vm run %s [flags] -- <command>); use \"konturctl vm create\" for a VM that stays up", name)
	}
	if *backend != staticpod.BackendDocker {
		return errNoExecForBackend(name, *backend)
	}
	if err := v.checkDeprecated(); err != nil {
		return err
	}
	if s.tty && !isTerminal(stdin) {
		return fmt.Errorf("-it needs a terminal on stdin, and konturctl's stdin is not one; drop -it (stdin is still passed through to the guest command either way)")
	}
	// Before anything is created: a mistyped -e is not worth a VM's
	// worth of setup and teardown to find out about.
	if err := s.checkGuestEnv(); err != nil {
		return err
	}
	if _, err := staticpod.Load(s.stateDir, name); err == nil {
		return fmt.Errorf("VM %q already exists (\"konturctl vm run\" creates and deletes the VM it names, so pick a name nothing else is using)", name)
	}

	spec := v.toSpec(name)
	spec.Backend = *backend

	// Every progress line goes to stderr, because stdout belongs to the
	// guest command: "konturctl vm run oneoff -- cat /etc/os-release >
	// os-release" has to write the guest's bytes and nothing else.
	fmt.Fprintf(stderr, "creating VM %q\n", name)
	if err := submitVM(ctx, spec, s.stateDir, stderr); err != nil {
		return err
	}

	// Deleted on the way out however this ends, including a signal that
	// cancelled ctx: the VM exists only for this one command, and leaving
	// it behind leaks a container, a tap device and an overlay per run.
	cleanup := func() {
		fmt.Fprintf(stderr, "deleting VM %q\n", name)
		if err := deleteVM(context.WithoutCancel(ctx), name, s.stateDir, "", stderr); err != nil {
			fmt.Fprintf(stderr, "konturctl: deleting VM %q: %v\n", name, err)
		}
	}
	keep := func(err error) error {
		if *keepOnFailure {
			fmt.Fprintf(stderr, "leaving VM %q in place (-keep-on-failure); delete it with \"konturctl vm delete %s -state-dir %s\"\n", name, name, s.stateDir)
			return err
		}
		cleanup()
		return err
	}

	// On stderr, like every other progress line here: stdout belongs to
	// the guest command.
	if err := waitForGuest(ctx, name, spec.Backend, *readyTimeout, stderr); err != nil {
		return keep(err)
	}

	code, err := dockervm.Exec(ctx, &dockervm.Docker{}, name, dockervm.ExecOptions{
		Command:  s.command,
		TTY:      s.tty,
		Env:      s.execEnv(),
		Workdir:  s.workdir,
		GuestEnv: s.env,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if err != nil {
		return keep(err)
	}
	// A non-zero status is the command's answer, not a failure of this
	// one: the VM goes away either way and the status is passed on.
	cleanup()
	return statusError(code)
}
