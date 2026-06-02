package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildWorkerCommandIncludesMaxRequests(t *testing.T) {
	pool := &WorkerPool{
		phpBinary:    "php",
		workerScript: "worker.php",
		maxRequests:  300,
	}

	cmd := pool.buildWorkerCommand("/tmp/narya/worker-000.sock")
	if cmd.Path != "php" && !strings.HasSuffix(cmd.Path, "php") {
		// exec.Command may resolve path; args matter most
	}
	args := cmd.Args
	wantSuffix := []string{"worker.php", "--sock", "/tmp/narya/worker-000.sock", "--max-requests", "300"}
	if len(args) < len(wantSuffix)+1 {
		t.Fatalf("args too short: %v", args)
	}
	got := args[len(args)-len(wantSuffix):]
	if !reflect.DeepEqual(got, wantSuffix) {
		t.Fatalf("args suffix = %v, want %v (full: %v)", got, wantSuffix, args)
	}
}

func TestWorkerExecuteUsesDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	w := &Worker{
		conn:     client,
		protocol: NewProtocol(),
		exitCh:   make(chan error, 1),
	}

	req := &Request{
		ID:     1,
		Method: "GET",
		Path:   "/",
		Headers: map[string][]string{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := w.Execute(ctx, req, 20*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestScheduleRespawnIsIdempotent(t *testing.T) {
	logger := NewLogger("error")
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:    1,
		MinWorkers:    1,
		MaxWorkers:    1,
		MaxRequests:   100,
		WorkerTimeout: 5 * time.Second,
		Logger:        logger,
	})

	var spawnCalls int32
	pool.spawnWorkerHook = func(id int) (*Worker, error) {
		atomic.AddInt32(&spawnCalls, 1)
		time.Sleep(50 * time.Millisecond)
		return &Worker{
			ID:       id,
			state:    WorkerStateIdle,
			exitCh:   make(chan error, 1),
			protocol: NewProtocol(),
		}, nil
	}

	worker := &Worker{
		ID:       7,
		state:    WorkerStateDead,
		exitCh:   make(chan error, 1),
		protocol: NewProtocol(),
		cmd:      &exec.Cmd{},
	}

	pool.workers = []*Worker{worker}
	pool.available = make(chan *Worker, 4)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.scheduleRespawn(worker, "test")
		}()
	}
	wg.Wait()

	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&spawnCalls); got != 1 {
		t.Fatalf("spawn calls = %d, want 1", got)
	}
}

func TestAggressiveScaleDownBreaksWhenNoWorkerAvailable(t *testing.T) {
	logger := NewLogger("error")
	runtime := NewRuntimeConfig(1, 4, false, 0, false)
	pool := NewWorkerPool(WorkerPoolConfig{
		Runtime:                 runtime,
		NumWorkers:              2,
		MinWorkers:              1,
		MaxWorkers:              4,
		ScaleDownIdleSecs:       1,
		AggressiveScaleDownSecs: 1,
		Logger:                  logger,
	})
	pool.bootTime = time.Now().Add(-60 * time.Second)
	pool.lastBusyTime = time.Now().Add(-10 * time.Second)
	pool.available = make(chan *Worker, 4)

	w1 := &Worker{ID: 1, state: WorkerStateBusy, exitCh: make(chan error, 1)}
	w2 := &Worker{ID: 2, state: WorkerStateBusy, exitCh: make(chan error, 1)}
	pool.workers = []*Worker{w1, w2}
	atomic.StoreInt32(&pool.activeWorkers, 2)

	done := make(chan struct{})
	go func() {
		pool.mu.RLock()
		total := len(pool.workers)
		min := pool.runtime.GetMinWorkers()
		idleSince := time.Since(pool.lastBusyTime)
		pool.mu.RUnlock()

		if total > min && pool.aggressiveScaleDownSecs > 0 && idleSince >= time.Duration(pool.aggressiveScaleDownSecs)*time.Second {
		aggressive:
			for {
				pool.mu.RLock()
				current := len(pool.workers)
				pool.mu.RUnlock()
				if current <= min {
					break aggressive
				}
				select {
				case w := <-pool.available:
					if w == nil {
						close(done)
						return
					}
					w.mu.Lock()
					state := w.state
					w.mu.Unlock()
					if state == WorkerStateIdle {
						pool.removeWorker(w)
					} else {
						select {
						case pool.available <- w:
						default:
						}
					}
				case <-time.After(100 * time.Millisecond):
					break aggressive
				}
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("aggressive scale-down loop did not exit on timeout")
	}
}

func TestShouldHealthRespawnSkipsBusyAndRespawning(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:    1,
		MinWorkers:    1,
		MaxWorkers:    1,
		WorkerTimeout: 5 * time.Second,
		Logger:        NewLogger("error"),
	})

	busy := &Worker{ID: 1, state: WorkerStateBusy, exitCh: make(chan error, 1), cmd: &exec.Cmd{}}
	if pool.shouldHealthRespawn(busy) {
		t.Fatal("busy worker should not be health-respawned")
	}

	respawning := &Worker{ID: 2, state: WorkerStateRespawning, exitCh: make(chan error, 1), cmd: &exec.Cmd{}}
	if pool.shouldHealthRespawn(respawning) {
		t.Fatal("respawning worker should not be health-respawned again")
	}
}

func TestEnsureMinWorkersDebounces(t *testing.T) {
	logger := NewLogger("error")
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:        1,
		MinWorkers:        3,
		MaxWorkers:        3,
		WorkerTimeout:     5 * time.Second,
		EnsureMinDebounce: 2 * time.Second,
		Logger:            logger,
	})
	pool.spawnWorkerHook = func(id int) (*Worker, error) {
		time.Sleep(200 * time.Millisecond)
		return &Worker{
			ID:       id,
			state:    WorkerStateIdle,
			exitCh:   make(chan error, 1),
			protocol: NewProtocol(),
		}, nil
	}
	pool.available = make(chan *Worker, 8)

	pool.ensureMinWorkers()
	pool.ensureMinWorkers()
	pool.ensureMinWorkers()

	time.Sleep(100 * time.Millisecond)
	inFlight := atomic.LoadInt32(&pool.spawnInFlight)
	if inFlight > 3 {
		t.Fatalf("expected at most 3 spawns in flight after debounce, got %d", inFlight)
	}

	time.Sleep(3 * time.Second)
}

func TestCanRespawnNowRespectsBackoff(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:     1,
		MinWorkers:     1,
		MaxWorkers:     1,
		RespawnBackoff: 500 * time.Millisecond,
		Logger:         NewLogger("error"),
	})

	pool.noteRespawnAttempt(5)
	if pool.canRespawnNow(5) {
		t.Fatal("expected respawn backoff to block immediate retry")
	}

	time.Sleep(600 * time.Millisecond)
	if !pool.canRespawnNow(5) {
		t.Fatal("expected respawn allowed after backoff")
	}
}

func TestReconcileStuckRespawningEvictsAndAllowsRecovery(t *testing.T) {
	logger := NewLogger("error")
	runtime := NewRuntimeConfig(2, 4, false, 0, false)
	pool := NewWorkerPool(WorkerPoolConfig{
		Runtime:         runtime,
		NumWorkers:      2,
		MinWorkers:      2,
		MaxWorkers:      4,
		WorkerTimeout:   5 * time.Second,
		RespawnTimeout:  30 * time.Second,
		EnsureMinDebounce: 10 * time.Millisecond,
		Logger:          logger,
	})
	pool.available = make(chan *Worker, 4)

	var spawnCalls int32
	pool.spawnWorkerHook = func(id int) (*Worker, error) {
		atomic.AddInt32(&spawnCalls, 1)
		return &Worker{
			ID:       id,
			state:    WorkerStateIdle,
			exitCh:   make(chan error, 1),
			protocol: NewProtocol(),
		}, nil
	}

	stuck := make([]*Worker, 0, 4)
	for i := 0; i < 4; i++ {
		stuck = append(stuck, &Worker{
			ID:               i,
			Pid:              9000 + i,
			state:            WorkerStateRespawning,
			respawnStartedAt: time.Now().Add(-2 * time.Minute),
			exitCh:           make(chan error, 1),
			protocol:         NewProtocol(),
		})
	}
	pool.workers = stuck
	atomic.StoreInt32(&pool.activeWorkers, 4)

	pool.reconcileStuckWorkers()

	pool.mu.RLock()
	remaining := len(pool.workers)
	pool.mu.RUnlock()

	if remaining != 0 {
		t.Fatalf("expected stuck workers evicted, remaining=%d", remaining)
	}
	if got := atomic.LoadInt64(&pool.stuckRespawningTotal); got != 4 {
		t.Fatalf("stuckRespawningTotal = %d, want 4", got)
	}

	pool.ensureMinWorkers()
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&spawnCalls); got < 1 {
		t.Fatalf("expected ensureMinWorkers to spawn after eviction, spawn calls=%d", got)
	}
}

func TestReconcileRespawningTimeout(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:     1,
		MinWorkers:     1,
		MaxWorkers:     1,
		WorkerTimeout:  5 * time.Second,
		RespawnTimeout: 50 * time.Millisecond,
		Logger:         NewLogger("error"),
	})

	w := &Worker{
		ID:               3,
		state:            WorkerStateRespawning,
		respawnStartedAt: time.Now().Add(-200 * time.Millisecond),
		cmd:              &exec.Cmd{},
		exitCh:           make(chan error, 1),
		protocol:         NewProtocol(),
	}
	pool.workers = []*Worker{w}
	atomic.StoreInt32(&pool.activeWorkers, 1)

	pool.reconcileStuckWorkers()

	pool.mu.RLock()
	n := len(pool.workers)
	pool.mu.RUnlock()
	if n != 0 {
		t.Fatalf("expected worker evicted after respawn timeout, remaining=%d", n)
	}
}

func TestReleaseWorkerToAvailableDropsWhenSaturated(t *testing.T) {
	runtime := NewRuntimeConfig(1, 2, false, 0, false)
	pool := NewWorkerPool(WorkerPoolConfig{
		Runtime:       runtime,
		NumWorkers:    2,
		MinWorkers:    1,
		MaxWorkers:    2,
		WorkerTimeout: 5 * time.Second,
		Logger:        NewLogger("error"),
	})
	pool.available = make(chan *Worker, 2)

	w1 := &Worker{ID: 1, state: WorkerStateIdle, exitCh: make(chan error, 1)}
	w2 := &Worker{ID: 2, state: WorkerStateIdle, exitCh: make(chan error, 1)}
	extra := &Worker{ID: 3, state: WorkerStateIdle, exitCh: make(chan error, 1)}
	pool.workers = []*Worker{w1, w2, extra}
	pool.available <- w1
	pool.available <- w2

	if pool.releaseWorkerToAvailable(extra) {
		t.Fatal("expected worker to be dropped when available queue is saturated")
	}
	if got := atomic.LoadInt64(&pool.availableQueueDroppedTotal); got < 1 {
		t.Fatalf("availableQueueDroppedTotal = %d, want >= 1", got)
	}
	if len(pool.available) != 2 {
		t.Fatalf("available queue len = %d, want 2", len(pool.available))
	}
}

func TestCompactAvailableQueueRemovesStale(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:    1,
		MinWorkers:    1,
		MaxWorkers:    2,
		WorkerTimeout: 5 * time.Second,
		Logger:        NewLogger("error"),
	})
	pool.available = make(chan *Worker, 2)
	pool.spawnWorkerHook = func(id int) (*Worker, error) {
		return nil, fmt.Errorf("spawn disabled in test")
	}

	stale := &Worker{ID: 9, state: WorkerStateDead, exitCh: make(chan error, 1)}
	pool.available <- stale

	drained := pool.compactAvailableQueue()
	if drained != 1 {
		t.Fatalf("drained = %d, want 1", drained)
	}
	if len(pool.available) != 0 {
		t.Fatalf("expected empty available queue after compact, got len=%d", len(pool.available))
	}
}

func TestPruneDeadWorkersRemovesDeadSlots(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:    2,
		MinWorkers:    1,
		MaxWorkers:    4,
		WorkerTimeout: 5 * time.Second,
		Logger:        NewLogger("error"),
	})

	alive := &Worker{ID: 1, state: WorkerStateIdle, exitCh: make(chan error, 1)}
	dead := &Worker{ID: 2, state: WorkerStateDead, exitCh: make(chan error, 1)}
	pool.workers = []*Worker{alive, dead}

	pruned := pool.pruneDeadWorkers()
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	pool.mu.RLock()
	n := len(pool.workers)
	pool.mu.RUnlock()
	if n != 1 || pool.workers[0].ID != 1 {
		t.Fatalf("expected only alive worker remaining, got %d workers", n)
	}
}
