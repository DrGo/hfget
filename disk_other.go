//go:build !unix && !windows

package hfget

import "fmt"

func platformAvailableSpace(path string) (int64, error) {
	return 0, fmt.Errorf("available disk space is not supported on this platform")
}
