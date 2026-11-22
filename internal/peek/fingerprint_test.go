package peek_test

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/peek"
)

func TestFingerprintHeadAndGrowingLogStability(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Create a >256 KB log file (enters header_only 64 KB peek mode).
	p := filepath.Join(dir, "app.log")
	head := strings.Repeat("2026-11-21 INFO started service\n", 2500) // ~80 KB
	tail := strings.Repeat("tail line\n", 25000)                      // ~250 KB -> total ~330 KB
	if err := os.WriteFile(p, []byte(head+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fp1, err := peek.Fingerprint(ctx, p, info1.Size())
	if err != nil || fp1 == "" {
		t.Fatalf("fp1 failed: fp=%q err=%v", fp1, err)
	}

	// Append more bytes past the 64 KB peek window -> fingerprint must stay identical.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("new appended log line\n", 5000)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	info2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := peek.Fingerprint(ctx, p, info2.Size())
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("growing large log changed head fingerprint: %q != %q", fp1, fp2)
	}

	// Editing the first 64 KB MUST change the fingerprint.
	if err := os.WriteFile(p, []byte("CHANGED"+head[7:]+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	info3, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fp3, err := peek.Fingerprint(ctx, p, info3.Size())
	if err != nil {
		t.Fatal(err)
	}
	if fp3 == fp1 {
		t.Fatalf("editing head bytes did not change fingerprint")
	}
}

func TestFingerprintSmallFileBoundaryTransition(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.txt")

	// Small file (<= 256 KB) vs > 256 KB sharing the same 64 KB prefix must have different fingerprints
	// because one is full and the other is header_only.
	prefix := bytes.Repeat([]byte("a"), peek.MaxHeaderTextBytes)
	if err := os.WriteFile(p, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	fpSmall, err := peek.Fingerprint(ctx, p, int64(len(prefix)))
	if err != nil || fpSmall == "" {
		t.Fatalf("fpSmall failed: %q %v", fpSmall, err)
	}

	large := append(append([]byte{}, prefix...), bytes.Repeat([]byte("b"), peek.MaxFullTextBytes)...)
	if err := os.WriteFile(p, large, 0o644); err != nil {
		t.Fatal(err)
	}
	fpLarge, err := peek.Fingerprint(ctx, p, int64(len(large)))
	if err != nil || fpLarge == "" {
		t.Fatalf("fpLarge failed: %q %v", fpLarge, err)
	}
	if fpSmall == fpLarge {
		t.Fatalf("small full file and truncated large file collided on fingerprint %q", fpSmall)
	}
}

func TestFingerprintZipCentralDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	writeZipWithComment := func(path string, entries map[string]string, comment string) int64 {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		if comment != "" {
			if err := zw.SetComment(comment); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range entries {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	z1 := filepath.Join(dir, "a.xlsx")
	s1 := writeZipWithComment(z1, map[string]string{
		"xl/workbook.xml": "<workbook/>",
		"media/big.bin":   strings.Repeat("Z", 1024*1024),
	}, "archive comment with PK\x05\x06 fake sig")

	// Exact copy under another name -> identical central-directory fingerprint.
	z2 := filepath.Join(dir, "copy.xlsx")
	raw, err := os.ReadFile(z1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z2, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	fp1, err := peek.Fingerprint(ctx, z1, s1)
	if err != nil || fp1 == "" {
		t.Fatalf("fp1 failed: %q %v", fp1, err)
	}
	fp2, err := peek.Fingerprint(ctx, z2, s1)
	if err != nil || fp2 != fp1 {
		t.Fatalf("copy zip fingerprint mismatch: %q != %q (err=%v)", fp2, fp1, err)
	}

	// Modifying an inner member changes its CRC32 in the central directory -> new fingerprint.
	z3 := filepath.Join(dir, "modified.xlsx")
	s3 := writeZipWithComment(z3, map[string]string{
		"xl/workbook.xml": "<workbook updated='1'/>",
		"media/big.bin":   strings.Repeat("Z", 1024*1024),
	}, "")
	fp3, err := peek.Fingerprint(ctx, z3, s3)
	if err != nil || fp3 == "" {
		t.Fatalf("fp3 failed: %q %v", fp3, err)
	}
	if fp3 == fp1 {
		t.Fatalf("modified archive unexpectedly matched original central-directory fingerprint")
	}

	// Corrupt/non-zip file with .zip extension returns ("", nil) so worker marks StatusUnreadable.
	bad := filepath.Join(dir, "corrupt.zip")
	if err := os.WriteFile(bad, []byte("not a real zip file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpBad, err := peek.Fingerprint(ctx, bad, 26)
	if err != nil || fpBad != "" {
		t.Fatalf("corrupt zip fingerprint = (%q, %v), want (\"\", nil)", fpBad, err)
	}
}

func TestFingerprintSharedAcrossExtensionsWithDistinctPeekerIDs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Identical small text bytes under .md, .txt, .csv, .tsv share the same pure content
	// fingerprint while PeekerIDForExt distinguishes their peeker families (used by store's
	// composite (blake3, peeker_id, peeker_version) key).
	sample := []byte("# Title\ncol1,col2\n1,2\n")
	mdPath := filepath.Join(dir, "a.md")
	txtPath := filepath.Join(dir, "a.txt")
	csvPath := filepath.Join(dir, "a.csv")
	tsvPath := filepath.Join(dir, "a.tsv")
	for _, p := range []string{mdPath, txtPath, csvPath, tsvPath} {
		if err := os.WriteFile(p, sample, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fpMD, _ := peek.Fingerprint(ctx, mdPath, int64(len(sample)))
	fpTXT, _ := peek.Fingerprint(ctx, txtPath, int64(len(sample)))
	fpCSV, _ := peek.Fingerprint(ctx, csvPath, int64(len(sample)))
	fpTSV, _ := peek.Fingerprint(ctx, tsvPath, int64(len(sample)))

	if fpMD == "" || fpMD != fpTXT || fpCSV != fpTSV || fpMD != fpCSV {
		t.Fatalf("expected identical pure content fingerprints for <=64KB sample: md=%q txt=%q csv=%q tsv=%q", fpMD, fpTXT, fpCSV, fpTSV)
	}
	if peek.PeekerIDForExt(".md") == peek.PeekerIDForExt(".txt") || peek.PeekerIDForExt(".csv") == peek.PeekerIDForExt(".tsv") {
		t.Fatalf("expected distinct peeker IDs across .md/.txt and .csv/.tsv")
	}

	// Identical ZIP bytes under .xlsx and .zip share the same central-directory fingerprint
	// with distinct peeker IDs.
	xlsxPath := filepath.Join(dir, "report.xlsx")
	zipPath := filepath.Join(dir, "report.zip")
	size := writeZip(t, xlsxPath, map[string]string{"xl/workbook.xml": "<workbook/>"})
	raw, err := os.ReadFile(xlsxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	fpXLSX, _ := peek.Fingerprint(ctx, xlsxPath, size)
	fpZIP, _ := peek.Fingerprint(ctx, zipPath, size)
	if fpXLSX == "" || fpXLSX != fpZIP {
		t.Fatalf("expected identical zipcd fingerprint for .xlsx (%q) and .zip (%q)", fpXLSX, fpZIP)
	}
	if peek.PeekerIDForExt(".xlsx") == peek.PeekerIDForExt(".zip") {
		t.Fatalf("expected distinct peeker IDs for .xlsx and .zip")
	}
}

func TestFingerprintZipWithPrependedStubAndTrailingJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	makeRawZip := func(content string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("doc.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	stub := bytes.Repeat([]byte("X"), 5000)
	zA := makeRawZip("version-A-content")
	zB := makeRawZip("version-B-content")

	// Prepended-stub (SFX) zips with different inner contents must hash the real central
	// directory bytes (not the stub) and therefore differ.
	sfxA := filepath.Join(dir, "sfx_a.zip")
	sfxB := filepath.Join(dir, "sfx_b.zip")
	if err := os.WriteFile(sfxA, append(append([]byte{}, stub...), zA...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sfxB, append(append([]byte{}, stub...), zB...), 0o644); err != nil {
		t.Fatal(err)
	}

	fpA, err := peek.Fingerprint(ctx, sfxA, int64(len(stub)+len(zA)))
	if err != nil || fpA == "" {
		t.Fatalf("sfxA fingerprint failed: %q %v", fpA, err)
	}
	fpB, err := peek.Fingerprint(ctx, sfxB, int64(len(stub)+len(zB)))
	if err != nil || fpB == "" {
		t.Fatalf("sfxB fingerprint failed: %q %v", fpB, err)
	}
	if fpA == fpB {
		t.Fatalf("prepended-stub ZIPs with different member contents collided on %q", fpA)
	}

	// ZIP with trailing junk after EOCD must still locate and hash the central directory.
	trailing := filepath.Join(dir, "trailing.zip")
	rawTrailing := append(append([]byte{}, zA...), []byte("TRAILING-JUNK-BYTES")...)
	if err := os.WriteFile(trailing, rawTrailing, 0o644); err != nil {
		t.Fatal(err)
	}
	fpTrail, err := peek.Fingerprint(ctx, trailing, int64(len(rawTrailing)))
	if err != nil || fpTrail == "" {
		t.Fatalf("trailing-junk ZIP fingerprint failed: %q %v", fpTrail, err)
	}
	if fpTrail != fpA {
		t.Fatalf("trailing-junk ZIP central dir hash %q != sfxA central dir hash %q", fpTrail, fpA)
	}
}
