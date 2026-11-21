package store

import (
	"context"
	"testing"
)

func TestStoreUpsertAndFTSSearch(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	_, err = st.UpsertIndexedFile(ctx, FileRecord{
		Workspace:   "/ws",
		AbsPath:     "/ws/Clients/Acme/Q3-Sales.xlsx",
		RelPath:     "Clients/Acme/Q3-Sales.xlsx",
		Folder:      "Clients/Acme",
		Filename:    "Q3-Sales.xlsx",
		Ext:         ".xlsx",
		Size:        1024,
		MtimeUnixNs: 100,
		ContentHash: "hash-acme",
		IndexDepth:  DepthHeaderOnly,
		Status:      "indexed",
	}, &BlobRecord{
		ContentHash:      "hash-acme",
		ExtractorVersion: 1,
		IndexDepth:       DepthHeaderOnly,
		Headings:         "Summary Transactions Revenue",
		Body:             "Sheets: Summary, Transactions",
		OutlineJSON:      `[{"title":"Summary"}]`,
	})
	if err != nil {
		t.Fatalf("upsert acme: %v", err)
	}

	_, err = st.UpsertIndexedFile(ctx, FileRecord{
		Workspace:   "/ws",
		AbsPath:     "/ws/Notes/random.md",
		RelPath:     "Notes/random.md",
		Folder:      "Notes",
		Filename:    "random.md",
		Ext:         ".md",
		Size:        256,
		MtimeUnixNs: 200,
		ContentHash: "hash-note",
		IndexDepth:  DepthFull,
		Status:      "indexed",
	}, &BlobRecord{
		ContentHash:      "hash-note",
		ExtractorVersion: 1,
		IndexDepth:       DepthFull,
		Headings:         "Meeting Notes",
		Body:             "Discussed cancellation fee of 15000 with legal team",
		OutlineJSON:      `[]`,
	})
	if err != nil {
		t.Fatalf("upsert note: %v", err)
	}

	// Punctuation like '-' must not cause an FTS5 NOT syntax error.
	hits, err := st.Search(ctx, "Q3-Sales", 10)
	if err != nil {
		t.Fatalf("search Q3-Sales: %v", err)
	}
	if len(hits) != 1 || hits[0].Filename != "Q3-Sales.xlsx" {
		t.Fatalf("expected Q3-Sales.xlsx, got %+v", hits)
	}

	// Porter stemming + snippet extraction.
	hits, err = st.Search(ctx, "cancelling 15000", 10)
	if err != nil {
		t.Fatalf("search body: %v", err)
	}
	if len(hits) != 1 || hits[0].Filename != "random.md" {
		t.Fatalf("expected random.md, got %+v", hits)
	}
	if hits[0].Snippet == "" {
		t.Fatalf("expected non-empty FTS snippet")
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalFiles != 2 || stats.TotalBlobs != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
