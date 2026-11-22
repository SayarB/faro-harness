package peek

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/SayarB/faro-harness/internal/store"
)

func readBounded(path string, limit int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	// Read limit+1 bytes to know whether the file was truncated.
	buf, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(buf)) > limit
	if truncated {
		buf = buf[:limit]
	}
	return buf, truncated, nil
}

func looksBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	// Allow incomplete trailing UTF-8 sequence at truncation edge.
	if len(sample) > 4 {
		sample = sample[:len(sample)-4]
	}
	return !utf8.Valid(sample)
}

func peekPlainText(_ context.Context, path string, size int64) Result {
	limit := int64(MaxFullTextBytes)
	if size > MaxFullTextBytes {
		limit = MaxHeaderTextBytes
	}

	raw, truncated, err := readBounded(path, limit)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	if looksBinary(raw) {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusOK,
		}
	}

	text := strings.TrimSpace(string(raw))
	depth := store.DepthFull
	if truncated {
		depth = store.DepthHeaderOnly
		text = truncateUTF8(text, MaxHeaderTextBytes)
	}

	lines := 0
	if text != "" {
		lines = strings.Count(text, "\n") + 1
	}

	return Result{
		IndexDepth:  depth,
		IndexStatus: store.StatusOK,
		BodyPreview: text,
		UnitCount:   lines,
	}
}

type mdHeading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

func peekMarkdown(_ context.Context, path string, size int64) Result {
	limit := int64(MaxFullTextBytes)
	if size > MaxFullTextBytes {
		limit = MaxHeaderTextBytes
	}

	raw, truncated, err := readBounded(path, limit)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	if looksBinary(raw) {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusOK,
		}
	}

	text := string(raw)
	var headings []mdHeading
	var headingLines []string
	var title string
	inFence := false
	lineCount := 0

	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 16*1024), 512*1024)
	for scanner.Scan() {
		lineCount++
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(line, "#") {
			continue
		}
		level := 0
		for level < len(line) && level < 6 && line[level] == '#' {
			level++
		}
		if level < len(line) && line[level] == ' ' {
			hText := strings.TrimSpace(strings.TrimRight(line[level+1:], "# "))
			if hText != "" {
				headings = append(headings, mdHeading{Level: level, Text: hText})
				headingLines = append(headingLines, hText)
				if title == "" && level == 1 {
					title = hText
				}
			}
		}
	}
	if title == "" && len(headingLines) > 0 {
		title = headingLines[0]
	}

	outlineBytes, _ := json.Marshal(struct {
		Type     string      `json:"type"`
		Headings []mdHeading `json:"headings"`
	}{
		Type:     "markdown",
		Headings: headings,
	})

	depth := store.DepthFull
	body := strings.TrimSpace(text)
	if truncated {
		depth = store.DepthHeaderOnly
		body = truncateUTF8(body, MaxHeaderTextBytes)
	}

	return Result{
		IndexDepth:  depth,
		IndexStatus: store.StatusOK,
		Title:       title,
		Headings:    strings.Join(headingLines, "\n"),
		BodyPreview: body,
		OutlineJSON: string(outlineBytes),
		UnitCount:   lineCount,
	}
}

func peekCSV(_ context.Context, path string, _ int64, isTSV bool) Result {
	raw, truncated, err := readBounded(path, MaxHeaderTextBytes)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	if looksBinary(raw) {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusOK,
		}
	}

	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	if isTSV {
		r.Comma = '\t'
	}

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return Result{
			IndexDepth:  store.DepthFull,
			IndexStatus: store.StatusOK,
		}
	}
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}

	cleanCols := make([]string, 0, len(header))
	for _, col := range header {
		col = strings.TrimSpace(strings.TrimPrefix(col, "\uFEFF"))
		if col != "" {
			cleanCols = append(cleanCols, col)
		}
	}

	var bodyLines []string
	if len(cleanCols) > 0 {
		bodyLines = append(bodyLines, strings.Join(cleanCols, " | "))
	}

	rowCount := 0
	reachedEOF := false
	for rowCount < MaxCSVSampleRows {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			reachedEOF = true
			break
		}
		if err != nil {
			// Truncated mid-line at byte boundary is expected for large CSVs.
			break
		}
		rowCount++
		cells := make([]string, len(rec))
		for i, c := range rec {
			cells[i] = strings.TrimSpace(c)
		}
		bodyLines = append(bodyLines, strings.Join(cells, " | "))
	}
	if !reachedEOF && !truncated {
		if _, err := r.Read(); errors.Is(err, io.EOF) {
			reachedEOF = true
		}
	}

	depth := store.DepthHeaderOnly
	if reachedEOF && !truncated {
		depth = store.DepthFull
	}

	outlineBytes, _ := json.Marshal(struct {
		Type    string   `json:"type"`
		Columns []string `json:"columns"`
	}{
		Type:    "csv",
		Columns: cleanCols,
	})

	return Result{
		IndexDepth:  depth,
		IndexStatus: store.StatusOK,
		Headings:    strings.Join(cleanCols, "\n"),
		BodyPreview: truncateUTF8(strings.Join(bodyLines, "\n"), MaxHeaderTextBytes),
		OutlineJSON: string(outlineBytes),
		UnitCount:   rowCount,
	}
}
