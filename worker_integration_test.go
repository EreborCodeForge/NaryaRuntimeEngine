//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func phpAvailable() bool {
	if runtime.GOOS == "windows" {
		return false
	}
	out, err := exec.Command("php", "-m").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "msgpack")
}

func fixturePath(name string) string {
	return filepath.Join("tests", "fixtures", name)
}

func startTestPool(t *testing.T, script string, maxRequests int, workerTimeout time.Duration) (*WorkerPool, string, func()) {
	t.Helper()
	if !phpAvailable() {
		t.Skip("php with msgpack not available")
	}

	sockDir, err := os.MkdirTemp("", "narya-int-*")
	if err != nil {
		t.Fatal(err)
	}

	logger := NewLogger("error")
	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:    1,
		MinWorkers:    1,
		MaxWorkers:    1,
		MaxRequests:   maxRequests,
		WorkerTimeout: workerTimeout,
		PHPBinary:     "php",
		WorkerScript:  fixturePath(script),
		SocketDir:     sockDir,
		Logger:        logger,
	})

	if err := pool.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPoolReady(t, pool)

	return pool, sockDir, func() {
		pool.Stop()
		os.RemoveAll(sockDir)
	}
}

func waitForPoolReady(t *testing.T, pool *WorkerPool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case w := <-pool.available:
			if w != nil {
				pool.available <- w
				return
			}
		default:
		}
		pool.mu.RLock()
		n := len(pool.workers)
		pool.mu.RUnlock()
		if n >= 1 {
			time.Sleep(300 * time.Millisecond)
			select {
			case w := <-pool.available:
				if w != nil {
					pool.available <- w
					return
				}
			default:
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout waiting for worker")
}

func poolExecuteHTTP(t *testing.T, pool *WorkerPool, path string) int {
	t.Helper()
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Method = "GET"
	req.Path = path
	req.URI = path
	req.Headers = map[string][]string{}
	req.TimeoutMs = int(pool.workerTimeout.Milliseconds())

	ctx, cancel := context.WithTimeout(context.Background(), pool.workerTimeout+2*time.Second)
	defer cancel()

	resp, err := pool.Execute(ctx, req, pool.workerTimeout)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return resp.Status
}

func TestWorkerRespawnAfterExit(t *testing.T) {
	pool, sockDir, cleanup := startTestPool(t, "worker_exit_after_one.php", 1000, 5*time.Second)
	defer cleanup()
	_ = sockDir

	if status := poolExecuteHTTP(t, pool, "/"); status != 200 {
		t.Fatalf("first status = %d", status)
	}

	var firstPID int
	pool.mu.RLock()
	if len(pool.workers) > 0 {
		firstPID = pool.workers[0].Pid
	}
	pool.mu.RUnlock()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		pool.mu.RLock()
		ready := len(pool.workers) > 0 && pool.workers[0].Pid != 0 && pool.workers[0].Pid != firstPID
		pool.mu.RUnlock()
		if ready {
			break
		}
	}

	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if len(pool.workers) == 0 || pool.workers[0].Pid == firstPID {
		t.Fatalf("expected respawn with new PID, first=%d current=%v", firstPID, pool.workers)
	}

	if status := poolExecuteHTTP(t, pool, "/"); status != 200 {
		t.Fatalf("second status = %d", status)
	}
}

func TestWorkerTimeoutOnSleep(t *testing.T) {
	pool, _, cleanup := startTestPool(t, "worker_sleep.php", 1000, 500*time.Millisecond)
	defer cleanup()

	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Method = "GET"
	req.Path = "/slow"
	req.URI = "/slow"
	req.Headers = map[string][]string{}
	req.TimeoutMs = 500

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := pool.Execute(ctx, req, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	deadline := time.Now().Add(10 * time.Second)
	var deadPID int
	pool.mu.RLock()
	if len(pool.workers) > 0 {
		deadPID = pool.workers[0].Pid
	}
	pool.mu.RUnlock()

	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		pool.mu.RLock()
		if len(pool.workers) > 0 && pool.workers[0].Pid != deadPID && pool.workers[0].Pid != 0 {
			pool.mu.RUnlock()
			return
		}
		pool.mu.RUnlock()
	}
	t.Fatal("expected worker respawn after timeout")
}

func TestMaxRequestsRecycle(t *testing.T) {
	pool, _, cleanup := startTestPool(t, "worker_echo.php", 3, 5*time.Second)
	defer cleanup()

	var pids []int
	for i := 0; i < 5; i++ {
		if status := poolExecuteHTTP(t, pool, "/"); status != 200 {
			t.Fatalf("request %d status = %d", i+1, status)
		}
		pool.mu.RLock()
		if len(pool.workers) > 0 {
			pids = append(pids, pool.workers[0].Pid)
		}
		pool.mu.RUnlock()
		time.Sleep(200 * time.Millisecond)
	}

	changed := false
	for i := 1; i < len(pids); i++ {
		if pids[i] != pids[0] && pids[i] != 0 {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatalf("expected PID change after max_requests=3, pids=%v", pids)
	}
}

func TestIntegrationDebugWorkersEndpoint(t *testing.T) {
	if !phpAvailable() {
		t.Skip("php with msgpack not available")
	}

	sockDir, err := os.MkdirTemp("", "narya-http-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)

	cfg := DefaultConfig()
	cfg.Workers.MinWorkers = 1
	cfg.Workers.MaxWorkers = 1
	cfg.Workers.Count = 1
	cfg.Workers.SocketDir = sockDir
	cfg.PHP.WorkerScript = fixturePath("worker_echo.php")

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.pool.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.pool.Stop()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv.httpServer = &http.Server{Handler: http.NewServeMux()}
	mux := http.NewServeMux()
	mux.HandleFunc("/narya/debug/workers", srv.handleDebugWorkers)
	srv.httpServer.Handler = mux

	go srv.httpServer.Serve(ln)
	defer srv.httpServer.Close()

	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get("http://" + ln.Addr().String() + "/narya/debug/workers")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var data map[string]any
			if json.Unmarshal(body, &data) == nil {
				if aw, ok := data["active_workers"].(float64); ok && aw >= 1 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("workers not ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
