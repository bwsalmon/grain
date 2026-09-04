package guestbuild

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fakedockerPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakedocker-build")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fakedockerPath = filepath.Join(dir, "fakedocker")
	cmd := exec.Command("go", "build", "-o", fakedockerPath, "./testdata/fakedocker")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building fakedocker test helper: " + err.Error())
	}

	os.Exit(m.Run())
}

// testOptions returns Options pointed at fakedocker, plus a function
// reading back the calls it recorded as one []string per invocation.
func testOptions(t *testing.T) (Options, func() [][]string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKEDOCKER_LOG", logPath)

	opts := Options{
		From:            "ghcr.io/bwsalmon/kontur:debian12",
		Setup:           "#!/bin/sh\necho 'hello'\n",
		Tag:             "my-guest:dev",
		DockerBinary:    fakedockerPath,
		ShutdownTimeout: 90 * time.Second,
		Stdout:          io.Discard,
		Stderr:          io.Discard,
	}
	return opts, func() [][]string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("reading fakedocker log: %v", err)
		}
		var calls [][]string
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line != "" {
				call := strings.Split(line, "\x1f")
				for i := range call {
					call[i] = strings.ReplaceAll(call[i], "\x1e", "\n")
				}
				calls = append(calls, call)
			}
		}
		return calls
	}
}

// find returns the first recorded call whose args contain every one of
// want, in order but not necessarily adjacent.
func find(calls [][]string, want ...string) []string {
	for _, call := range calls {
		joined := strings.Join(call, " ")
		rest := joined
		ok := true
		for _, w := range want {
			i := strings.Index(rest, w)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(w):]
		}
		if ok {
			return call
		}
	}
	return nil
}

// count returns how many recorded calls begin with the given args.
func count(calls [][]string, prefix ...string) int {
	n := 0
	for _, call := range calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n
}

func TestBuildRunsTheWholeSequence(t *testing.T) {
	opts, calls := testOptions(t)
	if err := Build(context.Background(), opts); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := calls()

	// A guest needs a network before any of the rest works, so the boot
	// goes through the docker backend's three containers -- namespace
	// holder, netshim, then the VM joining that namespace -- rather than
	// a lone `docker run`, which would leave cloud-hypervisor with no
	// --net and the guest unreachable.
	if find(got, "run", "-d", "--entrypoint", "/usr/local/bin/kontur", opts.From, "sleep") == nil {
		t.Errorf("no network namespace holder started; calls were %v", got)
	}
	if find(got, "run", "--rm", "--network", "container:", opts.From, "netshim") == nil {
		t.Errorf("netshim never ran, so the guest would have no tap; calls were %v", got)
	}
	// The VM container gets netshim's own settings rather than a
	// precomputed CHV_NET: it derives its --net (and the guest's ip=)
	// from the identity on the namespace's interface at boot.
	if find(got, "run", "-d", "--network", "container:", "--device", "/dev/kvm", "NETSHIM_VM=", opts.From, "run") == nil {
		t.Errorf("no VM container attached to the namespace with a tap; calls were %v", got)
	}
	// No disk mount and no CHV_DISK_IMAGE: the guest being customized is
	// the one baked into the image, which is the whole premise.
	for _, call := range got {
		for _, a := range call {
			if strings.HasPrefix(a, "CHV_DISK_IMAGE=") {
				t.Errorf("CHV_DISK_IMAGE was set; the image's own guest should boot: %v", call)
			}
		}
	}
	// The setup script reaches the guest base64-encoded, so quoting and
	// newlines in it never have to survive two shells.
	encoded := base64.StdEncoding.EncodeToString([]byte(opts.Setup))
	if find(got, "exec", "kontur", "exec", "--", encoded) == nil {
		t.Errorf("the setup script was not staged base64-encoded; calls were %v", got)
	}
	if find(got, "exec", "kontur", "exec", "--", setupPath) == nil {
		t.Errorf("the staged setup script was never run; calls were %v", got)
	}
	if find(got, "exec", "kontur", "exec", "--", "rm -f /etc/ssh/ssh_host_*") == nil {
		t.Errorf("the scrub never ran; calls were %v", got)
	}
	// Stopping with the caller's own timeout, not docker's default: what
	// is being committed is the guest's filesystem, so the wait for a
	// clean power-off is the point.
	if find(got, "stop", "-t", "90") == nil {
		t.Errorf("no `docker stop` with the configured timeout; calls were %v", got)
	}
	if find(got, "commit", "org.opencontainers.image.base.name="+opts.From, opts.Tag) == nil {
		t.Errorf("no `docker commit` recording the base image; calls were %v", got)
	}
}

func TestBuildDoesNotCommitAKilledGuest(t *testing.T) {
	// 137 is a container docker had to SIGKILL, i.e. a guest that never
	// finished powering off -- so the filesystem in its disk image may
	// be mid-write, and committing it would bake that into every VM
	// cloned from the result.
	opts, calls := testOptions(t)
	t.Setenv("FAKEDOCKER_EXIT", "137")

	err := Build(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error for a guest that had to be killed, got nil")
	}
	if !strings.Contains(err.Error(), "shutdown-timeout") {
		t.Errorf("error = %v, want it to point at -shutdown-timeout", err)
	}
	if c := find(calls(), "commit"); c != nil {
		t.Errorf("committed anyway: %v", c)
	}
}

func TestBuildRefusesAnUncleanExit(t *testing.T) {
	// Any non-zero exit means the supervisor did not bring the VM down
	// the way it was asked to, and the console will show a guest that
	// looks perfectly healthy -- the number is the only evidence, so it
	// has to reach the caller rather than being swallowed on the way to
	// a commit.
	opts, calls := testOptions(t)
	t.Setenv("FAKEDOCKER_EXIT", "2")

	err := Build(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error for a container that exited uncleanly, got nil")
	}
	if !strings.Contains(err.Error(), "exited 2") {
		t.Errorf("error = %v, want it to name the exit code", err)
	}
	if c := find(calls(), "commit"); c != nil {
		t.Errorf("committed despite an unclean exit: %v", c)
	}
}

func TestBuildCleansUpAfterAFailedSetupScript(t *testing.T) {
	opts, calls := testOptions(t)
	t.Setenv("FAKEDOCKER_FAIL_CONTAINS", setupPath)

	err := Build(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error for a failing setup script, got nil")
	}
	got := calls()
	if c := find(got, "commit"); c != nil {
		t.Errorf("committed a guest whose setup script failed: %v", c)
	}
	// Create force-removes stale containers of its own before starting,
	// so two `rm -f` calls are its; the cleanup adds the VM and the
	// namespace holder, which would otherwise leak a container and a tap
	// device per failed build.
	if n := count(got, "rm", "-f"); n != 4 {
		t.Errorf("`rm -f` calls = %d, want 4 (2 from Create, 2 cleaning up); calls were %v", n, got)
	}
}

func TestBuildKeepsTheContainerOnFailureWhenAsked(t *testing.T) {
	opts, calls := testOptions(t)
	opts.KeepOnFailure = true
	t.Setenv("FAKEDOCKER_FAIL_CONTAINS", setupPath)

	err := Build(context.Background(), opts)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "docker logs") {
		t.Errorf("error = %v, want it to name the container left behind", err)
	}
	if n := count(calls(), "rm", "-f"); n != 2 {
		t.Errorf("`rm -f` calls = %d, want only Create's own 2: nothing should be removed under -keep-on-failure", n)
	}
}

func TestBuildFailsFastWhenTheContainerExits(t *testing.T) {
	// A container that is gone will not become reachable however long
	// the caller waits, so this must not sit out the ready timeout.
	opts, calls := testOptions(t)
	opts.ReadyTimeout = time.Hour
	t.Setenv("FAKEDOCKER_RUNNING", "false")
	t.Setenv("FAKEDOCKER_FAIL_CONTAINS", "kontur exec")

	done := make(chan error, 1)
	go func() { done <- Build(context.Background(), opts) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "exited before") {
			t.Errorf("error = %v, want it to say the container exited", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Build waited out the ready timeout instead of noticing the container had exited")
	}
	if c := find(calls(), "commit"); c != nil {
		t.Errorf("committed despite never reaching the guest: %v", c)
	}
}
