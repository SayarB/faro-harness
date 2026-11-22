package worker_test

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/worker"
)

func createTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineIndexesDeduplicatesSkipsAndPrunes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Create sample workspace files including a subdirectory, duplicate, ignored file, and binary.
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mdContent := "# Launch Checklist\n\n## Storage\nUsing SQLite FTS5 with BLAKE3 dedup.\n"
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(mdContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exact duplicate in another folder to test BLAKE3 dedup.
	if err := os.WriteFile(filepath.Join(dir, "reports", "notes_copy.md"), []byte(mdContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "metrics.csv"), []byte("Month,ARR\nOct,50000\nNov,62000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	createTestZip(t, filepath.Join(dir, "archive.zip"), map[string]string{
		"a/one.txt": "1",
		"b/two.txt": "2",
	})
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte{0x89, 'P', 'N', 'G', 0, 1}, 0o644); err != nil {
		t.Fatal(err)
	}
	// Ignored files/dirs that must not appear in catalog.
	if err := os.WriteFile(filepath.Join(dir, "~$locked.xlsx"), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{NumWorkers: 2, BatchSize: 10})
	defer pipe.Close()

	snap, err := pipe.IndexWorkspace(ctx)
	if err != nil {
		t.Fatalf("IndexWorkspace failed: %v", err)
	}
	if snap.Indexed != 5 {
		t.Fatalf("indexed = %d, want 5 (got %+v)", snap.Indexed, snap)
	}

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("ListFiles count = %d, want 5", len(entries))
	}

	// Verify BLAKE3 dedup: notes.md and reports/notes_copy.md share 1 blob, plus metrics.csv + archive.zip = 3 blobs total.
	blobs, err := st.BlobCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 3 {
		t.Fatalf("BlobCount = %d, want 3 (deduplicated notes.md + metrics.csv + archive.zip)", blobs)
	}

	// Second crawl with no changes should hit fast-path skip for all 5 files.
	snap2, err := pipe.IndexWorkspace(ctx)
	if err != nil {
		t.Fatalf("second IndexWorkspace failed: %v", err)
	}
	if snap2.Skipped != 5 {
		t.Fatalf("second crawl skipped = %d, want 5 (got %+v)", snap2.Skipped, snap2)
	}

	// Delete a file and re-crawl -> should prune it from SQLite and FTS5.
	if err := os.Remove(filepath.Join(dir, "reports", "metrics.csv")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.IndexWorkspace(ctx); err != nil {
		t.Fatalf("third IndexWorkspace failed: %v", err)
	}
	entriesAfter, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != 4 {
		t.Fatalf("ListFiles after delete = %d, want 4", len(entriesAfter))
	}
	hits, err := st.Search(ctx, cat.Workspace.ID, "ARR", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected deleted CSV to be absent from FTS, got %+v", hits)
	}
}

func TestPipelineNoDeadlockUnderLiveFloodAndDirtyRerun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Tiny queue cap + 1 worker to aggressively stress dirty re-runs & live bursts (H1).
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{
		NumWorkers:    1,
		BackfillCap:   2,
		BatchSize:     4,
		FlushInterval: 20 * time.Millisecond,
	})
	pipe.Start(ctx)
	defer pipe.Close()

	var paths []string
	for i := 0; i < 20; i++ {
		p := filepath.Join(dir, fmt.Sprintf("doc_%02d.md", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("# Doc %d\nbody", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	// Hammer EnqueueLive with overlapping re-enqueues of the same files.
	for round := 0; round < 5; round++ {
		for _, p := range paths {
			pipe.EnqueueLive(ctx, p)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := pipe.WaitIdle(waitCtx); err != nil {
		t.Fatalf("WaitIdle deadlocked or timed out: %v", err)
	}

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(paths) {
		t.Fatalf("indexed %d files, want %d", len(entries), len(paths))
	}
}

func TestPipelineMissingRootDoesNotWipeCatalog(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.md"), []byte("# Important\nKeep me."), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{NumWorkers: 2})
	defer pipe.Close()

	if _, err := pipe.IndexWorkspace(ctx); err != nil {
		t.Fatalf("initial scan failed: %v", err)
	}

	// Simulate unmounted/missing root folder (H2).
	moved := filepath.Join(parent, "ws_moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}

	if _, err := pipe.IndexWorkspace(ctx); err == nil {
		t.Fatalf("expected error when workspace root is missing, got nil")
	}

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("missing root wiped catalog! entries=%d, want 1", len(entries))
	}
}

func TestPipelineCancelWithBacklogClosesCleanlyAndDoesNotClobberRows(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 40; i++ {
		p := filepath.Join(dir, fmt.Sprintf("file_%02d.md", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("# File %d\ncontent", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	bg := context.Background()
	cat, err := worker.NewCatalogIndexer(bg, st, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-index file_00 cleanly so we verify cancellation never clobbers it to StatusUnreadable (H8).
	if err := cat.IndexFile(bg, filepath.Join(dir, "file_00.md")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(bg)
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{
		NumWorkers:    1,
		BackfillCap:   64,
		BatchSize:     10,
		FlushInterval: 50 * time.Millisecond,
	})
	pipe.Start(ctx)

	for i := 0; i < 40; i++ {
		pipe.EnqueueLive(ctx, filepath.Join(dir, fmt.Sprintf("file_%02d.md", i)))
	}
	// Cancel immediately while backlog is still queued (H5).
	cancel()

	closed := make(chan error, 1)
	go func() { closed <- pipe.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Close hung after context cancel with backlog")
	}

	meta, err := st.GetFileMeta(bg, cat.Workspace.ID, "file_00.md")
	if err != nil || meta == nil {
		t.Fatalf("GetFileMeta(file_00.md) failed: meta=%+v err=%v", meta, err)
	}
	if meta.IndexStatus != store.StatusOK {
		t.Fatalf("file_00.md status clobbered to %q, want %q", meta.IndexStatus, store.StatusOK)
	}
}

func TestPipelineMidScanLiveUpsertNotPruned(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("# Existing\nhello"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Begin scan 1, index existing.md, then begin scan 2 and simulate a live file indexed
	// with activeScanID=2 while scan 2 is in progress (H7).
	scan1, err := st.BeginWorkspaceScan(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	cat.SetActiveScanID(scan1)
	if err := cat.IndexFile(ctx, filepath.Join(dir, "existing.md")); err != nil {
		t.Fatal(err)
	}

	scan2, err := st.BeginWorkspaceScan(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	cat.SetActiveScanID(scan2)

	// New file arrives mid-scan via live path (scanID=0, picks up activeScanID=scan2).
	livePath := filepath.Join(dir, "created_mid_scan.md")
	if err := os.WriteFile(livePath, []byte("# Mid Scan\nMust survive prune."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.IndexFile(ctx, livePath); err != nil {
		t.Fatal(err)
	}

	// Prune stale rows from scan1 (existing.md was not touched in scan2, created_mid_scan.md WAS).
	pruned, err := st.PruneStaleScanFiles(ctx, cat.Workspace.ID, scan2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].File.RelPath != "created_mid_scan.md" {
		t.Fatalf("mid-scan file should have survived prune; got %+v", entries)
	}
}

func TestPipelineConcurrentFlushAndCloseNeverPanics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\nhi"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{NumWorkers: 2, FlushInterval: 10 * time.Millisecond})
	pipe.Start(ctx)
	pipe.EnqueueLive(ctx, filepath.Join(dir, "a.md"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = pipe.Flush(ctx)
		}
	}()
	if err := pipe.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	<-done
}

func TestPipelineCloseDuringIndexWorkspaceDoesNotWipeCatalog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f_%02d.md", i)), []byte("# Doc\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Seed all 30 files cleanly.
	p1 := worker.NewPipeline(cat, worker.PipelineConfig{NumWorkers: 2})
	if _, err := p1.IndexWorkspace(ctx); err != nil {
		t.Fatal(err)
	}
	_ = p1.Close()

	// Start a second scan and close the pipeline concurrently without cancelling ctx.
	p2 := worker.NewPipeline(cat, worker.PipelineConfig{NumWorkers: 1, BackfillCap: 1})
	p2.Start(ctx)
	go func() {
		time.Sleep(1 * time.Millisecond)
		_ = p2.Close()
	}()
	_, _ = p2.IndexWorkspace(ctx)

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 30 {
		t.Fatalf("Close during IndexWorkspace wiped rows: got %d, want 30", len(entries))
	}
}

func TestWorkerAppendingLargeLogHitsBlobCacheWithoutReparsing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "server.log")

	// Start with a >256 KB log file (header_only 64 KB peek).
	initial := make([]byte, 300*1024)
	for i := range initial {
		initial[i] = 'a'
	}
	copy(initial, "2026-11-21 BOOT ok\n")
	if err := os.WriteFile(logPath, initial, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.IndexFile(ctx, logPath); err != nil {
		t.Fatal(err)
	}
	if hits := cat.Stats.Snapshot().BlobCacheHits; hits != 0 {
		t.Fatalf("initial BlobCacheHits = %d, want 0", hits)
	}

	// Append bytes well past the 64 KB header window and re-index -> size/mtime changed,
	// so fast-path doesn't skip, but head fingerprint matches -> instant blob cache hit!
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n2026-11-21 appended event\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if err := cat.IndexFile(ctx, logPath); err != nil {
		t.Fatal(err)
	}
	if hits := cat.Stats.Snapshot().BlobCacheHits; hits != 1 {
		t.Fatalf("after append BlobCacheHits = %d, want 1", hits)
	}
	blobs, err := st.BlobCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Fatalf("BlobCount = %d, want 1 (no blob churn on large log append)", blobs)
	}
}

func TestWorkerSameBytesDifferentExtensionsGetIndependentCards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Identical bytes under .csv and .txt (Opus M2 probe).
	csvBytes := []byte("Name,Score\nAlice,99\nBob,88\n")
	if err := os.WriteFile(filepath.Join(dir, "a.csv"), csvBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), csvBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a_copy.csv"), csvBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Identical ZIP bytes under .xlsx and .zip.
	xlsxPath := filepath.Join(dir, "report.xlsx")
	createTestZip(t, xlsxPath, map[string]string{
		"xl/workbook.xml": `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets><sheet name="Summary" sheetId="1"/></sheets></workbook>`,
	})
	rawZip, err := os.ReadFile(xlsxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.zip"), rawZip, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Index sequentially so cache hit on a_copy.csv is deterministic.
	for _, name := range []string{"a.csv", "b.txt", "a_copy.csv", "report.xlsx", "report.zip"} {
		if err := cat.IndexFile(ctx, filepath.Join(dir, name)); err != nil {
			t.Fatalf("IndexFile(%s) failed: %v", name, err)
		}
	}

	if hits := cat.Stats.Snapshot().BlobCacheHits; hits != 1 {
		t.Fatalf("BlobCacheHits = %d, want 1 (only a_copy.csv should hit a.csv's blob)", hits)
	}

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]store.FileEntry{}
	for _, e := range entries {
		byPath[e.File.RelPath] = e
	}

	// a.csv and b.txt share the exact same blake3 content hash, yet have independent peeker cards.
	if byPath["a.csv"].File.BLAKE3 != byPath["b.txt"].File.BLAKE3 {
		t.Fatalf("expected a.csv and b.txt to share content blake3")
	}
	if byPath["a.csv"].File.PeekerID != "csv" || byPath["a.csv"].Blob.OutlineJSON == "" {
		t.Fatalf("a.csv got wrong card: %+v", byPath["a.csv"])
	}
	if byPath["b.txt"].File.PeekerID != "text" || byPath["b.txt"].Blob.OutlineJSON != "" {
		t.Fatalf("b.txt inherited CSV outline! got %+v", byPath["b.txt"])
	}

	// report.xlsx and report.zip share the exact same zipcd blake3, yet have independent cards.
	if byPath["report.xlsx"].File.BLAKE3 != byPath["report.zip"].File.BLAKE3 {
		t.Fatalf("expected report.xlsx and report.zip to share zipcd blake3")
	}
	if byPath["report.xlsx"].File.PeekerID != "xlsx" || byPath["report.xlsx"].Blob.Headings != "Summary" {
		t.Fatalf("report.xlsx got wrong card: %+v", byPath["report.xlsx"])
	}
	if byPath["report.zip"].File.PeekerID != "zip" || byPath["report.zip"].Blob.Headings != "" {
		t.Fatalf("report.zip inherited XLSX card! got %+v", byPath["report.zip"])
	}
}
