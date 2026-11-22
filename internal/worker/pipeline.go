package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/SayarB/faro-harness/internal/fsutil"
	"github.com/SayarB/faro-harness/internal/store"
)

// ErrPipelineClosed is returned when a scan or flush is interrupted because the pipeline was closed.
var ErrPipelineClosed = errors.New("pipeline closed")

type jobOp uint8

const (
	opUpsert jobOp = iota + 1
	opRemove
)

type lane uint8

const (
	laneBackfill lane = iota + 1
	laneLive
)

type queuedJob struct {
	op      jobOp
	lane    lane
	attempt int
	scanID  int64
}

type retryEntry struct {
	timer  *time.Timer
	scanID int64
}

type writeCommand struct {
	upsert       *store.UpsertItem
	touchScanRel string
	touchScanID  int64
	deleteRel    string
	flushDone    chan error
}

// PipelineConfig tunes worker concurrency, batching, and retry limits.
type PipelineConfig struct {
	NumWorkers    int
	BatchSize     int
	FlushInterval time.Duration
	BackfillCap   int
	MaxRetries    int
	Logger        *log.Logger
	OnError       func(error)
}

func defaultPipelineConfig(cfg PipelineConfig) PipelineConfig {
	if cfg.NumWorkers <= 0 {
		n := runtime.NumCPU() / 2
		if n < 2 {
			n = 2
		}
		if n > 4 {
			n = 4
		}
		cfg.NumWorkers = n
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 150 * time.Millisecond
	}
	if cfg.BackfillCap <= 0 {
		cfg.BackfillCap = 256
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	return cfg
}

// Pipeline runs a fixed pool of indexing workers fed by a high-priority live queue
// and a bounded backfill queue, committing into SQLite through a single batched writer.
//
// All queue coordination uses a single mutex + condition variables so workers never
// deadlock re-enqueueing, WaitIdle never races with concurrent enqueues, prefix removes
// serialize cleanly with child upserts, and Close drains/aborts cleanly on cancellation.
type Pipeline struct {
	indexer *CatalogIndexer
	cfg     PipelineConfig

	writeMu     sync.RWMutex
	writeClosed bool
	writeCh     chan writeCommand

	mu          sync.Mutex
	workCond    *sync.Cond
	notFullCond *sync.Cond
	idleCond    *sync.Cond
	liveQueue   []string
	backfillQ   []string
	queued      map[string]queuedJob
	inFlight    map[string]queuedJob
	dirty       map[string]queuedJob
	retryTimers map[string]retryEntry
	pending     int
	scanPending map[int64]int
	scanErrors  map[int64]bool
	started     bool
	closed      bool

	startOnce sync.Once
	stopOnce  sync.Once
	parentCtx context.Context
	runCtx    context.Context
	cancel    context.CancelFunc
	stopWatch func() bool
	workerWG  sync.WaitGroup
	writerWG  sync.WaitGroup
}

// NewPipeline creates a Pipeline backed by indexer. Call Start(ctx) before enqueuing work,
// or use IndexWorkspace(ctx) for a one-shot crawl and flush.
func NewPipeline(indexer *CatalogIndexer, cfg PipelineConfig) *Pipeline {
	cfg = defaultPipelineConfig(cfg)
	p := &Pipeline{
		indexer:     indexer,
		cfg:         cfg,
		writeCh:     make(chan writeCommand, cfg.BatchSize*2),
		queued:      make(map[string]queuedJob),
		inFlight:    make(map[string]queuedJob),
		dirty:       make(map[string]queuedJob),
		retryTimers: make(map[string]retryEntry),
		scanPending: make(map[int64]int),
		scanErrors:  make(map[int64]bool),
	}
	p.workCond = sync.NewCond(&p.mu)
	p.notFullCond = sync.NewCond(&p.mu)
	p.idleCond = sync.NewCond(&p.mu)
	return p
}

// Catalog returns the underlying CatalogIndexer.
func (p *Pipeline) Catalog() *CatalogIndexer {
	return p.indexer
}

// Start launches the fixed worker pool and single batched SQLite writer.
// Calling Start after Close is a safe no-op.
func (p *Pipeline) Start(parentCtx context.Context) {
	p.startOnce.Do(func() {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(parentCtx)
		p.parentCtx = parentCtx
		p.runCtx = ctx
		p.cancel = cancel
		p.started = true
		p.mu.Unlock()

		// Wake any blocked workers/waiters immediately when parentCtx cancels (e.g. Ctrl-C).
		p.stopWatch = context.AfterFunc(ctx, func() {
			p.mu.Lock()
			p.workCond.Broadcast()
			p.notFullCond.Broadcast()
			p.idleCond.Broadcast()
			p.mu.Unlock()
		})

		p.writerWG.Add(1)
		go func() {
			defer p.writerWG.Done()
			p.runWriter()
		}()

		for i := 0; i < p.cfg.NumWorkers; i++ {
			p.workerWG.Add(1)
			go func() {
				defer p.workerWG.Done()
				p.runWorker(ctx)
			}()
		}
	})
}

// Close shuts down the pipeline cleanly without hanging on cancellation or panicking on concurrent Flush.
// If the parent context is still active, Close drains queued/in-flight jobs before stopping.
// If the parent context is cancelled (e.g. Ctrl-C), Close immediately drops unstarted backlog,
// cancels retry timers, waits for in-flight workers, and flushes completed writes.
func (p *Pipeline) Close() error {
	var flushErr error
	p.stopOnce.Do(func() {
		p.mu.Lock()
		if !p.started {
			p.closed = true
			p.idleCond.Broadcast()
			p.notFullCond.Broadcast()
			p.workCond.Broadcast()
			p.mu.Unlock()
			return
		}
		parentCtx := p.parentCtx
		// Drain pending work only while parentCtx has not been cancelled.
		for p.pending > 0 && parentCtx.Err() == nil && !p.closed {
			p.idleCond.Wait()
		}
		p.closed = true
		p.clearBacklogLocked()
		cancel := p.cancel
		stopWatch := p.stopWatch
		p.workCond.Broadcast()
		p.notFullCond.Broadcast()
		p.idleCond.Broadcast()
		p.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if stopWatch != nil {
			stopWatch()
		}
		p.workerWG.Wait()

		// Flush any writes already pushed by workers, then close writeCh under writeMu.Lock
		// so no concurrent Flush/sendWrite can ever send on a closed channel.
		flushErr = p.flushRaw(context.Background())

		p.writeMu.Lock()
		p.writeClosed = true
		close(p.writeCh)
		p.writeMu.Unlock()

		p.writerWG.Wait()
	})
	return flushErr
}

// --- scanPending-safe map helpers (caller must hold p.mu) ---

func (p *Pipeline) incScanLocked(scanID int64) {
	if scanID > 0 {
		p.scanPending[scanID]++
	}
}

func (p *Pipeline) decScanLocked(scanID int64) {
	if scanID <= 0 {
		return
	}
	p.scanPending[scanID]--
	if p.scanPending[scanID] <= 0 {
		delete(p.scanPending, scanID)
		p.idleCond.Broadcast()
	}
}

func (p *Pipeline) setQueuedLocked(path string, q queuedJob) {
	if old, ok := p.queued[path]; ok {
		p.decScanLocked(old.scanID)
	}
	p.queued[path] = q
	p.incScanLocked(q.scanID)
}

func (p *Pipeline) delQueuedLocked(path string) (queuedJob, bool) {
	old, ok := p.queued[path]
	if ok {
		delete(p.queued, path)
		p.decScanLocked(old.scanID)
	}
	return old, ok
}

func (p *Pipeline) setInFlightLocked(path string, q queuedJob) {
	if old, ok := p.inFlight[path]; ok {
		p.decScanLocked(old.scanID)
	}
	p.inFlight[path] = q
	p.incScanLocked(q.scanID)
}

func (p *Pipeline) delInFlightLocked(path string) (queuedJob, bool) {
	old, ok := p.inFlight[path]
	if ok {
		delete(p.inFlight, path)
		p.decScanLocked(old.scanID)
	}
	return old, ok
}

func (p *Pipeline) setDirtyLocked(path string, q queuedJob) {
	if old, ok := p.dirty[path]; ok {
		p.decScanLocked(old.scanID)
	}
	p.dirty[path] = q
	p.incScanLocked(q.scanID)
}

func (p *Pipeline) delDirtyLocked(path string) (queuedJob, bool) {
	old, ok := p.dirty[path]
	if ok {
		delete(p.dirty, path)
		p.decScanLocked(old.scanID)
	}
	return old, ok
}

func (p *Pipeline) setRetryLocked(path string, r retryEntry) {
	if old, ok := p.retryTimers[path]; ok {
		old.timer.Stop()
		p.decScanLocked(old.scanID)
	}
	p.retryTimers[path] = r
	p.incScanLocked(r.scanID)
}

func (p *Pipeline) delRetryLocked(path string) (retryEntry, bool) {
	old, ok := p.retryTimers[path]
	if ok {
		delete(p.retryTimers, path)
		p.decScanLocked(old.scanID)
	}
	return old, ok
}

func (p *Pipeline) markScanError(scanID int64) {
	if scanID <= 0 {
		return
	}
	p.mu.Lock()
	p.scanErrors[scanID] = true
	p.mu.Unlock()
}

func (p *Pipeline) consumeScanError(scanID int64) bool {
	p.mu.Lock()
	hadErr := p.scanErrors[scanID]
	delete(p.scanErrors, scanID)
	p.mu.Unlock()
	return hadErr
}

// clearBacklogLocked stops all retry timers and drops unstarted queued/dirty items.
// Caller must hold p.mu.
func (p *Pipeline) clearBacklogLocked() {
	for path, r := range p.retryTimers {
		r.timer.Stop()
		p.delRetryLocked(path)
		p.pending--
	}
	for path := range p.queued {
		p.delQueuedLocked(path)
		p.pending--
	}
	for path := range p.dirty {
		p.delDirtyLocked(path)
	}
	p.liveQueue = nil
	p.backfillQ = nil
}

// IndexFile implements Indexer by submitting a high-priority live upsert.
func (p *Pipeline) IndexFile(ctx context.Context, path string) error {
	p.EnqueueLive(ctx, path)
	return nil
}

// RemoveFile implements Remover by submitting a high-priority live remove.
func (p *Pipeline) RemoveFile(ctx context.Context, path string) error {
	p.EnqueueRemove(ctx, path)
	return nil
}

// PruneMissing implements Pruner.
func (p *Pipeline) PruneMissing(ctx context.Context, seenRelPaths map[string]struct{}) (int, error) {
	return p.indexer.PruneMissing(ctx, seenRelPaths)
}

// EnqueueLive submits a high-priority upsert (e.g. from fsnotify), coalescing duplicates
// and promoting any queued backfill job for the same path.
func (p *Pipeline) EnqueueLive(ctx context.Context, path string) {
	p.enqueue(ctx, path, opUpsert, laneLive, 0)
}

// EnqueueRemove submits a high-priority delete (e.g. from fsnotify Remove/Rename).
func (p *Pipeline) EnqueueRemove(ctx context.Context, path string) {
	p.enqueue(ctx, path, opRemove, laneLive, 0)
}

// EnqueueBackfill submits a low-priority upsert (e.g. from initial WalkDir crawl),
// applying backpressure when the backfill queue reaches BackfillCap.
func (p *Pipeline) EnqueueBackfill(ctx context.Context, path string) {
	p.enqueue(ctx, path, opUpsert, laneBackfill, 0)
}

func (p *Pipeline) enqueueBackfillWithScan(ctx context.Context, path string, scanID int64) bool {
	return p.enqueue(ctx, path, opUpsert, laneBackfill, scanID)
}

func (p *Pipeline) enqueue(ctx context.Context, path string, op jobOp, targetLane lane, scanID int64) bool {
	if ctx.Err() != nil {
		return false
	}
	if absPath, err := filepath.Abs(path); err == nil {
		path = absPath
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || (p.runCtx != nil && p.runCtx.Err() != nil) {
		return false
	}

	// If a retry timer is waiting for this path, cancel it and carry its scanID forward.
	if r, ok := p.delRetryLocked(path); ok {
		r.timer.Stop()
		p.pending--
		scanID = max(scanID, r.scanID)
	}

	// If already in-flight, record as dirty (carrying scanID so waitScanIdle waits for it).
	if cur, busy := p.inFlight[path]; busy {
		prev := p.dirty[path]
		p.setDirtyLocked(path, queuedJob{
			op:     op,
			lane:   maxLane(targetLane, prev.lane),
			scanID: max(max(scanID, prev.scanID), cur.scanID),
		})
		return true
	}

	// If already queued, coalesce, carry max scanID, and promote to live lane if needed.
	if existing, ok := p.queued[path]; ok {
		promoted := existing.lane == laneBackfill && targetLane == laneLive
		existing.op = op
		existing.scanID = max(existing.scanID, scanID)
		if promoted {
			existing.lane = laneLive
			p.liveQueue = append(p.liveQueue, path)
		}
		p.setQueuedLocked(path, existing)
		p.workCond.Signal()
		return true
	}

	// Apply backpressure only on backfill lane so live events and workers never block.
	if targetLane == laneBackfill {
		stop := context.AfterFunc(ctx, func() {
			p.mu.Lock()
			p.notFullCond.Broadcast()
			p.mu.Unlock()
		})
		for len(p.backfillQ) >= p.cfg.BackfillCap && !p.closed && ctx.Err() == nil && (p.runCtx == nil || p.runCtx.Err() == nil) {
			p.notFullCond.Wait()
		}
		stop()

		if p.closed || ctx.Err() != nil || (p.runCtx != nil && p.runCtx.Err() != nil) {
			return false
		}
		// Re-check coalescing after Wait() released p.mu.
		if cur, busy := p.inFlight[path]; busy {
			prev := p.dirty[path]
			p.setDirtyLocked(path, queuedJob{
				op:     op,
				lane:   maxLane(targetLane, prev.lane),
				scanID: max(max(scanID, prev.scanID), cur.scanID),
			})
			return true
		}
		if existing, ok := p.queued[path]; ok {
			existing.scanID = max(existing.scanID, scanID)
			p.setQueuedLocked(path, existing)
			return true
		}
	}

	p.setQueuedLocked(path, queuedJob{
		op:     op,
		lane:   targetLane,
		scanID: scanID,
	})
	p.pending++
	if targetLane == laneLive {
		p.liveQueue = append(p.liveQueue, path)
	} else {
		p.backfillQ = append(p.backfillQ, path)
	}
	p.workCond.Signal()
	return true
}

func maxLane(a, b lane) lane {
	if a == laneLive || b == laneLive {
		return laneLive
	}
	return laneBackfill
}

// WaitIdle blocks until all queued, in-flight, and retry-pending jobs finish and commit to SQLite.
func (p *Pipeline) WaitIdle(ctx context.Context) error {
	p.mu.Lock()
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.idleCond.Broadcast()
		p.mu.Unlock()
	})
	for p.pending > 0 && !p.closed && ctx.Err() == nil && (p.runCtx == nil || p.runCtx.Err() == nil) {
		p.idleCond.Wait()
	}
	closed := p.closed
	p.mu.Unlock()
	stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if closed {
		return ErrPipelineClosed
	}
	return p.Flush(ctx)
}

// waitScanIdle waits until every job carrying scanID (including promoted live, dirty, or retry jobs)
// has finished and flushed to SQLite. Returns ErrPipelineClosed if Close() interrupted the scan.
func (p *Pipeline) waitScanIdle(ctx context.Context, scanID int64) error {
	p.mu.Lock()
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.idleCond.Broadcast()
		p.mu.Unlock()
	})
	for p.scanPending[scanID] > 0 && !p.closed && ctx.Err() == nil && (p.runCtx == nil || p.runCtx.Err() == nil) {
		p.idleCond.Wait()
	}
	closed := p.closed
	p.mu.Unlock()
	stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if closed {
		return ErrPipelineClosed
	}
	return p.Flush(ctx)
}

// Flush forces the single SQLite writer to commit all currently buffered writes.
func (p *Pipeline) Flush(ctx context.Context) error {
	p.mu.Lock()
	started := p.started
	closed := p.closed
	p.mu.Unlock()
	if !started {
		return nil
	}
	if closed {
		return ErrPipelineClosed
	}
	return p.flushRaw(ctx)
}

func (p *Pipeline) flushRaw(ctx context.Context) error {
	ch := make(chan error, 1)

	p.writeMu.RLock()
	if p.writeClosed {
		p.writeMu.RUnlock()
		return ErrPipelineClosed
	}
	select {
	case p.writeCh <- writeCommand{flushDone: ch}:
	case <-ctx.Done():
		p.writeMu.RUnlock()
		return ctx.Err()
	}
	p.writeMu.RUnlock()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IndexWorkspace walks the workspace root, stamps all existing files with a new scan generation,
// flushes SQLite, prunes only stale rows outside errored subtrees, and updates last_scan_at.
// If the workspace root is missing/unreadable, the pipeline closes mid-scan, or any scan write/file
// error occurred, pruning is skipped so healthy rows are never wiped.
func (p *Pipeline) IndexWorkspace(ctx context.Context) (StatsSnapshot, error) {
	root := p.indexer.Workspace.RootPath

	// Guard against missing, unmounted, or unreadable workspace roots before touching the catalog.
	rootInfo, err := os.Stat(root)
	if err != nil {
		return p.indexer.Stats.Snapshot(), fmt.Errorf("workspace root unavailable %q: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return p.indexer.Stats.Snapshot(), fmt.Errorf("workspace root %q is not a directory", root)
	}
	rootDir, err := os.Open(root)
	if err != nil {
		return p.indexer.Stats.Snapshot(), fmt.Errorf("open workspace root %q: %w", root, err)
	}
	_ = rootDir.Close()

	p.Start(ctx)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.indexer.Stats.Snapshot(), ErrPipelineClosed
	}
	p.mu.Unlock()

	scanID, err := p.indexer.Store.BeginWorkspaceScan(ctx, p.indexer.Workspace.ID)
	if err != nil {
		return p.indexer.Stats.Snapshot(), err
	}
	p.indexer.SetActiveScanID(scanID)

	var erroredPrefixes []string
	var scanDropped bool
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			if path == root {
				return fmt.Errorf("walk workspace root %q: %w", root, walkErr)
			}
			if rel, rerr := p.indexer.RelPath(path); rerr == nil && rel != "" && rel != "." {
				erroredPrefixes = append(erroredPrefixes, rel)
			}
			p.reportError(walkErr)
			if d == nil || d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && fsutil.IsIgnoredDir(d.Name()) {
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
		if !p.enqueueBackfillWithScan(ctx, path, scanID) {
			scanDropped = true
			return ErrPipelineClosed
		}
		return nil
	})
	if walkErr != nil {
		p.consumeScanError(scanID)
		return p.indexer.Stats.Snapshot(), walkErr
	}
	if scanDropped {
		p.consumeScanError(scanID)
		return p.indexer.Stats.Snapshot(), ErrPipelineClosed
	}

	if err := p.waitScanIdle(ctx, scanID); err != nil {
		p.consumeScanError(scanID)
		return p.indexer.Stats.Snapshot(), err
	}

	// If any individual file or writer batch/touch failed during this scan, skip pruning so
	// untouched healthy rows are never deleted due to transient errors.
	if p.consumeScanError(scanID) {
		return p.indexer.Stats.Snapshot(), nil
	}

	pruned, err := p.indexer.Store.PruneStaleScanFiles(ctx, p.indexer.Workspace.ID, scanID, erroredPrefixes)
	if err != nil {
		return p.indexer.Stats.Snapshot(), err
	}
	if pruned > 0 {
		p.indexer.Stats.Deleted.Add(int64(pruned))
	}

	if err := p.indexer.Store.TouchWorkspaceScan(ctx, p.indexer.Workspace.ID); err != nil {
		return p.indexer.Stats.Snapshot(), err
	}
	return p.indexer.Stats.Snapshot(), nil
}

// hasPathConflictLocked reports whether starting (path, op) could race out-of-order with
// an active in-flight prefix remove or child upsert.
func (p *Pipeline) hasPathConflictLocked(path string, op jobOp) bool {
	sep := string(filepath.Separator)
	for otherPath, other := range p.inFlight {
		if otherPath == path {
			return true
		}
		if op == opRemove && strings.HasPrefix(otherPath, path+sep) {
			// Wait for in-flight child job before deleting parent prefix.
			return true
		}
		if op == opUpsert && other.op == opRemove && strings.HasPrefix(path, otherPath+sep) {
			// Wait for in-flight parent prefix delete before upserting child.
			return true
		}
	}
	return false
}

// hasEarlierRemoveInLiveQueueLocked prevents a later child upsert in liveQueue from jumping
// ahead of an earlier queued parent prefix remove.
func (p *Pipeline) hasEarlierRemoveInLiveQueueLocked(idx int, path string) bool {
	sep := string(filepath.Separator)
	for j := 0; j < idx; j++ {
		earlier := p.liveQueue[j]
		if q, ok := p.queued[earlier]; ok && q.lane == laneLive && q.op == opRemove {
			if earlier == path || strings.HasPrefix(path, earlier+sep) {
				return true
			}
		}
	}
	return false
}

func (p *Pipeline) popNextJob(ctx context.Context) (string, queuedJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		if p.closed || ctx.Err() != nil {
			return "", queuedJob{}, false
		}

		// Scan liveQueue first, skipping only stale entries or entries serialized behind a conflicting prefix op.
		for i := 0; i < len(p.liveQueue); {
			path := p.liveQueue[i]
			q, ok := p.queued[path]
			if !ok || q.lane != laneLive {
				p.liveQueue = append(p.liveQueue[:i], p.liveQueue[i+1:]...)
				continue
			}
			if p.hasPathConflictLocked(path, q.op) || (q.op == opUpsert && p.hasEarlierRemoveInLiveQueueLocked(i, path)) {
				i++
				continue
			}
			p.liveQueue = append(p.liveQueue[:i], p.liveQueue[i+1:]...)
			p.delQueuedLocked(path)
			p.setInFlightLocked(path, q)
			return path, q, true
		}

		// Then scan backfillQ.
		for i := 0; i < len(p.backfillQ); {
			path := p.backfillQ[i]
			q, ok := p.queued[path]
			if !ok || q.lane != laneBackfill {
				p.backfillQ = append(p.backfillQ[:i], p.backfillQ[i+1:]...)
				p.notFullCond.Signal()
				continue
			}
			if p.hasPathConflictLocked(path, q.op) || p.hasEarlierRemoveInLiveQueueLocked(len(p.liveQueue), path) {
				i++
				continue
			}
			p.backfillQ = append(p.backfillQ[:i], p.backfillQ[i+1:]...)
			p.notFullCond.Signal()
			p.delQueuedLocked(path)
			p.setInFlightLocked(path, q)
			return path, q, true
		}

		p.workCond.Wait()
	}
}

func (p *Pipeline) runWorker(ctx context.Context) {
	for {
		path, q, ok := p.popNextJob(ctx)
		if !ok {
			return
		}
		p.processJob(ctx, path, q)
	}
}

func (p *Pipeline) processJob(ctx context.Context, path string, q queuedJob) {
	retryNeeded := false

	switch q.op {
	case opRemove:
		if rel, err := p.indexer.RelPath(path); err == nil {
			p.sendWrite(ctx, writeCommand{deleteRel: rel})
		}
	case opUpsert:
		out, err := p.indexer.PrepareUpsert(ctx, path, q.scanID)
		if errors.Is(err, ErrFileChanged) && q.attempt < p.cfg.MaxRetries {
			retryNeeded = true
		} else if err != nil {
			if !isContextErr(err) {
				p.indexer.Stats.Errors.Add(1)
				p.markScanError(q.scanID)
				p.reportError(err)
			}
		} else if out.DeleteRel != "" {
			p.sendWrite(ctx, writeCommand{deleteRel: out.DeleteRel})
		} else if out.TouchScanRel != "" && out.TouchScanID > 0 {
			p.sendWrite(ctx, writeCommand{touchScanRel: out.TouchScanRel, touchScanID: out.TouchScanID})
		} else if out.Upsert != nil {
			p.sendWrite(ctx, writeCommand{upsert: out.Upsert})
		}
	}

	p.mu.Lock()
	p.delInFlightLocked(path)
	dirtyJob, isDirty := p.delDirtyLocked(path)

	// Wake any sibling/parent/child jobs that were waiting on this path in inFlight.
	p.workCond.Broadcast()

	// If the path was modified again while in-flight, push directly onto liveQueue (never blocks).
	if isDirty && !p.closed && ctx.Err() == nil {
		dirtyJob.lane = laneLive
		dirtyJob.scanID = max(dirtyJob.scanID, q.scanID)
		p.setQueuedLocked(path, dirtyJob)
		p.liveQueue = append(p.liveQueue, path)
		p.workCond.Broadcast()
		p.mu.Unlock()
		return
	}

	// If file changed mid-read, schedule a non-blocking retry timer instead of sleeping or blocking on a channel.
	if retryNeeded && !p.closed && ctx.Err() == nil {
		nextAttempt := q.attempt + 1
		backoff := time.Duration(nextAttempt) * 150 * time.Millisecond
		var timer *time.Timer
		timer = time.AfterFunc(backoff, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			cur, ok := p.retryTimers[path]
			if !ok || cur.timer != timer {
				return
			}
			p.delRetryLocked(path)
			if p.closed || ctx.Err() != nil {
				p.pending--
				if p.pending == 0 {
					p.idleCond.Broadcast()
				}
				return
			}
			if _, busy := p.inFlight[path]; busy {
				// Preserve any existing dirty op (e.g. opRemove) while carrying scanID.
				if prev, hasDirty := p.dirty[path]; hasDirty {
					prev.scanID = max(prev.scanID, cur.scanID)
					p.setDirtyLocked(path, prev)
				} else {
					p.setDirtyLocked(path, queuedJob{op: q.op, lane: laneLive, attempt: nextAttempt, scanID: cur.scanID})
				}
				p.pending--
				if p.pending == 0 {
					p.idleCond.Broadcast()
				}
				return
			}
			if existing, ok := p.queued[path]; ok {
				existing.attempt = nextAttempt
				existing.scanID = max(existing.scanID, cur.scanID)
				p.setQueuedLocked(path, existing)
				p.pending--
				if p.pending == 0 {
					p.idleCond.Broadcast()
				}
				return
			}
			p.setQueuedLocked(path, queuedJob{op: q.op, lane: laneLive, attempt: nextAttempt, scanID: cur.scanID})
			p.liveQueue = append(p.liveQueue, path)
			p.workCond.Broadcast()
		})
		p.setRetryLocked(path, retryEntry{timer: timer, scanID: q.scanID})
		p.mu.Unlock()
		return
	}

	p.pending--
	if p.pending == 0 {
		p.idleCond.Broadcast()
	}
	p.mu.Unlock()
}

func (p *Pipeline) sendWrite(ctx context.Context, cmd writeCommand) {
	if ctx.Err() != nil {
		return
	}
	p.writeMu.RLock()
	defer p.writeMu.RUnlock()
	if p.writeClosed {
		return
	}
	select {
	case <-ctx.Done():
	case p.writeCh <- cmd:
	}
}

func (p *Pipeline) runWriter() {
	batch := make([]store.UpsertItem, 0, p.cfg.BatchSize)
	touches := make(map[int64][]string)
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	flushTouches := func() error {
		var firstErr error
		for scanID, rels := range touches {
			if len(rels) == 0 {
				continue
			}
			if err := p.indexer.Store.TouchFileScanBatch(context.Background(), p.indexer.Workspace.ID, scanID, rels); err != nil {
				p.indexer.Stats.Errors.Add(int64(len(rels)))
				p.markScanError(scanID)
				p.reportError(err)
				if firstErr == nil {
					firstErr = err
				}
			}
			delete(touches, scanID)
		}
		return firstErr
	}

	flushUpserts := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := p.indexer.Store.UpsertBatch(context.Background(), batch)
		if err == nil {
			p.indexer.Stats.Indexed.Add(int64(len(batch)))
			batch = batch[:0]
			return nil
		}

		// Batch failed (e.g. FK race on a single item): fall back to per-item commits so
		// one bad row never silently drops up to 49 healthy files.
		if len(batch) == 1 {
			p.indexer.Stats.Errors.Add(1)
			p.markScanError(batch[0].File.LastSeenScanID)
			p.reportError(err)
			batch = batch[:0]
			return err
		}

		var firstErr error
		for i := range batch {
			if itemErr := p.indexer.Store.UpsertBatch(context.Background(), batch[i:i+1]); itemErr != nil {
				p.indexer.Stats.Errors.Add(1)
				p.markScanError(batch[i].File.LastSeenScanID)
				p.reportError(fmt.Errorf("upsert %s: %w", batch[i].File.RelPath, itemErr))
				if firstErr == nil {
					firstErr = itemErr
				}
			} else {
				p.indexer.Stats.Indexed.Add(1)
			}
		}
		batch = batch[:0]
		return firstErr
	}

	flushAll := func() error {
		err1 := flushUpserts()
		err2 := flushTouches()
		return errors.Join(err1, err2)
	}

	for {
		select {
		case cmd, ok := <-p.writeCh:
			if !ok {
				_ = flushAll()
				return
			}
			if cmd.upsert != nil {
				batch = append(batch, *cmd.upsert)
				if len(batch) >= p.cfg.BatchSize {
					_ = flushUpserts()
				}
			}
			if cmd.touchScanRel != "" && cmd.touchScanID > 0 {
				touches[cmd.touchScanID] = append(touches[cmd.touchScanID], cmd.touchScanRel)
				if len(touches[cmd.touchScanID]) >= p.cfg.BatchSize {
					_ = flushTouches()
				}
			}
			if cmd.deleteRel != "" {
				// Flush pending upserts first to preserve upsert -> delete order.
				_ = flushAll()
				n, err := p.indexer.Store.DeletePath(context.Background(), p.indexer.Workspace.ID, cmd.deleteRel)
				if err != nil {
					p.indexer.Stats.Errors.Add(1)
					p.reportError(err)
				} else if n > 0 {
					p.indexer.Stats.Deleted.Add(int64(n))
				}
			}
			if cmd.flushDone != nil {
				cmd.flushDone <- flushAll()
			}
		case <-ticker.C:
			_ = flushAll()
		}
	}
}

func (p *Pipeline) reportError(err error) {
	if p.cfg.OnError != nil {
		p.cfg.OnError(err)
		return
	}
	if p.cfg.Logger != nil {
		p.cfg.Logger.Printf("[worker] error: %v", err)
	}
}
