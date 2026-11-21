package peek

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/store"
)

func TestPeekZipCentralDirectory(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "taxes_2025.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"W2_2025.pdf", "receipts/hotel.jpg"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("placeholder"))
	}
	_ = zw.Close()
	_ = f.Close()

	res, err := File(zipPath, 512)
	if err != nil {
		t.Fatalf("peek zip: %v", err)
	}
	if res.Depth != store.DepthHeaderOnly {
		t.Fatalf("expected header_only, got %s", res.Depth)
	}
	if !strings.Contains(res.Body, "W2_2025.pdf") {
		t.Fatalf("expected W2_2025.pdf in body, got %q", res.Body)
	}
}

func TestPeekXLSXSheetNames(t *testing.T) {
	dir := t.TempDir()
	xlsxPath := filepath.Join(dir, "Q3_Sales.xlsx")

	f, err := os.Create(xlsxPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("xl/workbook.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheets>
    <sheet name="Summary" sheetId="1" r:id="rId1"/>
    <sheet name="Transactions" sheetId="2" r:id="rId2"/>
  </sheets>
</workbook>`))
	_ = zw.Close()
	_ = f.Close()

	res, err := File(xlsxPath, 1024)
	if err != nil {
		t.Fatalf("peek xlsx: %v", err)
	}
	if res.Depth != store.DepthHeaderOnly {
		t.Fatalf("expected header_only, got %s", res.Depth)
	}
	if !strings.Contains(res.Headings, "Summary") || !strings.Contains(res.Headings, "Transactions") {
		t.Fatalf("expected sheet names in headings, got %q", res.Headings)
	}
}
