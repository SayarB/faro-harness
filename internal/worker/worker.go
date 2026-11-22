// Package worker coordinates indexing work triggered for workspace files.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SayarB/faro-harness/internal/fsutil"
	"github.com/SayarB/faro-harness/internal/peek"
	"github.com/SayarB/faro-harness/internal/store"
)

const (
	// DefaultPerFileTimeout bounds upfront peeking latency for any single file.
	DefaultPerFileTimeout = 500 * time.Millisecond
)

// ErrFileChanged indicates the file's size or mtime changed while being read.
var ErrFileChanged = errors.New("file modified during indexing")

// Indexer processes a single file path to index it into Faro's card catalog.
type Indexer interface {
	IndexFile(ctx context.Context, path string) error
}

// Remover handles deletion of a removed/renamed file or directory path from the catalog.
type Remover interface {
	RemoveFile(ctx context.Context, path string) error
}

// Pruner removes indexed files that are no longer present in the workspace.
type Pruner interface {
	PruneMissing(ctx context.Context, seenRelPaths map[string]struct{}) (int, error)
}

// StubIndexer is a placeholder Indexer implementation useful for tests and smoke runs.
type StubIndexer struct {
	Logger  *log.Logger
	OnIndex func(ctx context.Context, path string) error
}

// NewStubIndexer creates a stub indexer that logs each triggered file path
// and optionally invokes onIndex.
func NewStubIndexer(logger *log.Logger, onIndex func(ctx context.Context, path string) error) *StubIndexer {
	return &StubIndexer{
		Logger:  logger,
		OnIndex: onIndex,
	}
}

// IndexFile implements Indexer as a stub.
func (s *StubIndexer) IndexFile(ctx context.Context, path string) error {
	if s.Logger != nil {
		s.Logger.Printf("[indexer-stub] started for %s", path)
	}
	if s.OnIndex != nil {
		return s.OnIndex(ctx, path)
	}
	return nil
}

// Stats tracks indexing counters using lock-free atomics.
type Stats struct {
	Indexed       atomic.Int64
	Skipped       atomic.Int64
	BlobCacheHits atomic.Int64
	Placeholders  atomic.Int64
	Deleted       atomic.Int64
	Errors        atomic.Int64
}

// StatsSnapshot is a plain-value copy of Stats.
type StatsSnapshot struct {
	Indexed       int64
	Skipped       int64
	BlobCacheHits int64
	Placeholders  int64
	Deleted       int64
	Errors        int64
}

// Snapshot returns a point-in-time copy of the counters.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Indexed:       s.Indexed.Load(),
		Skipped:       s.Skipped.Load(),
		BlobCacheHits: s.BlobCacheHits.Load(),
		Placeholders:  s.Placeholders.Load(),
		Deleted:       s.Deleted.Load(),
		Errors:        s.Errors.Load(),
	}
}

// PrepareOutcome describes the write action produced by PrepareUpsert.
type PrepareOutcome struct {
	Upsert       *store.UpsertItem
	TouchScanRel string
	TouchScanID  int64
	DeleteRel    string
}

// CatalogIndexer indexes files for a single workspace into a SQLite Store using
// bounded BLAKE3 card fingerprinting and format peekers.
type CatalogIndexer struct {
	Store          *store.Store
	Workspace      store.Workspace
	PerFileTimeout time.Duration
	Stats          Stats

	activeScanID atomic.Int64
}

// NewCatalogIndexer registers rootPath in st and returns a configured CatalogIndexer.
func NewCatalogIndexer(ctx context.Context, st *store.Store, rootPath string) (*CatalogIndexer, error) {
	if st == nil {
		return nil, errors.New("store is required")
	}
	ws, err := st.EnsureWorkspace(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	c := &CatalogIndexer{
		Store:          st,
		Workspace:      ws,
		PerFileTimeout: DefaultPerFileTimeout,
	}
	c.activeScanID.Store(ws.CurrentScanID)
	return c, nil
}

// SetActiveScanID records the latest workspace scan generation so live upserts
// created mid-scan are stamped with the active scan ID and never pruned.
func (c *CatalogIndexer) SetActiveScanID(scanID int64) {
	for {
		cur := c.activeScanID.Load()
		if scanID <= cur {
			return
		}
		if c.activeScanID.CompareAndSwap(cur, scanID) {
			return
		}
	}
}

// ActiveScanID returns the highest known scan generation for this workspace.
func (c *CatalogIndexer) ActiveScanID() int64 {
	return c.activeScanID.Load()
}

// RelPath resolves an absolute or relative path into a workspace-relative slash path.
func (c *CatalogIndexer) RelPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(c.Workspace.RootPath, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside workspace %q", abs, c.Workspace.RootPath)
	}
	return filepath.ToSlash(rel), nil
}

// IndexFile computes the file's card and immediately commits it to the store.
// For high-throughput batching and concurrency, prefer driving via Pipeline.
func (c *CatalogIndexer) IndexFile(ctx context.Context, path string) error {
	out, err := c.PrepareUpsert(ctx, path, 0)
	if err != nil {
		if !isContextErr(err) {
			c.Stats.Errors.Add(1)
		}
		return err
	}
	if out.DeleteRel != "" {
		n, err := c.Store.DeletePath(ctx, c.Workspace.ID, out.DeleteRel)
		if err != nil {
			c.Stats.Errors.Add(1)
			return err
		}
		if n > 0 {
			c.Stats.Deleted.Add(int64(n))
		}
		return nil
	}
	if out.TouchScanRel != "" && out.TouchScanID > 0 {
		if err := c.Store.TouchFileScanBatch(ctx, c.Workspace.ID, out.TouchScanID, []string{out.TouchScanRel}); err != nil {
			c.Stats.Errors.Add(1)
			return err
		}
		return nil
	}
	if out.Upsert == nil {
		return nil
	}
	if err := c.Store.UpsertBatch(ctx, []store.UpsertItem{*out.Upsert}); err != nil {
		c.Stats.Errors.Add(1)
		return err
	}
	c.Stats.Indexed.Add(1)
	return nil
}

// RemoveFile removes path (whether a file or a directory prefix) from the workspace catalog.
func (c *CatalogIndexer) RemoveFile(ctx context.Context, path string) error {
	rel, err := c.RelPath(path)
	if err != nil {
		return err
	}
	n, err := c.Store.DeletePath(ctx, c.Workspace.ID, rel)
	if err != nil {
		c.Stats.Errors.Add(1)
		return err
	}
	if n > 0 {
		c.Stats.Deleted.Add(int64(n))
	}
	return nil
}

// PruneMissing removes rows for files no longer on disk.
func (c *CatalogIndexer) PruneMissing(ctx context.Context, seenRelPaths map[string]struct{}) (int, error) {
	n, err := c.Store.DeleteMissing(ctx, c.Workspace.ID, seenRelPaths)
	if err != nil {
		c.Stats.Errors.Add(1)
		return 0, err
	}
	if n > 0 {
		c.Stats.Deleted.Add(int64(n))
	}
	return n, nil
}

// PrepareUpsert runs stages 1–4 of the indexing pipeline (filter, stat, fast-path skip,
// BLAKE3 hash, blob cache lookup, and bounded peek) without committing to SQLite.
// Context cancellation/deadline errors from ctx are returned directly and never persisted as unreadable rows.
func (c *CatalogIndexer) PrepareUpsert(ctx context.Context, path string, scanID int64) (PrepareOutcome, error) {
	if err := ctx.Err(); err != nil {
		return PrepareOutcome{}, err
	}
	if fsutil.IsIgnoredFile(path) {
		return PrepareOutcome{}, nil
	}

	rel, err := c.RelPath(path)
	if err != nil {
		return PrepareOutcome{}, err
	}

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PrepareOutcome{DeleteRel: rel}, nil
		}
		return PrepareOutcome{}, err
	}

	// Reject directories, symlinks, and non-indexable special files while allowing
	// Windows Cloud Files API reparse points (OneDrive/Dropbox) through to IsCloudPlaceholder.
	if !fsutil.IsIndexableFile(info) {
		return PrepareOutcome{}, nil
	}

	effectiveScanID := max(scanID, c.activeScanID.Load())
	filename := filepath.Base(path)
	folder := filepath.ToSlash(filepath.Dir(rel))
	if folder == "." {
		folder = ""
	}
	ext := strings.ToLower(filepath.Ext(filename))
	size := info.Size()
	mtimeNS := info.ModTime().UnixNano()
	now := time.Now().Unix()

	// Stage 1: Cloud placeholder check BEFORE opening the file.
	if fsutil.IsCloudPlaceholder(path, info) {
		existing, err := c.Store.GetFileMeta(ctx, c.Workspace.ID, rel)
		if err != nil {
			return PrepareOutcome{}, err
		}
		if existing != nil && existing.Size == size && existing.MtimeNS == mtimeNS && existing.IndexStatus == store.StatusPlaceholder {
			c.Stats.Skipped.Add(1)
			if effectiveScanID > existing.LastSeenScanID {
				return PrepareOutcome{TouchScanRel: rel, TouchScanID: effectiveScanID}, nil
			}
			return PrepareOutcome{}, nil
		}
		c.Stats.Placeholders.Add(1)
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					PeekerVersion:  0,
					IndexDepth:     store.DepthMetadataOnly,
					IndexStatus:    store.StatusPlaceholder,
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
			},
		}, nil
	}

	targetPeekerID := peek.PeekerIDForExt(ext)
	targetVer := peek.PeekerVersionForExt(ext)

	// Stage 2: Fast-path skip when (size, mtime_ns, peeker_id, peeker_version) are unchanged.
	existing, err := c.Store.GetFileMeta(ctx, c.Workspace.ID, rel)
	if err != nil {
		return PrepareOutcome{}, err
	}
	if existing != nil &&
		existing.Size == size &&
		existing.MtimeNS == mtimeNS &&
		existing.PeekerID == targetPeekerID &&
		existing.PeekerVersion == targetVer &&
		(existing.IndexStatus == store.StatusOK || existing.IndexStatus == store.StatusTooLarge) {
		c.Stats.Skipped.Add(1)
		if effectiveScanID > existing.LastSeenScanID {
			return PrepareOutcome{TouchScanRel: rel, TouchScanID: effectiveScanID}, nil
		}
		return PrepareOutcome{}, nil
	}

	// Stage 3: Metadata-only formats skip hashing and content reads.
	if targetPeekerID == "" {
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					PeekerID:       "",
					PeekerVersion:  0,
					IndexDepth:     store.DepthMetadataOnly,
					IndexStatus:    store.StatusOK,
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
			},
		}, nil
	}

	// Stage 4: Hash only the bounded bytes consumed by the peeker (head prefix or ZIP central directory)
	// and check the content_blobs cache keyed by (blake3, peeker_id, peeker_version).
	hashHex, err := peek.Fingerprint(ctx, path, size)
	if err != nil {
		if isContextErr(err) || ctx.Err() != nil {
			return PrepareOutcome{}, err
		}
		if errors.Is(err, os.ErrNotExist) {
			return PrepareOutcome{DeleteRel: rel}, nil
		}
		if errors.Is(err, peek.ErrTooLarge) {
			return PrepareOutcome{
				Upsert: &store.UpsertItem{
					File: store.FileRecord{
						WorkspaceID:    c.Workspace.ID,
						RelPath:        rel,
						Filename:       filename,
						Folder:         folder,
						Ext:            ext,
						Size:           size,
						MtimeNS:        mtimeNS,
						PeekerID:       targetPeekerID,
						PeekerVersion:  targetVer,
						IndexDepth:     store.DepthMetadataOnly,
						IndexStatus:    store.StatusTooLarge,
						ErrorMsg:       err.Error(),
						IndexedAt:      now,
						LastSeenScanID: effectiveScanID,
					},
				},
			}, nil
		}
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					PeekerID:       targetPeekerID,
					PeekerVersion:  targetVer,
					IndexDepth:     store.DepthMetadataOnly,
					IndexStatus:    store.StatusUnreadable,
					ErrorMsg:       err.Error(),
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
			},
		}, nil
	}

	// Defense-in-depth: for ZIP/OOXML containers, never fall through to unbounded zip.OpenReader
	// when Fingerprint could not validate a bounded central directory.
	if hashHex == "" && peek.IsZipContainer(ext) {
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					PeekerID:       targetPeekerID,
					PeekerVersion:  targetVer,
					IndexDepth:     store.DepthMetadataOnly,
					IndexStatus:    store.StatusUnreadable,
					ErrorMsg:       "invalid or unreadable zip central directory",
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
			},
		}, nil
	}

	var cachedBlob *store.ContentBlob
	if hashHex != "" {
		cachedBlob, err = c.Store.GetBlob(ctx, hashHex, targetPeekerID, targetVer)
		if err != nil {
			return PrepareOutcome{}, err
		}
	}
	if cachedBlob != nil {
		if stable, err := verifyStable(path, size, mtimeNS); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return PrepareOutcome{DeleteRel: rel}, nil
			}
			return PrepareOutcome{}, err
		} else if !stable {
			return PrepareOutcome{}, ErrFileChanged
		}
		c.Stats.BlobCacheHits.Add(1)
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					BLAKE3:         hashHex,
					PeekerID:       cachedBlob.PeekerID,
					PeekerVersion:  cachedBlob.PeekerVersion,
					IndexDepth:     cachedBlob.IndexDepth,
					IndexStatus:    store.StatusOK,
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
				Blob: nil, // Reuse existing blob in content_blobs
			},
		}, nil
	}

	// Cache miss: run bounded format peeker with timeout.
	timeout := c.PerFileTimeout
	if timeout <= 0 {
		timeout = DefaultPerFileTimeout
	}
	peekCtx, cancel := context.WithTimeout(ctx, timeout)
	res := peek.Peek(peekCtx, path, size)
	peekTimedOut := peekCtx.Err() != nil
	cancel()

	// If parent context was cancelled, abort without clobbering existing DB rows.
	if err := ctx.Err(); err != nil {
		return PrepareOutcome{}, err
	}

	// Verify file did not change mid-read (guards against partial writes).
	if stable, err := verifyStable(path, size, mtimeNS); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PrepareOutcome{DeleteRel: rel}, nil
		}
		return PrepareOutcome{}, err
	} else if !stable {
		return PrepareOutcome{}, ErrFileChanged
	}

	// Never cache a degraded card produced under per-file timeout.
	if peekTimedOut {
		return PrepareOutcome{
			Upsert: &store.UpsertItem{
				File: store.FileRecord{
					WorkspaceID:    c.Workspace.ID,
					RelPath:        rel,
					Filename:       filename,
					Folder:         folder,
					Ext:            ext,
					Size:           size,
					MtimeNS:        mtimeNS,
					PeekerID:       targetPeekerID,
					PeekerVersion:  res.PeekerVersion,
					IndexDepth:     store.DepthMetadataOnly,
					IndexStatus:    store.StatusError,
					ErrorMsg:       "peek timed out",
					IndexedAt:      now,
					LastSeenScanID: effectiveScanID,
				},
			},
		}, nil
	}

	var blob *store.ContentBlob
	fileBlake3 := ""
	if hashHex != "" && res.IndexStatus == store.StatusOK && res.IndexDepth != store.DepthMetadataOnly {
		fileBlake3 = hashHex
		blob = &store.ContentBlob{
			BLAKE3:        hashHex,
			PeekerID:      targetPeekerID,
			PeekerVersion: res.PeekerVersion,
			IndexDepth:    res.IndexDepth,
			Title:         res.Title,
			Headings:      res.Headings,
			BodyPreview:   res.BodyPreview,
			OutlineJSON:   res.OutlineJSON,
			UnitCount:     res.UnitCount,
			ExtractedAt:   now,
		}
	}

	return PrepareOutcome{
		Upsert: &store.UpsertItem{
			File: store.FileRecord{
				WorkspaceID:    c.Workspace.ID,
				RelPath:        rel,
				Filename:       filename,
				Folder:         folder,
				Ext:            ext,
				Size:           size,
				MtimeNS:        mtimeNS,
				BLAKE3:         fileBlake3,
				PeekerID:       targetPeekerID,
				PeekerVersion:  res.PeekerVersion,
				IndexDepth:     res.IndexDepth,
				IndexStatus:    res.IndexStatus,
				ErrorMsg:       res.ErrorMsg,
				IndexedAt:      now,
				LastSeenScanID: effectiveScanID,
			},
			Blob: blob,
		},
	}, nil
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func verifyStable(path string, wantSize int64, wantMtimeNS int64) (bool, error) {
	after, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return after.Size() == wantSize && after.ModTime().UnixNano() == wantMtimeNS, nil
}
