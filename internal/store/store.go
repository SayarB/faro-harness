// Package store manages the local SQLite (WAL) + FTS5 card catalog.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DepthFull         = "full"
	DepthHeaderOnly   = "header_only"
	DepthMetadataOnly = "metadata_only"

	StatusOK          = "ok"
	StatusPlaceholder = "placeholder"
	StatusUnreadable  = "unreadable"
	StatusTooLarge    = "too_large"
	StatusError       = "error"

	currentSchemaVersion = 4
)

// Workspace represents a registered root folder in the catalog.
type Workspace struct {
	ID            int64
	RootPath      string
	AddedAt       int64
	LastScanAt    int64
	CurrentScanID int64
}

// ContentBlob holds deduplicated extracted card content keyed by (BLAKE3, PeekerID, PeekerVersion).
type ContentBlob struct {
	BLAKE3        string
	PeekerID      string
	PeekerVersion int
	IndexDepth    string
	Title         string
	Headings      string
	BodyPreview   string
	OutlineJSON   string
	UnitCount     int
	ExtractedAt   int64
}

// FileRecord holds per-path metadata in a workspace.
type FileRecord struct {
	ID             int64
	WorkspaceID    int64
	RelPath        string
	Filename       string
	Folder         string
	Ext            string
	Size           int64
	MtimeNS        int64
	BLAKE3         string
	PeekerID       string
	PeekerVersion  int
	IndexDepth     string
	IndexStatus    string
	ErrorMsg       string
	IndexedAt      int64
	LastSeenScanID int64
}

// FileMeta is the lightweight projection used for change detection.
type FileMeta struct {
	ID             int64
	Size           int64
	MtimeNS        int64
	BLAKE3         string
	PeekerID       string
	PeekerVersion  int
	IndexDepth     string
	IndexStatus    string
	LastSeenScanID int64
}

// UpsertItem bundles a FileRecord and an optional newly extracted ContentBlob.
type UpsertItem struct {
	File FileRecord
	Blob *ContentBlob
}

// FileEntry joins FileRecord with its ContentBlob (if any) for inspection.
type FileEntry struct {
	File FileRecord
	Blob *ContentBlob
}

// SearchHit represents a single BM25-ranked FTS5, trigram, or metadata-filter match.
type SearchHit struct {
	FileID      int64
	WorkspaceID int64
	RelPath     string
	Filename    string
	Folder      string
	Ext         string
	Size        int64
	MtimeNS     int64
	IndexDepth  string
	IndexStatus string
	Title       string
	Headings    string
	Snippet     string
	Rank        float64
	MatchSource string
}

// Store wraps SQLite WAL connections for Faro's card catalog.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	path   string
	shared bool
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS workspaces (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	root_path       TEXT NOT NULL UNIQUE,
	added_at        INTEGER NOT NULL,
	last_scan_at    INTEGER NOT NULL DEFAULT 0,
	current_scan_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS content_blobs (
	blake3         TEXT NOT NULL,
	peeker_id      TEXT NOT NULL,
	peeker_version INTEGER NOT NULL,
	index_depth    TEXT NOT NULL,
	title          TEXT NOT NULL DEFAULT '',
	headings       TEXT NOT NULL DEFAULT '',
	body_preview   TEXT NOT NULL DEFAULT '',
	outline_json   TEXT NOT NULL DEFAULT '',
	unit_count     INTEGER NOT NULL DEFAULT 0,
	extracted_at   INTEGER NOT NULL,
	PRIMARY KEY (blake3, peeker_id, peeker_version)
);

CREATE TABLE IF NOT EXISTS files (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id      INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	rel_path          TEXT NOT NULL,
	filename          TEXT NOT NULL,
	folder            TEXT NOT NULL,
	ext               TEXT NOT NULL,
	size              INTEGER NOT NULL,
	mtime_ns          INTEGER NOT NULL,
	blake3            TEXT,
	peeker_id         TEXT NOT NULL DEFAULT '',
	peeker_version    INTEGER NOT NULL DEFAULT 0,
	index_depth       TEXT NOT NULL,
	index_status      TEXT NOT NULL,
	error_msg         TEXT NOT NULL DEFAULT '',
	indexed_at        INTEGER NOT NULL,
	last_seen_scan_id INTEGER NOT NULL DEFAULT 0,
	UNIQUE(workspace_id, rel_path),
	FOREIGN KEY (blake3, peeker_id, peeker_version)
		REFERENCES content_blobs(blake3, peeker_id, peeker_version)
);

CREATE INDEX IF NOT EXISTS idx_files_blob ON files(blake3, peeker_id, peeker_version);
CREATE INDEX IF NOT EXISTS idx_files_workspace_ext ON files(workspace_id, ext);
CREATE INDEX IF NOT EXISTS idx_files_workspace_scan ON files(workspace_id, last_seen_scan_id);

CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
	filename,
	headings,
	folder,
	body,
	tokenize='porter unicode61 remove_diacritics 2',
	prefix='2 3'
);

CREATE VIRTUAL TABLE IF NOT EXISTS files_trigram USING fts5(
	filename,
	folder,
	tokenize='trigram'
);
`

// Open opens (or creates) the SQLite catalog at dbPath and applies schema migrations.
// Pass ":memory:" for an ephemeral in-memory catalog (useful in tests).
func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		var err error
		dbPath, err = DefaultDBPath()
		if err != nil {
			return nil, fmt.Errorf("resolve default db path: %w", err)
		}
	}

	isMemory := dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:")
	if !isMemory {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := buildDSN(dbPath, isMemory)
	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)

	if err := migrateSchema(writer); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	if isMemory {
		return &Store{
			writer: writer,
			reader: writer,
			path:   dbPath,
			shared: true,
		}, nil
	}

	reader, err := sql.Open("sqlite", buildReaderDSN(dbPath))
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)

	return &Store{
		writer: writer,
		reader: reader,
		path:   dbPath,
	}, nil
}

func migrateSchema(db *sql.DB) error {
	var userVersion int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		return err
	}

	if userVersion == 0 {
		var hasFiles int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&hasFiles); err != nil {
			return err
		}
		if hasFiles == 0 {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(schemaSQL); err != nil {
				return err
			}
			if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
				return err
			}
			return tx.Commit()
		}
		// Legacy unversioned v1 DB: step through v1 -> v2 -> v3.
		if err := migrateV1ToV2(db); err != nil {
			return err
		}
		userVersion = 2
	}

	if userVersion < 2 {
		if err := migrateV1ToV2(db); err != nil {
			return err
		}
		userVersion = 2
	}

	if userVersion < 3 {
		if err := migrateV2ToV3(db); err != nil {
			return err
		}
		userVersion = 3
	}

	if userVersion < 4 {
		if err := migrateV3ToV4(db); err != nil {
			return err
		}
	}

	return nil
}

func migrateV1ToV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := addColumnIfMissing(tx, "workspaces", "current_scan_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "files", "last_seen_scan_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_files_workspace_scan ON files(workspace_id, last_seen_scan_id)`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE IF EXISTS files_fts`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE VIRTUAL TABLE files_fts USING fts5(
			filename,
			headings,
			folder,
			body,
			tokenize='porter unicode61 remove_diacritics 2'
		)
	`); err != nil {
		return err
	}

	insStmt, err := tx.Prepare(`INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insStmt.Close()

	type ftsRow struct {
		id       int64
		filename string
		folder   string
		title    string
		headings string
		body     string
	}

	const chunkSize = 200
	var lastID int64
	for {
		rows, err := tx.Query(`
			SELECT f.id, f.filename, f.folder, COALESCE(b.title, ''), COALESCE(b.headings, ''), COALESCE(b.body_preview, '')
			FROM files f
			LEFT JOIN content_blobs b ON b.blake3 = f.blake3
			WHERE f.id > ?
			ORDER BY f.id
			LIMIT ?
		`, lastID, chunkSize)
		if err != nil {
			return err
		}
		var chunk []ftsRow
		for rows.Next() {
			var r ftsRow
			if err := rows.Scan(&r.id, &r.filename, &r.folder, &r.title, &r.headings, &r.body); err != nil {
				_ = rows.Close()
				return err
			}
			chunk = append(chunk, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(chunk) == 0 {
			break
		}
		for _, r := range chunk {
			if _, err := insStmt.Exec(r.id, normalizeForFTS(r.filename), combineFTSHeadings(r.title, r.headings), normalizeForFTS(r.folder), r.body); err != nil {
				return err
			}
			lastID = r.id
		}
	}

	if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateV2ToV3 upgrades content_blobs to composite PRIMARY KEY (blake3, peeker_id, peeker_version)
// and files to composite FOREIGN KEY (blake3, peeker_id, peeker_version), preserving files.id so
// rowid-keyed files_fts stays aligned.
// Existing v2 rows receive peeker_id='' (forcing a clean re-peek into the new peeker-scoped cards
// on the next scan while keeping existing cards searchable immediately).
func migrateV2ToV3(db *sql.DB) error {
	// PRAGMA foreign_keys must be toggled outside a transaction.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Preserve AUTOINCREMENT high-water mark so previously deleted top IDs are not reused.
	var prevFileSeq int64
	_ = tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'files'`).Scan(&prevFileSeq)

	if _, err := tx.Exec(`
		CREATE TABLE content_blobs_v3 (
			blake3         TEXT NOT NULL,
			peeker_id      TEXT NOT NULL,
			peeker_version INTEGER NOT NULL,
			index_depth    TEXT NOT NULL,
			title          TEXT NOT NULL DEFAULT '',
			headings       TEXT NOT NULL DEFAULT '',
			body_preview   TEXT NOT NULL DEFAULT '',
			outline_json   TEXT NOT NULL DEFAULT '',
			unit_count     INTEGER NOT NULL DEFAULT 0,
			extracted_at   INTEGER NOT NULL,
			PRIMARY KEY (blake3, peeker_id, peeker_version)
		)
	`); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO content_blobs_v3 (
			blake3, peeker_id, peeker_version, index_depth, title, headings, body_preview, outline_json, unit_count, extracted_at
		)
		SELECT blake3, '', peeker_version, index_depth, title, headings, body_preview, outline_json, unit_count, extracted_at
		FROM content_blobs
	`); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		CREATE TABLE files_v3 (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id      INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			rel_path          TEXT NOT NULL,
			filename          TEXT NOT NULL,
			folder            TEXT NOT NULL,
			ext               TEXT NOT NULL,
			size              INTEGER NOT NULL,
			mtime_ns          INTEGER NOT NULL,
			blake3            TEXT,
			peeker_id         TEXT NOT NULL DEFAULT '',
			peeker_version    INTEGER NOT NULL DEFAULT 0,
			index_depth       TEXT NOT NULL,
			index_status      TEXT NOT NULL,
			error_msg         TEXT NOT NULL DEFAULT '',
			indexed_at        INTEGER NOT NULL,
			last_seen_scan_id INTEGER NOT NULL DEFAULT 0,
			UNIQUE(workspace_id, rel_path),
			FOREIGN KEY (blake3, peeker_id, peeker_version)
				REFERENCES content_blobs(blake3, peeker_id, peeker_version)
		)
	`); err != nil {
		return err
	}

	// Preserve files.id so rowid-keyed files_fts stays valid. If a legacy file row's
	// peeker_version ever drifted from its blob's peeker_version, align to the blob's
	// peeker_version so the composite FK holds cleanly.
	if _, err := tx.Exec(`
		INSERT INTO files_v3 (
			id, workspace_id, rel_path, filename, folder, ext, size, mtime_ns,
			blake3, peeker_id, peeker_version, index_depth, index_status, error_msg, indexed_at, last_seen_scan_id
		)
		SELECT
			f.id, f.workspace_id, f.rel_path, f.filename, f.folder, f.ext, f.size, f.mtime_ns,
			CASE WHEN b.blake3 IS NOT NULL THEN f.blake3 ELSE NULL END,
			'',
			CASE WHEN b.blake3 IS NOT NULL THEN b.peeker_version ELSE f.peeker_version END,
			f.index_depth, f.index_status, f.error_msg, f.indexed_at, f.last_seen_scan_id
		FROM files f
		LEFT JOIN content_blobs b ON b.blake3 = f.blake3
	`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE files`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE content_blobs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE content_blobs_v3 RENAME TO content_blobs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE files_v3 RENAME TO files`); err != nil {
		return err
	}
	if prevFileSeq > 0 {
		res, err := tx.Exec(`UPDATE sqlite_sequence SET seq = MAX(seq, ?) WHERE name = 'files'`, prevFileSeq)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('files', ?)`, prevFileSeq); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_files_blob ON files(blake3, peeker_id, peeker_version)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_files_workspace_ext ON files(workspace_id, ext)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_files_workspace_scan ON files(workspace_id, last_seen_scan_id)`); err != nil {
		return err
	}

	// Verify composite foreign keys before committing.
	fkRows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	if fkRows.Next() {
		_ = fkRows.Close()
		return errors.New("foreign key violation during v2->v3 migration")
	}
	if err := fkRows.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateV3ToV4 rebuilds files_fts with prefix='2 3' and upgraded camelCase/digit tokenization,
// and creates + backfills files_trigram on (filename, folder) keyed by rowid = files.id.
func migrateV3ToV4(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DROP TABLE IF EXISTS files_fts`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE VIRTUAL TABLE files_fts USING fts5(
			filename,
			headings,
			folder,
			body,
			tokenize='porter unicode61 remove_diacritics 2',
			prefix='2 3'
		)
	`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE IF EXISTS files_trigram`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE VIRTUAL TABLE files_trigram USING fts5(
			filename,
			folder,
			tokenize='trigram'
		)
	`); err != nil {
		return err
	}

	insFTSStmt, err := tx.Prepare(`INSERT INTO files_fts (rowid, filename, headings, folder, body) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insFTSStmt.Close()

	insTriStmt, err := tx.Prepare(`INSERT INTO files_trigram (rowid, filename, folder) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insTriStmt.Close()

	type ftsRow struct {
		id       int64
		filename string
		folder   string
		title    string
		headings string
		body     string
	}

	const chunkSize = 200
	var lastID int64
	for {
		rows, err := tx.Query(`
			SELECT f.id, f.filename, f.folder, COALESCE(b.title, ''), COALESCE(b.headings, ''), COALESCE(b.body_preview, '')
			FROM files f
			LEFT JOIN content_blobs b
				ON b.blake3 = f.blake3
				AND b.peeker_id = f.peeker_id
				AND b.peeker_version = f.peeker_version
			WHERE f.id > ?
			ORDER BY f.id
			LIMIT ?
		`, lastID, chunkSize)
		if err != nil {
			return err
		}
		var chunk []ftsRow
		for rows.Next() {
			var r ftsRow
			if err := rows.Scan(&r.id, &r.filename, &r.folder, &r.title, &r.headings, &r.body); err != nil {
				_ = rows.Close()
				return err
			}
			chunk = append(chunk, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(chunk) == 0 {
			break
		}
		for _, r := range chunk {
			if _, err := insFTSStmt.Exec(r.id, normalizeForFTS(r.filename), combineFTSHeadings(r.title, r.headings), normalizeForFTS(r.folder), r.body); err != nil {
				return err
			}
			if _, err := insTriStmt.Exec(r.id, r.filename, r.folder); err != nil {
				return err
			}
			lastID = r.id
		}
	}

	if _, err := tx.Exec(`PRAGMA user_version = 4`); err != nil {
		return err
	}
	return tx.Commit()
}

func addColumnIfMissing(tx *sql.Tx, table, column, def string) error {
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, def))
	return err
}

func buildDSN(dbPath string, isMemory bool) string {
	params := url.Values{}
	params.Add("_pragma", "foreign_keys(ON)")
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "temp_store(MEMORY)")
	if !isMemory {
		params.Add("_pragma", "journal_mode(WAL)")
		params.Add("_pragma", "synchronous(NORMAL)")
	}
	return formatSQLiteURI(dbPath, isMemory, params)
}

func buildReaderDSN(dbPath string) string {
	params := url.Values{}
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "temp_store(MEMORY)")
	params.Add("_pragma", "query_only(ON)")
	return formatSQLiteURI(dbPath, false, params)
}

func formatSQLiteURI(dbPath string, isMemory bool, params url.Values) string {
	if strings.HasPrefix(dbPath, "file:") {
		sep := "?"
		if strings.Contains(dbPath, "?") {
			sep = "&"
		}
		return dbPath + sep + params.Encode()
	}
	if isMemory {
		return "file::memory:?" + params.Encode()
	}
	return "file:" + filepath.ToSlash(dbPath) + "?" + params.Encode()
}

// Path returns the filesystem path (or ":memory:") backing this Store.
func (s *Store) Path() string {
	return s.path
}

// Close closes the underlying SQLite connections.
func (s *Store) Close() error {
	var err1, err2 error
	if !s.shared && s.reader != nil {
		err1 = s.reader.Close()
	}
	if s.writer != nil {
		err2 = s.writer.Close()
	}
	return errors.Join(err1, err2)
}

// EnsureWorkspace returns the workspace row for rootPath, creating it if needed.
func (s *Store) EnsureWorkspace(ctx context.Context, rootPath string) (Workspace, error) {
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("abs workspace path: %w", err)
	}
	now := time.Now().Unix()

	_, err = s.writer.ExecContext(ctx,
		`INSERT INTO workspaces (root_path, added_at) VALUES (?, ?)
		 ON CONFLICT(root_path) DO NOTHING`,
		absRoot, now,
	)
	if err != nil {
		return Workspace{}, fmt.Errorf("insert workspace: %w", err)
	}

	var ws Workspace
	err = s.writer.QueryRowContext(ctx,
		`SELECT id, root_path, added_at, last_scan_at, current_scan_id FROM workspaces WHERE root_path = ?`,
		absRoot,
	).Scan(&ws.ID, &ws.RootPath, &ws.AddedAt, &ws.LastScanAt, &ws.CurrentScanID)
	if err != nil {
		return Workspace{}, fmt.Errorf("query workspace: %w", err)
	}
	return ws, nil
}

// BeginWorkspaceScan atomically increments current_scan_id for workspaceID and returns the new generation.
func (s *Store) BeginWorkspaceScan(ctx context.Context, workspaceID int64) (int64, error) {
	var scanID int64
	err := s.writer.QueryRowContext(ctx,
		`UPDATE workspaces SET current_scan_id = current_scan_id + 1 WHERE id = ? RETURNING current_scan_id`,
		workspaceID,
	).Scan(&scanID)
	if err != nil {
		return 0, fmt.Errorf("begin workspace scan: %w", err)
	}
	return scanID, nil
}

// TouchWorkspaceScan updates last_scan_at for a workspace.
func (s *Store) TouchWorkspaceScan(ctx context.Context, workspaceID int64) error {
	_, err := s.writer.ExecContext(ctx,
		`UPDATE workspaces SET last_scan_at = ? WHERE id = ?`,
		time.Now().Unix(), workspaceID,
	)
	return err
}

// TouchFileScanBatch stamps last_seen_scan_id for unchanged files seen during a workspace scan.
func (s *Store) TouchFileScanBatch(ctx context.Context, workspaceID int64, scanID int64, relPaths []string) error {
	if len(relPaths) == 0 || scanID <= 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin touch scan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE files
		SET last_seen_scan_id = MAX(last_seen_scan_id, ?)
		WHERE workspace_id = ? AND rel_path = ?
	`)
	if err != nil {
		return fmt.Errorf("prepare touch scan: %w", err)
	}
	defer stmt.Close()

	for _, rp := range relPaths {
		if _, err := stmt.ExecContext(ctx, scanID, workspaceID, filepath.ToSlash(rp)); err != nil {
			return fmt.Errorf("touch scan for %s: %w", rp, err)
		}
	}
	return tx.Commit()
}

// GetFileMeta looks up existing metadata for a workspace-relative path.
// Returns (nil, nil) if not found.
func (s *Store) GetFileMeta(ctx context.Context, workspaceID int64, relPath string) (*FileMeta, error) {
	var m FileMeta
	var blake3 sql.NullString
	err := s.reader.QueryRowContext(ctx,
		`SELECT id, size, mtime_ns, blake3, peeker_id, peeker_version, index_depth, index_status, last_seen_scan_id
		 FROM files WHERE workspace_id = ? AND rel_path = ?`,
		workspaceID, filepath.ToSlash(relPath),
	).Scan(&m.ID, &m.Size, &m.MtimeNS, &blake3, &m.PeekerID, &m.PeekerVersion, &m.IndexDepth, &m.IndexStatus, &m.LastSeenScanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.BLAKE3 = blake3.String
	return &m, nil
}

// GetBlob retrieves a content-addressed blob by its composite key (blake3, peekerID, peekerVersion).
// Returns (nil, nil) if not found.
func (s *Store) GetBlob(ctx context.Context, blake3, peekerID string, peekerVersion int) (*ContentBlob, error) {
	if blake3 == "" {
		return nil, nil
	}
	var b ContentBlob
	err := s.reader.QueryRowContext(ctx,
		`SELECT blake3, peeker_id, peeker_version, index_depth, title, headings, body_preview, outline_json, unit_count, extracted_at
		 FROM content_blobs WHERE blake3 = ? AND peeker_id = ? AND peeker_version = ?`,
		blake3, peekerID, peekerVersion,
	).Scan(&b.BLAKE3, &b.PeekerID, &b.PeekerVersion, &b.IndexDepth, &b.Title, &b.Headings, &b.BodyPreview, &b.OutlineJSON, &b.UnitCount, &b.ExtractedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpsertBatch atomically upserts content_blobs, files, and rowid-keyed files_fts rows in a single transaction.
func (s *Store) UpsertBatch(ctx context.Context, items []UpsertItem) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	blobStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO content_blobs (
			blake3, peeker_id, peeker_version, index_depth, title, headings, body_preview, outline_json, unit_count, extracted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(blake3, peeker_id, peeker_version) DO UPDATE SET
			index_depth  = excluded.index_depth,
			title        = excluded.title,
			headings     = excluded.headings,
			body_preview = excluded.body_preview,
			outline_json = excluded.outline_json,
			unit_count   = excluded.unit_count,
			extracted_at = excluded.extracted_at
	`)
	if err != nil {
		return fmt.Errorf("prepare blob upsert: %w", err)
	}
	defer blobStmt.Close()

	fileStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO files (
			workspace_id, rel_path, filename, folder, ext, size, mtime_ns,
			blake3, peeker_id, peeker_version, index_depth, index_status, error_msg, indexed_at, last_seen_scan_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, rel_path) DO UPDATE SET
			filename          = excluded.filename,
			folder            = excluded.folder,
			ext               = excluded.ext,
			size              = excluded.size,
			mtime_ns          = excluded.mtime_ns,
			blake3            = excluded.blake3,
			peeker_id         = excluded.peeker_id,
			peeker_version    = excluded.peeker_version,
			index_depth       = excluded.index_depth,
			index_status      = excluded.index_status,
			error_msg         = excluded.error_msg,
			indexed_at        = excluded.indexed_at,
			last_seen_scan_id = MAX(files.last_seen_scan_id, excluded.last_seen_scan_id)
		RETURNING id
	`)
	if err != nil {
		return fmt.Errorf("prepare file upsert: %w", err)
	}
	defer fileStmt.Close()

	delFTSStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_fts WHERE rowid = ?`)
	if err != nil {
		return fmt.Errorf("prepare fts delete: %w", err)
	}
	defer delFTSStmt.Close()

	insFTSStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO files_fts (rowid, filename, headings, folder, body)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare fts insert: %w", err)
	}
	defer insFTSStmt.Close()

	delTriStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_trigram WHERE rowid = ?`)
	if err != nil {
		return fmt.Errorf("prepare trigram delete: %w", err)
	}
	defer delTriStmt.Close()

	insTriStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO files_trigram (rowid, filename, folder)
		VALUES (?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare trigram insert: %w", err)
	}
	defer insTriStmt.Close()

	lookupBlobStmt, err := tx.PrepareContext(ctx, `
		SELECT title, headings, body_preview
		FROM content_blobs
		WHERE blake3 = ? AND peeker_id = ? AND peeker_version = ?
	`)
	if err != nil {
		return fmt.Errorf("prepare blob lookup: %w", err)
	}
	defer lookupBlobStmt.Close()

	for _, item := range items {
		f := item.File
		f.RelPath = filepath.ToSlash(f.RelPath)
		f.Folder = filepath.ToSlash(f.Folder)

		var title, headings, body string
		if item.Blob != nil {
			b := item.Blob
			if _, err := blobStmt.ExecContext(ctx,
				b.BLAKE3, b.PeekerID, b.PeekerVersion, b.IndexDepth, b.Title, b.Headings,
				b.BodyPreview, b.OutlineJSON, b.UnitCount, b.ExtractedAt,
			); err != nil {
				return fmt.Errorf("upsert blob (%s,%s,%d): %w", b.BLAKE3, b.PeekerID, b.PeekerVersion, err)
			}
			title, headings, body = b.Title, b.Headings, b.BodyPreview
		} else if f.BLAKE3 != "" {
			if err := lookupBlobStmt.QueryRowContext(ctx, f.BLAKE3, f.PeekerID, f.PeekerVersion).Scan(&title, &headings, &body); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lookup blob (%s,%s,%d): %w", f.BLAKE3, f.PeekerID, f.PeekerVersion, err)
			}
		}

		var blake3Param any
		if f.BLAKE3 != "" {
			blake3Param = f.BLAKE3
		}

		var fileID int64
		if err := fileStmt.QueryRowContext(ctx,
			f.WorkspaceID, f.RelPath, f.Filename, f.Folder, f.Ext, f.Size, f.MtimeNS,
			blake3Param, f.PeekerID, f.PeekerVersion, f.IndexDepth, f.IndexStatus, f.ErrorMsg, f.IndexedAt, f.LastSeenScanID,
		).Scan(&fileID); err != nil {
			return fmt.Errorf("upsert file %s: %w", f.RelPath, err)
		}

		if _, err := delFTSStmt.ExecContext(ctx, fileID); err != nil {
			return fmt.Errorf("delete fts for %s: %w", f.RelPath, err)
		}

		if _, err := insFTSStmt.ExecContext(ctx,
			fileID,
			normalizeForFTS(f.Filename),
			combineFTSHeadings(title, headings),
			normalizeForFTS(f.Folder),
			body,
		); err != nil {
			return fmt.Errorf("insert fts for %s: %w", f.RelPath, err)
		}

		if _, err := delTriStmt.ExecContext(ctx, fileID); err != nil {
			return fmt.Errorf("delete trigram for %s: %w", f.RelPath, err)
		}
		if _, err := insTriStmt.ExecContext(ctx, fileID, f.Filename, f.Folder); err != nil {
			return fmt.Errorf("insert trigram for %s: %w", f.RelPath, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert tx: %w", err)
	}
	return nil
}

// DeleteFile removes a single file and its FTS entry from a workspace.
// The associated content_blob is kept for deferred GC so renames hit the cache.
func (s *Store) DeleteFile(ctx context.Context, workspaceID int64, relPath string) error {
	return s.DeleteFiles(ctx, workspaceID, []string{relPath})
}

// DeletePath removes a file at relPath OR all files nested under directory relPath + "/".
// Returns the number of rows removed.
func (s *Store) DeletePath(ctx context.Context, workspaceID int64, relPath string) (int, error) {
	clean := strings.TrimSuffix(filepath.ToSlash(relPath), "/")
	if clean == "" || clean == "." {
		return 0, nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin delete path tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Use a case-sensitive BINARY range instead of LIKE (SQLite LIKE is ASCII case-insensitive
	// and cannot use the (workspace_id, rel_path) B-tree index). ASCII '/' is 0x2F and '0' is 0x30.
	prefixStart := clean + "/"
	prefixEnd := clean + "0"
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM files WHERE workspace_id = ? AND (rel_path = ? OR (rel_path >= ? AND rel_path < ?))`,
		workspaceID, clean, prefixStart, prefixEnd,
	)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	delFTSStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_fts WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer delFTSStmt.Close()

	delTriStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_trigram WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer delTriStmt.Close()

	delFileStmt, err := tx.PrepareContext(ctx, `DELETE FROM files WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer delFileStmt.Close()

	for _, id := range ids {
		if _, err := delFTSStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
		if _, err := delTriStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
		if _, err := delFileStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// DeleteFiles removes multiple workspace-relative paths and their FTS entries in one transaction.
func (s *Store) DeleteFiles(ctx context.Context, workspaceID int64, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	findStmt, err := tx.PrepareContext(ctx, `SELECT id FROM files WHERE workspace_id = ? AND rel_path = ?`)
	if err != nil {
		return err
	}
	defer findStmt.Close()

	delFTSStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_fts WHERE rowid = ?`)
	if err != nil {
		return err
	}
	defer delFTSStmt.Close()

	delTriStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_trigram WHERE rowid = ?`)
	if err != nil {
		return err
	}
	defer delTriStmt.Close()

	delFileStmt, err := tx.PrepareContext(ctx, `DELETE FROM files WHERE id = ?`)
	if err != nil {
		return err
	}
	defer delFileStmt.Close()

	for _, rp := range relPaths {
		var id int64
		err := findStmt.QueryRowContext(ctx, workspaceID, filepath.ToSlash(rp)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := delFTSStmt.ExecContext(ctx, id); err != nil {
			return err
		}
		if _, err := delTriStmt.ExecContext(ctx, id); err != nil {
			return err
		}
		if _, err := delFileStmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// PruneStaleScanFiles deletes rows in workspaceID with last_seen_scan_id < scanID,
// skipping any path under skipPrefixes (subtrees that errored during the walk).
func (s *Store) PruneStaleScanFiles(ctx context.Context, workspaceID int64, scanID int64, skipPrefixes []string) (int, error) {
	if scanID <= 0 {
		return 0, nil
	}

	normSkips := make([]string, 0, len(skipPrefixes))
	for _, p := range skipPrefixes {
		clean := strings.TrimSuffix(filepath.ToSlash(p), "/")
		if clean == "" || clean == "." {
			return 0, nil
		}
		normSkips = append(normSkips, clean)
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin prune scan tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, rel_path FROM files WHERE workspace_id = ? AND last_seen_scan_id < ?`,
		workspaceID, scanID,
	)
	if err != nil {
		return 0, err
	}

	var staleIDs []int64
	for rows.Next() {
		var id int64
		var rp string
		if err := rows.Scan(&id, &rp); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if hasAnyPrefix(rp, normSkips) {
			continue
		}
		staleIDs = append(staleIDs, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(staleIDs) == 0 {
		return 0, nil
	}

	delFTSStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_fts WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer delFTSStmt.Close()

	delTriStmt, err := tx.PrepareContext(ctx, `DELETE FROM files_trigram WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer delTriStmt.Close()

	delFileStmt, err := tx.PrepareContext(ctx, `DELETE FROM files WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer delFileStmt.Close()

	for _, id := range staleIDs {
		if _, err := delFTSStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
		if _, err := delTriStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
		if _, err := delFileStmt.ExecContext(ctx, id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(staleIDs), nil
}

// DeleteMissing removes any file rows in workspaceID whose rel_path is not in seen.
func (s *Store) DeleteMissing(ctx context.Context, workspaceID int64, seen map[string]struct{}) (int, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT rel_path FROM files WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return 0, err
	}
	var missing []string
	for rows.Next() {
		var rp string
		if err := rows.Scan(&rp); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if _, ok := seen[rp]; !ok {
			missing = append(missing, rp)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return 0, nil
	}
	if err := s.DeleteFiles(ctx, workspaceID, missing); err != nil {
		return 0, err
	}
	return len(missing), nil
}

// ListFiles returns all indexed files (joined with their content blobs) for a workspace,
// ordered by rel_path. If workspaceID == 0, lists across all workspaces.
func (s *Store) ListFiles(ctx context.Context, workspaceID int64) ([]FileEntry, error) {
	query := `
		SELECT
			f.id, f.workspace_id, f.rel_path, f.filename, f.folder, f.ext,
			f.size, f.mtime_ns, f.blake3, f.peeker_id, f.peeker_version, f.index_depth,
			f.index_status, f.error_msg, f.indexed_at, f.last_seen_scan_id,
			b.blake3, b.peeker_id, b.peeker_version, b.index_depth, b.title, b.headings,
			b.body_preview, b.outline_json, b.unit_count, b.extracted_at
		FROM files f
		LEFT JOIN content_blobs b
			ON b.blake3 = f.blake3
			AND b.peeker_id = f.peeker_id
			AND b.peeker_version = f.peeker_version
	`
	var args []any
	if workspaceID > 0 {
		query += ` WHERE f.workspace_id = ?`
		args = append(args, workspaceID)
	}
	query += ` ORDER BY f.workspace_id, f.rel_path`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []FileEntry
	for rows.Next() {
		var f FileRecord
		var fBlake3 sql.NullString
		var bBlake3, bPeekerID, bDepth, bTitle, bHeadings, bBody, bOutline sql.NullString
		var bVer, bUnits, bExtracted sql.NullInt64

		if err := rows.Scan(
			&f.ID, &f.WorkspaceID, &f.RelPath, &f.Filename, &f.Folder, &f.Ext,
			&f.Size, &f.MtimeNS, &fBlake3, &f.PeekerID, &f.PeekerVersion, &f.IndexDepth,
			&f.IndexStatus, &f.ErrorMsg, &f.IndexedAt, &f.LastSeenScanID,
			&bBlake3, &bPeekerID, &bVer, &bDepth, &bTitle, &bHeadings,
			&bBody, &bOutline, &bUnits, &bExtracted,
		); err != nil {
			return nil, err
		}
		f.BLAKE3 = fBlake3.String

		entry := FileEntry{File: f}
		if bBlake3.Valid {
			entry.Blob = &ContentBlob{
				BLAKE3:        bBlake3.String,
				PeekerID:      bPeekerID.String,
				PeekerVersion: int(bVer.Int64),
				IndexDepth:    bDepth.String,
				Title:         bTitle.String,
				Headings:      bHeadings.String,
				BodyPreview:   bBody.String,
				OutlineJSON:   bOutline.String,
				UnitCount:     int(bUnits.Int64),
				ExtractedAt:   bExtracted.Int64,
			}
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// GCOrphanBlobs deletes content_blobs unreferenced by any file row (matching the composite
// (blake3, peeker_id, peeker_version) key) and older than cutoff.
func (s *Store) GCOrphanBlobs(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.writer.ExecContext(ctx, `
		DELETE FROM content_blobs
		WHERE extracted_at <= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM files f
		      WHERE f.blake3 = content_blobs.blake3
		        AND f.peeker_id = content_blobs.peeker_id
		        AND f.peeker_version = content_blobs.peeker_version
		  )
	`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BlobCount returns the total number of deduplicated rows in content_blobs.
func (s *Store) BlobCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_blobs`).Scan(&n)
	return n, err
}

func combineFTSHeadings(title, headings string) string {
	if title == "" || strings.Contains(headings, title) {
		return headings
	}
	if headings == "" {
		return title
	}
	return title + "\n" + headings
}

func hasAnyPrefix(relPath string, prefixes []string) bool {
	for _, p := range prefixes {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return true
		}
	}
	return false
}
