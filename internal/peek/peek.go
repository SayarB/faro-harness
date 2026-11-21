package peek

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/SayarB/faro-harness/internal/store"
)

const (
	// ExtractorVersion bumps when peek logic changes so blobs cleanly re-extract.
	ExtractorVersion = 1

	MaxTextFileBytes = 5 * 1024 * 1024 // skip full-text reading for text files > 5 MB
	MaxIndexedChars  = 64 * 1024       // cap FTS5 body text at 64 KB (matches OpenCode's MaxText)
	MaxZipEntries    = 200             // cap central-directory filename list
)

// Result is the compact card extracted from a file without heavy parsing.
type Result struct {
	Depth       store.IndexDepth
	Headings    string
	Body        string
	OutlineJSON string
}

var textExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".csv": true, ".tsv": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".xml": true,
	".html": true, ".htm": true, ".log": true, ".ini": true, ".env": true,
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".sql": true, ".sh": true, ".css": true,
}

// File inspects a file using fast, bounded header peeks.
// Heavy formats (big PDFs, large spreadsheets, media) are kept lightweight so
// the agent inspects them on demand via code execution.
func File(absPath string, size int64) (Result, error) {
	ext := strings.ToLower(filepath.Ext(absPath))

	switch {
	case ext == ".xlsx":
		return peekXLSX(absPath)
	case ext == ".zip":
		return peekZip(absPath)
	case textExtensions[ext]:
		if size > MaxTextFileBytes {
			return peekTextHead(absPath)
		}
		return peekText(absPath)
	default:
		return Result{
			Depth:       store.DepthMetadataOnly,
			OutlineJSON: "[]",
		}, nil
	}
}

func peekText(absPath string) (Result, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	buf, err := io.ReadAll(io.LimitReader(f, MaxIndexedChars))
	if err != nil {
		return Result{}, err
	}
	if !utf8.Valid(buf) {
		return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
	}

	text := string(buf)
	headings := extractMarkdownHeadings(text)
	return Result{
		Depth:       store.DepthFull,
		Headings:    strings.Join(headings, " | "),
		Body:        text,
		OutlineJSON: "[]",
	}, nil
}

func peekTextHead(absPath string) (Result, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	buf, err := io.ReadAll(io.LimitReader(f, 8*1024))
	if err != nil {
		return Result{}, err
	}
	if !utf8.Valid(buf) {
		return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
	}
	return Result{
		Depth:       store.DepthHeaderOnly,
		Headings:    "large-text-file",
		Body:        string(buf),
		OutlineJSON: "[]",
	}, nil
}

// peekZip reads ONLY the ZIP central directory at the end of the file (zero decompression).
func peekZip(absPath string) (Result, error) {
	zr, err := zip.OpenReader(absPath)
	if err != nil {
		return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
	}
	defer zr.Close()

	limit := min(len(zr.File), MaxZipEntries)
	names := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		names = append(names, zr.File[i].Name)
	}
	body := "Archive entries: " + strings.Join(names, "\n")
	if len(body) > MaxIndexedChars {
		body = body[:MaxIndexedChars]
	}
	return Result{
		Depth:       store.DepthHeaderOnly,
		Headings:    strings.Join(names[:min(20, len(names))], " "),
		Body:        body,
		OutlineJSON: "[]",
	}, nil
}

type xlsxWorkbook struct {
	Sheets []struct {
		Name string `xml:"name,attr"`
	} `xml:"sheets>sheet"`
}

// peekXLSX reads ONLY xl/workbook.xml from the .xlsx ZIP container to extract sheet names
// in ~1ms without parsing hundreds of thousands of worksheet rows.
func peekXLSX(absPath string) (Result, error) {
	zr, err := zip.OpenReader(absPath)
	if err != nil {
		return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != "xl/workbook.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
		}
		defer rc.Close()

		var wb xlsxWorkbook
		if err := xml.NewDecoder(io.LimitReader(rc, 512*1024)).Decode(&wb); err != nil {
			return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
		}

		names := make([]string, 0, len(wb.Sheets))
		for _, s := range wb.Sheets {
			if s.Name != "" {
				names = append(names, s.Name)
			}
		}
		joined := strings.Join(names, ", ")
		return Result{
			Depth:       store.DepthHeaderOnly,
			Headings:    joined,
			Body:        "Excel workbook sheets: " + joined,
			OutlineJSON: "[]",
		}, nil
	}

	return Result{Depth: store.DepthMetadataOnly, OutlineJSON: "[]"}, nil
}

func extractMarkdownHeadings(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			h := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			if h != "" {
				out = append(out, h)
				if len(out) >= 30 {
					break
				}
			}
		}
	}
	return out
}
