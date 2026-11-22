// Package peek extracts fast bounded headers and metadata cards from local files.
package peek

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/SayarB/faro-harness/internal/store"
)

const (
	// MaxFullTextBytes is the threshold under which plain-text/Markdown files are indexed as "full".
	MaxFullTextBytes = 256 * 1024 // 256 KB
	// MaxHeaderTextBytes is the cap for truncated body previews.
	MaxHeaderTextBytes = 64 * 1024 // 64 KB
	// MaxXMLPartBytes bounds uncompressed XML reads inside OOXML containers (zip-bomb guard).
	MaxXMLPartBytes = 2 * 1024 * 1024 // 2 MB
	// MaxZipEntries caps how many entry paths are listed from a .zip central directory.
	MaxZipEntries = 200
	// MaxPPTXSlides caps how many slides are peeked upfront in a .pptx deck.
	MaxPPTXSlides = 30
	// MaxCSVSampleRows caps data rows included in a CSV preview.
	MaxCSVSampleRows = 25
	// MaxXLSXSampleRows caps rows peeked from sheet 1 of an .xlsx workbook.
	MaxXLSXSampleRows = 12
)

// Result contains the bounded card extracted by a peeker.
type Result struct {
	PeekerVersion int
	IndexDepth    string
	IndexStatus   string
	Title         string
	Headings      string
	BodyPreview   string
	OutlineJSON   string
	UnitCount     int
	ErrorMsg      string
}

var textExts = map[string]struct{}{
	".txt":  {},
	".log":  {},
	".json": {},
	".yaml": {},
	".yml":  {},
	".toml": {},
	".xml":  {},
	".ini":  {},
	".conf": {},
	".sql":  {},
	".go":   {},
	".py":   {},
	".ts":   {},
	".tsx":  {},
	".js":   {},
	".jsx":  {},
	".html": {},
	".css":  {},
	".sh":   {},
	".rst":  {},
	".org":  {},
	".tex":  {},
}

// PeekerIDForExt returns the canonical peeker family identifier for a file extension,
// or "" if no content peeker is registered.
func PeekerIDForExt(ext string) string {
	ext = strings.ToLower(ext)
	switch ext {
	case ".md", ".markdown":
		return "md"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	case ".zip":
		return "zip"
	default:
		if _, ok := textExts[ext]; ok {
			return "text"
		}
		return ""
	}
}

// IsZipContainer reports whether ext uses a ZIP/OOXML container peeker.
func IsZipContainer(ext string) bool {
	switch PeekerIDForExt(ext) {
	case "zip", "xlsx", "pptx":
		return true
	default:
		return false
	}
}

// PeekerVersionForExt returns the current peeker version for a normalized lowercase extension
// (0 means metadata-only / no content peeker registered).
func PeekerVersionForExt(ext string) int {
	if PeekerIDForExt(ext) == "" {
		return 0
	}
	return 1
}

// IsPeekable reports whether a file extension has an upfront content peeker.
func IsPeekable(ext string) bool {
	return PeekerVersionForExt(ext) > 0
}

// Peek extracts a bounded metadata/header card for path without exceeding format caps.
// Callers must verify the file is not a cloud placeholder before calling Peek.
func Peek(ctx context.Context, path string, size int64) Result {
	if err := ctx.Err(); err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusError,
			ErrorMsg:    err.Error(),
		}
	}

	ext := strings.ToLower(filepath.Ext(path))
	ver := PeekerVersionForExt(ext)
	if ver == 0 {
		return Result{
			PeekerVersion: 0,
			IndexDepth:    store.DepthMetadataOnly,
			IndexStatus:   store.StatusOK,
		}
	}

	var res Result
	switch ext {
	case ".md", ".markdown":
		res = peekMarkdown(ctx, path, size)
	case ".csv", ".tsv":
		res = peekCSV(ctx, path, size, ext == ".tsv")
	case ".xlsx":
		res = peekXLSX(ctx, path)
	case ".pptx":
		res = peekPPTX(ctx, path)
	case ".zip":
		res = peekZIP(ctx, path)
	default:
		res = peekPlainText(ctx, path, size)
	}
	res.PeekerVersion = ver
	return res
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Back up to a valid UTF-8 rune boundary.
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
