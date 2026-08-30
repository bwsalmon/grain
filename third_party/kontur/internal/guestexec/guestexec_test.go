package guestexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestFromEnv_RequiresAddr(t *testing.T) {
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want an error when KONTUR_EXEC_ADDR is unset")
	}
}

func TestFromEnv_RejectsAddrWithoutPort(t *testing.T) {
	t.Setenv(envAddr, "169.254.100.2")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want an error for an address with no port")
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	t.Setenv(envAddr, "169.254.100.2:22")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.User != defaultUser {
		t.Errorf("User = %q, want %q", cfg.User, defaultUser)
	}
	if cfg.KeyPath != defaultKeyPath {
		t.Errorf("KeyPath = %q, want %q", cfg.KeyPath, defaultKeyPath)
	}
	if cfg.ConnectTimeout != defaultConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, defaultConnectTimeout)
	}
}

func TestFromEnv_Overrides(t *testing.T) {
	t.Setenv(envAddr, "169.254.100.2:22")
	t.Setenv(envUser, "debug")
	t.Setenv(envKeyPath, "/tmp/other-key")
	t.Setenv(envConnectTimeout, "5s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.User != "debug" {
		t.Errorf("User = %q, want %q", cfg.User, "debug")
	}
	if cfg.KeyPath != "/tmp/other-key" {
		t.Errorf("KeyPath = %q, want %q", cfg.KeyPath, "/tmp/other-key")
	}
	if cfg.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout = %v, want 5s", cfg.ConnectTimeout)
	}
}

func TestFromEnv_InvalidConnectTimeout(t *testing.T) {
	t.Setenv(envAddr, "169.254.100.2:22")
	t.Setenv(envConnectTimeout, "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want an error for an invalid duration")
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

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("boom")); got != 1 {
		t.Errorf("exitCode(non-exit error) = %d, want 1", got)
	}
	// The *ssh.ExitError branch (a genuine remote exit code) is covered
	// end-to-end by TestRun_ExecutesGivenCommandAndReportsExitCode and
	// TestRun_NonZeroExitIsNotAnError below, against a real (fake) sshd
	// -- constructing one by hand here would just restate ssh.Waitmsg's
	// own wire format.
}

// --- end-to-end tests against a fake in-process sshd ---

// fakeGuestSSHD starts a minimal SSH server on loopback that accepts
// publicKey (any other key is rejected, the same way the reference guest
// image only trusts its own baked-in keys), and for each session either
// runs handleExec (when the client sends an "exec" request) or
// handleShell (a bare "shell" request, no command) -- standing in for
// deploy/guest-image's ForceCommand wrapper without needing a real guest
// to test against.
func fakeGuestSSHD(t *testing.T, publicKey ssh.PublicKey, handleExec func(cmd string, ch ssh.Channel), handleShell func(ch ssh.Channel)) (addr string) {
	t.Helper()

	hostSigner, err := ssh.NewSignerFromKey(genEd25519(t))
	if err != nil {
		t.Fatalf("creating host key: %v", err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), publicKey.Marshal()) {
				return nil, errors.New("unauthorized key")
			}
			return nil, nil
		},
	}
	config.AddHostKey(hostSigner)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// Loops over every connection (not just the first) so a test that
	// deliberately fails auth, forcing guestexec's own retry loop to
	// dial again, doesn't hang waiting for a second connection nothing
	// would ever accept.
	go func() {
		for {
			nConn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				sConn, chans, reqs, err := ssh.NewServerConn(nConn, config)
				if err != nil {
					return
				}
				defer sConn.Close()
				go ssh.DiscardRequests(reqs)

				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						newCh.Reject(ssh.UnknownChannelType, "only session channels supported")
						continue
					}
					ch, requests, err := newCh.Accept()
					if err != nil {
						return
					}
					go func() {
						defer ch.Close()
						for req := range requests {
							switch req.Type {
							case "exec":
								var payload struct{ Command string }
								ssh.Unmarshal(req.Payload, &payload)
								if req.WantReply {
									req.Reply(true, nil)
								}
								handleExec(payload.Command, ch)
								return
							case "shell":
								if req.WantReply {
									req.Reply(true, nil)
								}
								handleShell(ch)
								return
							case "pty-req", "window-change":
								if req.WantReply {
									req.Reply(true, nil)
								}
							default:
								if req.WantReply {
									req.Reply(false, nil)
								}
							}
						}
					}()
				}
			}()
		}
	}()

	return l.Addr().String()
}

func genEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return priv
}

// writeClientKey generates a fresh ed25519 keypair, writes the private
// half (in the OpenSSH format ssh.ParsePrivateKey/loadKey expects) to a
// file under t.TempDir, and returns its path plus the public key so the
// test's fake sshd can authorize exactly that key.
func writeClientKey(t *testing.T) (path string, pub ssh.PublicKey) {
	t.Helper()
	priv := genEd25519(t)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshaling private key: %v", err)
	}
	path = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}
	return path, signer.PublicKey()
}

func TestRun_ExecutesGivenCommandAndReportsExitCode(t *testing.T) {
	keyPath, pub := writeClientKey(t)

	var gotCmd string
	addr := fakeGuestSSHD(t, pub,
		func(cmd string, ch ssh.Channel) {
			gotCmd = cmd
			io.WriteString(ch, "hello from guest\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 7}))
		},
		func(ch ssh.Channel) {
			t.Error("handleShell called, want handleExec for a non-empty command")
		},
	)

	cfg := Config{Addr: addr, User: "root", KeyPath: keyPath, ConnectTimeout: 2 * time.Second}
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), cfg, []string{"echo", "hello world"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 7 {
		t.Errorf("Run() code = %d, want 7", code)
	}
	if want := "'echo' 'hello world'"; gotCmd != want {
		t.Errorf("guest received command %q, want %q", gotCmd, want)
	}
	if got := stdout.String(); got != "hello from guest\n" {
		t.Errorf("stdout = %q, want %q", got, "hello from guest\n")
	}
}

func TestRun_EmptyCommandRequestsShell(t *testing.T) {
	keyPath, pub := writeClientKey(t)

	shellCalled := false
	addr := fakeGuestSSHD(t, pub,
		func(cmd string, ch ssh.Channel) {
			t.Error("handleExec called, want handleShell for an empty command")
		},
		func(ch ssh.Channel) {
			shellCalled = true
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
		},
	)

	cfg := Config{Addr: addr, User: "root", KeyPath: keyPath, ConnectTimeout: 2 * time.Second}
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), cfg, nil, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Errorf("Run() code = %d, want 0", code)
	}
	if !shellCalled {
		t.Error("guest never received a shell request")
	}
}

func TestRun_ForwardsStdin(t *testing.T) {
	keyPath, pub := writeClientKey(t)

	var got bytes.Buffer
	addr := fakeGuestSSHD(t, pub,
		func(cmd string, ch ssh.Channel) {
			io.Copy(&got, ch)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
		},
		func(ch ssh.Channel) {},
	)

	cfg := Config{Addr: addr, User: "root", KeyPath: keyPath, ConnectTimeout: 2 * time.Second}
	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), cfg, []string{"cat"}, strings.NewReader("piped in"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got.String() != "piped in" {
		t.Errorf("guest received stdin %q, want %q", got.String(), "piped in")
	}
}

func TestRun_RejectsWrongKey(t *testing.T) {
	_, wantPub := writeClientKey(t)
	wrongKeyPath, _ := writeClientKey(t)

	addr := fakeGuestSSHD(t, wantPub,
		func(cmd string, ch ssh.Channel) {},
		func(ch ssh.Channel) {},
	)

	cfg := Config{Addr: addr, User: "root", KeyPath: wrongKeyPath, ConnectTimeout: 500 * time.Millisecond}
	_, err := Run(context.Background(), cfg, []string{"true"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Run() error = nil, want an error when authenticating with an unauthorized key")
	}
}

// TestRun_HandshakeHangIsBounded exercises a server that accepts the TCP
// connection but never speaks SSH at all -- regression coverage for
// dialOnce needing its own deadline, since ssh.Dial's own
// ClientConfig.Timeout only bounds the TCP connect, not the handshake
// (see dialOnce's doc comment).
func TestRun_HandshakeHangIsBounded(t *testing.T) {
	keyPath, _ := writeClientKey(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Accept and hold the connection open without ever sending
			// an SSH version banner.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	cfg := Config{Addr: l.Addr().String(), User: "root", KeyPath: keyPath, ConnectTimeout: 500 * time.Millisecond}
	start := time.Now()
	_, err = Run(context.Background(), cfg, []string{"true"}, strings.NewReader(""), io.Discard, io.Discard)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run() error = nil, want an error when the server never completes the SSH handshake")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run() took %s to give up, want it bounded by ConnectTimeout/dialTimeout", elapsed)
	}
}

func TestRun_NonZeroExitIsNotAnError(t *testing.T) {
	keyPath, pub := writeClientKey(t)

	addr := fakeGuestSSHD(t, pub,
		func(cmd string, ch ssh.Channel) {
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 42}))
		},
		func(ch ssh.Channel) {},
	)

	cfg := Config{Addr: addr, User: "root", KeyPath: keyPath, ConnectTimeout: 2 * time.Second}
	code, err := Run(context.Background(), cfg, []string{"false"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a mere non-zero remote exit", err)
	}
	if code != 42 {
		t.Errorf("Run() code = %d, want 42", code)
	}
}
