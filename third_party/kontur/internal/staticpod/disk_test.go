package staticpod

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writableSpec returns a spec whose writable disk overlay can be prepared
// entirely within t's temp dirs: imagesDir stands in for -images-hostpath
// (with a fake source disk image already in it) and diskDir for
// -disk-hostpath.
func writableSpec(t *testing.T) (s VMSpec, imagesDir, diskDir string) {
	t.Helper()
	imagesDir = t.TempDir()
	diskDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(imagesDir, "disk.img"), []byte("source disk contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	s = baseSpec()
	s.DiskReadOnly = false
	s.ImagesHostPath = imagesDir
	s.DiskHostPath = diskDir
	return s, imagesDir, diskDir
}

func TestPrepareWritableDisk_ReadOnlyIsNoOp(t *testing.T) {
	s := baseSpec() // DiskReadOnly true by default
	s.DiskHostPath = filepath.Join(t.TempDir(), "vm-disks")

	if err := PrepareWritableDisk(s); err != nil {
		t.Fatalf("PrepareWritableDisk() error = %v", err)
	}
	if _, err := os.Stat(s.DiskHostPath); !os.IsNotExist(err) {
		t.Errorf("PrepareWritableDisk() created %s for a read-only disk, want nothing created", s.DiskHostPath)
	}
}

func TestPrepareWritableDisk_CreatesQcow2OverlayBackedBySource(t *testing.T) {
	s, _, _ := writableSpec(t)

	if err := PrepareWritableDisk(s); err != nil {
		t.Fatalf("PrepareWritableDisk() error = %v", err)
	}

	dest := filepath.Join(s.WritableDiskDir(), "disk.qcow2")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading qcow2 overlay: %v", err)
	}
	if len(data) < qcow2HeaderSize {
		t.Fatalf("qcow2 overlay is only %d bytes, want at least a full header", len(data))
	}

	if magic := binary.BigEndian.Uint32(data[0:4]); magic != qcow2Magic {
		t.Errorf("qcow2 magic = %#x, want %#x", magic, qcow2Magic)
	}
	if version := binary.BigEndian.Uint32(data[4:8]); version != qcow2Version {
		t.Errorf("qcow2 version = %d, want %d", version, qcow2Version)
	}
	if size := binary.BigEndian.Uint64(data[24:32]); size != uint64(len("source disk contents")) {
		t.Errorf("qcow2 virtual size = %d, want %d", size, len("source disk contents"))
	}

	backingOffset := binary.BigEndian.Uint64(data[8:16])
	backingSize := binary.BigEndian.Uint32(data[16:20])
	backingFile := string(data[backingOffset : backingOffset+uint64(backingSize)])
	if backingFile != s.DiskImage {
		t.Errorf("qcow2 backing file = %q, want %q (the container-visible path, not the host path)", backingFile, s.DiskImage)
	}
}

func TestPrepareWritableDisk_IdempotentPreservesExistingOverlay(t *testing.T) {
	s, _, _ := writableSpec(t)

	if err := PrepareWritableDisk(s); err != nil {
		t.Fatalf("first PrepareWritableDisk() error = %v", err)
	}

	dest := filepath.Join(s.WritableDiskDir(), "disk.qcow2")
	if err := os.WriteFile(dest, []byte("guest wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PrepareWritableDisk(s); err != nil {
		t.Fatalf("second PrepareWritableDisk() error = %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "guest wrote this" {
		t.Errorf("second PrepareWritableDisk() clobbered the existing overlay: got %q", got)
	}
}

func TestPrepareWritableDisk_MissingSourceIsAnError(t *testing.T) {
	s, imagesDir, _ := writableSpec(t)
	if err := os.Remove(filepath.Join(imagesDir, "disk.img")); err != nil {
		t.Fatal(err)
	}

	if err := PrepareWritableDisk(s); err == nil {
		t.Fatalf("PrepareWritableDisk() = nil, want an error for a missing source image")
	}
}
