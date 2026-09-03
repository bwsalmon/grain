//go:build !linux

package sysstat

import "fmt"

// DiskUsage is the non-Linux half of the same split pkg/procgroup uses:
// this package's readings are Linux kernel interfaces (see the package
// doc comment), and a build for anything else keeps compiling and
// reports the same "no reading to give" every other optional pane
// already has a shape for, rather than needing a syscall this file's
// platform may spell differently.
func DiskUsage(path string) (totalMB, usedMB int, err error) {
	return 0, 0, fmt.Errorf("sysstat: disk usage is only available on Linux")
}

// FilesystemID is the non-Linux half of the same split, and says the same
// thing DiskUsage above does: a caller with no usage figure to report has
// nothing to fold two of them together over either.
func FilesystemID(path string) (uint64, error) {
	return 0, fmt.Errorf("sysstat: filesystem identity is only available on Linux")
}
