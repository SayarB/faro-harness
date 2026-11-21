package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SayarB/faro-harness/internal/store"
)

func TestIndexerCrawlAndDedupe(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	content := []byte("# Acme Renewal\n\nLate cancellation penalty is $15,000.\n")
	if err := os.MkdirAll(filepath.Join(ws, "Clients", "Acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "Clients", "Acme", "contract.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// Duplicate file in another folder should share one content-addressed blob.
	if err := os.WriteFile(filepath.Join(ws, "contract_copy.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// Ignored Office lock file should not be indexed.
	if err := os.WriteFile(filepath.Join(ws, "~$budget.xlsx"), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "faro.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	idx := NewIndexer(st, 2, nil)
	prog, err := idx.IndexWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("index workspace: %v", err)
	}
	if prog.Indexed != 2 || prog.Errors != 0 {
		t.Fatalf("expected 2 indexed files, got %+v", prog)
	}

	// Second pass must skip both unchanged files.
	prog2, err := idx.IndexWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if prog2.Indexed != 0 || prog2.Skipped != 2 {
		t.Fatalf("expected 2 skipped files on second run, got %+v", prog2)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalFiles != 2 || stats.TotalBlobs != 1 {
		t.Fatalf("expected 2 files sharing 1 deduplicated blob, got %+v", stats)
	}

	hits, err := st.Search(ctx, "cancellation penalty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 FTS hits for 'cancellation penalty', got %d", len(hits))
	}

	hits, err = st.Search(ctx, "$15,000", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 FTS hits for '$15,000', got %d", len(hits))
	}
}
