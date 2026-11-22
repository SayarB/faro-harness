//go:build windows

package fsutil

import (
	"io/fs"
	"os"
	"syscall"
)

const fileAttrDirectory uint32 = 0x00000010

// IsIndexableFile reports whether info represents an indexable file (including Windows
// Cloud Files API reparse points such as OneDrive/Dropbox files, which Go's stdlib
// classifies as ModeIrregular rather than ModeRegular). Directories and symlinks return false.
func IsIndexableFile(info os.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return false
	}
	if mode.IsRegular() {
		return true
	}
	// On Windows, non-symlink cloud reparse points (IO_REPARSE_TAG_CLOUD_*) have ModeIrregular set.
	if mode&os.ModeIrregular != 0 {
		if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && data != nil {
			return (data.FileAttributes & fileAttrDirectory) == 0
		}
		return true
	}
	return false
}

// IsIndexableDirEntry reports whether a WalkDir entry should be passed to the indexer.
// On Windows, ModeIrregular entries (cloud reparse points) are allowed through so
// Lstat + IsCloudPlaceholder can inspect them accurately.
func IsIndexableDirEntry(d fs.DirEntry) bool {
	if d == nil || d.IsDir() {
		return false
	}
	t := d.Type()
	if t&fs.ModeSymlink != 0 {
		return false
	}
	return t.IsRegular() || t&fs.ModeIrregular != 0
}
