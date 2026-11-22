package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// FolderSummary describes an immediate child folder and its total indexed file count.
type FolderSummary struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	FileCount int    `json:"file_count"`
}

// FolderListing holds the immediate child folders and files under a workspace folder.
type FolderListing struct {
	WorkspaceID int64           `json:"workspace_id"`
	Folder      string          `json:"folder"`
	Folders     []FolderSummary `json:"folders"`
	Files       []FileRecord    `json:"files"`
	Truncated   bool            `json:"truncated"`
}

// GetWorkspace returns a workspace by ID, or nil if not found.
func (s *Store) GetWorkspace(ctx context.Context, workspaceID int64) (*Workspace, error) {
	var ws Workspace
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, root_path, kind, bot_id, added_at, last_scan_at, current_scan_id
		FROM workspaces
		WHERE id = ?
	`, workspaceID).Scan(&ws.ID, &ws.RootPath, &ws.Kind, &ws.BotID, &ws.AddedAt, &ws.LastScanAt, &ws.CurrentScanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

// ListWorkspaces returns all registered workspaces ordered by ID.
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, root_path, kind, bot_id, added_at, last_scan_at, current_scan_id
		FROM workspaces
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		var ws Workspace
		if err := rows.Scan(&ws.ID, &ws.RootPath, &ws.Kind, &ws.BotID, &ws.AddedAt, &ws.LastScanAt, &ws.CurrentScanID); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// WorkspaceFileCount returns the number of indexed files in workspaceID.
func (s *Store) WorkspaceFileCount(ctx context.Context, workspaceID int64) (int64, error) {
	var n int64
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE workspace_id = ?`,
		workspaceID,
	).Scan(&n)
	return n, err
}

// GetFileByID returns the FileEntry (file + optional blob) for fileID, or nil if not found.
func (s *Store) GetFileByID(ctx context.Context, fileID int64) (*FileEntry, error) {
	return s.querySingleFileEntry(ctx, `WHERE f.id = ?`, fileID)
}

// GetFileByPath returns the FileEntry for (workspaceID, relPath), or nil if not found.
func (s *Store) GetFileByPath(ctx context.Context, workspaceID int64, relPath string) (*FileEntry, error) {
	clean := normalizeRelPath(relPath)
	return s.querySingleFileEntry(ctx, `WHERE f.workspace_id = ? AND f.rel_path = ?`, workspaceID, clean)
}

func (s *Store) querySingleFileEntry(ctx context.Context, where string, args ...any) (*FileEntry, error) {
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
		` + where

	var f FileRecord
	var fBlake3 sql.NullString
	var bBlake3, bPeekerID, bDepth, bTitle, bHeadings, bBody, bOutline sql.NullString
	var bVer, bUnits, bExtracted sql.NullInt64

	err := s.reader.QueryRowContext(ctx, query, args...).Scan(
		&f.ID, &f.WorkspaceID, &f.RelPath, &f.Filename, &f.Folder, &f.Ext,
		&f.Size, &f.MtimeNS, &fBlake3, &f.PeekerID, &f.PeekerVersion, &f.IndexDepth,
		&f.IndexStatus, &f.ErrorMsg, &f.IndexedAt, &f.LastSeenScanID,
		&bBlake3, &bPeekerID, &bVer, &bDepth, &bTitle, &bHeadings,
		&bBody, &bOutline, &bUnits, &bExtracted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.BLAKE3 = fBlake3.String

	entry := &FileEntry{File: f}
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
	return entry, nil
}

// FindFilesByBLAKE3 finds indexed files in workspaceID matching blake3 (useful to locate moved/copied files).
func (s *Store) FindFilesByBLAKE3(ctx context.Context, workspaceID int64, blake3 string) ([]FileRecord, error) {
	if blake3 == "" {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT
			id, workspace_id, rel_path, filename, folder, ext,
			size, mtime_ns, blake3, peeker_id, peeker_version, index_depth,
			index_status, error_msg, indexed_at, last_seen_scan_id
		FROM files
		WHERE workspace_id = ? AND blake3 = ?
		ORDER BY rel_path
	`, workspaceID, blake3)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var f FileRecord
		var hash sql.NullString
		if err := rows.Scan(
			&f.ID, &f.WorkspaceID, &f.RelPath, &f.Filename, &f.Folder, &f.Ext,
			&f.Size, &f.MtimeNS, &hash, &f.PeekerID, &f.PeekerVersion, &f.IndexDepth,
			&f.IndexStatus, &f.ErrorMsg, &f.IndexedAt, &f.LastSeenScanID,
		); err != nil {
			return nil, err
		}
		f.BLAKE3 = hash.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFolder returns immediate child subfolders (with recursive file counts) and immediate
// files directly inside folderRel ("" or "." for workspace root).
func (s *Store) ListFolder(ctx context.Context, workspaceID int64, folderRel string, limit int) (FolderListing, error) {
	if limit <= 0 {
		limit = 100
	}
	folder := normalizeFolderPath(folderRel)

	listing := FolderListing{
		WorkspaceID: workspaceID,
		Folder:      folder,
	}

	// 1. Immediate child subfolders + recursive file counts.
	var subRows *sql.Rows
	var err error
	if folder == "" {
		subRows, err = s.reader.QueryContext(ctx, `
			SELECT folder, COUNT(*)
			FROM files
			WHERE workspace_id = ? AND folder != '' AND folder != '.'
			GROUP BY folder
		`, workspaceID)
	} else {
		subRows, err = s.reader.QueryContext(ctx, `
			SELECT folder, COUNT(*)
			FROM files
			WHERE workspace_id = ? AND folder >= ? AND folder < ?
			GROUP BY folder
		`, workspaceID, folder+"/", folder+"0")
	}
	if err != nil {
		return FolderListing{}, err
	}
	childCounts := make(map[string]int)
	prefix := ""
	if folder != "" {
		prefix = folder + "/"
	}
	for subRows.Next() {
		var fPath string
		var cnt int
		if err := subRows.Scan(&fPath, &cnt); err != nil {
			_ = subRows.Close()
			return FolderListing{}, err
		}
		rest := strings.TrimPrefix(fPath, prefix)
		if rest == "" || rest == "." {
			continue
		}
		seg := rest
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			seg = rest[:idx]
		}
		childPath := seg
		if prefix != "" {
			childPath = prefix + seg
		}
		childCounts[childPath] += cnt
	}
	if err := subRows.Close(); err != nil {
		return FolderListing{}, err
	}
	if err := subRows.Err(); err != nil {
		return FolderListing{}, err
	}

	folderKeys := make([]string, 0, len(childCounts))
	for k := range childCounts {
		folderKeys = append(folderKeys, k)
	}
	sort.Strings(folderKeys)

	remaining := limit
	for _, k := range folderKeys {
		if remaining <= 0 {
			listing.Truncated = true
			break
		}
		name := k
		if idx := strings.LastIndexByte(k, '/'); idx >= 0 {
			name = k[idx+1:]
		}
		listing.Folders = append(listing.Folders, FolderSummary{
			Path:      k,
			Name:      name,
			FileCount: childCounts[k],
		})
		remaining--
	}

	// 2. Immediate files in this folder (check whether any exist if folders filled limit).
	fetchFiles := remaining + 1
	var fileRows *sql.Rows
	if folder == "" {
		fileRows, err = s.reader.QueryContext(ctx, `
			SELECT
				id, workspace_id, rel_path, filename, folder, ext,
				size, mtime_ns, blake3, peeker_id, peeker_version, index_depth,
				index_status, error_msg, indexed_at, last_seen_scan_id
			FROM files
			WHERE workspace_id = ? AND (folder = '' OR folder = '.')
			ORDER BY filename
			LIMIT ?
		`, workspaceID, fetchFiles)
	} else {
		fileRows, err = s.reader.QueryContext(ctx, `
			SELECT
				id, workspace_id, rel_path, filename, folder, ext,
				size, mtime_ns, blake3, peeker_id, peeker_version, index_depth,
				index_status, error_msg, indexed_at, last_seen_scan_id
			FROM files
			WHERE workspace_id = ? AND folder = ?
			ORDER BY filename
			LIMIT ?
		`, workspaceID, folder, fetchFiles)
	}
	if err != nil {
		return FolderListing{}, err
	}
	defer fileRows.Close()

	for fileRows.Next() {
		var f FileRecord
		var hash sql.NullString
		if err := fileRows.Scan(
			&f.ID, &f.WorkspaceID, &f.RelPath, &f.Filename, &f.Folder, &f.Ext,
			&f.Size, &f.MtimeNS, &hash, &f.PeekerID, &f.PeekerVersion, &f.IndexDepth,
			&f.IndexStatus, &f.ErrorMsg, &f.IndexedAt, &f.LastSeenScanID,
		); err != nil {
			return FolderListing{}, err
		}
		f.BLAKE3 = hash.String
		if len(listing.Files) >= remaining {
			listing.Truncated = true
			break
		}
		listing.Files = append(listing.Files, f)
	}
	if err := fileRows.Err(); err != nil {
		return FolderListing{}, err
	}

	return listing, nil
}

func normalizeRelPath(p string) string {
	s := strings.TrimSpace(filepath.ToSlash(p))
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	return s
}

func normalizeFolderPath(p string) string {
	s := normalizeRelPath(p)
	s = strings.TrimSuffix(s, "/")
	if s == "." {
		return ""
	}
	return s
}
