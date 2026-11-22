package worker

import (
	"context"
	"time"

	"github.com/SayarB/faro-harness/internal/store"
)

// ProgressInterval is how often IndexFolder reports live counters.
const ProgressInterval = 500 * time.Millisecond

// IndexFolder registers root as a workspace (if needed) and runs one full index pass.
// If onProgress is non-nil it is called with live counters every ProgressInterval while the
// pass runs. The counters have no total, so callers should present them as activity, not a percentage.
// The final snapshot is returned (not passed to onProgress); callers report completion themselves.
func IndexFolder(ctx context.Context, st *store.Store, root string, onProgress func(StatsSnapshot)) (store.Workspace, StatsSnapshot, error) {
	cat, err := NewCatalogIndexer(ctx, st, root)
	if err != nil {
		return store.Workspace{}, StatsSnapshot{}, err
	}
	pipe := NewPipeline(cat, PipelineConfig{})
	defer pipe.Close()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if onProgress == nil {
			return
		}
		t := time.NewTicker(ProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				onProgress(cat.Stats.Snapshot())
			case <-done:
				return
			}
		}
	}()

	snap, err := pipe.IndexWorkspace(ctx)
	close(done)
	<-finished
	return cat.Workspace, snap, err
}
