package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// IndexDepth records how deeply a file has been inspected so the agent knows
// whether search hits cover full text or only cheap metadata/headers.
type IndexDepth string

const (
	DepthMetadataOnly IndexDepth = "metadata_only"
	DepthHeaderOnly   IndexDepth = "header_only"
	DepthFull         IndexDepth = "full"
)

// FileRecord mirrors a file on disk (Tier 0 metadata + link to content blob).
type FileRecord struct {
	ID           int64      `json:"id"`
	Workspace    string     `json:"workspace"`
	AbsPath      string     `json:"absPath"`
	RelPath      string     `json:"relPath"`
	Folder       string     `json:"folder"`
	Filename     string     `json:"filename"`
	Ext          string     `json:"ext"`
	Size         int64      `json:"size"`
	MtimeUnixNs  int64      `json:"mtimeUnixNs"`
	ContentHash  string     `json:"contentHash"`
	IndexDepth   IndexDepth `json:"indexDepth"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
	UpdatedAt    int64      `json:"updatedAt"`
}

// BlobRecord stores content-addressed extracted text & structural outline.
type BlobRecord struct {
	ContentHash      string     `json:"contentHash"`
	ExtractorVersion int        `json:"extractorVersion"`
	IndexDepth       IndexDepth `json:"indexDepth"`
	Headings         string     `json:"headings"`
	Body             string     `json:"body"`
	OutlineJSON      string     `json:"outlineJson"`
	CreatedAt        int64      `json:"createdAt"`
}

// SearchHit is returned by FTS5 BM25 queries.
type SearchHit struct {
	FileID      int64      `json:"fileId"`
	AbsPath     string     `json:"absPath"`
	RelPath     string     `json:"relPath"`
	Filename    string     `json:"filename"`
	Folder      string     `json:"folder"`
	Ext         string     `json:"ext"`
	Size        int64      `json:"size"`
	MtimeUnixNs int64      `json:"mtimeUnixNs"`
	IndexDepth  IndexDepth `json:"indexDepth"`
	Snippet     string     `json:"snippet"`
	Score       float64    `json:"score"`
}

// Stats summarizes the current index state for UI status display.
type Stats struct {
	TotalFiles   int64            `json:"totalFiles"`
	TotalBlobs   int64            `json:"totalBlobs"`
	ByDepth      map[string]int64 `json:"byDepth"`
	Workspaces   []string         `json:"workspaces"`
}

// Store wraps an embedded SQLite database (WAL + FTS5).
type Store struct {
	db *sql.DB
}

// Open creates or opens the SQLite database at dbPath and runs schema setup.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer connection keeps WAL writes serialized and eliminates SQLITE_BUSY races.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`PRAGMA busy_timeout = 5000;`,
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE IF NOT EXISTS files (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace     TEXT NOT NULL,
			abs_path      TEXT NOT NULL UNIQUE,
			rel_path      TEXT NOT NULL,
			folder        TEXT NOT NULL,
			filename      TEXT NOT NULL,
			ext           TEXT NOT NULL,
			size          INTEGER NOT NULL,
			mtime_unix_ns INTEGER NOT NULL,
			content_hash  TEXT NOT NULL DEFAULT '',
			index_depth   TEXT NOT NULL DEFAULT 'metadata_only',
			status        TEXT NOT NULL DEFAULT 'pending',
			error_message TEXT NOT NULL DEFAULT '',
			updated_at    INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_files_workspace ON files(workspace);`,
		`CREATE INDEX IF NOT EXISTS idx_files_hash ON files(content_hash);`,
		`CREATE INDEX IF NOT EXISTS idx_files_mtime ON files(mtime_unix_ns DESC);`,
		`CREATE TABLE IF NOT EXISTS blobs (
			content_hash      TEXT NOT NULL,
			extractor_version INTEGER NOT NULL,
			index_depth       TEXT NOT NULL,
			headings          TEXT NOT NULL DEFAULT '',
			body              TEXT NOT NULL DEFAULT '',
			outline_json      TEXT NOT NULL DEFAULT '[]',
			created_at        INTEGER NOT NULL,
			PRIMARY KEY (content_hash, extractor_version)
		);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
			filename,
			folder,
			headings,
			body,
			tokenize = 'porter unicode61'
		);`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate sqlite (%s): %w", stmt[:min(48, len(stmt))], err)
		}
	}
	return nil
}

// GetFileByPath looks up an existing file record so unchanged files can be skipped via (size, mtime).
func (s *Store) GetFileByPath(ctx context.Context, absPath string) (*FileRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, workspace, abs_path, rel_path, folder, filename, ext,
		       size, mtime_unix_ns, content_hash, index_depth, status, error_message, updated_at
		FROM files WHERE abs_path = ?
	`, absPath)

	var f FileRecord
	err := row.Scan(
		&f.ID, &f.Workspace, &f.AbsPath, &f.RelPath, &f.Folder, &f.Filename, &f.Ext,
		&f.Size, &f.MtimeUnixNs, &f.ContentHash, &f.IndexDepth, &f.Status, &f.ErrorMessage, &f.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// GetBlob checks if content_hash has already been extracted at extractorVersion.
func (s *Store) GetBlob(ctx context.Context, contentHash string, extractorVersion int) (*BlobRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT content_hash, extractor_version, index_depth, headings, body, outline_json, created_at
		FROM blobs WHERE content_hash = ? AND extractor_version = ?
	`, contentHash, extractorVersion)

	var b BlobRecord
	err := row.Scan(
		&b.ContentHash, &b.ExtractorVersion, &b.IndexDepth, &b.Headings, &b.Body, &b.OutlineJSON, &b.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpsertIndexedFile atomically saves the file record, optional content blob, and FTS5 entry.
func (s *Store) UpsertIndexedFile(ctx context.Context, f FileRecord, blob *BlobRecord) (int64, error) {
	now := time.Now().UnixMilli()
	if f.UpdatedAt == 0 {
		f.UpdatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if blob != nil {
		if blob.CreatedAt == 0 {
			blob.CreatedAt = now
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO blobs (content_hash, extractor_version, index_depth, headings, body, outline_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(content_hash, extractor_version) DO UPDATE SET
				index_depth = excluded.index_depth,
				headings = excluded.headings,
				body = excluded.body,
				outline_json = excluded.outline_json
		`, blob.ContentHash, blob.ExtractorVersion, blob.IndexDepth, blob.Headings, blob.Body, blob.OutlineJSON, blob.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("upsert blob: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO files (workspace, abs_path, rel_path, folder, filename, ext, size, mtime_unix_ns, content_hash, index_depth, status, error_message, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(abs_path) DO UPDATE SET
			workspace = excluded.workspace,
			rel_path = excluded.rel_path,
			folder = excluded.folder,
			filename = excluded.filename,
			ext = excluded.ext,
			size = excluded.size,
			mtime_unix_ns = excluded.mtime_unix_ns,
			content_hash = excluded.content_hash,
			index_depth = excluded.index_depth,
			status = excluded.status,
			error_message = excluded.error_message,
			updated_at = excluded.updated_at
	`, f.Workspace, f.AbsPath, f.RelPath, f.Folder, f.Filename, f.Ext, f.Size, f.MtimeUnixNs, f.ContentHash, f.IndexDepth, f.Status, f.ErrorMessage, f.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("upsert file: %w", err)
	}

	var fileID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM files WHERE abs_path = ?`, f.AbsPath).Scan(&fileID); err != nil {
		return 0, fmt.Errorf("fetch file id: %w", err)
	}

	headings, body := "", ""
	if blob != nil {
		headings = blob.Headings
		body = blob.Body
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, fileID); err != nil {
		return 0, fmt.Errorf("delete fts row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO docs_fts (rowid, filename, folder, headings, body)
		VALUES (?, ?, ?, ?, ?)
	`, fileID, f.Filename, f.Folder, headings, body); err != nil {
		return 0, fmt.Errorf("insert fts row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return fileID, nil
}

// DeleteFileByPath removes a deleted file from both files and docs_fts.
func (s *Store) DeleteFileByPath(ctx context.Context, absPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var fileID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE abs_path = ?`, absPath).Scan(&fileID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// Search queries FTS5 with weighted BM25 (filename > headings > folder > body) and returns snippets.
func (s *Store) Search(ctx context.Context, rawQuery string, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	ftsQuery := SanitizeFTSQuery(rawQuery)
	if ftsQuery == "" {
		return s.RecentFiles(ctx, limit)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			f.id, f.abs_path, f.rel_path, f.filename, f.folder, f.ext,
			f.size, f.mtime_unix_ns, f.index_depth,
			snippet(docs_fts, 3, '[', ']', '...', 24) AS preview,
			bm25(docs_fts, 10.0, 3.0, 5.0, 1.0) AS score
		FROM docs_fts
		JOIN files f ON f.id = docs_fts.rowid
		WHERE docs_fts MATCH ?
		ORDER BY score ASC, f.mtime_unix_ns DESC
		LIMIT ?
	`, ftsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	hits := make([]SearchHit, 0)
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.FileID, &h.AbsPath, &h.RelPath, &h.Filename, &h.Folder, &h.Ext,
			&h.Size, &h.MtimeUnixNs, &h.IndexDepth, &h.Snippet, &h.Score,
		); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// RecentFiles returns the most recently modified files when the query is empty.
func (s *Store) RecentFiles(ctx context.Context, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, abs_path, rel_path, filename, folder, ext, size, mtime_unix_ns, index_depth
		FROM files
		ORDER BY mtime_unix_ns DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := make([]SearchHit, 0)
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.FileID, &h.AbsPath, &h.RelPath, &h.Filename, &h.Folder, &h.Ext,
			&h.Size, &h.MtimeUnixNs, &h.IndexDepth,
		); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// Stats returns aggregate counts across the local index.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{
		ByDepth:    make(map[string]int64),
		Workspaces: make([]string, 0),
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&st.TotalFiles); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&st.TotalBlobs); err != nil {
		return st, err
	}

	depthRows, err := s.db.QueryContext(ctx, `SELECT index_depth, COUNT(*) FROM files GROUP BY index_depth`)
	if err != nil {
		return st, err
	}
	defer depthRows.Close()
	for depthRows.Next() {
		var depth string
		var count int64
		if err := depthRows.Scan(&depth, &count); err != nil {
			return st, err
		}
		st.ByDepth[depth] = count
	}

	wsRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT workspace FROM files ORDER BY workspace`)
	if err != nil {
		return st, err
	}
	defer wsRows.Close()
	for wsRows.Next() {
		var ws string
		if err := wsRows.Scan(&ws); err != nil {
			return st, err
		}
		st.Workspaces = append(st.Workspaces, ws)
	}
	return st, nil
}

// SanitizeFTSQuery quotes user tokens so punctuation like '-' or ':' cannot trigger FTS5 syntax errors.
func SanitizeFTSQuery(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, tok := range fields {
		clean := strings.ReplaceAll(tok, `"`, "")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			continue
		}
		// Enable prefix search for natural interactive typing when token has >= 2 chars.
		if len(clean) >= 2 && !strings.HasSuffix(clean, "*") {
			parts = append(parts, `"`+clean+`"*`)
		} else {
			clean = strings.TrimSuffix(clean, "*")
			if clean != "" {
				parts = append(parts, `"`+clean+`"`)
			}
		}
	}
	return strings.Join(parts, " ")
}
