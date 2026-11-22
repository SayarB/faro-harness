// Package watcher recursively watches a workspace folder, debounces filesystem bursts,
// and forwards file upserts and removals to the configured indexer worker.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SayarB/faro-harness/internal/fsutil"
	"github.com/SayarB/faro-harness/internal/worker"
	"github.com/fsnotify/fsnotify"
)

const (
	defaultDebounceDelay = 300 * time.Millisecond
	defaultMaxConcurrent = 4
)

// Watcher watches a directory tree for file additions, edits, renames, and deletions
// and triggers the configured indexer worker with per-path debouncing.
type Watcher struct {
	folder        string
	indexer       worker.Indexer
	fsw           *fsnotify.Watcher
	logger        *log.Logger
	onError       func(error)
	debounceDelay time.Duration
	initialScan   bool

	watchMu     sync.Mutex
	watchedDirs map[string]struct{}

	timerMu sync.Mutex
	timers  map[string]*time.Timer
	stopped bool
}

// Option configures optional Watcher behavior.
type Option func(*Watcher)

// WithLogger sets a logger for watcher diagnostics.
func WithLogger(logger *log.Logger) Option {
	return func(w *Watcher) {
		w.logger = logger
	}
}

// WithErrorHandler sets a callback invoked when watching or indexing returns an error.
func WithErrorHandler(fn func(error)) Option {
	return func(w *Watcher) {
		w.onError = fn
	}
}

// WithDebounce overrides the per-path quiet window before firing an upsert (default 300ms).
func WithDebounce(d time.Duration) Option {
	return func(w *Watcher) {
		if d >= 0 {
			w.debounceDelay = d
		}
	}
}

// WithInitialScan controls whether Run walks and reconciles existing files on startup (default true).
func WithInitialScan(enabled bool) Option {
	return func(w *Watcher) {
		w.initialScan = enabled
	}
}

// New creates a recursive Watcher for folder and registers OS watches on all non-ignored subdirectories.
func New(folder string, indexer worker.Indexer, opts ...Option) (*Watcher, error) {
	if folder == "" {
		return nil, errors.New("folder path is required")
	}
	if indexer == nil {
		return nil, errors.New("indexer is required")
	}

	absFolder, err := filepath.Abs(folder)
	if err != nil {
		return nil, fmt.Errorf("resolve folder path: %w", err)
	}
	info, err := os.Stat(absFolder)
	if err != nil {
		return nil, fmt.Errorf("stat folder %q: %w", absFolder, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", absFolder)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	w := &Watcher{
		folder:        absFolder,
		indexer:       indexer,
		fsw:           fsw,
		debounceDelay: defaultDebounceDelay,
		initialScan:   true,
		watchedDirs:   make(map[string]struct{}),
		timers:        make(map[string]*time.Timer),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}

	if err := w.addWatchRecursive(absFolder); err != nil {
		_ = fsw.Close()
		return nil, err
	}
	return w, nil
}

// Folder returns the absolute path of the watched folder.
func (w *Watcher) Folder() string {
	return w.folder
}

// Close releases underlying OS watch resources.
func (w *Watcher) Close() error {
	w.stopAllTimers()
	return w.fsw.Close()
}

// Run blocks until ctx is cancelled or the underlying watcher closes.
// On startup (when initialScan is true), it reconciles existing files and prunes stale entries.
// Live filesystem events are debounced per path and dispatched through a bounded worker pool.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// Bounded dispatch semaphore for indexers that do not manage their own pipeline.
	sem := make(chan struct{}, defaultMaxConcurrent)
	var wg sync.WaitGroup
	defer func() {
		// Cancel runCtx and stop all timers BEFORE waiting on wg so initial scans and
		// timer callbacks exit promptly without racing wg.Add against wg.Wait.
		cancelRun()
		w.stopAllTimers()
		wg.Wait()
	}()

	dispatchUpsert := func(path string) {
		if pipe, ok := w.indexer.(*worker.Pipeline); ok {
			pipe.EnqueueLive(runCtx, path)
			return
		}
		w.timerMu.Lock()
		if w.stopped || runCtx.Err() != nil {
			w.timerMu.Unlock()
			return
		}
		wg.Add(1)
		w.timerMu.Unlock()

		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				return
			}
			if err := w.indexer.IndexFile(runCtx, path); err != nil && !errors.Is(err, context.Canceled) {
				w.reportError(fmt.Errorf("index %q: %w", path, err))
			}
		}()
	}

	dispatchRemove := func(path string) {
		if pipe, ok := w.indexer.(*worker.Pipeline); ok {
			pipe.EnqueueRemove(runCtx, path)
			return
		}
		rem, ok := w.indexer.(worker.Remover)
		if !ok {
			return
		}
		w.timerMu.Lock()
		if w.stopped || runCtx.Err() != nil {
			w.timerMu.Unlock()
			return
		}
		wg.Add(1)
		w.timerMu.Unlock()

		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				return
			}
			if err := rem.RemoveFile(runCtx, path); err != nil && !errors.Is(err, context.Canceled) {
				w.reportError(fmt.Errorf("remove %q: %w", path, err))
			}
		}()
	}

	// Run startup crawl asynchronously so live fsnotify events are never blocked.
	if w.initialScan {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runInitialScan(runCtx, dispatchUpsert)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(runCtx, ev, dispatchUpsert, dispatchRemove)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			w.reportError(err)
		}
	}
}

func (w *Watcher) runInitialScan(ctx context.Context, dispatchUpsert func(string)) {
	if pipe, ok := w.indexer.(*worker.Pipeline); ok {
		if _, err := pipe.IndexWorkspace(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, worker.ErrPipelineClosed) {
			w.reportError(fmt.Errorf("initial workspace scan: %w", err))
		}
		return
	}

	seen := make(map[string]struct{})
	var hadWalkErr bool
	_ = filepath.WalkDir(w.folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			hadWalkErr = true
			w.reportError(err)
			if d == nil || d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != w.folder && fsutil.IsIgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !fsutil.IsIndexableDirEntry(d) {
			return nil
		}
		if fsutil.IsIgnoredFile(path) {
			return nil
		}
		if rel, err := filepath.Rel(w.folder, path); err == nil {
			seen[filepath.ToSlash(rel)] = struct{}{}
		}
		dispatchUpsert(path)
		return nil
	})

	if !hadWalkErr {
		if pruner, ok := w.indexer.(worker.Pruner); ok && ctx.Err() == nil {
			if _, err := pruner.PruneMissing(ctx, seen); err != nil && !errors.Is(err, context.Canceled) {
				w.reportError(fmt.Errorf("prune missing: %w", err))
			}
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, ev fsnotify.Event, dispatchUpsert func(string), dispatchRemove func(string)) {
	path := ev.Name
	if path == "" {
		return
	}

	// Handle deletes and renames (old path) immediately.
	// Store.DeletePath handles both single files and renamed/trashed directory subtrees.
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		w.cancelTimer(path)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			wasWatchedDir := w.removeWatchesUnder(path)
			// Never filter a removed directory through IsIgnoredFile (e.g. "backup.bak/" or "old~/").
			if wasWatchedDir || !fsutil.IsIgnoredFile(path) {
				dispatchRemove(path)
			}
		}
		if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
			return
		}
	}

	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
		return
	}

	info, err := os.Lstat(path)
	if err != nil {
		return
	}

	// If a new directory appeared (mkdir or renamed into workspace), register recursive
	// watches and index any files already inside it.
	if info.IsDir() {
		if ev.Has(fsnotify.Create) && !fsutil.IsIgnoredDir(filepath.Base(path)) {
			if err := w.addWatchRecursive(path); err != nil {
				w.reportError(err)
			}
			w.enqueueExistingInDir(ctx, path, dispatchUpsert)
		}
		return
	}

	if !fsutil.IsIndexableFile(info) {
		return
	}
	if fsutil.IsIgnoredFile(path) {
		return
	}

	w.scheduleDebounced(ctx, path, dispatchUpsert)
}

func (w *Watcher) scheduleDebounced(ctx context.Context, path string, dispatchUpsert func(string)) {
	if w.debounceDelay == 0 {
		dispatchUpsert(path)
		return
	}

	w.timerMu.Lock()
	defer w.timerMu.Unlock()

	if w.stopped {
		return
	}
	if existing, ok := w.timers[path]; ok {
		existing.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(w.debounceDelay, func() {
		w.timerMu.Lock()
		if cur, ok := w.timers[path]; !ok || cur != timer || w.stopped {
			w.timerMu.Unlock()
			return
		}
		delete(w.timers, path)
		w.timerMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		dispatchUpsert(path)
	})
	w.timers[path] = timer
}

func (w *Watcher) cancelTimer(path string) {
	w.timerMu.Lock()
	if t, ok := w.timers[path]; ok {
		t.Stop()
		delete(w.timers, path)
	}
	// Also cancel any pending timers for files nested under a removed directory.
	prefix := path + string(filepath.Separator)
	for p, t := range w.timers {
		if strings.HasPrefix(p, prefix) {
			t.Stop()
			delete(w.timers, p)
		}
	}
	w.timerMu.Unlock()
}

func (w *Watcher) stopAllTimers() {
	w.timerMu.Lock()
	w.stopped = true
	for p, t := range w.timers {
		t.Stop()
		delete(w.timers, p)
	}
	w.timerMu.Unlock()
}

func (w *Watcher) addWatchRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != w.folder && fsutil.IsIgnoredDir(d.Name()) {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(path); err != nil {
			return fmt.Errorf("watch folder %q: %w", path, err)
		}
		w.watchMu.Lock()
		w.watchedDirs[path] = struct{}{}
		w.watchMu.Unlock()
		return nil
	})
}

// removeWatchesUnder unregisters path and every watched subdirectory beneath it so
// inotify/kqueue watches don't leak when a directory tree is renamed or moved to Trash.
// Returns true if path (or any descendant) was a watched directory.
func (w *Watcher) removeWatchesUnder(path string) bool {
	prefix := path + string(filepath.Separator)
	w.watchMu.Lock()
	var toRemove []string
	for d := range w.watchedDirs {
		if d == path || strings.HasPrefix(d, prefix) {
			toRemove = append(toRemove, d)
			delete(w.watchedDirs, d)
		}
	}
	w.watchMu.Unlock()

	for _, d := range toRemove {
		_ = w.fsw.Remove(d)
	}
	return len(toRemove) > 0
}

func (w *Watcher) enqueueExistingInDir(ctx context.Context, root string, dispatchUpsert func(string)) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && fsutil.IsIgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !fsutil.IsIndexableDirEntry(d) || fsutil.IsIgnoredFile(path) {
			return nil
		}
		w.scheduleDebounced(ctx, path, dispatchUpsert)
		return nil
	})
}

func (w *Watcher) reportError(err error) {
	if w.onError != nil {
		w.onError(err)
		return
	}
	if w.logger != nil {
		w.logger.Printf("[watcher] error: %v", err)
	}
}
