//go:build !darwin && !windows

package fsutil

import "os"

// IsCloudPlaceholder reports whether path is an offline/evicted cloud placeholder.
// On Linux/other platforms only explicit ".name.icloud" stub names are flagged.
func IsCloudPlaceholder(path string, _ os.FileInfo) bool {
	return IsICloudStubName(path)
}
