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

func seedSearchCatalog(t *testing.T, st *store.Store, wsID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	items := []store.UpsertItem{
		{
			File: store.FileRecord{
				WorkspaceID:   wsID,
				RelPath:       "finance/ClientInvoiceMarch2026.pdf",
				Filename:      "ClientInvoiceMarch2026.pdf",
				Folder:        "finance",
				Ext:           ".pdf",
				Size:          4096,
				MtimeNS:       3000,
				BLAKE3:        "h_inv_march",
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "h_inv_march",
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				Title:         "Acme March Invoice",
				Headings:      "Acme Corp Billing",
				BodyPreview:   "Acme quarterly revenue invoice for March services rendered.",
				ExtractedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID:   wsID,
				RelPath:       "finance/q4/Q4Forecast.xlsx",
				Filename:      "Q4Forecast.xlsx",
				Folder:        "finance/q4",
				Ext:           ".xlsx",
				Size:          8192,
				MtimeNS:       5000,
				BLAKE3:        "h_q4",
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "h_q4",
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				Title:         "Q4 Forecast",
				Headings:      "Summary\nAcme Pipeline",
				BodyPreview:   "Projected quarterly growth and revenue targets for Q4.",
				ExtractedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID:   wsID,
				RelPath:       "finance_old/archive.xlsx",
				Filename:      "archive.xlsx",
				Folder:        "finance_old",
				Ext:           ".xlsx",
				Size:          1024,
				MtimeNS:       1000,
				BLAKE3:        "h_old",
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "h_old",
				PeekerID:      "xlsx",
				PeekerVersion: 1,
				IndexDepth:    store.DepthHeaderOnly,
				Title:         "Old Archive",
				BodyPreview:   "Historical numbers of the past.",
				ExtractedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID:   wsID,
				RelPath:       "ops/HTTPServerConfig.yaml",
				Filename:      "HTTPServerConfig.yaml",
				Folder:        "ops",
				Ext:           ".yaml",
				Size:          512,
				MtimeNS:       4000,
				BLAKE3:        "h_http",
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "h_http",
				PeekerID:      "text",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				BodyPreview:   "listen: 127.0.0.1:8080\n",
				ExtractedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID:   wsID,
				RelPath:       "intl/2026請求書.pdf",
				Filename:      "2026請求書.pdf",
				Folder:        "intl",
				Ext:           ".pdf",
				Size:          2048,
				MtimeNS:       2000,
				IndexDepth:    store.DepthMetadataOnly,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
		},
	}

	if err := st.UpsertBatch(ctx, items); err != nil {
		t.Fatalf("seed UpsertBatch failed: %v", err)
	}
}

func TestSearchForgivingORStopwordsMatchAllAndExactPhrase(t *testing.T) {
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
	seedSearchCatalog(t, st, ws.ID)

	// Natural queries with stopwords ("from", "the", "for") must succeed with OR-by-default,
	// and the doc matching BOTH acme + invoice + march must rank #1.
	for _, q := range []string{"invoice from acme", "the acme invoice for march"} {
		res, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: q, Limit: 10})
		if err != nil {
			t.Fatalf("SearchWithOptions(%q) failed: %v", q, err)
		}
		if len(res.Hits) == 0 {
			t.Fatalf("SearchWithOptions(%q) returned 0 hits", q)
		}
		if res.Hits[0].Filename != "ClientInvoiceMarch2026.pdf" {
			t.Fatalf("query %q top hit = %q, want ClientInvoiceMarch2026.pdf", q, res.Hits[0].Filename)
		}
	}

	// Stopword-only query must fall back to original terms instead of failing or returning empty.
	stopRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "of the", Limit: 10})
	if err != nil {
		t.Fatalf("stopword-only search failed: %v", err)
	}
	if len(stopRes.Hits) == 0 {
		t.Fatalf("expected stopword-only fallback to match 'of the past'")
	}

	// MatchAll (AND) restricts to docs containing ALL terms.
	andRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Query:       "acme invoice",
		MatchAll:    true,
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(andRes.Hits) != 1 || andRes.Hits[0].Filename != "ClientInvoiceMarch2026.pdf" {
		t.Fatalf("MatchAll('acme invoice') = %+v, want only ClientInvoiceMarch2026.pdf", andRes.Hits)
	}

	// MatchAll with a short (<3 rune) term ("Q4") must NOT let trigram leak non-Q4 invoices.
	shortAndRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Query:       "Q4 invoice",
		MatchAll:    true,
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shortAndRes.Hits) != 0 {
		t.Fatalf("MatchAll('Q4 invoice') leaked hits: %+v", shortAndRes.Hits)
	}

	// ExactPhrase must match adjacent "quarterly revenue" (ClientInvoiceMarch2026) and exclude
	// "quarterly growth and revenue" (Q4Forecast).
	phraseRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		ExactPhrase: "quarterly revenue",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(phraseRes.Hits) != 1 || phraseRes.Hits[0].Filename != "ClientInvoiceMarch2026.pdf" {
		t.Fatalf("ExactPhrase('quarterly revenue') = %+v, want only ClientInvoiceMarch2026.pdf", phraseRes.Hits)
	}
}

func TestSearchCamelCaseAcronymsTrigramCJKAndDeleteSync(t *testing.T) {
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
	seedSearchCatalog(t, st, ws.ID)

	// Acronym + camelCase splitting: HTTPServerConfig.yaml found by "http" and "server".
	res, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "http server", MatchAll: true})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Filename != "HTTPServerConfig.yaml" {
		t.Fatalf("camelCase/acronym search = %+v, err=%v", res.Hits, err)
	}

	// Compact code preservation: Q4Forecast.xlsx found by "Q4" and "forecast".
	q4Res, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "Q4 forecast", MatchAll: true})
	if err != nil || len(q4Res.Hits) != 1 || q4Res.Hits[0].Filename != "Q4Forecast.xlsx" {
		t.Fatalf("Q4 forecast search = %+v, err=%v", q4Res.Hits, err)
	}

	// Mid-word substring match via files_trigram: "voice" matches ClientInvoiceMarch2026.pdf.
	subRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "voice"})
	if err != nil || len(subRes.Hits) != 1 || subRes.Hits[0].Filename != "ClientInvoiceMarch2026.pdf" {
		t.Fatalf("trigram substring 'voice' = %+v, err=%v", subRes.Hits, err)
	}
	if subRes.Hits[0].MatchSource != "trigram" {
		t.Fatalf("MatchSource = %q, want trigram", subRes.Hits[0].MatchSource)
	}

	// CJK filename match via files_trigram: "請求書" matches intl/2026請求書.pdf.
	cjkRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "請求書"})
	if err != nil || len(cjkRes.Hits) != 1 || cjkRes.Hits[0].Filename != "2026請求書.pdf" {
		t.Fatalf("CJK trigram search = %+v, err=%v", cjkRes.Hits, err)
	}

	// Deleting the CJK file must also clean up files_trigram (no ghost hits).
	if err := st.DeleteFile(ctx, ws.ID, "intl/2026請求書.pdf"); err != nil {
		t.Fatal(err)
	}
	afterDel, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "請求書"})
	if err != nil || len(afterDel.Hits) != 0 {
		t.Fatalf("expected 0 hits after delete, got %+v (err=%v)", afterDel.Hits, err)
	}
}

func TestSearchStructuredFiltersRecencyAndTruncation(t *testing.T) {
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
	seedSearchCatalog(t, st, ws.ID)

	// Filter-only query by ext ("xlsx" without leading dot) sorted by mtime_ns DESC.
	extRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Ext:         "xlsx",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(extRes.Hits) != 2 || extRes.Hits[0].Filename != "Q4Forecast.xlsx" || extRes.Hits[1].Filename != "archive.xlsx" {
		t.Fatalf("filter-only xlsx recency = %+v", extRes.Hits)
	}
	if extRes.Hits[0].MatchSource != "filter" || extRes.Truncated {
		t.Fatalf("unexpected metadata: source=%q truncated=%v", extRes.Hits[0].MatchSource, extRes.Truncated)
	}

	// Truncation check: Limit=1 over 2 xlsx files sets Truncated=true.
	truncRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Ext:         ".xlsx",
		Limit:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(truncRes.Hits) != 1 || !truncRes.Truncated {
		t.Fatalf("expected 1 hit + Truncated=true, got len=%d truncated=%v", len(truncRes.Hits), truncRes.Truncated)
	}

	// Folder prefix filter: "finance" matches "finance" and "finance/q4", NOT "finance_old".
	folderRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Folder:      "finance",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folderRes.Hits) != 2 {
		t.Fatalf("folder='finance' returned %d hits, want 2: %+v", len(folderRes.Hits), folderRes.Hits)
	}

	// Mtime range + IndexDepth filter.
	rangeRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		MtimeAfter:  2500,
		MtimeBefore: 4500,
		IndexDepth:  store.DepthFull,
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rangeRes.Hits) != 2 || rangeRes.Hits[0].Filename != "HTTPServerConfig.yaml" || rangeRes.Hits[1].Filename != "ClientInvoiceMarch2026.pdf" {
		t.Fatalf("mtime+depth filter = %+v", rangeRes.Hits)
	}

	// Terms slice merged with Query.
	termsRes, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Terms:       []string{"listen", "8080"},
		MatchAll:    true,
		Limit:       10,
	})
	if err != nil || len(termsRes.Hits) != 1 || termsRes.Hits[0].Filename != "HTTPServerConfig.yaml" {
		t.Fatalf("Terms search = %+v, err=%v", termsRes.Hits, err)
	}
}

func TestStoreMigratesV3ToV4(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy_v3.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	v3DDL := `
	PRAGMA foreign_keys = ON;
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
		UNIQUE(workspace_id, rel_path),
		FOREIGN KEY (blake3, peeker_id, peeker_version)
			REFERENCES content_blobs(blake3, peeker_id, peeker_version)
	);
	CREATE VIRTUAL TABLE files_fts USING fts5(filename, headings, folder, body, tokenize='porter unicode61 remove_diacritics 2');
	INSERT INTO workspaces (id, root_path, added_at) VALUES (1, '/tmp/ws', 100);
	INSERT INTO content_blobs (blake3, peeker_id, peeker_version, index_depth, title, body_preview, extracted_at)
		VALUES ('h1', 'text', 1, 'full', 'Doc', 'body text', 100);
	INSERT INTO files (id, workspace_id, rel_path, filename, folder, ext, size, mtime_ns, blake3, peeker_id, peeker_version, index_depth, index_status, indexed_at)
		VALUES (11, 1, 'finance/ClientInvoiceMarch2026.txt', 'ClientInvoiceMarch2026.txt', 'finance', '.txt', 9, 100, 'h1', 'text', 1, 'full', 'ok', 100);
	INSERT INTO files (id, workspace_id, rel_path, filename, folder, ext, size, mtime_ns, blake3, peeker_id, peeker_version, index_depth, index_status, indexed_at)
		VALUES (12, 1, 'intl/2026請求書.pdf', '2026請求書.pdf', 'intl', '.pdf', 9, 100, NULL, '', 0, 'metadata_only', 'ok', 100);
	-- Old v3 files_fts did NOT split camelCase:
	INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (11, 'ClientInvoiceMarch2026 txt', 'Doc', 'finance', 'body text');
	INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (12, '2026請求書 pdf', '', 'intl', '');
	PRAGMA user_version = 3;
	`
	if _, err := rawDB.Exec(v3DDL); err != nil {
		t.Fatalf("seed v3 DB failed: %v", err)
	}
	_ = rawDB.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open(v3 DB) failed: %v", err)
	}

	// Rebuilt files_fts must now find camelCase token "invoice" and year "2026".
	invRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: 1, Query: "invoice", MatchAll: true})
	if err != nil || len(invRes.Hits) != 1 || invRes.Hits[0].FileID != 11 {
		t.Fatalf("migrated camelCase search failed: hits=%+v err=%v", invRes.Hits, err)
	}

	// Backfilled files_trigram must find substring "voice" and CJK "請求書".
	subRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: 1, Query: "voice"})
	if err != nil || len(subRes.Hits) != 1 || subRes.Hits[0].FileID != 11 {
		t.Fatalf("migrated trigram substring search failed: hits=%+v err=%v", subRes.Hits, err)
	}
	cjkRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: 1, Query: "請求書"})
	if err != nil || len(cjkRes.Hits) != 1 || cjkRes.Hits[0].FileID != 12 {
		t.Fatalf("migrated trigram CJK search failed: hits=%+v err=%v", cjkRes.Hits, err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-opening must be idempotent.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("second Open after v3->v4 migration failed: %v", err)
	}
	_ = st2.Close()
}

func TestSearchReviewEdgeCases(t *testing.T) {
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

	now := time.Now().Unix()
	if err := st.UpsertBatch(ctx, []store.UpsertItem{
		{
			// Body contains raw CamelCase & letter+digit identifiers.
			File: store.FileRecord{
				WorkspaceID:   ws.ID,
				RelPath:       "notes/spec.md",
				Filename:      "spec.md",
				Folder:        "notes",
				Ext:           ".md",
				Size:          100,
				MtimeNS:       1000,
				BLAKE3:        "h_spec",
				PeekerID:      "md",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				IndexStatus:   store.StatusOK,
				IndexedAt:     now,
			},
			Blob: &store.ContentBlob{
				BLAKE3:        "h_spec",
				PeekerID:      "md",
				PeekerVersion: 1,
				IndexDepth:    store.DepthFull,
				BodyPreview:   "References ClientInvoice and March2026 inside body text.",
				ExtractedAt:   now,
			},
		},
		{
			// Plural acronym filename (MyPDFs.txt) + Export_final_v2.xlsx + Indic script filename.
			File: store.FileRecord{
				WorkspaceID: ws.ID,
				RelPath:     "docs/MyPDFs.txt",
				Filename:    "MyPDFs.txt",
				Folder:      "docs",
				Ext:         ".txt",
				Size:        10,
				MtimeNS:     2000,
				IndexDepth:  store.DepthMetadataOnly,
				IndexStatus: store.StatusOK,
				IndexedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID: ws.ID,
				RelPath:     "finance/Export_final_v2.xlsx",
				Filename:    "Export_final_v2.xlsx",
				Folder:      "finance",
				Ext:         ".xlsx",
				Size:        10,
				MtimeNS:     3000,
				IndexDepth:  store.DepthMetadataOnly,
				IndexStatus: store.StatusOK,
				IndexedAt:   now,
			},
		},
		{
			File: store.FileRecord{
				WorkspaceID: ws.ID,
				RelPath:     "intl/हिन्दी_रिपोर्ट.txt",
				Filename:    "हिन्दी_रिपोर्ट.txt",
				Folder:      "intl",
				Ext:         ".txt",
				Size:        10,
				MtimeNS:     4000,
				IndexDepth:  store.DepthMetadataOnly,
				IndexStatus: store.StatusOK,
				IndexedAt:   now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Body camelCase + letter-digit under MatchAll must find notes/spec.md.
	for _, q := range []string{"ClientInvoice", "March2026"} {
		res, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: q, MatchAll: true})
		if err != nil || len(res.Hits) != 1 || res.Hits[0].RelPath != "notes/spec.md" {
			t.Fatalf("MatchAll(%q) on body = %+v, err=%v", q, res.Hits, err)
		}
	}

	// 2. Punctuation-only query must NOT dump the whole catalog.
	for _, q := range []string{"!!!", "-", "()", "---"} {
		res, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: q})
		if err != nil || len(res.Hits) != 0 {
			t.Fatalf("punctuation-only query %q returned %d hits, want 0 (err=%v)", q, len(res.Hits), err)
		}
	}

	// 3. Plural acronym MyPDFs.txt must match "pdf" in FTS5 (Porter stems pdfs -> pdf).
	pdfRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "pdf", MatchAll: true})
	if err != nil || len(pdfRes.Hits) != 1 || pdfRes.Hits[0].RelPath != "docs/MyPDFs.txt" {
		t.Fatalf("plural acronym search 'pdf' = %+v, err=%v", pdfRes.Hits, err)
	}

	// 4. Indic script with combining marks must stay intact and match cleanly.
	indicRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "रिपोर्ट", MatchAll: true})
	if err != nil || len(indicRes.Hits) != 1 || indicRes.Hits[0].RelPath != "intl/हिन्दी_रिपोर्ट.txt" {
		t.Fatalf("Indic search = %+v, err=%v", indicRes.Hits, err)
	}

	// 5. Punctuation-only ExactPhrase must NOT be silently dropped when Query is set.
	badPhrase, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: ws.ID,
		Query:       "ClientInvoice",
		ExactPhrase: "---",
	})
	if err != nil || len(badPhrase.Hits) != 0 {
		t.Fatalf("ExactPhrase='---' should return 0 hits, got %+v (err=%v)", badPhrase.Hits, err)
	}

	// 6. Raw literal substring with underscore ("Export_fin") matches Export_final_v2.xlsx via trigram.
	litRes, err := st.SearchWithOptions(ctx, store.SearchOptions{WorkspaceID: ws.ID, Query: "Export_fin"})
	if err != nil || len(litRes.Hits) == 0 || litRes.Hits[0].RelPath != "finance/Export_final_v2.xlsx" {
		t.Fatalf("literal substring 'Export_fin' = %+v, err=%v", litRes.Hits, err)
	}
}
