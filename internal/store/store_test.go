package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/store"
	_ "modernc.org/sqlite"
)

func TestStoreUpsertSearchRankingAndDedup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	defer st.Close()

	ws, err := st.EnsureWorkspace(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("EnsureWorkspace failed: %v", err)
	}

	now := time.Now().Unix()
	sharedBlob := &store.ContentBlob{
		BLAKE3:        "deadbeef01",
		PeekerID:      "xlsx",
		PeekerVersion: 1,
		IndexDepth:    store.DepthHeaderOnly,
		Title:         "Quarterly Revenue",
		Headings:      "Summary\nQ3 Forecast",
		BodyPreview:   "Revenue grew 22% across enterprise accounts in Q3.",
		OutlineJSON:   `{"type":"xlsx","sheets":["Summary","Q3 Forecast"]}`,
		UnitCount:     2,
		ExtractedAt:   now,
	}

	err = st.UpsertBatch(ctx, []store.UpsertItem{
		{
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "finance/Export_final_v2.xlsx",
				Filename:      "Export_final_v2.xlsx",
				Folder:        "finance",
				Ext:           ".xlsx",
				Size:          12000,
				MtimeNS:       1000,
				BLAKE3:        sharedBlob.BLAKE3,
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: sharedBlob,
		},
		{
			// Duplicate file pointing at the exact same (BLAKE3, PeekerID, PeekerVersion) blob.
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "archive/revenue_backup.xlsx",
				Filename:      "revenue_backup.xlsx",
				Folder:        "archive",
				Ext:           ".xlsx",
				Size:          12000,
				MtimeNS:       1001,
				BLAKE3:        sharedBlob.BLAKE3,
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: nil,
		},
		{
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "notes/meeting.md",
				Filename:      "meeting.md",
				Folder:        "notes",
				Ext:           ".md",
				Size:          500,
				MtimeNS:       1002,
				BLAKE3:        "cafebabe02",
				PeekerID:      "md",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "cafebabe02",
				PeekerID:      "md",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				Title:         "Weekly Sync",
				Headings:      "Weekly Sync",
				BodyPreview:   "Discussed the final Q3 revenue numbers with sales.",
				UnitCount:     10,
				ExtractedAt:   now,
			},
		},
	})
	if err != nil {
		t.Fatalf("UpsertBatch failed: %v", err)
	}

	// Verify deduplication: 3 files, only 2 blobs.
	blobCount, err := st.BlobCount(ctx)
	if err != nil {
		t.Fatalf("BlobCount failed: %v", err)
	}
	if blobCount != 2 {
		t.Fatalf("BlobCount = %d, want 2", blobCount)
	}

	// Verify separator normalization: "final" hits "Export_final_v2.xlsx" ranked above body-only hit "meeting.md".
	hits, err := st.Search(ctx, ws.ID, "final", 10)
	if err != nil {
		t.Fatalf("Search(final) failed: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search(final) returned %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Filename != "Export_final_v2.xlsx" {
		t.Fatalf("top hit = %q, want Export_final_v2.xlsx (filename weight > body weight)", hits[0].Filename)
	}

	// Verify heading/title ranking & cache hit indexing for revenue_backup.xlsx.
	revHits, err := st.Search(ctx, ws.ID, "revenue", 10)
	if err != nil {
		t.Fatalf("Search(revenue) failed: %v", err)
	}
	if len(revHits) != 3 {
		t.Fatalf("Search(revenue) returned %d hits, want 3", len(revHits))
	}
	if revHits[0].Filename != "revenue_backup.xlsx" {
		t.Fatalf("top revenue hit = %q, want revenue_backup.xlsx", revHits[0].Filename)
	}

	// Verify delete + orphan blob GC.
	if err := st.DeleteFile(ctx, ws.ID, "notes/meeting.md"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	removed, err := st.GCOrphanBlobs(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("GCOrphanBlobs failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("GCOrphanBlobs removed %d, want 1", removed)
	}
}

func TestStoreDeletePathPrefixAndScanGenerationPruning(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	ws, err := st.EnsureWorkspace(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	scan1, err := st.BeginWorkspaceScan(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	makeItem := func(rel string, scanID int64) store.UpsertItem {
		return store.UpsertItem{
			File: store.FileRecord{
				WorkspaceID:    ws.ID,
				RelPath:        rel,
				Filename:       rel,
				Folder:         "root",
				Ext:            ".md",
				Size:           10,
				MtimeNS:        100,
				IndexDepth:     store.DepthMetadataOnly,
				IndexStatus:    store.StatusOK,
				IndexedAt:      now,
				LastSeenScanID: scanID,
			},
		}
	}

	if err := st.UpsertBatch(ctx, []store.UpsertItem{
		makeItem("clients_a/acme/one.md", scan1),
		makeItem("clients_a/two.md", scan1),
		makeItem("Clients_A/case_keep.md", scan1),
		makeItem("clients_a_other/keep.md", scan1),
		makeItem("restricted/secret.md", scan1),
	}); err != nil {
		t.Fatal(err)
	}

	// DeletePath on directory "clients_a" must be case-sensitive and must NOT touch
	// "Clients_A/case_keep.md" or "clients_a_other/keep.md".
	n, err := st.DeletePath(ctx, ws.ID, "clients_a")
	if err != nil {
		t.Fatalf("DeletePath failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeletePath removed %d rows, want 2", n)
	}

	// Start scan 2: touch "clients_a_other/keep.md" and "Clients_A/case_keep.md", skip pruning "restricted", add a stale file.
	scan2, err := st.BeginWorkspaceScan(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(ctx, []store.UpsertItem{makeItem("old/deleted.md", scan1)}); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchFileScanBatch(ctx, ws.ID, scan2, []string{"clients_a_other/keep.md", "Clients_A/case_keep.md"}); err != nil {
		t.Fatal(err)
	}

	pruned, err := st.PruneStaleScanFiles(ctx, ws.ID, scan2, []string{"restricted"})
	if err != nil {
		t.Fatalf("PruneStaleScanFiles failed: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only old/deleted.md)", pruned)
	}

	entries, err := st.ListFiles(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("remaining entries = %d, want 3 (keep.md + case_keep.md + restricted/secret.md)", len(entries))
	}
}

func TestStoreCompositeBlobKeyIsolatesPeekersVersionsAndGC(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ws, err := st.EnsureWorkspace(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const sameHash = "samecontenthash001"
	now := time.Now().Unix()

	// Insert same blake3 under two different peekers ("csv" v1 and "text" v1).
	if err := st.UpsertBatch(ctx, []store.UpsertItem{
		{
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "a.csv",
				Filename:      "a.csv",
				Ext:           ".csv",
				Size:          20,
				MtimeNS:       1,
				BLAKE3:        sameHash,
				PeekerID:      "csv",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        sameHash,
				PeekerID:      "csv",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				Headings:      "ColA\nColB",
				OutlineJSON:   `{"type":"csv","columns":["ColA","ColB"]}`,
				ExtractedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "b.txt",
				Filename:      "b.txt",
				Ext:           ".txt",
				Size:          20,
				MtimeNS:       1,
				BLAKE3:        sameHash,
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        sameHash,
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				BodyPreview:   "ColA,ColB\n1,2",
				ExtractedAt:   now,
			},
		},
	}); err != nil {
		t.Fatalf("UpsertBatch failed: %v", err)
	}

	bCSV, err := st.GetBlob(ctx, sameHash, "csv", 1)
	if err != nil || bCSV == nil || bCSV.OutlineJSON == "" {
		t.Fatalf("GetBlob(csv,1) = %+v, err=%v", bCSV, err)
	}
	bTXT, err := st.GetBlob(ctx, sameHash, "text", 1)
	if err != nil || bTXT == nil || bTXT.OutlineJSON != "" {
		t.Fatalf("GetBlob(text,1) = %+v, err=%v", bTXT, err)
	}
	bMiss, err := st.GetBlob(ctx, sameHash, "csv", 2)
	if err != nil || bMiss != nil {
		t.Fatalf("GetBlob(csv,2) = %+v, err=%v, want nil", bMiss, err)
	}

	// Upgrade a.csv to peeker_version=2; old (sameHash, "csv", 1) becomes orphaned and GC'd
	// while (sameHash, "csv", 2) and (sameHash, "text", 1) remain untouched.
	if err := st.UpsertBatch(ctx, []store.UpsertItem{
		{
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "a.csv",
				Filename:      "a.csv",
				Ext:           ".csv",
				Size:          20,
				MtimeNS:       1,
				BLAKE3:        sameHash,
				PeekerID:      "csv",
				PeekerVersion: 2,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        sameHash,
				PeekerID:      "csv",
				PeekerVersion: 2,
				IndexDepth:    store.DepthFull,
				Headings:      "ColA\nColB",
				OutlineJSON:   `{"type":"csv","version":2}`,
				ExtractedAt:   now,
			},
		},
	}); err != nil {
		t.Fatalf("upgrade upsert failed: %v", err)
	}

	blobsBeforeGC, _ := st.BlobCount(ctx)
	if blobsBeforeGC != 3 {
		t.Fatalf("BlobCount before GC = %d, want 3", blobsBeforeGC)
	}

	removed, err := st.GCOrphanBlobs(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("GCOrphanBlobs failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("GCOrphanBlobs removed %d, want 1 (only old csv v1)", removed)
	}
	if old, _ := st.GetBlob(ctx, sameHash, "csv", 1); old != nil {
		t.Fatalf("old (csv,1) blob should have been GC'd")
	}
	if v2, _ := st.GetBlob(ctx, sameHash, "csv", 2); v2 == nil {
		t.Fatalf("(csv,2) blob should still exist")
	}
	if txt, _ := st.GetBlob(ctx, sameHash, "text", 1); txt == nil {
		t.Fatalf("(text,1) blob should still exist")
	}
}

func TestStoreMigratesV2ToV3(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy_v2.db")

	// Seed a realistic v2 database manually.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	v2DDL := `
	PRAGMA foreign_keys = ON;
	CREATE TABLE workspaces (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_path TEXT NOT NULL UNIQUE,
		added_at INTEGER NOT NULL,
		last_scan_at INTEGER NOT NULL DEFAULT 0,
		current_scan_id INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE content_blobs (
		blake3 TEXT PRIMARY KEY,
		peeker_version INTEGER NOT NULL,
		index_depth TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		headings TEXT NOT NULL DEFAULT '',
		body_preview TEXT NOT NULL DEFAULT '',
		outline_json TEXT NOT NULL DEFAULT '',
		unit_count INTEGER NOT NULL DEFAULT 0,
		extracted_at INTEGER NOT NULL
	);
	CREATE TABLE files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		rel_path TEXT NOT NULL,
		filename TEXT NOT NULL,
		folder TEXT NOT NULL,
		ext TEXT NOT NULL,
		size INTEGER NOT NULL,
		mtime_ns INTEGER NOT NULL,
		blake3 TEXT REFERENCES content_blobs(blake3),
		peeker_version INTEGER NOT NULL DEFAULT 0,
		index_depth TEXT NOT NULL,
		index_status TEXT NOT NULL,
		error_msg TEXT NOT NULL DEFAULT '',
		indexed_at INTEGER NOT NULL,
		last_seen_scan_id INTEGER NOT NULL DEFAULT 0,
		UNIQUE(workspace_id, rel_path)
	);
	CREATE VIRTUAL TABLE files_fts USING fts5(filename, headings, folder, body, tokenize='porter unicode61 remove_diacritics 2');
	INSERT INTO workspaces (id, root_path, added_at) VALUES (1, '/tmp/ws', 100);
	INSERT INTO content_blobs (blake3, peeker_version, index_depth, title, headings, body_preview, extracted_at)
		VALUES ('abc123', 1, 'full', 'Legacy Note', 'Legacy Note', 'hello legacy world', 100);
	INSERT INTO files (id, workspace_id, rel_path, filename, folder, ext, size, mtime_ns, blake3, peeker_version, index_depth, index_status, indexed_at)
		VALUES (7, 1, 'note.md', 'note.md', '', '.md', 18, 100, 'abc123', 1, 'full', 'ok', 100);
	INSERT INTO files (id, workspace_id, rel_path, filename, folder, ext, size, mtime_ns, blake3, peeker_version, index_depth, index_status, indexed_at)
		VALUES (8, 1, 'pic.png', 'pic.png', '', '.png', 10, 100, NULL, 0, 'metadata_only', 'ok', 100);
	INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (7, 'note md', 'Legacy Note', '', 'hello legacy world');
	INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (8, 'pic png', '', '', '');
	PRAGMA user_version = 2;
	`
	if _, err := rawDB.Exec(v2DDL); err != nil {
		t.Fatalf("seed v2 DB failed: %v", err)
	}
	_ = rawDB.Close()

	// Opening with store.Open must migrate v2 -> v3 cleanly while preserving rows & FTS alignment.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open(v2 DB) failed: %v", err)
	}
	entries, err := st.ListFiles(ctx, 1)
	if err != nil {
		t.Fatalf("ListFiles after v2->v3 failed: %v", err)
	}
	if len(entries) != 2 || entries[0].File.ID != 7 || entries[0].Blob == nil || entries[0].Blob.Title != "Legacy Note" {
		t.Fatalf("unexpected migrated entries: %+v", entries)
	}
	hits, err := st.Search(ctx, 1, "legacy", 5)
	if err != nil || len(hits) != 1 || hits[0].FileID != 7 {
		t.Fatalf("Search after v2->v3 failed: hits=%+v err=%v", hits, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-opening must be idempotent.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("second Open after migration failed: %v", err)
	}
	_ = st2.Close()
}
