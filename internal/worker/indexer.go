package worker

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/SayarB/faro-harness/internal/peek"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/watcher"
	"lukechampine.com/blake3"
)

// Progress reports live indexing counts to the UI or CLI.
type Progress struct {
	Workspace string `json:"workspace"`
	Scanned   int64  `json:"scanned"`
	Indexed   int64  `json:"indexed"`
	Skipped   int64  `json:"skipped"`
	Errors    int64  `json:"errors"`
	Done      bool   `json:"done"`
}

// Indexer runs a bounded Go worker pool that crawls a workspace, hashes changed files
// with BLAKE3, deduplicates against blobs, and commits fast peeks into SQLite FTS5.
type Indexer struct {
	store      *store.Store
	workers    int
	onProgress func(Progress)
}

func NewIndexer(st *store.Store, workers int, onProgress func(Progress)) *Indexer {
	if workers <= 0 {
		workers = max(2, min(4, runtime.NumCPU()))
	}
	return &Indexer{
		store:      st,
		workers:    workers,
		onProgress: onProgress,
	}
}

// IndexWorkspace walks root and indexes all changed files concurrently.
func (idx *Indexer) IndexWorkspace(ctx context.Context, root string) (Progress, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Progress{}, err
	}

	paths := make(chan string, 256)
	var scanned, indexed, skipped, errs atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < idx.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				if ctx.Err() != nil {
					return
				}
				didIndex, err := idx.IndexFile(ctx, absRoot, p)
				if err != nil {
					errs.Add(1)
				} else if didIndex {
					indexed.Add(1)
				} else {
					skipped.Add(1)
				}
				if idx.onProgress != nil && (indexed.Load()+skipped.Load())%25 == 0 {
					idx.onProgress(Progress{
						Workspace: absRoot,
						Scanned:   scanned.Load(),
						Indexed:   indexed.Load(),
						Skipped:   skipped.Load(),
						Errors:    errs.Load(),
					})
				}
			}
		}()
	}

	walkErr := filepath.WalkDir(absRoot, func(p string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if p != absRoot && watcher.ShouldIgnore(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		scanned.Add(1)
		select {
		case paths <- p:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	close(paths)
	wg.Wait()

	final := Progress{
		Workspace: absRoot,
		Scanned:   scanned.Load(),
		Indexed:   indexed.Load(),
		Skipped:   skipped.Load(),
		Errors:    errs.Load(),
		Done:      true,
	}
	if idx.onProgress != nil {
		idx.onProgress(final)
	}
	return final, walkErr
}

// IndexFile checks (size, mtime), hashes with BLAKE3 on change, reuses cached blobs,
// and upserts the file + FTS5 entry. Returns true if the file was (re)indexed.
func (idx *Indexer) IndexFile(ctx context.Context, workspace, absPath string) (bool, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}

	size := info.Size()
	mtimeNs := info.ModTime().UnixNano()

	existing, err := idx.store.GetFileByPath(ctx, absPath)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.Size == size && existing.MtimeUnixNs == mtimeNs && existing.Status == "indexed" {
		return false, nil
	}

	relPath, err := filepath.Rel(workspace, absPath)
	if err != nil {
		relPath = filepath.Base(absPath)
	}
	relPath = filepath.ToSlash(relPath)
	folder := filepath.ToSlash(filepath.Dir(relPath))
	if folder == "." {
		folder = ""
	}
	filename := filepath.Base(absPath)
	ext := strings.ToLower(filepath.Ext(filename))

	hash, err := hashFileBLAKE3(absPath)
	if err != nil {
		return false, fmt.Errorf("hash %s: %w", absPath, err)
	}

	// Content-addressed cache hit: skip extraction on renames, moves, and duplicates.
	blob, err := idx.store.GetBlob(ctx, hash, peek.ExtractorVersion)
	if err != nil {
		return false, err
	}
	if blob == nil {
		peeked, peekErr := peek.File(absPath, size)
		if peekErr != nil {
			// Record Tier 0 metadata even if content extraction fails.
			_, saveErr := idx.store.UpsertIndexedFile(ctx, store.FileRecord{
				Workspace:    workspace,
				AbsPath:      absPath,
				RelPath:      relPath,
				Folder:       folder,
				Filename:     filename,
				Ext:          ext,
				Size:         size,
				MtimeUnixNs:  mtimeNs,
				ContentHash:  hash,
				IndexDepth:   store.DepthMetadataOnly,
				Status:       "error",
				ErrorMessage: peekErr.Error(),
			}, nil)
			return true, saveErr
		}
		blob = &store.BlobRecord{
			ContentHash:      hash,
			ExtractorVersion: peek.ExtractorVersion,
			IndexDepth:       peeked.Depth,
			Headings:         peeked.Headings,
			Body:             peeked.Body,
			OutlineJSON:      peeked.OutlineJSON,
		}
	}

	_, err = idx.store.UpsertIndexedFile(ctx, store.FileRecord{
		Workspace:   workspace,
		AbsPath:     absPath,
		RelPath:     relPath,
		Folder:      folder,
		Filename:    filename,
		Ext:         ext,
		Size:        size,
		MtimeUnixNs: mtimeNs,
		ContentHash: hash,
		IndexDepth:  blob.IndexDepth,
		Status:      "indexed",
	}, blob)
	return true, err
}

func hashFileBLAKE3(absPath string) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
