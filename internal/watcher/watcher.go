package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Event represents a debounced filesystem change.
type Event struct {
	Path    string
	Removed bool
}

// ShouldIgnore skips OS noise, Office lock files, and developer dependency trees.
func ShouldIgnore(path string) bool {
	base := filepath.Base(path)
	if base == ".DS_Store" || base == "Thumbs.db" || base == "desktop.ini" {
		return true
	}
	// Microsoft Office temporary lock files (e.g. ~$Budget.xlsx)
	if strings.HasPrefix(base, "~$") || strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".part") {
		return true
	}
	switch base {
	case ".git", "node_modules", ".venv", "__pycache__", ".Trash", ".faro":
		return true
	}
	return false
}

// Watcher monitors a workspace directory recursively with debounced event delivery.
type Watcher struct {
	fsw      *fsnotify.Watcher
	debounce time.Duration
	onEvent  func(Event)
}

func New(debounce time.Duration, onEvent func(Event)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if debounce <= 0 {
		debounce = 350 * time.Millisecond
	}
	return &Watcher{
		fsw:      fsw,
		debounce: debounce,
		onEvent:  onEvent,
	}, nil
}

// AddRecursive registers root and all non-ignored subdirectories.
func (w *Watcher) AddRecursive(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && ShouldIgnore(p) {
				return filepath.SkipDir
			}
			_ = w.fsw.Add(p)
		}
		return nil
	})
}

// Run processes filesystem events until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()

	var mu sync.Mutex
	timers := make(map[string]*time.Timer)

	schedule := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[ev.Path]; ok {
			t.Stop()
		}
		timers[ev.Path] = time.AfterFunc(w.debounce, func() {
			mu.Lock()
			delete(timers, ev.Path)
			mu.Unlock()
			w.onEvent(ev)
		})
	}

	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, t := range timers {
				t.Stop()
			}
			mu.Unlock()
			return nil

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			if ShouldIgnore(ev.Name) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.AddRecursive(ev.Name)
					continue
				}
			}
			removed := ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)
			schedule(Event{Path: ev.Name, Removed: removed})

		case _, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
		}
	}
}
