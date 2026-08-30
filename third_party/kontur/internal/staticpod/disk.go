package staticpod

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareWritableDisk makes s's own writable qcow2 overlay exist on the
// host before its container starts, when s.DiskReadOnly is false.
// ImagesMountPath is always mounted read-only (see its doc comment: it's
// a shared node-local image cache several VMs may read from
// concurrently, so making it writable would let one VM corrupt another's
// backing file), so a genuinely writable root filesystem instead gets a
// small qcow2 overlay of its own under WritableDiskDir, a directory only
// this VM's container ever mounts, read-write, with the source image as
// its qcow2 backing file: the guest's writes land in the overlay as new
// qcow2 clusters, while unwritten regions still read straight through to
// the shared source image.
//
// This is far cheaper than the byte-for-byte (or copy-on-write clone)
// copy this function used to make: creating the overlay costs a fixed
// few hundred KiB regardless of the source image's size, since nothing
// is copied into it up front.
//
// It's a no-op if s.DiskReadOnly is true, and idempotent once the
// overlay exists: a later "vm update" or container restart that calls
// this again finds the overlay already there and leaves it alone, rather
// than discarding whatever the guest has since written to it.
func PrepareWritableDisk(s VMSpec) error {
	if s.DiskReadOnly {
		return nil
	}

	src := filepath.Join(s.ImagesHostPath, strings.TrimPrefix(s.DiskImage, ImagesMountPath+"/"))
	destDir := s.WritableDiskDir()
	dest := filepath.Join(destDir, writableDiskFileName)

	if _, err := os.Stat(dest); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for existing writable disk overlay %s: %w", dest, err)
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("statting source disk %s: %w", src, err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating writable disk directory %s: %w", destDir, err)
	}
	// s.DiskImage (rather than src) is what's written into the overlay
	// as its backing file path: cloud-hypervisor opens that path itself,
	// inside the VM container, where it's s.DiskImage -- the
	// container-visible path under ImagesMountPath -- that resolves to
	// the source image, not src (its host-visible path).
	if err := writeQcow2Overlay(dest, s.DiskImage, info.Size()); err != nil {
		return fmt.Errorf("creating qcow2 overlay %s backed by %s: %w", dest, s.DiskImage, err)
	}
	return nil
}
