// Package fsutil provides cross-platform filesystem filtering and cloud-placeholder detection.
package fsutil

import (
	"path/filepath"
	"strings"
)

var ignoredDirs = map[string]struct{}{
	".git":         {},
	".hg":          {},
	".svn":         {},
	"node_modules": {},
	".trash":       {},
	"$recycle.bin": {},
	"__pycache__":  {},
	".venv":        {},
	"venv":         {},
	".cache":       {},
	".idea":        {},
	".vscode":      {},
}

var ignoredExactFiles = map[string]struct{}{
	".ds_store":   {},
	"thumbs.db":   {},
	"desktop.ini": {},
	".localized":  {},
}

var ignoredSuffixes = []string{
	".tmp",
	".temp",
	".part",
	".crdownload",
	".download",
	".swp",
	".swo",
	".swx",
	".bak",
	"~",
}

// IsIgnoredDir reports whether a directory name should be skipped during walks and watches.
func IsIgnoredDir(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if _, ok := ignoredDirs[base]; ok {
		return true
	}
	return false
}

// IsIgnoredFile reports whether a file path is transient OS/editor/download noise
// that should not be indexed.
func IsIgnoredFile(path string) bool {
	base := filepath.Base(path)
	lower := strings.ToLower(base)

	if _, ok := ignoredExactFiles[lower]; ok {
		return true
	}
	// Microsoft Office and LibreOffice lock/temp files.
	if strings.HasPrefix(base, "~$") || strings.HasPrefix(base, ".~lock.") {
		return true
	}
	// macOS AppleDouble resource forks.
	if strings.HasPrefix(base, "._") {
		return true
	}
	for _, suffix := range ignoredSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// IsICloudStubName reports whether a filename matches an evicted macOS iCloud placeholder stub
// (e.g. ".Report.pdf.icloud").
func IsICloudStubName(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, ".") && strings.HasSuffix(strings.ToLower(base), ".icloud")
}
