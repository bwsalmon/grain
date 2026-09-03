package qcow2

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
// regression: an earlier version of WriteOverlay left the backing
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
	if err := WriteOverlay(overlay, backing, 4096); err != nil {
		t.Fatalf("WriteOverlay() error = %v", err)
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

// TestWriteQcow2Overlay_LargeVirtualDisk exercises WriteOverlay at a
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
	if err := WriteOverlay(overlay, backing, sizeBytes); err != nil {
		t.Fatalf("WriteOverlay() error = %v", err)
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
	// formula WriteOverlay is supposed to implement, rather than
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

// newOverlay writes a fresh overlay of the given virtual size over a
// sparse backing file of the same size, and returns both paths.
func newOverlay(t *testing.T, sizeBytes int64) (overlay, backing string) {
	t.Helper()
	dir := t.TempDir()
	backing = filepath.Join(dir, "disk.img")
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
	overlay = filepath.Join(dir, "overlay.qcow2")
	if err := WriteOverlay(overlay, backing, sizeBytes); err != nil {
		t.Fatalf("WriteOverlay() error = %v", err)
	}
	return overlay, backing
}

// TestResize_ChangesNothingButTheTwoHeaderFields is the whole reason a
// resize is safe to do on an existing overlay on every boot: growing the
// virtual disk allocates no clusters and rewrites no data, so everything
// the guest has already written is exactly where it was. Comparing the
// file byte for byte is a stricter statement of that than reading the
// header back would be.
func TestResize_ChangesNothingButTheTwoHeaderFields(t *testing.T) {
	const (
		startBytes = 1 << 30 // 1GiB: l1_size 2, so growing it changes
		grownBytes = 8 << 30 // 8GiB: l1_size 16, without needing a second L1 cluster
	)
	overlay, _ := newOverlay(t, startBytes)

	before, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := Resize(overlay, grownBytes); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	after, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("file length = %d after resize, want %d unchanged", len(after), len(before))
	}

	// size (24:32) and l1_size (36:40) are the two fields a grow is
	// allowed to touch; every other byte, L1 table included, must be
	// untouched.
	for i := range before {
		if (i >= 24 && i < 32) || (i >= 36 && i < 40) {
			continue
		}
		if after[i] != before[i] {
			t.Fatalf("byte %d changed from %#x to %#x: resize rewrote something other than the header's size fields", i, before[i], after[i])
		}
	}

	if got := int64(binary.BigEndian.Uint64(after[24:32])); got != grownBytes {
		t.Errorf("header size = %d, want %d", got, grownBytes)
	}
	wantL1 := uint32((int64(grownBytes) + qcow2L1CoverageBytes - 1) / qcow2L1CoverageBytes)
	if got := binary.BigEndian.Uint32(after[36:40]); got != wantL1 {
		t.Errorf("header l1_size = %d, want %d", got, wantL1)
	}

	size, err := VirtualSize(overlay)
	if err != nil {
		t.Fatalf("VirtualSize() error = %v", err)
	}
	if size != grownBytes {
		t.Errorf("VirtualSize() = %d, want %d", size, grownBytes)
	}
}

// TestResize_SameSizeAndSmaller covers the two calls PrepareOverlay makes
// on a VM whose disk size hasn't changed and on one whose has been turned
// down: the first is a no-op (it happens on every boot), and the second
// must fail rather than cut clusters -- and a guest filesystem -- off at
// the new end.
func TestResize_SameSizeAndSmaller(t *testing.T) {
	const sizeBytes = 1 << 30
	overlay, _ := newOverlay(t, sizeBytes)

	before, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := Resize(overlay, sizeBytes); err != nil {
		t.Fatalf("Resize() to the current size error = %v, want no-op", err)
	}
	after, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("Resize() to the current size rewrote the image")
	}

	err = Resize(overlay, sizeBytes/2)
	if err == nil {
		t.Fatal("Resize() to a smaller size succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "shrinking") {
		t.Errorf("Resize() error = %v, want it to say shrinking is unsupported", err)
	}
	if size, err := VirtualSize(overlay); err != nil || size != sizeBytes {
		t.Errorf("VirtualSize() = %d, %v after a rejected shrink, want %d unchanged", size, err, int64(sizeBytes))
	}
}

// TestResize_RefusesGrowthBeyondTheAllocatedL1Table pins the one growth
// Resize deliberately doesn't do: moving the L1 table to a larger
// allocation. One 64KiB cluster of L1 entries covers 4TiB, so this only
// bites well past any disk a caller asks for -- but it has to fail loudly
// rather than write an l1_size the image has no room for.
func TestResize_RefusesGrowthBeyondTheAllocatedL1Table(t *testing.T) {
	overlay, _ := newOverlay(t, 1<<20)

	const beyondOneL1Cluster = qcow2L1CoverageBytes*qcow2L2EntriesPerTable + 1 // just over 4TiB
	err := Resize(overlay, beyondOneL1Cluster)
	if err == nil {
		t.Fatal("Resize() past a single L1 table cluster succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "L1 table cluster") {
		t.Errorf("Resize() error = %v, want it to name the L1 table as the limit", err)
	}
	if size, err := VirtualSize(overlay); err != nil || size != 1<<20 {
		t.Errorf("VirtualSize() = %d, %v after a rejected resize, want %d unchanged", size, err, 1<<20)
	}
}

func TestResize_RejectsSomethingThatIsNotAQcow2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, []byte("raw disk bytes, not a qcow2 header"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Resize(path, 1<<30); err == nil {
		t.Fatal("Resize() on a raw image succeeded, want an error")
	}
}

// TestResize_QemuImgAgrees checks the resized image against a separate
// qcow2 implementation, the same way TestWriteQcow2Overlay_* do: a header
// this package is happy to read back is no evidence that a real qcow2
// reader (cloud-hypervisor's, in production) accepts it.
func TestResize_QemuImgAgrees(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	const grownBytes = 16 << 30
	overlay, _ := newOverlay(t, 1<<30)

	if err := Resize(overlay, grownBytes); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	out, err := exec.Command("qemu-img", "info", "--output=json", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), fmt.Sprintf(`"virtual-size": %d`, int64(grownBytes))) {
		t.Errorf("qemu-img info did not report virtual-size %d after the resize:\n%s", int64(grownBytes), out)
	}
	if out, err := exec.Command("qemu-img", "check", overlay).CombinedOutput(); err != nil {
		t.Errorf("qemu-img check on a resized overlay: %v\n%s", err, out)
	}
}

// TestResize_KeepsWhatTheGuestWrote is the same promise as
// TestResize_ChangesNothingButTheTwoHeaderFields, made against an overlay
// that actually has allocated clusters in it (qemu-io writing through the
// qcow2 layer, as a guest's writes do) rather than a freshly created one:
// a resize that relocated or renumbered anything would lose them, and
// growing a VM's disk on its next boot must not cost it its data.
func TestResize_KeepsWhatTheGuestWrote(t *testing.T) {
	if _, err := exec.LookPath("qemu-io"); err != nil {
		t.Skip("qemu-io not available")
	}
	overlay, _ := newOverlay(t, 1<<30)

	// Two writes far apart in the virtual disk, so they land under
	// different L1 entries (one L1 entry covers 512MiB) and the L1 table
	// really is in use rather than empty.
	const pattern = "0xab"
	for _, offset := range []string{"0", "600M"} {
		if out, err := exec.Command("qemu-io", "-c", "write -P "+pattern+" "+offset+" 64k", overlay).CombinedOutput(); err != nil {
			t.Fatalf("qemu-io write at %s: %v\n%s", offset, err, out)
		}
	}

	if err := Resize(overlay, 8<<30); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	for _, offset := range []string{"0", "600M"} {
		out, err := exec.Command("qemu-io", "-c", "read -P "+pattern+" "+offset+" 64k", overlay).CombinedOutput()
		if err != nil {
			t.Fatalf("qemu-io read at %s after resize: %v\n%s", offset, err, out)
		}
	}
	if out, err := exec.Command("qemu-img", "check", overlay).CombinedOutput(); err != nil {
		t.Errorf("qemu-img check after writing and resizing: %v\n%s", err, out)
	}
}
