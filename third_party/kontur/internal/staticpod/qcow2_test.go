package staticpod

import (
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
