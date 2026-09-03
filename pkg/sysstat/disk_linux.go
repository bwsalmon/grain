//go:build linux

package sysstat

import (
	"fmt"
	"syscall"
)

// DiskUsage reports the size and usage, in MiB, of the filesystem that
// holds path -- the same two numbers `df` prints for it, read with one
// statfs(2) rather than by shelling out.
//
// Used is total minus *free*, not total minus available: free counts the
// reserved blocks only root may allocate, and total-minus-free is what
// `df`'s own "Used" column reports. The daemon's own host disk filling
// up is a deployment-wide failure (every run's sandbox overlay is
// allocated out of it), so the number worth showing is the one an
// operator will see if they run `df` themselves.
//
// path is a path, not a device: statfs resolves it to whichever
// filesystem it falls on, so passing the daemon's -data-dir reports the
// disk that actually fills, wherever it happens to be mounted.
func DiskUsage(path string) (totalMB, usedMB int, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("sysstat: statfs %s: %w", path, err)
	}
	// Bsize is the block size every count below is in; f_blocks/f_bfree
	// are counts of those blocks, so both have to be scaled by it before
	// they mean anything in bytes.
	const mib = 1024 * 1024
	blockSize := uint64(st.Bsize)
	totalBytes := st.Blocks * blockSize
	usedBytes := (st.Blocks - st.Bfree) * blockSize
	return int(totalBytes / mib), int(usedBytes / mib), nil
}

// FilesystemID identifies the filesystem that holds path: st_dev from
// stat(2), the same number `du -x` and `find -xdev` compare to decide
// they have crossed onto another disk.
//
// It exists so a caller asking DiskUsage about several paths can tell
// which of them are one filesystem answering twice. grain's own
// -data-dir, -sandbox-dir and docker's data root are three separate
// volumes on one deployment and three names for one filesystem on the
// next, and only the kernel knows which (cmd/grain's own hostDisks). A
// path comparison cannot stand in for this, because the case that
// matters is a bind mount: terraform/gcp's /var/lib/grain-sandbox and
// /mnt/grain-sandbox/sandboxes share every block and no prefix.
//
// The value is opaque and worth nothing on its own -- it is only
// meaningful compared against another reading taken on this same machine
// at this same moment, and never worth storing.
func FilesystemID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("sysstat: stat %s: %w", path, err)
	}
	return uint64(st.Dev), nil
}
