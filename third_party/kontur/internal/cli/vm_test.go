package cli

import (
	"bytes"
	"context"
	"io"
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
	return runVMStdin(t, strings.NewReader(""), args...)
}

// runVMStdin is runVMArgs for the subcommands that have a stdin: "vm
// exec" and friends proxy it to the guest command.
func runVMStdin(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = runVM(context.Background(), args, stdin, &o, &e)
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
	if _, _, err := runVMArgs(t, "create", "web", "--disk", "x",
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

	// update cpus only; disk and kernel should be preserved
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

// TestVMCreate_DeprecatedNetFlags covers the flags the NAT mode left
// behind: a caller's existing "vm create" line keeps working (the values
// simply configure nothing now), except for -net nat, which asks for a
// mode that no longer exists and so has to be refused rather than
// silently reinterpreted.
func TestVMCreate_DeprecatedNetFlags(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--ip", "169.254.100.2",
		"--port", "30080",
		"--guest-port", "8080",
		"--bridge-cidr", "169.254.100.1/24",
		"--net", "flat",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create with deprecated flags error = %v, stderr = %s", err, stderr)
	}

	data, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "169.254.100.2::") || strings.Contains(string(data), "30080") {
		t.Errorf("deprecated flag values reached the manifest:\n%s", data)
	}

	if _, _, err := runVMArgs(t, "create", "other", "--disk", "/images/disk.img", "--net", "nat",
		"--state-dir", stateDir, "--static-pod-path", podDir); err == nil {
		t.Errorf("create with -net nat = nil error, want it refused rather than silently ignored")
	}
}

func TestVMUpdate_RecomputesAutoCmdlineOnDiskModeChange(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	if _, stderr, err := runVMArgs(t, "update", "web", "--disk-mode", "overlay", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}

	data, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "root=/dev/vda rw") {
		t.Errorf("cmdline was not recomputed for the new disk mode:\n%s", data)
	}
	if strings.Contains(string(data), "root=/dev/vda ro") {
		t.Errorf("stale read-only root still present after update:\n%s", data)
	}
}

// TestVMCreate_DefaultDiskModeIsBootable pins the default a VM gets when
// no disk flag is passed at all. It used to be read-only, which the
// reference guest never finishes booting from, so a create with defaults
// produced a VM that could not come up.
func TestVMCreate_DefaultDiskModeIsBootable(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	data, err := os.ReadFile(filepath.Join(podDir, "kontur-vm-web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "CHV_DISK_MODE") || !strings.Contains(string(data), `value: "overlay"`) {
		t.Errorf("manifest does not ask for an overlay disk:\n%s", data)
	}
	if !strings.Contains(string(data), "root=/dev/vda rw") {
		t.Errorf("manifest boots a read-only root:\n%s", data)
	}
}

// TestVMDiskReadOnly_StillSelectsReadOnly covers the deprecated flag now
// that -disk-mode has a real default rather than an empty one: passing
// only -disk-readonly has to keep deciding the mode, on create and on
// update alike, or the new default would silently override every caller
// still using it.
func TestVMDiskReadOnly_StillSelectsReadOnly(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")
	manifest := filepath.Join(podDir, "kontur-vm-web.yaml")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--disk-readonly=true",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `value: "readonly"`) {
		t.Errorf("-disk-readonly=true did not ask for a read-only disk:\n%s", data)
	}

	// And back again, from the same flag on an update: the saved spec
	// now records "readonly" as an explicit mode, so this only works if
	// the flag the caller passed still wins over the saved value.
	if _, stderr, err := runVMArgs(t, "update", "web",
		"--disk-readonly=false",
		"--state-dir", stateDir,
	); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	data, err = os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `value: "overlay"`) {
		t.Errorf("-disk-readonly=false did not ask for an overlay disk:\n%s", data)
	}
}

func TestVMUpdate_ExplicitCmdlineSurvivesLaterUpdates(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--kernel", "/images/vmlinux",
		"--cmdline", "console=ttyS0 root=/dev/vda ro custom=1",
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
	if _, _, err := runVMArgs(t, "create", "web", "--backend", "docker", "--disk", "x",
		"--state-dir", stateDir); err == nil {
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

// TestVMCreate_ImageDefaultFollowsTheBackend covers the other default a
// docker-backend caller used to have to override: the image. Containerd
// pulls the static-pod backend's image by reference, so that one has to
// name the node-local registry -- but a docker daemon already holds the
// image "docker build -t kontur:latest ." made, and naming a registry
// that only exists after "konturctl setup" gave it nothing to pull.
func TestVMCreate_ImageDefaultFollowsTheBackend(t *testing.T) {
	withFakeDocker(t)

	dockerState := filepath.Join(t.TempDir(), "state")
	if _, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--state-dir", dockerState,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}
	saved, err := staticpod.Load(dockerState, "web")
	if err != nil {
		t.Fatalf("saved state not found: %v", err)
	}
	if saved.KonturImage != staticpod.DockerImage {
		t.Errorf("KonturImage = %q, want the locally built %q", saved.KonturImage, staticpod.DockerImage)
	}

	// The static-pod backend keeps the registry reference: its images are
	// resolved by containerd, which cannot see a local docker daemon's.
	podState := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")
	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--state-dir", podState,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}
	saved, err = staticpod.Load(podState, "web")
	if err != nil {
		t.Fatalf("saved state not found: %v", err)
	}
	if saved.KonturImage != staticpod.StaticPodImage {
		t.Errorf("KonturImage = %q, want %q", saved.KonturImage, staticpod.StaticPodImage)
	}

	// An explicit -kontur-image still wins under either backend.
	namedState := filepath.Join(t.TempDir(), "state")
	if _, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--kontur-image", "kontur:custom",
		"--state-dir", namedState,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}
	saved, err = staticpod.Load(namedState, "web")
	if err != nil {
		t.Fatalf("saved state not found: %v", err)
	}
	if saved.KonturImage != "kontur:custom" {
		t.Errorf("KonturImage = %q, want the explicitly named kontur:custom", saved.KonturImage)
	}
}

// TestVMCreate_UnwritableStateDirFailsBeforeAnythingStarts is the order
// that used to be wrong: the state directory was only written after the
// containers had been started, so an unwritable one -- which the default
// /var/lib/kontur/vms is for anyone who isn't root -- left a VM running
// with no saved state to find it by again.
func TestVMCreate_UnwritableStateDirFailsBeforeAnythingStarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write into any directory, so there is nothing to refuse here")
	}
	withFakeDocker(t)
	calls := callLog(t)

	readonly := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(readonly, "vms")

	_, _, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--state-dir", stateDir,
	)
	if err == nil {
		t.Fatal("create into an unwritable state directory = nil error, want one")
	}
	if !strings.Contains(err.Error(), stateDir) || !strings.Contains(err.Error(), "-state-dir") {
		t.Errorf("error = %v, want it to name %q and the flag that fixes it", err, stateDir)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("docker was called before the state directory was checked: %v", got)
	}
}

func TestVMLifecycle_DockerRunOpts(t *testing.T) {
	withFakeDocker(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	_, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
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
	want := []string{"--network", "mynet", "-p", "8080:80"}
	if !reflect.DeepEqual(saved.DockerRunOpts, want) {
		t.Errorf("saved DockerRunOpts = %v, want %v", saved.DockerRunOpts, want)
	}

	// Repeating -docker-run-opt replaces the saved list rather than
	// appending to it.
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

// TestVMLifecycle_DiskSize covers -disk-size-mb the way every other
// per-VM setting is covered: it reaches the rendered manifest, it
// survives an update that doesn't mention it, a later update can raise
// it, and "list" reports it.
func TestVMLifecycle_DiskSize(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--disk-mode", "overlay",
		"--disk-size-mb", "8192",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	manifestPath := filepath.Join(podDir, "kontur-vm-web.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if !strings.Contains(string(data), "CHV_DISK_SIZE_MB") || !strings.Contains(string(data), `value: "8192"`) {
		t.Errorf("manifest missing CHV_DISK_SIZE_MB=8192:\n%s", data)
	}

	out, _, err := runVMArgs(t, "list", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	if !strings.Contains(out, "DISK_MB") || !strings.Contains(out, "8192") {
		t.Errorf("list output missing the VM's disk size:\n%s", out)
	}

	// An update about something else keeps it, like every other flag.
	if _, stderr, err := runVMArgs(t, "update", "web", "--cpus", "4", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	data, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not re-written: %v", err)
	}
	if !strings.Contains(string(data), `value: "8192"`) {
		t.Errorf("update lost the preserved -disk-size-mb value:\n%s", data)
	}

	// Raising it is how a VM's disk grows: the VM container resizes its
	// overlay on the next boot (see config.PrepareOverlay).
	if _, stderr, err := runVMArgs(t, "update", "web", "--disk-size-mb", "16384", "--state-dir", stateDir); err != nil {
		t.Fatalf("update error = %v, stderr = %s", err, stderr)
	}
	data, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest not re-written: %v", err)
	}
	if !strings.Contains(string(data), `value: "16384"`) {
		t.Errorf("manifest missing the raised disk size:\n%s", data)
	}
}

func TestVMCreate_DiskSizeRejectedWithoutOverlayMode(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	_, _, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--disk-mode", "persistent",
		"--disk-size-mb", "8192",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	)
	if err == nil {
		t.Fatal("create = nil error, want -disk-size-mb rejected outside overlay mode")
	}
	if _, statErr := os.Stat(filepath.Join(podDir, "kontur-vm-web.yaml")); !os.IsNotExist(statErr) {
		t.Error("a manifest was written despite the rejected flags")
	}
}
