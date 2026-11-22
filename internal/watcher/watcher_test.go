package watcher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/watcher"
	"github.com/SayarB/faro-harness/internal/worker"
)

func TestWatcherTriggersStubIndexerOnFileAdded(t *testing.T) {
	dir := t.TempDir()

	indexed := make(chan string, 4)
	stub := worker.NewStubIndexer(nil, func(_ context.Context, path string) error {
		indexed <- path
		return nil
	})

	w, err := watcher.New(dir, stub, watcher.WithDebounce(25*time.Millisecond))
	if err != nil {
		t.Fatalf("watcher.New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("hello faro"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	select {
	case got := <-indexed:
		if got != target {
			t.Fatalf("indexed path = %q, want %q", got, target)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for stub indexer to trigger for %q", target)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() returned %v, want context.Canceled", err)
	}
}

func TestWatcherIgnoresDirectoriesAndLockFiles(t *testing.T) {
	dir := t.TempDir()

	indexed := make(chan string, 4)
	stub := worker.NewStubIndexer(nil, func(_ context.Context, path string) error {
		indexed <- path
		return nil
	})

	w, err := watcher.New(dir, stub, watcher.WithDebounce(25*time.Millisecond))
	if err != nil {
		t.Fatalf("watcher.New failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Subdirectory and Office lock file should be ignored.
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "~$draft.docx"), []byte("lock"), 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("write .DS_Store: %v", err)
	}

	// Real file added afterwards should be the only one indexed.
	realFile := filepath.Join(dir, "invoice.pdf")
	if err := os.WriteFile(realFile, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}

	select {
	case got := <-indexed:
		if got != realFile {
			t.Fatalf("indexed path = %q, want %q", got, realFile)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for stub indexer on %q", realFile)
	}

	cancel()
	<-done
}

func TestWatcherRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	stub := worker.NewStubIndexer(nil, nil)
	if _, err := watcher.New(file, stub); err == nil {
		t.Fatalf("expected error when watching a regular file, got nil")
	}
}

func TestWatcherDebouncesRapidWrites(t *testing.T) {
	dir := t.TempDir()

	var calls atomic.Int32
	doneCh := make(chan string, 8)
	stub := worker.NewStubIndexer(nil, func(_ context.Context, path string) error {
		calls.Add(1)
		doneCh <- path
		return nil
	})

	w, err := watcher.New(dir, stub, watcher.WithDebounce(120*time.Millisecond), watcher.WithInitialScan(false))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	target := filepath.Join(dir, "draft.md")
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(target, []byte("burst write content"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case got := <-doneCh:
		if got != target {
			t.Fatalf("got %q, want %q", got, target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for debounced event")
	}

	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 coalesced index call, got %d", got)
	}

	cancel()
	<-runDone
}

func TestWatcherEndToEndWithPipelineRecursiveAndDelete(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{
		NumWorkers:    2,
		BatchSize:     4,
		FlushInterval: 30 * time.Millisecond,
	})
	pipe.Start(ctx)
	defer pipe.Close()

	w, err := watcher.New(dir, pipe, watcher.WithDebounce(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	// Add a nested folder and file dynamically.
	sub := filepath.Join(dir, "clients", "acme")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give fsnotify a moment to attach watch to new subdir.
	time.Sleep(80 * time.Millisecond)

	docPath := filepath.Join(sub, "proposal.md")
	if err := os.WriteFile(docPath, []byte("# Acme Proposal\nDeliverables for Q4.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait until FTS5 returns the indexed file.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := st.Search(ctx, cat.Workspace.ID, "Deliverables", 5)
		if err == nil && len(hits) == 1 && hits[0].RelPath == "clients/acme/proposal.md" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	hits, err := st.Search(ctx, cat.Workspace.ID, "Deliverables", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("expected 1 search hit for recursive file, got %d (err=%v)", len(hits), err)
	}

	// Now delete the file and confirm it is removed from the catalog.
	if err := os.Remove(docPath); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := st.ListFiles(ctx, cat.Workspace.ID)
		if err == nil && len(entries) == 0 {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected 0 entries after delete, got %d (err=%v)", len(entries), err)
	}

	cancel()
	<-runDone
}

func TestWatcherDirectoryRenameRemovesGhostRows(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed a nested directory (including a ".bak"-suffixed directory name) BEFORE starting watcher.
	oldDir := filepath.Join(dir, "clients.bak")
	if err := os.MkdirAll(filepath.Join(oldDir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "acme", "proposal.md"), []byte("# Proposal\nQ4 terms.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := worker.NewCatalogIndexer(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{
		NumWorkers:    2,
		BatchSize:     4,
		FlushInterval: 25 * time.Millisecond,
	})
	pipe.Start(ctx)
	defer pipe.Close()

	w, err := watcher.New(dir, pipe, watcher.WithDebounce(25*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	// Wait for initial scan to index clients.bak/acme/proposal.md.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := st.ListFiles(ctx, cat.Workspace.ID)
		if err == nil && len(entries) == 1 && entries[0].File.RelPath == "clients.bak/acme/proposal.md" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Rename directory clients/ -> archive/ (H3).
	newDir := filepath.Join(dir, "archive")
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}

	// Wait until old ghost row is gone and new path is indexed.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := st.ListFiles(ctx, cat.Workspace.ID)
		if err == nil && len(entries) == 1 && entries[0].File.RelPath == "archive/acme/proposal.md" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}

	entries, err := st.ListFiles(ctx, cat.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].File.RelPath != "archive/acme/proposal.md" {
		t.Fatalf("expected only archive/acme/proposal.md after directory rename, got %+v", entries)
	}

	cancel()
	<-runDone
}
