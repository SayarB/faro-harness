package peek

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/SayarB/faro-harness/internal/store"
)

func findZipFile(zr *zip.ReadCloser, target string) *zip.File {
	target = strings.TrimPrefix(strings.ToLower(path.Clean(target)), "/")
	for _, f := range zr.File {
		name := strings.TrimPrefix(strings.ToLower(path.Clean(f.Name)), "/")
		if name == target {
			return f
		}
	}
	return nil
}

func readZipPart(f *zip.File, maxBytes int64) ([]byte, error) {
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxBytes))
}

func extractCoreTitle(zr *zip.ReadCloser) string {
	data, err := readZipPart(findZipFile(zr, "docProps/core.xml"), 256*1024)
	if err != nil || len(data) == 0 {
		return ""
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	inTitle := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "title" {
				inTitle = true
			}
		case xml.EndElement:
			if t.Name.Local == "title" {
				inTitle = false
			}
		case xml.CharData:
			if inTitle {
				if s := strings.TrimSpace(string(t)); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// --- XLSX peeker ---

type xlsxWorkbook struct {
	Sheets []struct {
		Name string `xml:"name,attr"`
	} `xml:"sheets>sheet"`
}

func peekXLSX(ctx context.Context, filePath string) Result {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	defer zr.Close()

	if err := ctx.Err(); err != nil {
		return Result{IndexDepth: store.DepthMetadataOnly, IndexStatus: store.StatusError, ErrorMsg: err.Error()}
	}

	title := extractCoreTitle(zr)

	wbData, err := readZipPart(findZipFile(zr, "xl/workbook.xml"), MaxXMLPartBytes)
	if err != nil || len(wbData) == 0 {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    "missing or unreadable xl/workbook.xml",
		}
	}

	var wb xlsxWorkbook
	if err := xml.Unmarshal(wbData, &wb); err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    fmt.Sprintf("parse workbook.xml: %v", err),
		}
	}

	sheets := make([]string, 0, len(wb.Sheets))
	for _, s := range wb.Sheets {
		if name := strings.TrimSpace(s.Name); name != "" {
			sheets = append(sheets, name)
		}
	}

	sharedStrings := parseSharedStrings(zr)
	sampleRows := parseFirstSheetRows(zr, sharedStrings, MaxXLSXSampleRows)

	var bodyParts []string
	if len(sheets) > 0 {
		bodyParts = append(bodyParts, "Sheets: "+strings.Join(sheets, ", "))
	}
	if len(sampleRows) > 0 {
		bodyParts = append(bodyParts, sampleRows...)
	}

	outlineBytes, _ := json.Marshal(struct {
		Type   string   `json:"type"`
		Sheets []string `json:"sheets"`
	}{
		Type:   "xlsx",
		Sheets: sheets,
	})

	return Result{
		IndexDepth:  store.DepthHeaderOnly,
		IndexStatus: store.StatusOK,
		Title:       title,
		Headings:    strings.Join(sheets, "\n"),
		BodyPreview: truncateUTF8(strings.Join(bodyParts, "\n"), 32*1024),
		OutlineJSON: string(outlineBytes),
		UnitCount:   len(sheets),
	}
}

func parseSharedStrings(zr *zip.ReadCloser) []string {
	data, err := readZipPart(findZipFile(zr, "xl/sharedStrings.xml"), MaxXMLPartBytes)
	if err != nil || len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var result []string
	var cur strings.Builder
	inSI := false
	inT := false

	for len(result) < 2000 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				if inSI {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				inSI = false
				result = append(result, strings.TrimSpace(cur.String()))
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
	return result
}

func parseFirstSheetRows(zr *zip.ReadCloser, shared []string, maxRows int) []string {
	sheetFile := findZipFile(zr, "xl/worksheets/sheet1.xml")
	if sheetFile == nil {
		// Fall back to first worksheet found.
		for _, f := range zr.File {
			lower := strings.ToLower(f.Name)
			if strings.HasPrefix(lower, "xl/worksheets/sheet") && strings.HasSuffix(lower, ".xml") {
				sheetFile = f
				break
			}
		}
	}
	if sheetFile == nil {
		return nil
	}

	data, err := readZipPart(sheetFile, MaxXMLPartBytes)
	if err != nil || len(data) == 0 {
		return nil
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	var rows []string
	var curRow []string
	var curVal strings.Builder
	cellType := ""
	inRow := false
	inVal := false

	for len(rows) < maxRows {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				inRow = true
				curRow = curRow[:0]
			case "c":
				if inRow {
					cellType = ""
					for _, attr := range t.Attr {
						if attr.Name.Local == "t" {
							cellType = attr.Value
							break
						}
					}
					curVal.Reset()
				}
			case "v", "t":
				if inRow {
					inVal = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inVal = false
			case "c":
				if inRow {
					val := strings.TrimSpace(curVal.String())
					if cellType == "s" {
						if idx, err := strconv.Atoi(val); err == nil && idx >= 0 && idx < len(shared) {
							val = shared[idx]
						}
					}
					if val != "" {
						curRow = append(curRow, val)
					}
				}
			case "row":
				inRow = false
				if len(curRow) > 0 {
					rows = append(rows, strings.Join(curRow, " | "))
				}
			}
		case xml.CharData:
			if inVal {
				curVal.Write(t)
			}
		}
	}
	return rows
}

// --- PPTX peeker ---

type pptxSlideOutline struct {
	Index int    `json:"index"`
	Title string `json:"title,omitempty"`
}

func peekPPTX(ctx context.Context, filePath string) Result {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    err.Error(),
		}
	}
	defer zr.Close()

	if err := ctx.Err(); err != nil {
		return Result{IndexDepth: store.DepthMetadataOnly, IndexStatus: store.StatusError, ErrorMsg: err.Error()}
	}

	title := extractCoreTitle(zr)
	slidePaths, unresolvedRels := orderedPPTXSlides(zr)
	if len(slidePaths) == 0 {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    "no slides found in pptx archive",
		}
	}

	totalSlides := len(slidePaths) + unresolvedRels
	truncated := len(slidePaths) > MaxPPTXSlides || unresolvedRels > 0
	limit := min(len(slidePaths), MaxPPTXSlides)

	var headings []string
	var outline []pptxSlideOutline
	var bodyBuilder strings.Builder
	readSlides := 0
	missingSlides := 0

	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			truncated = true
			break
		}
		slideFile := findZipFile(zr, slidePaths[i])
		if slideFile == nil {
			missingSlides++
			truncated = true
			continue
		}
		data, err := readZipPart(slideFile, MaxXMLPartBytes)
		if err != nil || len(data) == 0 {
			missingSlides++
			truncated = true
			continue
		}
		readSlides++
		slideTitle, slideText := parsePPTXSlide(data)
		outline = append(outline, pptxSlideOutline{
			Index: i + 1,
			Title: slideTitle,
		})
		if slideTitle != "" {
			headings = append(headings, slideTitle)
			if title == "" {
				title = slideTitle
			}
		}
		if slideText != "" {
			chunk := fmt.Sprintf("[Slide %d] %s\n", i+1, slideText)
			if bodyBuilder.Len()+len(chunk) > MaxHeaderTextBytes {
				remaining := MaxHeaderTextBytes - bodyBuilder.Len()
				if remaining > 0 {
					bodyBuilder.WriteString(truncateUTF8(chunk, remaining))
				}
				truncated = true
				break
			}
			bodyBuilder.WriteString(chunk)
		}
	}

	if readSlides == 0 {
		return Result{
			IndexDepth:  store.DepthMetadataOnly,
			IndexStatus: store.StatusUnreadable,
			ErrorMsg:    fmt.Sprintf("could not read any of %d slide parts in pptx", totalSlides),
		}
	}

	depth := store.DepthFull
	if truncated || missingSlides > 0 {
		depth = store.DepthHeaderOnly
	}

	outlineBytes, _ := json.Marshal(struct {
		Type        string             `json:"type"`
		TotalSlides int                `json:"total_slides"`
		Slides      []pptxSlideOutline `json:"slides"`
	}{
		Type:        "pptx",
		TotalSlides: totalSlides,
		Slides:      outline,
	})

	return Result{
		IndexDepth:  depth,
		IndexStatus: store.StatusOK,
		Title:       title,
		Headings:    strings.Join(headings, "\n"),
		BodyPreview: strings.TrimSpace(bodyBuilder.String()),
		OutlineJSON: string(outlineBytes),
		UnitCount:   totalSlides,
	}
}

func orderedPPTXSlides(zr *zip.ReadCloser) (paths []string, unresolved int) {
	// Try reading ppt/presentation.xml + ppt/_rels/presentation.xml.rels for exact deck order.
	presData, err1 := readZipPart(findZipFile(zr, "ppt/presentation.xml"), MaxXMLPartBytes)
	relsData, err2 := readZipPart(findZipFile(zr, "ppt/_rels/presentation.xml.rels"), MaxXMLPartBytes)
	if err1 == nil && err2 == nil && len(presData) > 0 && len(relsData) > 0 {
		relTargets := map[string]string{}
		dec := xml.NewDecoder(bytes.NewReader(relsData))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Relationship" {
				var id, target string
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "Id":
						id = a.Value
					case "Target":
						target = a.Value
					}
				}
				if id != "" && target != "" {
					if unescaped, err := url.PathUnescape(target); err == nil {
						target = unescaped
					}
					if strings.HasPrefix(target, "/") {
						relTargets[id] = strings.TrimPrefix(path.Clean(target), "/")
					} else {
						relTargets[id] = strings.TrimPrefix(path.Clean("ppt/"+target), "/")
					}
				}
			}
		}

		var ordered []string
		dec2 := xml.NewDecoder(bytes.NewReader(presData))
		for {
			tok, err := dec2.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sldId" {
				for _, a := range se.Attr {
					if a.Name.Local == "id" && a.Name.Space != "" {
						if target, ok := relTargets[a.Value]; ok {
							ordered = append(ordered, target)
						} else {
							unresolved++
						}
					}
				}
			}
		}
		if len(ordered) > 0 {
			return ordered, unresolved
		}
	}

	// Fallback: collect ppt/slides/slideN.xml sorted numerically.
	type numberedSlide struct {
		num  int
		name string
	}
	var slides []numberedSlide
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		if strings.HasPrefix(lower, "ppt/slides/slide") && strings.HasSuffix(lower, ".xml") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(lower, "ppt/slides/slide"), ".xml")
			n, _ := strconv.Atoi(numStr)
			slides = append(slides, numberedSlide{num: n, name: f.Name})
		}
	}
	sort.Slice(slides, func(i, j int) bool {
		if slides[i].num != slides[j].num {
			return slides[i].num < slides[j].num
		}
		return slides[i].name < slides[j].name
	})
	out := make([]string, len(slides))
	for i, s := range slides {
		out[i] = s.name
	}
	return out, 0
}

func parsePPTXSlide(data []byte) (slideTitle string, fullText string) {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var titleParts []string
	var bodyParts []string
	var curPara strings.Builder

	inShape := false
	isTitleShape := false
	inPara := false
	inText := false

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sp":
				inShape = true
				isTitleShape = false
			case "ph":
				if inShape {
					for _, a := range t.Attr {
						if a.Name.Local == "type" && (a.Value == "title" || a.Value == "ctrTitle") {
							isTitleShape = true
						}
					}
				}
			case "p":
				if inShape {
					inPara = true
					curPara.Reset()
				}
			case "t":
				if inPara {
					inText = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if inPara {
					inPara = false
					if line := strings.TrimSpace(curPara.String()); line != "" {
						bodyParts = append(bodyParts, line)
						if isTitleShape {
							titleParts = append(titleParts, line)
						}
					}
				}
			case "sp":
				inShape = false
				isTitleShape = false
			}
		case xml.CharData:
			if inText {
				curPara.Write(t)
			}
		}
	}

	return strings.Join(titleParts, " — "), strings.Join(bodyParts, " | ")
}
