package main

import (
	"sync/atomic"
	"syscall"
	"time"
)

const defaultRespawnTimeout = 120 * time.Second

func (p *WorkerPool) isProcessDead(w *Worker) bool {
	w.mu.Lock()
	cmd := w.cmd
	w.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return true
	}
	return cmd.Process.Signal(syscall.Signal(0)) != nil
}

func (p *WorkerPool) countWorkersInState(states ...WorkerState) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	want := make(map[WorkerState]struct{}, len(states))
	for _, s := range states {
		want[s] = struct{}{}
	}

	n := 0
	for _, w := range p.workers {
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if _, ok := want[st]; ok {
			n++
		}
	}
	return n
}

func (p *WorkerPool) countOccupyingSlots() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	n := 0
	for _, w := range p.workers {
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if st != WorkerStateDead {
			n++
		}
	}
	return n + int(atomic.LoadInt32(&p.spawnInFlight))
}

func (p *WorkerPool) syncActiveWorkersCount() {
	n := p.countReadyWorkers() + p.countWorkersInState(WorkerStateStarting)
	atomic.StoreInt32(&p.activeWorkers, int32(n))
}

func (p *WorkerPool) livingWorkerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.countLivingWorkersLocked()
}

func (p *WorkerPool) countLivingWorkersLocked() int {
	n := 0
	for _, w := range p.workers {
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if st != WorkerStateDead {
			n++
		}
	}
	return n
}

func (p *WorkerPool) pruneDeadWorkersLocked() int {
	kept := make([]*Worker, 0, len(p.workers))
	pruned := 0
	for _, w := range p.workers {
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if st == WorkerStateDead {
			pruned++
			continue
		}
		kept = append(kept, w)
	}
	p.workers = kept
	return pruned
}

func (p *WorkerPool) pruneDeadWorkers() int {
	p.mu.Lock()
	pruned := p.pruneDeadWorkersLocked()
	p.mu.Unlock()
	if pruned > 0 {
		p.syncActiveWorkersCount()
		if p.logger != nil {
			p.logger.Debug("Pruned %d dead worker slots from pool", pruned)
		}
	}
	return pruned
}

func (p *WorkerPool) removeWorkerFromSliceIfPresent(id int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, w := range p.workers {
		if w.ID == id {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			return true
		}
	}
	return false
}

func (p *WorkerPool) forceEvictWorker(w *Worker, reason string) {
	if !p.removeWorkerFromSliceIfPresent(w.ID) {
		return
	}

	p.terminateWorker(w, reason)

	if cur := atomic.LoadInt32(&p.activeWorkers); cur > 0 {
		atomic.AddInt32(&p.activeWorkers, -1)
	}

	if p.logger != nil {
		p.logger.Warn("Evicted worker %d; reason=%s", w.ID, reason)
	}
}

func (p *WorkerPool) reconcileStuckWorkers() {
	p.mu.RLock()
	workers := make([]*Worker, len(p.workers))
	copy(workers, p.workers)
	p.mu.RUnlock()

	for _, w := range workers {
		w.mu.Lock()
		state := w.state
		started := w.respawnStartedAt
		w.mu.Unlock()

		switch state {
		case WorkerStateRespawning:
			stuck := false
			reason := ""
			if p.isProcessDead(w) {
				stuck = true
				reason = "process_dead_during_respawn"
			} else if !started.IsZero() && time.Since(started) > p.respawnTimeout {
				stuck = true
				reason = "respawn_timeout"
			}
			if stuck {
				atomic.AddInt64(&p.stuckRespawningTotal, 1)
				p.forceEvictWorker(w, reason)
			}
		case WorkerStateDead:
			p.forceEvictWorker(w, "dead_slot")
		}
	}

	p.syncActiveWorkersCount()
}
