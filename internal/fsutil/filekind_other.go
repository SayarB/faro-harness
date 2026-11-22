//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
)

// IsIndexableFile reports whether info represents a regular, non-symlink file.
func IsIndexableFile(info os.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	mode := info.Mode()
	return mode&os.ModeSymlink == 0 && mode.IsRegular()
}

// IsIndexableDirEntry reports whether a WalkDir entry represents a regular, non-symlink file.
func IsIndexableDirEntry(d fs.DirEntry) bool {
	if d == nil || d.IsDir() {
		return false
	}
	t := d.Type()
	return t&fs.ModeSymlink == 0 && t.IsRegular()
}
