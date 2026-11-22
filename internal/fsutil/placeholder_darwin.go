//go:build darwin

package fsutil

import (
	"os"
	"syscall"
)

// sfDataless is macOS SF_DATALESS (0x40000000): file is an evicted iCloud / FileProvider placeholder.
const sfDataless uint32 = 0x40000000

// IsCloudPlaceholder reports whether path is an offline/evicted cloud placeholder
// that should NOT be opened for content reading.
func IsCloudPlaceholder(path string, info os.FileInfo) bool {
	if IsICloudStubName(path) {
		return true
	}
	if info == nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return false
	}
	return (st.Flags & sfDataless) != 0
}
