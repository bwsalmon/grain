package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/staticpod"
)

// fakedockerDir is built once per test run from
// ../dockervm/testdata/fakedocker, named "docker" so tests can prepend it
// to PATH and exercise the "-backend docker" path (dockervm.Docker{}
// resolves the real docker CLI via PATH) without a real docker daemon.
var fakedockerDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakedocker-build")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "docker"), "../dockervm/testdata/fakedocker")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building fakedocker test helper: " + err.Error())
	}
	fakedockerDir = dir

	os.Exit(m.Run())
}

func runVMArgs(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = runVM(context.Background(), args, &o, &e)
	return o.String(), e.String(), err
}

// withFakeDocker prepends the fakedocker build to PATH for the duration of
// t, so code that shells out to "docker" hits the fake instead.
func withFakeDocker(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", fakedockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestVMLifecycle(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	_, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	)
	if err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	manifestPath := filepath.Join(podDir, "kontur-vm-web.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if !strings.Contains(string(data), `value: "2"`) {
		t.Errorf("manifest missing default CPUs=2:\n%s", data)
	}

	// create again should fail without touching anything
	if _, _, err := runVMArgs(t, "create", "web", "--disk", "x", "--ip", "169.254.100.2", "--port", "1",
		"--state-dir", stateDir, "--static-pod-path", podDir); err == nil {
		t.Fatalf("create of existing VM = nil error, want it to fail")
	}

	// list should show it
	out, _, err := runVMArgs(t, "list", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	if !strings.Contains(out, "web") {
		t.Errorf("list output missing web VM:\n%s", out)
	}

	// update cpus only; disk/kernel/ip should be preserved
	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "4", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	data, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not re-written: %v", err)
	}
	updated := string(data)
	if !strings.Contains(updated, `value: "4"`) {
		t.Errorf("manifest missing updated CPUs=4:\n%s", updated)
	}
	if !strings.Contains(updated, "/images/vmlinux") {
		t.Errorf("update lost preserved -kernel value:\n%s", updated)
	}

	// delete removes both manifest and state
	if _, stderr, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Fatalf("delete error = %v, stderr = %s", err, stderr)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest still exists after delete: err = %v", err)
	}

	// delete again is a no-op, not an error
	if _, _, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Errorf("second delete error = %v, want nil (idempotent)", err)
	}

	// update after delete should fail with a helpful message
	if _, _, err := runVMArgs(t, "update", "web", "--cpus", "2", "--state-dir", stateDir); err == nil {
		t.Fatalf("update of deleted VM = nil error, want it to fail")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("update error = %v, want it to mention the VM wasn't found", err)
	}
}

func TestVMUpdate_RecomputesAutoCmdlineOnIPChange(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	if _, stderr, err := runVMArgs(t, "update", "web", "--ip", "169.254.100.9", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}

	data, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ip=169.254.100.9::") {
		t.Errorf("cmdline was not recomputed for the new IP:\n%s", data)
	}
	if strings.Contains(string(data), "169.254.100.2") {
		t.Errorf("stale IP still present after update:\n%s", data)
	}
}

func TestVMUpdate_ExplicitCmdlineSurvivesLaterUpdates(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--cmdline", "console=ttyS0 root=/dev/vda ro custom=1",
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	// An unrelated update must not clobber the explicit cmdline.
	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "3", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}

	data, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "custom=1") {
		t.Errorf("explicit cmdline was lost on unrelated update:\n%s", data)
	}
}

func TestVMLifecycle_WritableDiskNeedsNoHostState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")
	imagesDir := t.TempDir()
	diskDir := filepath.Join(t.TempDir(), "vm-disks")

	if err := os.WriteFile(filepath.Join(imagesDir, "disk.img"), []byte("base image"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--disk-readonly=false",
		"--images-hostpath", imagesDir,
		"--disk-hostpath", diskDir,
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	)
	if err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	// No host-side overlay any more: the VM's writable disk is a qcow2
	// its own container creates against the shared image (see
	// config.PrepareOverlay), so konturctl writes nothing here and
	// -disk-hostpath has nothing left to configure.
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("konturctl created the disk-hostpath directory (err = %v), want it untouched", err)
	}

	manifest, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The manifest names the source image and asks for an overlay, rather
	// than pointing the guest at a host path this side had to prepare.
	if !strings.Contains(string(manifest), "/images/disk.img") {
		t.Errorf("manifest does not name the source disk image:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), "overlay") {
		t.Errorf("manifest does not ask for an overlay:\n%s", manifest)
	}
	if strings.Contains(string(manifest), "mountPath: /disk") {
		t.Errorf("manifest still mounts a host writable-disk directory:\n%s", manifest)
	}

	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "4", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	if _, stderr, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Fatalf("delete error = %v, stderr = %s", err, stderr)
	}
}

func TestRunVM_UnknownSubcommand(t *testing.T) {
	if _, _, err := runVMArgs(t, "frobnicate"); err == nil {
		t.Fatalf("runVM(frobnicate) = nil error, want one")
	}
}

func TestVMLifecycle_DockerBackend(t *testing.T) {
	withFakeDocker(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	// static-pod-only flags (like -static-pod-path) should have no
	// bearing on the docker backend: nothing gets written under podDir.
	podDir := filepath.Join(t.TempDir(), "manifests")

	_, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	)
	if err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}
	if entries, _ := os.ReadDir(podDir); len(entries) != 0 {
		t.Errorf("docker backend wrote to the static pod path: %v", entries)
	}

	saved, err := staticpod.Load(stateDir, "web")
	if err != nil {
		t.Fatalf("saved state not found: %v", err)
	}
	if saved.Backend != staticpod.BackendDocker {
		t.Errorf("saved Backend = %q, want %q", saved.Backend, staticpod.BackendDocker)
	}

	// create again should still fail, same as the static-pod backend.
	if _, _, err := runVMArgs(t, "create", "web", "--backend", "docker", "--disk", "x", "--ip", "169.254.100.2",
		"--port", "1", "--state-dir", stateDir); err == nil {
		t.Fatalf("create of existing docker-backend VM = nil error, want it to fail")
	}

	// update should carry the backend forward without repeating -backend.
	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "4", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	saved, err = staticpod.Load(stateDir, "web")
	if err != nil {
		t.Fatalf("saved state not found after update: %v", err)
	}
	if saved.Backend != staticpod.BackendDocker || saved.CPUs != 4 {
		t.Errorf("saved state after update = %+v, want Backend=docker, CPUs=4", saved)
	}

	if _, stderr, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Fatalf("delete error = %v, stderr = %s", err, stderr)
	}
	if _, err := staticpod.Load(stateDir, "web"); err == nil {
		t.Errorf("saved state still present after delete")
	}

	// delete again is a no-op, not an error (fakedocker's default
	// behaviour for "stop"/"rm" of names it hasn't been told are missing
	// is still success, but there's no saved state to find a backend
	// from either way -- runVMDelete must fall back sanely).
	if _, _, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Errorf("second delete error = %v, want nil (idempotent)", err)
	}
}

func TestVMLifecycle_FlatMode(t *testing.T) {
	withFakeDocker(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	_, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--net", "flat",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--docker-run-opt", "--network",
		"--docker-run-opt", "mynet",
		"--docker-run-opt", "-p",
		"--docker-run-opt", "8080:80",
		"--state-dir", stateDir,
	)
	if err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	saved, err := staticpod.Load(stateDir, "web")
	if err != nil {
		t.Fatalf("saved state not found: %v", err)
	}
	if !saved.IsFlat() {
		t.Errorf("saved NetMode = %q, want flat", saved.NetMode)
	}
	want := []string{"--network", "mynet", "-p", "8080:80"}
	if !reflect.DeepEqual(saved.DockerRunOpts, want) {
		t.Errorf("saved DockerRunOpts = %v, want %v", saved.DockerRunOpts, want)
	}

	// -ip is a NAT-mode setting; in flat mode the runtime assigns the
	// address, so passing one has to be rejected rather than ignored.
	if _, _, err := runVMArgs(t, "create", "other", "--backend", "docker", "--net", "flat",
		"--disk", "/images/disk.img", "--ip", "169.254.100.2", "--state-dir", stateDir); err == nil {
		t.Errorf("create with -net flat and -ip = nil error, want it rejected")
	}

	// An update carries the mode forward, and repeating -docker-run-opt
	// replaces the saved list rather than appending to it.
	if _, stderr, err := runVMArgs(t, "update", "web",
		"--docker-run-opt", "-p",
		"--docker-run-opt", "9090:80",
		"--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	saved, err = staticpod.Load(stateDir, "web")
	if err != nil {
		t.Fatalf("saved state not found after update: %v", err)
	}
	if !saved.IsFlat() {
		t.Errorf("saved NetMode after update = %q, want flat", saved.NetMode)
	}
	if want := []string{"-p", "9090:80"}; !reflect.DeepEqual(saved.DockerRunOpts, want) {
		t.Errorf("saved DockerRunOpts after update = %v, want %v (replaced, not appended)", saved.DockerRunOpts, want)
	}

	// A further update that omits -docker-run-opt entirely preserves the
	// previously saved list rather than clearing it: registerVMFlags
	// defaults the flag to the saved spec's own list, so "update" behaves
	// the same way for -docker-run-opt as it does for every other flag --
	// give it and it replaces, omit it and the saved value is kept.
	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "4", "--state-dir", stateDir); err != nil {
		t.Fatalf("update (no -docker-run-opt) error = %v, stderr = %s", err, stderr)
	}
	saved, err = staticpod.Load(stateDir, "web")
	if err != nil {
		t.Fatalf("saved state not found after second update: %v", err)
	}
	if want := []string{"-p", "9090:80"}; !reflect.DeepEqual(saved.DockerRunOpts, want) {
		t.Errorf("saved DockerRunOpts after update without -docker-run-opt = %v, want %v (preserved)", saved.DockerRunOpts, want)
	}
	if saved.CPUs != 4 {
		t.Errorf("saved CPUs after update without -docker-run-opt = %d, want 4", saved.CPUs)
	}

	if _, stderr, err := runVMArgs(t, "delete", "web", "--state-dir", stateDir); err != nil {
		t.Fatalf("delete error = %v, stderr = %s", err, stderr)
	}
}
