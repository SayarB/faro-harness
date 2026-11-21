package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/watcher"
	"github.com/SayarB/faro-harness/internal/worker"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App exposes Go worker & SQLite FTS5 indexing methods directly to the React UI via Wails bindings.
type App struct {
	ctx       context.Context
	store     *store.Store
	indexer   *worker.Indexer
	mu        sync.Mutex
	watchStop context.CancelFunc
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	dataDir, err := defaultDataDir()
	if err != nil {
		runtime.LogErrorf(ctx, "resolve data dir: %v", err)
		return
	}
	st, err := store.Open(filepath.Join(dataDir, "index.db"))
	if err != nil {
		runtime.LogErrorf(ctx, "open sqlite store: %v", err)
		return
	}
	a.store = st
	a.indexer = worker.NewIndexer(st, 0, func(p worker.Progress) {
		runtime.EventsEmit(a.ctx, "index:progress", p)
	})
}

func (a *App) shutdown(_ context.Context) {
	a.stopWatcher()
	if a.store != nil {
		_ = a.store.Close()
	}
}

func (a *App) stopWatcher() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.watchStop != nil {
		a.watchStop()
		a.watchStop = nil
	}
}

// PickWorkspace opens the native OS folder picker, indexes the selected directory,
// and starts a live debounced watcher on it.
func (a *App) PickWorkspace() (worker.Progress, error) {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a folder for Faro to index",
	})
	if err != nil || dir == "" {
		return worker.Progress{}, err
	}
	return a.IndexWorkspace(dir)
}

// IndexWorkspace crawls and watches root.
func (a *App) IndexWorkspace(root string) (worker.Progress, error) {
	if a.indexer == nil {
		return worker.Progress{}, fmt.Errorf("store not initialized")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return worker.Progress{}, err
	}

	prog, err := a.indexer.IndexWorkspace(a.ctx, absRoot)
	if err != nil {
		return prog, err
	}

	a.startWatcher(absRoot)
	return prog, nil
}

func (a *App) startWatcher(root string) {
	a.stopWatcher()

	wCtx, cancel := context.WithCancel(a.ctx)
	w, err := watcher.New(350*time.Millisecond, func(ev watcher.Event) {
		if ev.Removed {
			_ = a.store.DeleteFileByPath(wCtx, ev.Path)
			runtime.EventsEmit(a.ctx, "index:updated", ev.Path)
			return
		}
		if changed, err := a.indexer.IndexFile(wCtx, root, ev.Path); err == nil && changed {
			runtime.EventsEmit(a.ctx, "index:updated", ev.Path)
		}
	})
	if err != nil {
		cancel()
		return
	}
	if err := w.AddRecursive(root); err != nil {
		cancel()
		return
	}

	a.mu.Lock()
	a.watchStop = cancel
	a.mu.Unlock()

	go func() {
		_ = w.Run(wCtx)
	}()
}

// Search queries the local SQLite FTS5 index with BM25 ranking.
func (a *App) Search(query string, limit int) ([]store.SearchHit, error) {
	if a.store == nil {
		return []store.SearchHit{}, nil
	}
	return a.store.Search(a.ctx, query, limit)
}

// Stats returns aggregate counts of indexed files, blobs, and workspaces.
func (a *App) Stats() (store.Stats, error) {
	if a.store == nil {
		return store.Stats{ByDepth: map[string]int64{}, Workspaces: []string{}}, nil
	}
	return a.store.Stats(a.ctx)
}

func defaultDataDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "faro-harness"), nil
}
