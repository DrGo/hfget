//go:build unix

package hfget

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func platformAvailableSpace(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	bsize := int64(stat.Bsize)
	if bsize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size %d for %s", stat.Bsize, path)
	}
	// Bavail is space available to an unprivileged writer (excludes root-reserved blocks).
	return blocksToBytes(uint64(stat.Bavail), uint64(bsize)), nil
}
