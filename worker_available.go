package main

import (
	"sync/atomic"
)

func (p *WorkerPool) countAvailableQueued() int {
	return len(p.available)
}

// compactAvailableQueue drains the available channel, drops stale/dead entries, and
// re-enqueues at most max_workers idle workers.
func (p *WorkerPool) compactAvailableQueue() int {
	max := p.runtime.GetMaxWorkers()
	var valid []*Worker
	drained := 0

drain:
	for {
		select {
		case w := <-p.available:
			drained++
			if w == nil {
				continue
			}
			w.mu.Lock()
			st := w.state
			w.mu.Unlock()
			if st == WorkerStateIdle && (w.cmd == nil || !p.isProcessDead(w)) {
				valid = append(valid, w)
				continue
			}
			if st == WorkerStateDead || (w.cmd != nil && p.isProcessDead(w)) {
				p.forceEvictWorker(w, "purge_dead_available")
				continue
			}
			if st != WorkerStateRespawning {
				w.mu.Lock()
				w.state = WorkerStateDead
				w.mu.Unlock()
			}
			p.scheduleRespawn(w, "purge_stale_available")
		default:
			break drain
		}
	}

	if drained == 0 {
		return 0
	}

	if len(valid) > max {
		for _, w := range valid[max:] {
			atomic.AddInt64(&p.availableQueueDroppedTotal, 1)
			p.destroyWorker(w)
			p.removeWorkerFromSliceIfPresent(w.ID)
		}
		valid = valid[:max]
	}

	kept := 0
	for _, w := range valid {
		select {
		case p.available <- w:
			kept++
		default:
			atomic.AddInt64(&p.availableQueueDroppedTotal, 1)
			p.destroyWorker(w)
			p.removeWorkerFromSliceIfPresent(w.ID)
		}
	}

	p.syncActiveWorkersCount()
	return drained
}

// releaseWorkerToAvailable returns the worker to the pool or destroys it if the pool is at capacity.
func (p *WorkerPool) releaseWorkerToAvailable(w *Worker) bool {
	if w == nil {
		return false
	}

	w.mu.Lock()
	if w.state == WorkerStateDead || w.state == WorkerStateRespawning {
		w.mu.Unlock()
		return false
	}
	w.state = WorkerStateIdle
	w.mu.Unlock()

	if w.cmd != nil && p.isProcessDead(w) {
		p.scheduleRespawn(w, "dead_on_release_to_available")
		return false
	}

	if p.tryPushAvailable(w) {
		return true
	}

	drained := p.compactAvailableQueue()
	if drained > 0 && p.tryPushAvailable(w) {
		return true
	}

	max := p.runtime.GetMaxWorkers()
	if p.countReadyWorkers() >= max && p.countAvailableQueued() >= max {
		atomic.AddInt64(&p.availableQueueDroppedTotal, 1)
		if p.logger != nil {
			p.logger.Debug(
				"Worker %d destroyed: available queue saturated (ready=%d queued=%d max=%d)",
				w.ID, p.countReadyWorkers(), p.countAvailableQueued(), max,
			)
		}
		p.destroyWorker(w)
		p.removeWorkerFromSliceIfPresent(w.ID)
		p.syncActiveWorkersCount()
		return false
	}

	select {
	case p.available <- w:
		return true
	case <-p.ctx.Done():
		p.destroyWorker(w)
		p.removeWorkerFromSliceIfPresent(w.ID)
		return false
	}
}

func (p *WorkerPool) tryPushAvailable(w *Worker) bool {
	select {
	case p.available <- w:
		return true
	default:
		return false
	}
}

func (p *WorkerPool) acquireRespawnSlot() {
	select {
	case p.respawnSemaphore <- struct{}{}:
	case <-p.ctx.Done():
	}
}

func (p *WorkerPool) releaseRespawnSlot() {
	select {
	case <-p.respawnSemaphore:
	default:
	}
}
