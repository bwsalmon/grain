// Package guestexec implements "kontur exec": running a command inside
// the VM guest over SSH to its already-running sshd (see
// deploy/guest-image/README.md), rather than in the container itself --
// so `kubectl exec` on a kontur VM container ends up in the workload the
// VM is actually running, not the otherwise-empty scratch container
// wrapping it.
//
// This piggybacks entirely on machinery the guest image already has for
// interactive SSH access: the ForceCommand wrapper
// (deploy/guest-image/overlay-common/usr/local/libexec/kontur-ssh-console-wrap)
// already accepts an arbitrary SSH_ORIGINAL_COMMAND (or none, for a login
// shell) and mirrors its output to the guest's serial console the same
// way any other SSH session on this guest is observable. guestexec adds
// nothing guest-side beyond the dedicated keypair the Dockerfile's
// exec-keypair stage bakes in, purely so this always has a way to log
// in without depending on an operator's own GUEST_SSH_AUTHORIZED_KEY.
package guestexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

const (
	envAddr           = "KONTUR_EXEC_ADDR"
	envUser           = "KONTUR_EXEC_USER"
	envKeyPath        = "KONTUR_EXEC_KEY"
	envConnectTimeout = "KONTUR_EXEC_CONNECT_TIMEOUT"

	defaultUser           = "root"
	defaultKeyPath        = "/etc/kontur/exec_id_ed25519"
	defaultConnectTimeout = 30 * time.Second

	dialTimeout   = 3 * time.Second
	retryInterval = 500 * time.Millisecond
)

// Config holds everything needed to run a command inside the guest over
// SSH.
type Config struct {
	// Addr is the guest's own address, e.g. "169.254.100.2:22" -- the
	// same tap-attached address CHV_CMDLINE's "ip=" configures on the
	// guest side. It's reachable directly from this container's own
	// network namespace, which netshim mode already shares with it (see
	// the top-level README's "Pod-local networking" section), without
	// going through NETSHIM_GUEST_PORT's external DNAT at all. There is
	// no safe default -- nothing else in kontur's own config models the
	// guest's address as a value rather than a substring of CHV_CMDLINE
	// -- so this must be set explicitly wherever CHV_CMDLINE's "ip=" is
	// (konturctl's own backends do this automatically; see
	// internal/staticpod and internal/dockervm).
	Addr string

	// User is the guest account to log in as. Defaults to "root", the
	// only account the reference guest image (deploy/guest-image)
	// creates.
	User string

	// KeyPath is a private key authorized on the guest for User.
	// Defaults to the dedicated keypair the Dockerfile's exec-keypair
	// stage bakes into the image and authorizes on the reference guest
	// image, independent of any operator-supplied
	// GUEST_SSH_AUTHORIZED_KEY (see deploy/guest-image/README.md) -- a
	// custom CHV_DISK_IMAGE that wants "kontur exec" to work needs to
	// authorize the same public key (or point KONTUR_EXEC_KEY at one it
	// does authorize) itself.
	KeyPath string

	// ConnectTimeout bounds how long to keep retrying the initial
	// connection before giving up: the guest's sshd may not be up yet
	// immediately after the container starts, since that depends on
	// guest boot time, not just kontur's own startup.
	ConnectTimeout time.Duration
}

// FromEnv builds a Config from the process environment.
func FromEnv() (Config, error) {
	cfg := Config{
		Addr:    os.Getenv(envAddr),
		User:    getEnvDefault(envUser, defaultUser),
		KeyPath: getEnvDefault(envKeyPath, defaultKeyPath),
	}
	if cfg.Addr == "" {
		return Config{}, fmt.Errorf("%s is required: the guest's own address (e.g. \"169.254.100.2:22\", matching CHV_CMDLINE's ip= and directly reachable since this container shares netshim's network namespace)", envAddr)
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return Config{}, fmt.Errorf("%s: %w", envAddr, err)
	}

	cfg.ConnectTimeout = defaultConnectTimeout
	if v := os.Getenv(envConnectTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration %q", envConnectTimeout, v)
		}
		cfg.ConnectTimeout = d
	}

	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// Run connects to the guest and runs command, returning once the remote
// session ends. command is shell-quoted and joined into the single
// string the SSH protocol's "exec" request carries (see
// kontur-ssh-console-wrap's own SSH_ORIGINAL_COMMAND handling); an empty
// command instead requests an interactive login shell, matching that
// wrapper's own fallback.
//
// When stdin is a terminal, Run puts it into raw mode for the duration of
// the session (the same way an ordinary interactive `ssh` client does)
// and forwards SIGWINCH as PTY resizes; otherwise stdin/stdout/stderr are
// simply piped through. Either way, closing ctx tears the session down.
//
// The returned int is the remote command's exit code, following
// os/exec.Cmd.Wait's convention of being meaningful even when err is
// non-nil (a non-zero remote exit is reported through the code, not
// err).
func Run(ctx context.Context, cfg Config, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	signer, err := loadKey(cfg.KeyPath)
	if err != nil {
		return 0, fmt.Errorf("loading %s: %w", cfg.KeyPath, err)
	}

	client, err := dialWithRetry(ctx, cfg, signer)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return 0, fmt.Errorf("opening session to %s: %w", cfg.Addr, err)
	}
	defer session.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()

	session.Stdin = stdin
	session.Stdout = stdout

	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		restore, err := attachTTY(session, f)
		if err != nil {
			return 0, err
		}
		defer restore()
	} else {
		session.Stderr = stderr
	}

	cmd := shellJoin(command)
	if cmd == "" {
		err = session.Shell()
	} else {
		err = session.Start(cmd)
	}
	if err != nil {
		return 0, fmt.Errorf("starting remote command: %w", err)
	}

	waitErr := session.Wait()
	return exitCode(waitErr), sessionErr(waitErr)
}

// attachTTY puts f (stdin, already known to be a terminal) into raw mode,
// requests a remote PTY sized to match it, and forwards SIGWINCH as
// resizes for as long as the session lasts. The returned func restores
// the local terminal and must be called once the session ends.
func attachTTY(session *ssh.Session, f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("setting local terminal to raw mode: %w", err)
	}

	w, h, err := term.GetSize(fd)
	if err != nil {
		w, h = 80, 24
	}

	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color"
	}
	if err := session.RequestPty(termType, h, w, ssh.TerminalModes{}); err != nil {
		term.Restore(fd, state)
		return nil, fmt.Errorf("requesting pty: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for range sigCh {
			if w, h, err := term.GetSize(fd); err == nil {
				session.WindowChange(h, w)
			}
		}
	}()

	return func() {
		signal.Stop(sigCh)
		close(sigCh)
		<-stopped
		term.Restore(fd, state)
	}, nil
}

// loadKey reads and parses an unencrypted private key from path.
func loadKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

// dialWithRetry keeps trying to reach and authenticate against cfg.Addr
// until it succeeds, ctx is cancelled, or cfg.ConnectTimeout elapses --
// the guest's sshd may take a few seconds to come up after the container
// itself has started, since that depends on guest boot time.
//
// The host key is intentionally not verified: there is no side channel
// through which kontur could learn it ahead of time (the reference guest
// image regenerates host keys on first boot specifically so VMs don't
// share them, see deploy/guest-image/README.md), and this connection
// never leaves the same pod/network-namespace trust boundary that
// already grants the container /dev/kvm access to this exact guest.
func dialWithRetry(ctx context.Context, cfg Config, signer ssh.Signer) (*ssh.Client, error) {
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	deadline := time.Now().Add(cfg.ConnectTimeout)
	var lastErr error
	for {
		client, err := dialOnce(cfg.Addr, clientCfg)
		if err == nil {
			return client, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connecting to %s: timed out after %s, last error: %w", cfg.Addr, cfg.ConnectTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connecting to %s: %w", cfg.Addr, ctx.Err())
		case <-time.After(retryInterval):
		}
	}
}

// dialOnce makes a single attempt to connect and authenticate against
// addr, bounding the whole thing (TCP connect *and* the SSH handshake)
// by dialTimeout. ssh.Dial's own ClientConfig.Timeout only bounds the
// TCP connect (see golang.org/x/crypto/ssh's own Dial, which passes it
// straight to net.DialTimeout and nothing else) -- left at that alone, a
// server that accepts the TCP connection but never completes (or never
// finishes) the SSH handshake would hang a single attempt forever,
// regardless of ConnectTimeout.
func dialOnce(addr string, clientCfg *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		conn.Close()
		return nil, err
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// The deadline above only needs to cover the handshake; clear it so
	// it doesn't also cut off the session this connection goes on to
	// carry.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// shellJoin renders args as a single POSIX shell command line, so a
// command containing spaces or shell metacharacters round-trips through
// the guest's ForceCommand wrapper (which re-parses SSH_ORIGINAL_COMMAND
// via "sh -c", per the SSH "exec" request only ever carrying one string)
// the same way it would as an ordinary local argv.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// exitCode extracts the remote command's exit status from the error
// session.Wait returns, following os/exec.Cmd.Wait's own convention (0
// for a nil error).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	return 1
}

// sessionErr reports whether err (from session.Wait) reflects a
// kontur-side failure worth surfacing, as opposed to simply the remote
// command's own non-zero exit -- which isn't a "kontur exec" failure any
// more than a non-zero exit is a failure of "kontur run" itself (see
// cmd/kontur's runVM, which mirrors cloud-hypervisor's exit code the same
// way).
func sessionErr(err error) error {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}
