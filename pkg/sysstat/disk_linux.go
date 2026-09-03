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
