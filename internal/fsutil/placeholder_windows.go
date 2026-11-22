//go:build windows

package fsutil

import (
	"os"
	"syscall"
)

const (
	fileAttrOffline            uint32 = 0x00001000
	fileAttrRecallOnOpen       uint32 = 0x00040000
	fileAttrRecallOnDataAccess uint32 = 0x00400000
)

// IsCloudPlaceholder reports whether path is an offline/evicted cloud placeholder
// (OneDrive Files On-Demand, Dropbox Smart Sync, iCloud Drive for Windows) that
// should NOT be opened for content reading.
func IsCloudPlaceholder(path string, info os.FileInfo) bool {
	if IsICloudStubName(path) {
		return true
	}
	if info != nil {
		if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && data != nil {
			const mask = fileAttrOffline | fileAttrRecallOnOpen | fileAttrRecallOnDataAccess
			if (data.FileAttributes & mask) != 0 {
				return true
			}
		}
	}
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := syscall.GetFileAttributes(ptr)
	if err != nil {
		return false
	}
	const mask = fileAttrOffline | fileAttrRecallOnOpen | fileAttrRecallOnDataAccess
	return (attrs & mask) != 0
}
