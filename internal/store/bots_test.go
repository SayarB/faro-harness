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

func TestStoreMigratesV4ToV5AndIsolatesBotWorkspacesFromDefaultSearch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v4.db")

	// Seed a v4 database directly to verify v4 -> v5 migration.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw v4 db: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			root_path TEXT NOT NULL UNIQUE,
			added_at INTEGER NOT NULL,
			last_scan_at INTEGER NOT NULL DEFAULT 0,
			current_scan_id INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE content_blobs (
			blake3 TEXT NOT NULL,
			peeker_id TEXT NOT NULL,
			peeker_version INTEGER NOT NULL,
			index_depth TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			headings TEXT NOT NULL DEFAULT '',
			body_preview TEXT NOT NULL DEFAULT '',
			outline_json TEXT NOT NULL DEFAULT '',
			unit_count INTEGER NOT NULL DEFAULT 0,
			extracted_at INTEGER NOT NULL,
			PRIMARY KEY (blake3, peeker_id, peeker_version)
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
			blake3 TEXT,
			peeker_id TEXT NOT NULL DEFAULT '',
			peeker_version INTEGER NOT NULL DEFAULT 0,
			index_depth TEXT NOT NULL,
			index_status TEXT NOT NULL,
			error_msg TEXT NOT NULL DEFAULT '',
			indexed_at INTEGER NOT NULL,
			last_seen_scan_id INTEGER NOT NULL DEFAULT 0,
			UNIQUE(workspace_id, rel_path)
		);
		CREATE VIRTUAL TABLE files_fts USING fts5(filename, headings, folder, body, tokenize='porter unicode61 remove_diacritics 2', prefix='3');
		CREATE VIRTUAL TABLE files_trigram USING fts5(filename, folder, tokenize='trigram');
		PRAGMA user_version = 4;
	`)
	if err != nil {
		_ = rawDB.Close()
		t.Fatalf("seed v4 schema: %v", err)
	}
	_ = rawDB.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open migrated v4->v5: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	userWS, err := st.EnsureWorkspace(ctx, filepath.Join(t.TempDir(), "user-docs"))
	if err != nil {
		t.Fatalf("EnsureWorkspace user: %v", err)
	}
	if userWS.Kind != store.WorkspaceKindUser {
		t.Fatalf("expected default workspace kind %q, got %q", store.WorkspaceKindUser, userWS.Kind)
	}

	// Manually insert a kind='bot' workspace + file to confirm default Search excludes it.
	botWSRoot := filepath.Join(t.TempDir(), "bot-home")
	botWS, err := st.EnsureWorkspace(ctx, botWSRoot)
	if err != nil {
		t.Fatalf("EnsureWorkspace bot: %v", err)
	}
	// Mark botWS as kind='bot' directly to simulate an indexed bot folder.
	rawCheck, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db for kind update: %v", err)
	}
	if _, err := rawCheck.Exec(`UPDATE workspaces SET kind = 'bot', bot_id = 'bot_1' WHERE id = ?`, botWS.ID); err != nil {
		_ = rawCheck.Close()
		t.Fatalf("mark bot workspace kind: %v", err)
	}
	_ = rawCheck.Close()

	now := time.Now().Unix()
	if err := st.UpsertBatch(ctx, []store.UpsertItem{
		{
			File: store.FileRecord{
				WorkspaceID: userWS.ID, RelPath: "Acme_Invoice.md", Filename: "Acme_Invoice.md", Folder: ".", Ext: ".md",
				Size: 32, MtimeNS: now, BLAKE3: "h_user", PeekerID: "md", PeekerVersion: 1, IndexDepth: store.DepthFull, IndexStatus: store.StatusOK, IndexedAt: now,
			},
			Blob: &store.ContentBlob{
				BLAKE3: "h_user", PeekerID: "md", PeekerVersion: 1, IndexDepth: store.DepthFull, BodyPreview: "Acme real user invoice", ExtractedAt: now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID: botWS.ID, RelPath: "memory.md", Filename: "memory.md", Folder: ".", Ext: ".md",
				Size: 32, MtimeNS: now, BLAKE3: "h_bot", PeekerID: "md", PeekerVersion: 1, IndexDepth: store.DepthFull, IndexStatus: store.StatusOK, IndexedAt: now,
			},
			Blob: &store.ContentBlob{
				BLAKE3: "h_bot", PeekerID: "md", PeekerVersion: 1, IndexDepth: store.DepthFull, BodyPreview: "Acme bot private note", ExtractedAt: now,
			},
		},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	hits, err := st.Search(ctx, 0, "Acme", 10)
	if err != nil {
		t.Fatalf("Search default: %v", err)
	}
	if len(hits) != 1 || hits[0].WorkspaceID != userWS.ID {
		t.Fatalf("expected default search to return only user workspace hit, got %+v", hits)
	}
}
