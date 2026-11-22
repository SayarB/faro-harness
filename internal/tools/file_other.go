//go:build !unix

package tools

import (
	"fmt"
	"os"
)

// openRegularNoFollow checks Lstat, opens the file, and verifies via Fstat+SameFile
// that the opened handle matches the non-symlink regular file.
func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	finfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%q changed while opening", path)
	}
	return f, finfo, nil
}
