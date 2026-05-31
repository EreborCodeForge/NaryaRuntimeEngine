// WorkerPool: N PHP worker processes (engine -> worker 1..N).
// GetWorker/ReleaseWorker use channel + per-worker mutex; no global lock during Execute.
// Each Worker is one OS process (exec); one request at a time per worker (w.mu).
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type WorkerState int

const (
	WorkerStateIdle WorkerState = iota
	WorkerStateBusy
	WorkerStateDead
	WorkerStateStarting
	WorkerStateRespawning
)

type Worker struct {
	ID             int
	Pid            int
	cmd            *exec.Cmd
	sockPath       string
	listener       net.Listener
	conn           net.Conn
	state          WorkerState
	requestCount   int64
	startTime      time.Time
	lastActiveTime time.Time
	protocol       *Protocol
	exitCh         chan error
	waitOnce       sync.Once
	mu             sync.Mutex
}

type WorkerPool struct {
	workers                 []*Worker
	available               chan *Worker
	sockDir                 string
	phpBinary               string
	workerScript            string
	maxRequests             int
	workerTimeout           time.Duration
	initialWorkers          int
	runtime                 *RuntimeConfig
	minWorkers              int
	maxWorkers              int
	protocol                *Protocol
	mu                      sync.RWMutex
	running                 bool
	wg                      sync.WaitGroup
	ctx                     context.Context
	cancel                  context.CancelFunc
	totalRequests           int64
	activeWorkers           int32
	nextWorkerID            int32
	logger                  *Logger
	lastBusyTime            time.Time
	lastBusyTimeMu          sync.Mutex
	scaleDownIdleSecs       int
	aggressiveScaleDownSecs int
	warmupStaggerMs         int
	fastWarmup              bool
	lastStableLog           time.Time
	lastStableLogMu         sync.Mutex
	backpressureEnabled     bool
	backpressureMaxQueue    int
	queueTimeoutEnabled     bool
	queueTimeoutMs          int
	queuedRequests          int32
	bootTime                time.Time
	busyWorkers             int32
	lastScaleUp             time.Time
	lastScaleUpMu           sync.Mutex

	// spawnWorkerHook is set in tests to intercept spawn/respawn.
	spawnWorkerHook func(id int) (*Worker, error)
}

type WorkerPoolConfig struct {
	NumWorkers              int
	MinWorkers              int
	MaxWorkers              int
	MaxRequests             int
	WorkerTimeout           time.Duration
	PHPBinary               string
	WorkerScript            string
	SocketDir               string
	Logger                  *Logger
	ScaleDownIdleSecs       int
	AggressiveScaleDownSecs int
	WarmupStaggerMs         int
	FastWarmup              bool
	BackpressureEnabled     bool
	BackpressureMaxQueue    int
	QueueTimeoutEnabled     bool
	QueueTimeoutMs          int
	KeepWarm                bool
	Runtime                 *RuntimeConfig
}

func NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())

	sockDir := cfg.SocketDir
	if sockDir == "" {
		sockDir = filepath.Join(os.TempDir(), "narya")
	}

	maxWorkers := cfg.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = cfg.NumWorkers
	}
	minWorkers := cfg.MinWorkers
	if minWorkers <= 0 {
		minWorkers = cfg.NumWorkers
	}

	runtime := cfg.Runtime
	if runtime == nil {
		runtime = NewRuntimeConfig(minWorkers, maxWorkers, cfg.BackpressureEnabled, cfg.BackpressureMaxQueue, cfg.KeepWarm)
	}
	return &WorkerPool{
		workers:                 make([]*Worker, 0, maxWorkers),
		available:               make(chan *Worker, maxWorkers),
		sockDir:                 sockDir,
		phpBinary:               cfg.PHPBinary,
		workerScript:            cfg.WorkerScript,
		maxRequests:             cfg.MaxRequests,
		workerTimeout:           cfg.WorkerTimeout,
		initialWorkers:          cfg.NumWorkers,
		runtime:                 runtime,
		minWorkers:              minWorkers,
		maxWorkers:              maxWorkers,
		protocol:                NewProtocol(),
		ctx:                     ctx,
		cancel:                    cancel,
		logger:                  cfg.Logger,
		lastBusyTime:            time.Now(),
		scaleDownIdleSecs:       cfg.ScaleDownIdleSecs,
		aggressiveScaleDownSecs: cfg.AggressiveScaleDownSecs,
		warmupStaggerMs:         cfg.WarmupStaggerMs,
		fastWarmup:              cfg.FastWarmup,
		backpressureEnabled:     cfg.BackpressureEnabled,
		backpressureMaxQueue:    cfg.BackpressureMaxQueue,
		queueTimeoutEnabled:     cfg.QueueTimeoutEnabled,
		queueTimeoutMs:          cfg.QueueTimeoutMs,
	}
}

func (p *WorkerPool) buildWorkerCommand(sockPath string) *exec.Cmd {
	args := []string{
		p.workerScript,
		"--sock", sockPath,
	}
	if p.maxRequests >= 0 {
		args = append(args, "--max-requests", strconv.Itoa(p.maxRequests))
	}
	return exec.Command(p.phpBinary, args...)
}

func (w *Worker) startReaper(logger *Logger) {
	w.waitOnce.Do(func() {
		go func() {
			var err error
			if w.cmd != nil {
				err = w.cmd.Wait()
			}

			w.mu.Lock()
			if w.state != WorkerStateRespawning {
				w.state = WorkerStateDead
			}
			if w.conn != nil {
				_ = w.conn.Close()
			}
			if w.listener != nil {
				_ = w.listener.Close()
			}
			w.mu.Unlock()

			select {
			case w.exitCh <- err:
			default:
			}

			if err != nil && logger != nil {
				logger.Warn("Worker %d process exited: %v", w.ID, err)
			}
		}()
	})
}

func (p *WorkerPool) terminateWorker(w *Worker, reason string) {
	if w.conn != nil {
		_ = w.conn.Close()
	}

	if w.listener != nil {
		_ = w.listener.Close()
	}

	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Signal(syscall.SIGTERM)

		select {
		case <-w.exitCh:
		case <-time.After(3 * time.Second):
			_ = w.cmd.Process.Kill()
			select {
			case <-w.exitCh:
			case <-time.After(2 * time.Second):
				if p.logger != nil {
					p.logger.Warn("Worker %d did not exit after SIGKILL; reason=%s", w.ID, reason)
				}
			}
		}
	}

	_ = os.Remove(w.sockPath)
}

func (p *WorkerPool) markRespawning(w *Worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == WorkerStateRespawning {
		return false
	}

	w.state = WorkerStateRespawning
	return true
}

func (p *WorkerPool) markDead(w *Worker) {
	w.mu.Lock()
	w.state = WorkerStateDead
	w.mu.Unlock()
}

func (p *WorkerPool) scheduleRespawn(w *Worker, reason string) {
	if !p.markRespawning(w) {
		if p.logger != nil {
			p.logger.Debug("Worker %d already respawning; reason=%s", w.ID, reason)
		}
		return
	}

	go func() {
		newWorker, err := p.respawnWorker(w)
		if err != nil {
			if p.logger != nil {
				p.logger.Error("Failed to respawn worker %d: %v", w.ID, err)
			}
			p.markDead(w)
			p.ensureMinWorkers()
			return
		}

		select {
		case p.available <- newWorker:
			if p.logger != nil {
				p.logger.Info("Worker %d respawned successfully; reason=%s", newWorker.ID, reason)
			}
		case <-p.ctx.Done():
			p.destroyWorker(newWorker)
		}
	}()
}

func (p *WorkerPool) removeWorkerFromSlice(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, w := range p.workers {
		if w.ID == id {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			return
		}
	}
}

func (p *WorkerPool) doSpawnWorker(id int) (*Worker, error) {
	if p.spawnWorkerHook != nil {
		return p.spawnWorkerHook(id)
	}
	return p.spawnWorker(id)
}

func (p *WorkerPool) Start() error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("pool is already running")
	}
	if err := os.MkdirAll(p.sockDir, 0700); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("failed to create socket directory: %w", err)
	}
	entries, _ := os.ReadDir(p.sockDir)
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sock" {
			_ = os.Remove(filepath.Join(p.sockDir, e.Name()))
		}
	}
	p.bootTime = time.Now()
	p.nextWorkerID = 0
	p.running = true
	p.mu.Unlock()

	minW, maxW := p.runtime.GetMinWorkers(), p.runtime.GetMaxWorkers()
	p.logger.Info("Starting pool (min=%d max=%d, UDS: %s)", minW, maxW, p.sockDir)
	staggerMs := p.warmupStaggerMs
	if staggerMs > 0 {
		p.logger.Info("Spawning %d min workers with %dms stagger (faster ready under load)", minW, staggerMs)
	} else {
		p.logger.Info("Spawning %d min workers in parallel; scale-up to max on load (≈70%% utilization)", minW)
	}

	for i := 0; i < minW; i++ {
		if i > 0 && staggerMs > 0 {
			time.Sleep(time.Duration(staggerMs) * time.Millisecond)
		}
		go p.addWorker()
	}

	if p.fastWarmup && minW < maxW {
		go p.warmUpToMax()
	}

	go p.scalerLoop()
	go p.healthMonitor()

	if p.runtime.GetBackpressureEnabled() && p.queueTimeoutEnabled {
		p.logger.Info("Backpressure + Queue timeout: max_queue=%d, timeout=%dms", p.runtime.GetBackpressureMaxQueue(), p.queueTimeoutMs)
	} else if p.runtime.GetBackpressureEnabled() {
		p.logger.Info("Backpressure enabled: max_queue=%d", p.runtime.GetBackpressureMaxQueue())
	} else if p.queueTimeoutEnabled {
		p.logger.Info("Queue timeout enabled: timeout=%dms", p.queueTimeoutMs)
	} else {
		p.logger.Info("No queue limit (default mode)")
	}

	return nil
}

func (p *WorkerPool) warmUpToMax() {
	staggerMs := p.warmupStaggerMs
	if staggerMs <= 0 {
		staggerMs = 100
	}
	minW, maxW := p.runtime.GetMinWorkers(), p.runtime.GetMaxWorkers()
	for i := minW; i < maxW; i++ {
		time.Sleep(time.Duration(staggerMs) * time.Millisecond)
		go p.addWorker()
	}
}

func (p *WorkerPool) spawnWorker(id int) (*Worker, error) {
	sockPath := filepath.Join(p.sockDir, fmt.Sprintf("worker-%03d.sock", id))

	if err := os.MkdirAll(p.sockDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create socket directory: %w", err)
	}
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		p.logger.Warn("Error removing old socket %s: %v", sockPath, err)
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create UDS listener: %w", err)
	}

	if err := os.Chmod(sockPath, 0600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to set socket permissions: %w", err)
	}

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if i == 0 {
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}

	cmd := p.buildWorkerCommand(sockPath)
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		listener.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("failed to start PHP process: %w", err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	worker := &Worker{
		ID:             id,
		Pid:            pid,
		cmd:            cmd,
		sockPath:       sockPath,
		listener:       listener,
		state:          WorkerStateStarting,
		startTime:      time.Now(),
		lastActiveTime: time.Now(),
		protocol:       p.protocol,
		exitCh:         make(chan error, 1),
	}
	worker.startReaper(p.logger)

	connChan := make(chan net.Conn, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errChan <- err
			return
		}
		connChan <- conn
	}()

	select {
	case conn := <-connChan:
		worker.conn = conn
		if err := p.protocol.Handshake(conn); err != nil {
			conn.Close()
			cmd.Process.Kill()
			listener.Close()
			os.Remove(sockPath)
			return nil, fmt.Errorf("handshake failed: %w", err)
		}
		worker.state = WorkerStateIdle
		p.logger.Debug("Worker %d started (PID: %d, socket: %s)", id, cmd.Process.Pid, sockPath)
	case err := <-errChan:
		cmd.Process.Kill()
		listener.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("failed to accept connection: %w", err)
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		listener.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("timeout waiting for worker connection")
	}

	return worker, nil
}

func (p *WorkerPool) GetWorker(ctx context.Context) (*Worker, error) {
	if worker := p.tryGetWorker(); worker != nil {
		return worker, nil
	}

	backpressureEnabled := p.runtime.GetBackpressureEnabled()
	backpressureMaxQueue := p.runtime.GetBackpressureMaxQueue()

	if backpressureEnabled && backpressureMaxQueue > 0 {
		for {
			current := atomic.LoadInt32(&p.queuedRequests)
			if current >= int32(backpressureMaxQueue) {
				return nil, fmt.Errorf("service unavailable: queue full (%d/%d)", current, backpressureMaxQueue)
			}
			if atomic.CompareAndSwapInt32(&p.queuedRequests, current, current+1) {
				break
			}
		}
		defer atomic.AddInt32(&p.queuedRequests, -1)
	} else if backpressureEnabled {
		atomic.AddInt32(&p.queuedRequests, 1)
		defer atomic.AddInt32(&p.queuedRequests, -1)
	}

	p.mu.RLock()
	n := len(p.workers)
	p.mu.RUnlock()
	if n < p.runtime.GetMaxWorkers() {
		go p.addWorker()
	}

	var queueCtx context.Context
	var queueCancel context.CancelFunc
	if p.queueTimeoutEnabled {
		queueCtx, queueCancel = context.WithTimeout(ctx, time.Duration(p.queueTimeoutMs)*time.Millisecond)
		defer queueCancel()
	} else {
		queueCtx = ctx
	}

	for {
		select {
		case worker := <-p.available:
			if worker == nil {
				return nil, fmt.Errorf("pool stopped")
			}
			if w := p.validateAndPrepareWorker(worker); w != nil {
				return w, nil
			}
			continue

		case <-queueCtx.Done():
			if p.queueTimeoutEnabled && queueCtx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("service unavailable: queue timeout (%dms)", p.queueTimeoutMs)
			}
			return nil, ctx.Err()
		}
	}
}

func (p *WorkerPool) tryGetWorker() *Worker {
	select {
	case worker := <-p.available:
		if worker == nil {
			return nil
		}
		if w := p.validateAndPrepareWorker(worker); w != nil {
			return w
		}
		return nil
	default:
		return nil
	}
}

func (p *WorkerPool) isWorkerDead(w *Worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == WorkerStateDead {
		return true
	}

	select {
	case <-w.exitCh:
		w.state = WorkerStateDead
		return true
	default:
	}

	if w.cmd == nil || w.cmd.Process == nil {
		w.state = WorkerStateDead
		return true
	}

	if err := w.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		w.state = WorkerStateDead
		return true
	}

	return false
}

func (p *WorkerPool) validateAndPrepareWorker(worker *Worker) *Worker {
	if p.isWorkerDead(worker) {
		p.scheduleRespawn(worker, "dead_on_validate")
		return nil
	}

	worker.mu.Lock()
	worker.state = WorkerStateBusy
	worker.lastActiveTime = time.Now()
	worker.mu.Unlock()
	atomic.AddInt32(&p.busyWorkers, 1)
	return worker
}

func (p *WorkerPool) ReleaseWorker(worker *Worker, resp *Response) {
	if atomic.AddInt32(&p.busyWorkers, -1) < 0 {
		atomic.StoreInt32(&p.busyWorkers, 0)
	}

	worker.mu.Lock()
	worker.requestCount++
	count := worker.requestCount
	currentState := worker.state

	shouldRecycle := false

	if currentState == WorkerStateDead {
		worker.mu.Unlock()
		p.scheduleRespawn(worker, "dead_on_release")
		return
	}

	if resp != nil && resp.Meta.Recycle {
		shouldRecycle = true
		p.logger.Info("Worker %d requested cooperative recycle", worker.ID)
	}

	if p.maxRequests > 0 && count >= int64(p.maxRequests) {
		shouldRecycle = true
		p.logger.Info("Worker %d reached request limit (%d)", worker.ID, p.maxRequests)
	}

	if shouldRecycle {
		worker.state = WorkerStateRespawning
		worker.mu.Unlock()
		p.scheduleRespawn(worker, "max_requests_or_cooperative_recycle")
		return
	}

	worker.state = WorkerStateIdle
	worker.mu.Unlock()

	p.lastBusyTimeMu.Lock()
	p.lastBusyTime = time.Now()
	p.lastBusyTimeMu.Unlock()

	atomic.AddInt64(&p.totalRequests, 1)

	select {
	case p.available <- worker:
	case <-p.ctx.Done():
		p.destroyWorker(worker)
	}
}

func (p *WorkerPool) respawnWorker(old *Worker) (*Worker, error) {
	p.terminateWorker(old, "respawn")

	atomic.AddInt32(&p.activeWorkers, -1)

	newWorker, err := p.doSpawnWorker(old.ID)
	if err != nil {
		p.removeWorkerFromSlice(old.ID)
		return nil, err
	}

	p.mu.Lock()
	replaced := false
	for i, w := range p.workers {
		if w.ID == old.ID {
			p.workers[i] = newWorker
			replaced = true
			break
		}
	}
	if !replaced {
		p.workers = append(p.workers, newWorker)
	}
	p.mu.Unlock()

	atomic.AddInt32(&p.activeWorkers, 1)
	return newWorker, nil
}

func (p *WorkerPool) addWorker() {
	for {
		cur := atomic.LoadInt32(&p.activeWorkers)
		if cur >= int32(p.runtime.GetMaxWorkers()) {
			return
		}
		if atomic.CompareAndSwapInt32(&p.activeWorkers, cur, cur+1) {
			break
		}
	}

	id := int(atomic.AddInt32(&p.nextWorkerID, 1) - 1)

	go func() {
		worker, err := p.doSpawnWorker(id)
		if err != nil {
			atomic.AddInt32(&p.activeWorkers, -1)
			p.logger.Error("Scale-up: failed to create worker %d: %v", id, err)
			p.ensureMinWorkers()
			return
		}
		p.mu.Lock()
		p.workers = append(p.workers, worker)
		currentTotal := len(p.workers)
		p.mu.Unlock()

		select {
		case p.available <- worker:
			currentActive := atomic.LoadInt32(&p.activeWorkers)
			p.logger.Info("Scale-up: worker %d (PID %d) added; active=%d, total_slots=%d", worker.ID, worker.Pid, currentActive, currentTotal)
		case <-p.ctx.Done():
			p.destroyWorker(worker)
			atomic.AddInt32(&p.activeWorkers, -1)
		default:
			p.destroyWorker(worker)
			atomic.AddInt32(&p.activeWorkers, -1)
		}
	}()
}

func (p *WorkerPool) removeWorker(w *Worker) {
	if time.Since(p.bootTime) < 30*time.Second {
		select {
		case p.available <- w:
		default:
		}
		return
	}

	p.mu.Lock()
	if len(p.workers) <= p.runtime.GetMinWorkers() {
		p.mu.Unlock()
		select {
		case p.available <- w:
		default:
		}
		return
	}
	for i, worker := range p.workers {
		if worker.ID == w.ID {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			break
		}
	}
	currentTotal := len(p.workers)
	p.mu.Unlock()

	minW := p.runtime.GetMinWorkers()
	p.destroyWorker(w)
	newActive := atomic.AddInt32(&p.activeWorkers, -1)
	if newActive < int32(minW) {
		atomic.StoreInt32(&p.activeWorkers, int32(minW))
		newActive = int32(minW)
	}
	p.logger.Info("Scale-down: worker %d (PID %d) removed; active=%d, total_slots=%d", w.ID, w.Pid, newActive, currentTotal)
}

func (p *WorkerPool) destroyWorker(w *Worker) {
	p.terminateWorker(w, "destroy")
}

const scaleUpUtilizationThreshold = 0.70
const scaleUpCooldownMs = 400

func (p *WorkerPool) ensureMinWorkers() {
	min := p.runtime.GetMinWorkers()

	p.mu.RLock()
	current := len(p.workers)
	p.mu.RUnlock()

	for current < min {
		go p.addWorker()
		current++
	}
}

func (p *WorkerPool) scalerLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.mu.RLock()
			total := len(p.workers)
			p.mu.RUnlock()
			min := p.runtime.GetMinWorkers()
			max := p.runtime.GetMaxWorkers()

			busy := atomic.LoadInt32(&p.busyWorkers)
			if busy < 0 {
				busy = 0
			}

			if total >= min && total < max && total > 0 {
				utilization := float64(busy) / float64(total)
				if utilization >= scaleUpUtilizationThreshold {
					p.lastScaleUpMu.Lock()
					canScaleUp := time.Since(p.lastScaleUp) >= scaleUpCooldownMs*time.Millisecond
					if canScaleUp {
						p.lastScaleUp = time.Now()
						p.lastScaleUpMu.Unlock()
						go p.addWorker()
					} else {
						p.lastScaleUpMu.Unlock()
					}
				}
			}

			if p.runtime.GetKeepWarm() {
				p.ensureMinWorkers()
				continue
			}

			if time.Since(p.bootTime) < 30*time.Second {
				p.ensureMinWorkers()
				continue
			}

			p.lastBusyTimeMu.Lock()
			idleSince := time.Since(p.lastBusyTime)
			p.lastBusyTimeMu.Unlock()

			if p.scaleDownIdleSecs > 0 && idleSince < time.Duration(p.scaleDownIdleSecs)*time.Second {
				p.ensureMinWorkers()
				continue
			}

			if total <= min {
				thresholdSec := p.aggressiveScaleDownSecs
				if thresholdSec <= 0 {
					thresholdSec = p.scaleDownIdleSecs
				}
				if thresholdSec > 0 && idleSince >= time.Duration(thresholdSec)*time.Second {
					p.lastStableLogMu.Lock()
					if time.Since(p.lastStableLog) >= 30*time.Second {
						active := atomic.LoadInt32(&p.activeWorkers)
						if active < 0 {
							active = 0
						}
						p.logger.Info("Pool idle and stable: %d active workers (min=%d, max=%d)", active, min, max)
						p.lastStableLog = time.Now()
					}
					p.lastStableLogMu.Unlock()
				}
				p.ensureMinWorkers()
				continue
			}

			if p.aggressiveScaleDownSecs > 0 && idleSince >= time.Duration(p.aggressiveScaleDownSecs)*time.Second {
			aggressive:
				for {
					p.mu.RLock()
					current := len(p.workers)
					p.mu.RUnlock()
					if current <= min {
						break aggressive
					}

					select {
					case w := <-p.available:
						if w == nil {
							return
						}
						w.mu.Lock()
						state := w.state
						w.mu.Unlock()
						if state == WorkerStateIdle {
							p.removeWorker(w)
						} else {
							select {
							case p.available <- w:
							default:
							}
						}
					case <-time.After(100 * time.Millisecond):
						break aggressive
					}
				}
				p.ensureMinWorkers()
				continue
			}

			if p.scaleDownIdleSecs > 0 {
				select {
				case w := <-p.available:
					if w == nil {
						return
					}
					w.mu.Lock()
					state := w.state
					w.mu.Unlock()
					if state == WorkerStateIdle {
						p.removeWorker(w)
					} else {
						select {
						case p.available <- w:
						default:
						}
					}
				case <-time.After(100 * time.Millisecond):
				}
			}

			p.ensureMinWorkers()
		}
	}
}

func (p *WorkerPool) Execute(ctx context.Context, req *Request, timeout time.Duration) (*Response, error) {
	worker, err := p.GetWorker(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker: %w", err)
	}

	req.WorkerID = strconv.Itoa(worker.ID)
	req.RuntimeVersion = RuntimeVersion

	resp, err := worker.Execute(ctx, req, timeout)
	if err != nil {
		worker.mu.Lock()
		worker.state = WorkerStateDead
		worker.mu.Unlock()
		p.ReleaseWorker(worker, nil)
		return nil, err
	}
	p.ReleaseWorker(worker, resp)
	return resp, nil
}

func (w *Worker) Execute(ctx context.Context, req *Request, timeout time.Duration) (*Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return nil, fmt.Errorf("connection not established")
	}

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	if err := w.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("failed to set worker deadline: %w", err)
	}
	defer w.conn.SetDeadline(time.Time{})

	if err := w.protocol.SendRequest(w.conn, req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	resp, err := w.protocol.ReceiveResponse(w.conn)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("worker timeout or request cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return resp, nil
}

func (p *WorkerPool) healthMonitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.checkAndRespawnDeadWorkers()
		}
	}
}

func (p *WorkerPool) checkAndRespawnDeadWorkers() {
	p.mu.RLock()
	workers := make([]*Worker, len(p.workers))
	copy(workers, p.workers)
	p.mu.RUnlock()

	var deadCount int
	for i, worker := range workers {
		if p.isWorkerDead(worker) {
			deadCount++
			if i > 0 {
				time.Sleep(400 * time.Millisecond)
			}
			p.scheduleRespawn(worker, "health_monitor")
		}
	}

	if deadCount > 0 {
		p.logger.Info("Respawning %d dead workers...", deadCount)
	}

	p.ensureMinWorkers()
}

func (p *WorkerPool) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.mu.Unlock()

	p.logger.Info("Stopping worker pool...")
	p.cancel()

	p.mu.RLock()
	workers := make([]*Worker, len(p.workers))
	copy(workers, p.workers)
	p.mu.RUnlock()

	for _, worker := range workers {
		p.terminateWorker(worker, "stop")
	}

	os.RemoveAll(p.sockDir)
	p.logger.Info("Worker pool stopped")
}

func workerStateString(s WorkerState) string {
	switch s {
	case WorkerStateIdle:
		return "idle"
	case WorkerStateBusy:
		return "busy"
	case WorkerStateDead:
		return "dead"
	case WorkerStateStarting:
		return "starting"
	case WorkerStateRespawning:
		return "respawning"
	default:
		return "unknown"
	}
}

func (p *WorkerPool) Stats() PoolStats {
	p.mu.RLock()
	workers := make([]*Worker, len(p.workers))
	copy(workers, p.workers)
	p.mu.RUnlock()

	detail := make([]WorkerInfo, 0, len(workers))
	for _, w := range workers {
		w.mu.Lock()
		detail = append(detail, WorkerInfo{
			Id:           w.ID,
			Pid:          w.Pid,
			State:        workerStateString(w.state),
			RequestCount: w.requestCount,
			StartTime:    w.startTime,
			UptimeSecs:   time.Since(w.startTime).Seconds(),
		})
		w.mu.Unlock()
	}

	return PoolStats{
		TotalRequests:    atomic.LoadInt64(&p.totalRequests),
		ActiveWorkers:    int32(len(workers)),
		AvailableWorkers: int32(len(p.available)),
		WorkersDetail:    detail,
		MinWorkers:       p.runtime.GetMinWorkers(),
		MaxWorkers:       p.runtime.GetMaxWorkers(),
	}
}

type WorkerInfo struct {
	Id           int       `json:"id"`
	Pid          int       `json:"pid"`
	State        string    `json:"state"`
	RequestCount int64     `json:"request_count"`
	StartTime    time.Time `json:"start_time"`
	UptimeSecs   float64   `json:"uptime_secs"`
}

type PoolStats struct {
	TotalRequests    int64        `json:"total_requests"`
	ActiveWorkers    int32        `json:"active_workers"`
	AvailableWorkers int32        `json:"available_workers"`
	MinWorkers       int          `json:"min_workers"`
	MaxWorkers       int          `json:"max_workers"`
	WorkersDetail    []WorkerInfo `json:"workers,omitempty"`
}
