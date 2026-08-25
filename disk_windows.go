//go:build windows

package hfget

import (
	"math"

	"golang.org/x/sys/windows"
)

func platformAvailableSpace(path string) (int64, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeToCaller, &total, &totalFree); err != nil {
		return 0, err
	}
	if freeToCaller > uint64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(freeToCaller), nil
}
