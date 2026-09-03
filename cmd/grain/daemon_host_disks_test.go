package main

// Which filesystems the host status pane reports, and how the readings
// for them are folded together (grain/task-148).
//
// The pane used to show one "disk" figure, taken from -data-dir's own
// filesystem, on the assumption that everything a run writes lands
// there. It stopped being true once terraform/gcp gave sandboxes a
// volume of their own (task 134, #635): docker's data root -- the
// sandbox image and every kontur VM's qcow2 overlay -- and
// HostSandboxes' per-run checkouts moved onto a 100 GB disk the UI never
// looked at, while the store's 20 GB disk went on reading as healthy
// however full the other one got.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHostDisksReportsTheStoreTheSandboxRootAndDockersOwn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostDisks only ever has a docker data root to check on Linux")
	}

	dataDir, sandboxDir, dockerRoot := t.TempDir(), t.TempDir(), t.TempDir()
	disks := hostDisks(config{dataDir: dataDir, sandboxDir: sandboxDir, dockerRootDir: dockerRoot})

	want := []hostDisk{
		{holds: "store", path: dataDir},
		{holds: "sandboxes", path: sandboxDir},
		{holds: "docker", path: dockerRoot},
	}
	if len(disks) != len(want) {
		t.Fatalf("hostDisks = %v, want %v", disks, want)
	}
	for i, w := range want {
		if disks[i] != w {
			t.Errorf("hostDisks[%d] = %v, want %v", i, disks[i], w)
		}
	}
}

// A kontur deployment keeps no checkout on this host, so -sandbox-dir is
// allowed to be empty (daemon()'s own argument checks) -- and an empty
// path is not a filesystem to statfs.
func TestHostDisksOmitsAnUnsetSandboxDir(t *testing.T) {
	disks := hostDisks(config{dataDir: t.TempDir(), dockerRootDir: filepath.Join(t.TempDir(), "no-such-dir")})

	if len(disks) != 1 || disks[0].holds != "store" {
		t.Fatalf("hostDisks = %v, want the store's disk alone", disks)
	}
}

// Dropped rather than carried as a broken row: a containerised daemon is
// shown docker's socket and not the tree behind it, so dockerd names a
// path this process cannot stat -- and on terraform/gcp that path is the
// sandbox volume the entry above already reports through its bind mount,
// so nothing is lost by leaving it out.
func TestHostDisksOmitsADockerRootItCannotRead(t *testing.T) {
	dataDir, sandboxDir := t.TempDir(), t.TempDir()
	disks := hostDisks(config{dataDir: dataDir, sandboxDir: sandboxDir, dockerRootDir: "/mnt/grain-sandbox/docker-that-is-not-mounted-here"})

	for _, d := range disks {
		if d.holds == "docker" {
			t.Fatalf("hostDisks = %v, want no entry for a docker root this process cannot stat", disks)
		}
	}
	if len(disks) != 2 {
		t.Fatalf("hostDisks = %v, want the store and the sandbox root", disks)
	}
}

// The single-disk case: a developer's laptop, and any deployment without
// terraform/gcp's own separate sandbox volume. Three paths, one
// filesystem, one row -- naming all three, rather than three identical
// rows that would each claim to be a disk of its own.
func TestDiskUsageFoldsPathsThatShareAFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.DiskUsage only ever works on Linux")
	}

	root := t.TempDir()
	sandboxes := filepath.Join(root, "sandboxes")
	docker := filepath.Join(root, "docker")
	for _, dir := range []string{sandboxes, docker} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	got := diskUsage([]hostDisk{
		{holds: "store", path: root},
		{holds: "sandboxes", path: sandboxes},
		{holds: "docker", path: docker},
	})

	if len(got) != 1 {
		t.Fatalf("diskUsage = %v, want one entry for one filesystem", got)
	}
	if strings.Join(got[0].Holds, ",") != "store,sandboxes,docker" {
		t.Errorf("Holds = %v, want every path that shares the filesystem, in the order asked for", got[0].Holds)
	}
	// The first path onto a filesystem is the one shown, so the row reads
	// as the same thing an operator would run `df` against.
	if got[0].Path != root {
		t.Errorf("Path = %q, want %q", got[0].Path, root)
	}
	if got[0].TotalMB <= 0 || got[0].UsedMB > got[0].TotalMB {
		t.Errorf("usage = %d/%d MB, want a real df reading", got[0].UsedMB, got[0].TotalMB)
	}
	if got[0].Error != "" {
		t.Errorf("Error = %q, want none", got[0].Error)
	}
}

// One unreadable path costs its own row a figure and nothing else. The
// case this is really about is docker's data root on a containerised
// daemon: dockerd reports a host path (/mnt/grain-sandbox/docker) that
// scripts/setup.sh does not mount into the container, so statfs fails
// there while the store's and the sandbox root's readings are both fine.
func TestDiskUsageCarriesOneUnreadablePathWithoutLosingTheRest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sysstat.DiskUsage only ever works on Linux")
	}

	root := t.TempDir()
	missing := filepath.Join(root, "no-such-dir")

	got := diskUsage([]hostDisk{
		{holds: "store", path: root},
		{holds: "docker", path: missing},
	})

	if len(got) != 2 {
		t.Fatalf("diskUsage = %v, want an entry each", got)
	}
	if got[0].TotalMB <= 0 {
		t.Errorf("store TotalMB = %d, want a real df reading", got[0].TotalMB)
	}
	if got[1].Error == "" {
		t.Errorf("docker entry = %v, want the reason it has no figure", got[1])
	}
	if got[1].TotalMB != 0 || got[1].UsedMB != 0 {
		t.Errorf("docker usage = %d/%d MB, want 0/0 -- the pane's own \"no figure\"", got[1].UsedMB, got[1].TotalMB)
	}
	// Not folded into the store's row: an unreadable path has no
	// filesystem identity to compare, and claiming it shares one would
	// put a figure next to a path this process cannot even see.
	if len(got[1].Holds) != 1 || got[1].Holds[0] != "docker" {
		t.Errorf("docker Holds = %v, want just its own", got[1].Holds)
	}
}
