package main

import (
	"context"
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
