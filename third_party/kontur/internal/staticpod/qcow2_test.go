package staticpod

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteQcow2Overlay_PinsRawBackingFormat guards against a real
// regression: an earlier version of writeQcow2Overlay left the backing
// file's format unpinned, on the assumption that qcow2 readers fall back
// to probing it from content, the way qcow2 v2 always did. cloud-hypervisor
// doesn't -- given a raw backing file (every source disk image
// PrepareWritableDisk uses one for) and no pinned format, it trips its own
// backing-chain-depth guard instead ("Maximum disk nesting depth
// exceeded"). This uses qemu-img, a separate qcow2 implementation, as an
// independent check that the header extension is well-formed and actually
// pins the format, rather than re-deriving the same byte offsets this
// package's own hand-rolled writer used to get wrong.
func TestWriteQcow2Overlay_PinsRawBackingFormat(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}

	dir := t.TempDir()
	backing := filepath.Join(dir, "disk.img")
	// Content that isn't valid qcow2 or any other recognizable image
	// format: the point of pinning the format is that a reader shouldn't
	// need to guess from this.
	if err := os.WriteFile(backing, []byte("raw disk bytes, not a qcow2 header or anything else recognizable"), 0o644); err != nil {
		t.Fatal(err)
	}

	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := writeQcow2Overlay(overlay, backing, 4096); err != nil {
		t.Fatalf("writeQcow2Overlay() error = %v", err)
	}

	out, err := exec.Command("qemu-img", "info", "--output=json", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"backing-filename-format": "raw"`) {
		t.Errorf("qemu-img info did not report a pinned raw backing-filename-format:\n%s", out)
	}

	// qemu-img refuses to open an image whose backing format isn't
	// pinned without an explicit -U override, precisely because guessing
	// it from unrecognizable content is unsafe. Succeeding here without
	// -U confirms the format really is pinned, not just reported as such
	// by chance.
	if out, err := exec.Command("qemu-img", "check", overlay).CombinedOutput(); err != nil {
		t.Errorf("qemu-img check (no -U, relying on the pinned backing format): %v\n%s", err, out)
	}
}

// TestWriteQcow2Overlay_LargeVirtualDisk exercises writeQcow2Overlay at a
// virtual disk size representative of a real kontur disk image, rather
// than the 4096-byte one TestWriteQcow2Overlay_PinsRawBackingFormat uses.
// qcow2L1CoverageBytes is 512MiB, so a 4096-byte disk always takes the
// l1Size == 1 path; a real disk image is many times that, and a bug in
// the l1Size/l1TableClusters math (e.g. an off-by-one in the ceiling
// division, or writing the wrong header field) would only surface once
// l1Size actually grows past 1. The backing file is created sparse
// (os.Truncate, not written byte for byte) so this stays cheap despite
// the size.
func TestWriteQcow2Overlay_LargeVirtualDisk(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}

	dir := t.TempDir()
	backing := filepath.Join(dir, "disk.img")
	// 64GiB plus one sector, so the ceiling division has to round up
	// past an exact multiple of qcow2L1CoverageBytes rather than landing
	// on one by chance. (A whole extra byte, rather than a 512-byte
	// sector, would work for l1Size's own math too, but qemu-img rounds
	// a virtual-size query down to the nearest sector, which would then
	// make the "virtual-size" assertion below fail for the wrong
	// reason.)
	const sizeBytes = 64<<30 + 512
	f, err := os.Create(backing)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := writeQcow2Overlay(overlay, backing, sizeBytes); err != nil {
		t.Fatalf("writeQcow2Overlay() error = %v", err)
	}

	out, err := exec.Command("qemu-img", "info", "--output=json", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), fmt.Sprintf(`"virtual-size": %d`, int64(sizeBytes))) {
		t.Errorf("qemu-img info did not report virtual-size %d:\n%s", sizeBytes, out)
	}

	if out, err := exec.Command("qemu-img", "check", overlay).CombinedOutput(); err != nil {
		t.Errorf("qemu-img check on a large virtual disk: %v\n%s", err, out)
	}

	// Independently check the header's own l1_size field against the
	// formula writeQcow2Overlay is supposed to implement, rather than
	// relying solely on qemu-img accepting whatever it wrote.
	hdr, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(hdr) < 40 {
		t.Fatalf("overlay file too short to hold a qcow2 header: %d bytes", len(hdr))
	}
	gotL1Size := binary.BigEndian.Uint32(hdr[36:40])
	wantL1Size := uint32((int64(sizeBytes) + qcow2L1CoverageBytes - 1) / qcow2L1CoverageBytes)
	if gotL1Size != wantL1Size {
		t.Errorf("header l1_size = %d, want %d", gotL1Size, wantL1Size)
	}
}
