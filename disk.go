package hfget

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// ErrInsufficientSpace is returned when the destination filesystem does not
// have enough free space for the planned download.
var ErrInsufficientSpace = errors.New("insufficient disk space")

var (
	spaceLookupMu sync.Mutex
	spaceLookup   = platformAvailableSpace
)

// AvailableSpace returns the number of bytes available to the current user on
// the filesystem that contains path. If path does not exist, the nearest
// existing ancestor is used so the check can run before the destination is created.
func AvailableSpace(path string) (int64, error) {
	if path == "" {
		path = "."
	}
	path = existingAncestor(path)
	spaceLookupMu.Lock()
	fn := spaceLookup
	spaceLookupMu.Unlock()
	return fn(path)
}

// EnsureWritableSpace reports whether path has enough free space to execute
// plan, including temporary files used while merging multi-part downloads.
// If free space cannot be determined, the check is skipped (nil error) so
// exotic filesystems do not block downloads.
func EnsureWritableSpace(path string, plan *DownloadPlan, numConnections int) error {
	needed := requiredDownloadSpace(plan, numConnections)
	if needed <= 0 {
		return nil
	}
	avail, err := AvailableSpace(path)
	if err != nil {
		return nil
	}
	return checkSpace(path, avail, needed)
}

func checkSpace(path string, avail, needed int64) error {
	if needed <= 0 || avail >= needed {
		return nil
	}
	return fmt.Errorf("%w at %s: need %s, only %s available",
		ErrInsufficientSpace, path, formatBytes(needed), formatBytes(avail))
}

// requiredDownloadSpace is the bytes that must be free to complete plan.
// Multi-threaded LFS downloads keep chunks plus a merge temp (~one extra copy
// of the largest such file) on disk at peak.
func requiredDownloadSpace(plan *DownloadPlan, numConnections int) int64 {
	if plan == nil || plan.TotalDownloadSize <= 0 {
		return 0
	}
	needed := plan.TotalDownloadSize
	if numConnections < 1 {
		numConnections = 5
	}
	threshold := int64(numConnections) * 1024 * 1024
	var extra int64
	for _, f := range plan.FilesToDownload {
		if f.File.LFS.IsLFS && f.File.Size >= threshold && f.File.Size > extra {
			extra = f.File.Size
		}
	}
	if extra > math.MaxInt64-needed {
		return math.MaxInt64
	}
	return needed + extra
}

func existingAncestor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs
		}
		abs = parent
	}
}

func blocksToBytes(blocks, blockSize uint64) int64 {
	if blockSize == 0 {
		return 0
	}
	if blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64
	}
	return int64(blocks * blockSize)
}
