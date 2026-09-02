package kontur

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Exists is how ensure() decides a slot's VM is already built, so it has
// to agree with what `konturctl vm create`/`vm delete` actually leave
// behind: one "<state-dir>/<name>.json" per VM, removed on delete.
func TestExistsFollowsTheStateFile(t *testing.T) {
	dir := t.TempDir()
	if Exists(dir, "sandbox-0") {
		t.Error("Exists() on an empty state directory = true, want false")
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Exists(dir, "sandbox-0") {
		t.Error("Exists() with the VM's state file present = false, want true")
	}
	if Exists(dir, "sandbox-1") {
		t.Error("Exists() for a different VM name = true, want false")
	}
}

func TestPodName(t *testing.T) {
	if got, want := PodName("sandbox-0"), "kontur-vm-sandbox-0"; got != want {
		t.Errorf("PodName() = %q, want %q", got, want)
	}
}

// writeFakeKonturctl installs a shell script named "konturctl" on PATH
// -- the operator-facing binary
// vm's Create/Delete actually exec, not the container-facing "kontur"
// binary bwsalmon/kontur's own cmd/kontur is a different program entirely
// (see the package doc comment) -- which appends every invocation's argv
// to argvLog (one line per call, space-joined) and exits with exitCode.
func writeFakeKonturctl(t *testing.T, argvLog string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %s
exit %d
`, argvLog, exitCode)
	path := filepath.Join(dir, "konturctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCreateRunsKonturVMCreate(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	writeFakeKonturctl(t, log, 0)

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
	writeFakeKonturctl(t, filepath.Join(t.TempDir(), "argv.log"), 1)

	if err := Create(context.Background(), "/var/lib/kontur/vms", "sandbox-0"); err == nil {
		t.Error("Create() with a failing kontur binary: got nil error, want one")
	}
}

func TestDeleteRunsKonturVMDelete(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	writeFakeKonturctl(t, log, 0)

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
