package main

import (
	"sync/atomic"
	"time"
)

const (
	spawnCircuitFailureThreshold = 8
	spawnCircuitOpenDuration     = 30 * time.Second
)

func (p *WorkerPool) noteSpawnSuccess() {
	atomic.StoreInt32(&p.consecutiveSpawnFailures, 0)
}

func (p *WorkerPool) noteSpawnFailure() {
	n := atomic.AddInt32(&p.consecutiveSpawnFailures, 1)
	if int(n) >= spawnCircuitFailureThreshold {
		p.spawnCircuitMu.Lock()
		p.spawnCircuitOpenUntil = time.Now().Add(spawnCircuitOpenDuration)
		p.spawnCircuitMu.Unlock()
		if p.logger != nil {
			p.logger.Warn(
				"Spawn circuit open for %s after %d consecutive failures; pausing new spawns",
				spawnCircuitOpenDuration,
				n,
			)
		}
	}
}

func (p *WorkerPool) spawnCircuitOpen() bool {
	p.spawnCircuitMu.Lock()
	openUntil := p.spawnCircuitOpenUntil
	p.spawnCircuitMu.Unlock()
	if openUntil.IsZero() {
		return false
	}
	if time.Now().Before(openUntil) {
		return true
	}
	p.spawnCircuitMu.Lock()
	if time.Now().After(p.spawnCircuitOpenUntil) {
		p.spawnCircuitOpenUntil = time.Time{}
		atomic.StoreInt32(&p.consecutiveSpawnFailures, 0)
	}
	p.spawnCircuitMu.Unlock()
	return false
}

func (p *WorkerPool) countDeadWorkers() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, w := range p.workers {
		w.mu.Lock()
		st := w.state
		w.mu.Unlock()
		if st == WorkerStateDead {
			n++
		}
	}
	return n
}

func (p *WorkerPool) evictDeadWorker(w *Worker, reason string) {
	p.forceEvictWorker(w, reason)
}
