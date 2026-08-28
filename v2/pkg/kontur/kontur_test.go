package kontur

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPortReadsSavedVMState(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sandbox-0", vmState{Port: 30080})

	port, err := Port(dir, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if port != 30080 {
		t.Errorf("Port() = %d, want 30080", port)
	}
}

func TestPortRejectsMissingOrInvalid(t *testing.T) {
	dir := t.TempDir()

	if _, err := Port(dir, "no-such-vm"); err == nil {
		t.Error("Port() on a missing VM: got nil error, want one")
	}

	data, _ := json.Marshal(vmState{Port: 0})
	if err := os.WriteFile(filepath.Join(dir, "zero-port.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Port(dir, "zero-port"); err == nil {
		t.Error("Port() on a saved spec with no port: got nil error, want one")
	}
}

func TestPodName(t *testing.T) {
	if got, want := PodName("sandbox-0"), "kontur-vm-sandbox-0"; got != want {
		t.Errorf("PodName() = %q, want %q", got, want)
	}
}

// writeFakeCrictl installs a shell script named "crictl" on PATH (a temp
// directory prepended ahead of the real PATH, restored on cleanup) that
// answers a `pods --name <name> ...` call with podsJSON and an `inspectp
// ... <id>` call with inspectJSON, the same "fake the executable, not the
// exec.Command call" technique bwsalmon/kontur's own internal/hypervisor
// tests use (testdata/fakechv) to exercise real command construction
// without a real crictl/containerd on hand.
func writeFakeCrictl(t *testing.T, podsJSON, inspectJSON string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake crictl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *inspectp*)
    cat <<'EOF'
%s
EOF
    ;;
  *pods*)
    cat <<'EOF'
%s
EOF
    ;;
  *)
    echo "fake crictl: unexpected args: $*" >&2
    exit 1
    ;;
esac
`, inspectJSON, podsJSON)
	path := filepath.Join(dir, "crictl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPodIPResolvesThroughPodsThenInspectp(t *testing.T) {
	writeFakeCrictl(t,
		`{"items":[{"id":"abc123"}]}`,
		`{"status":{"network":{"ip":"10.100.5.7"}}}`,
	)

	ip, err := PodIP(context.Background(), "unix:///run/containerd/containerd.sock", "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.100.5.7" {
		t.Errorf("PodIP() = %q, want %q", ip, "10.100.5.7")
	}
}

func TestPodIPErrorsWhenPodNotFound(t *testing.T) {
	writeFakeCrictl(t, `{"items":[]}`, `{}`)

	if _, err := PodIP(context.Background(), "unix:///run/containerd/containerd.sock", "sandbox-0"); err == nil {
		t.Error("PodIP() with no matching pod: got nil error, want one")
	}
}

func TestPodIPErrorsWhenNetworkNotYetAssigned(t *testing.T) {
	writeFakeCrictl(t,
		`{"items":[{"id":"abc123"}]}`,
		`{"status":{"network":{"ip":""}}}`,
	)

	if _, err := PodIP(context.Background(), "unix:///run/containerd/containerd.sock", "sandbox-0"); err == nil {
		t.Error("PodIP() with an empty network IP: got nil error, want one")
	}
}

// writeFakeKontur installs a shell script named "kontur" on PATH (same
// technique as writeFakeCrictl above), which appends every invocation's
// argv to argvLog (one line per call, space-joined) and exits with
// exitCode.
func writeFakeKontur(t *testing.T, argvLog string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kontur script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %s
exit %d
`, argvLog, exitCode)
	path := filepath.Join(dir, "kontur")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCreateRunsKonturVMCreate(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	writeFakeKontur(t, log, 0)

	if err := Create(context.Background(), "/var/lib/kontur/vms", "sandbox-0", "-image", "grain-sandbox.qcow2"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm create sandbox-0 -state-dir /var/lib/kontur/vms -image grain-sandbox.qcow2\n"
	if string(got) != want {
		t.Errorf("kontur invoked with %q, want %q", got, want)
	}
}

func TestCreateReturnsErrorOnFailure(t *testing.T) {
	writeFakeKontur(t, filepath.Join(t.TempDir(), "argv.log"), 1)

	if err := Create(context.Background(), "/var/lib/kontur/vms", "sandbox-0"); err == nil {
		t.Error("Create() with a failing kontur binary: got nil error, want one")
	}
}

func TestDeleteRunsKonturVMDelete(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	writeFakeKontur(t, log, 0)

	if err := Delete(context.Background(), "/var/lib/kontur/vms", "sandbox-0"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm delete sandbox-0 -state-dir /var/lib/kontur/vms\n"
	if string(got) != want {
		t.Errorf("kontur invoked with %q, want %q", got, want)
	}
}
