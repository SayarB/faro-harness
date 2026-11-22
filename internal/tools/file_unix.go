//go:build unix

package tools

import (
	"fmt"
	"os"
	"syscall"
)

// openRegularNoFollow opens a regular file without following a final symlink component
// and verifies via Fstat on the open descriptor that it is a regular file.
func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%q is not a regular file", path)
	}
	return f, info, nil
}
