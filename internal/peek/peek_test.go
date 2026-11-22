package peek_test

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/peek"
	"github.com/SayarB/faro-harness/internal/store"
)

func writeZip(t *testing.T, dest string, files map[string]string) int64 {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatalf("create zip %s: %v", dest, err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat %s: %v", dest, err)
	}
	return info.Size()
}

func TestPeekMarkdown(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "roadmap.md")
	content := "# Q4 Product Plan\n\nIntro paragraph.\n\n```bash\n# not a heading\n```\n\n## Milestones\nShip indexer.\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res := peek.Peek(context.Background(), p, int64(len(content)))
	if res.IndexDepth != store.DepthFull {
		t.Fatalf("depth = %q, want full", res.IndexDepth)
	}
	if res.Title != "Q4 Product Plan" {
		t.Fatalf("title = %q, want Q4 Product Plan", res.Title)
	}
	if !strings.Contains(res.Headings, "Milestones") || strings.Contains(res.Headings, "not a heading") {
		t.Fatalf("unexpected headings: %q", res.Headings)
	}
}

func TestPeekCSV(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "expenses.csv")
	content := "Date,Vendor,Amount\n2026-10-01,Acme Cloud,420.50\n2026-10-02,Figma,45.00\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res := peek.Peek(context.Background(), p, int64(len(content)))
	if res.IndexDepth != store.DepthFull {
		t.Fatalf("depth = %q, want full", res.IndexDepth)
	}
	if !strings.Contains(res.Headings, "Vendor") || !strings.Contains(res.BodyPreview, "Acme Cloud") {
		t.Fatalf("unexpected CSV peek result: %+v", res)
	}
	if res.UnitCount != 2 {
		t.Fatalf("unit_count = %d, want 2", res.UnitCount)
	}
}

func TestPeekXLSX(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "budget.xlsx")
	size := writeZip(t, p, map[string]string{
		"docProps/core.xml": `<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>FY26 Budget</dc:title></cp:coreProperties>`,
		"xl/workbook.xml":   `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets><sheet name="Summary" sheetId="1"/><sheet name="Headcount" sheetId="2"/></sheets></workbook>`,
		"xl/sharedStrings.xml": `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Department</t></si><si><t>Spend</t></si><si><t>Engineering</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>125000</v></c></row></sheetData></worksheet>`,
	})

	res := peek.Peek(context.Background(), p, size)
	if res.IndexDepth != store.DepthHeaderOnly {
		t.Fatalf("depth = %q, want header_only", res.IndexDepth)
	}
	if res.Title != "FY26 Budget" {
		t.Fatalf("title = %q, want FY26 Budget", res.Title)
	}
	if !strings.Contains(res.Headings, "Summary") || !strings.Contains(res.Headings, "Headcount") {
		t.Fatalf("headings = %q, want Summary and Headcount", res.Headings)
	}
	if !strings.Contains(res.BodyPreview, "Engineering | 125000") {
		t.Fatalf("body = %q, want resolved cell preview", res.BodyPreview)
	}
}

func TestPeekPPTX(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pitch.pptx")
	size := writeZip(t, p, map[string]string{
		"docProps/core.xml": `<?xml version="1.0"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Series A Deck</dc:title></cp:coreProperties>`,
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/><p:sldId id="257" r:id="rId2"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="slides/slide1.xml"/><Relationship Id="rId2" Target="slides/slide2.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": `<?xml version="1.0"?><p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:nvSpPr><p:nvPr><p:ph type="ctrTitle"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Faro Vision</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:nvPr/></p:nvSpPr><p:txBody><a:p><a:r><a:t>Local-first AI harness</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		"ppt/slides/slide2.xml": `<?xml version="1.0"?><p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:nvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Traction &amp; Metrics</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:nvPr/></p:nvSpPr><p:txBody><a:p><a:r><a:t>10k weekly active folders</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})

	res := peek.Peek(context.Background(), p, size)
	if res.IndexDepth != store.DepthFull {
		t.Fatalf("depth = %q, want full", res.IndexDepth)
	}
	if res.Title != "Series A Deck" {
		t.Fatalf("title = %q, want Series A Deck", res.Title)
	}
	if !strings.Contains(res.Headings, "Faro Vision") || !strings.Contains(res.Headings, "Traction & Metrics") {
		t.Fatalf("headings = %q", res.Headings)
	}
	if !strings.Contains(res.BodyPreview, "10k weekly active folders") {
		t.Fatalf("body = %q", res.BodyPreview)
	}
	if res.UnitCount != 2 {
		t.Fatalf("unit_count = %d, want 2", res.UnitCount)
	}
}

func TestPeekPPTXAbsoluteRelationshipTargetsAndMissingSlides(t *testing.T) {
	dir := t.TempDir()

	// Absolute target starting with "/" (common in Google Slides & LibreOffice exports).
	absPath := filepath.Join(dir, "abs_rels.pptx")
	absSize := writeZip(t, absPath, map[string]string{
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="/ppt/slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": `<?xml version="1.0"?><p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:nvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Absolute Target Slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})
	res := peek.Peek(context.Background(), absPath, absSize)
	if res.IndexDepth != store.DepthFull || res.IndexStatus != store.StatusOK {
		t.Fatalf("abs rels peek = (%q, %q), want (full, ok)", res.IndexDepth, res.IndexStatus)
	}
	if !strings.Contains(res.Headings, "Absolute Target Slide") {
		t.Fatalf("headings = %q, want Absolute Target Slide", res.Headings)
	}

	// Missing slide part must NOT claim depth=full.
	brokenPath := filepath.Join(dir, "missing_slide.pptx")
	brokenSize := writeZip(t, brokenPath, map[string]string{
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="/ppt/slides/does_not_exist.xml"/></Relationships>`,
	})
	brokenRes := peek.Peek(context.Background(), brokenPath, brokenSize)
	if brokenRes.IndexDepth == store.DepthFull || brokenRes.IndexStatus != store.StatusUnreadable {
		t.Fatalf("missing slide peek = (%q, %q), want (metadata_only, unreadable)", brokenRes.IndexDepth, brokenRes.IndexStatus)
	}
}

func TestPeekZIPAndBinaryFallback(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "bundle.zip")
	zsize := writeZip(t, zp, map[string]string{
		"contracts/nda.pdf": "pdf",
		"invoices/oct.csv":  "a,b",
	})

	zres := peek.Peek(context.Background(), zp, zsize)
	if zres.IndexDepth != store.DepthHeaderOnly || zres.UnitCount != 2 {
		t.Fatalf("unexpected zip peek: %+v", zres)
	}
	if !strings.Contains(zres.BodyPreview, "contracts/nda.pdf") {
		t.Fatalf("zip body missing entry: %q", zres.BodyPreview)
	}

	bp := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(bp, []byte{0x89, 'P', 'N', 'G', 0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	bres := peek.Peek(context.Background(), bp, 7)
	if bres.IndexDepth != store.DepthMetadataOnly || bres.IndexStatus != store.StatusOK {
		t.Fatalf("unexpected binary peek: %+v", bres)
	}
}
