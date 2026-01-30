package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	mu             sync.Mutex
}

type WorkerPool struct {
	workers       []*Worker
	available     chan *Worker
	sockDir       string
	phpBinary     string
	workerScript  string
	maxRequests   int
	workerTimeout time.Duration
	initialWorkers int
	minWorkers     int
	maxWorkers     int
	protocol       *Protocol
	mu            sync.RWMutex
	running       bool
	wg            sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	totalRequests int64
	activeWorkers int32
	nextWorkerID  int32
	logger        *Logger

	lastBusyTime           time.Time
	lastBusyTimeMu         sync.Mutex
	scaleDownIdleSecs      int
	aggressiveScaleDownSecs int

	lastStableLog   time.Time
	lastStableLogMu sync.Mutex

	backpressureEnabled  bool
	backpressureMaxQueue int
	queueTimeoutEnabled  bool
	queueTimeoutMs       int

	queuedRequests int32
}

type WorkerPoolConfig struct {
	NumWorkers    int
	MinWorkers    int
	MaxWorkers    int
	MaxRequests   int
	WorkerTimeout time.Duration
	PHPBinary     string
	WorkerScript  string
	SocketDir     string
	Logger        *Logger

	ScaleDownIdleSecs int
	AggressiveScaleDownSecs int

	BackpressureEnabled  bool
	BackpressureMaxQueue int
	QueueTimeoutEnabled  bool
	QueueTimeoutMs       int
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

	return &WorkerPool{
		workers:             make([]*Worker, 0, maxWorkers),
		available:           make(chan *Worker, maxWorkers),
		sockDir:             sockDir,
		phpBinary:           cfg.PHPBinary,
		workerScript:        cfg.WorkerScript,
		maxRequests:         cfg.MaxRequests,
		workerTimeout:       cfg.WorkerTimeout,
		initialWorkers:      cfg.NumWorkers,
		minWorkers:          minWorkers,
		maxWorkers:          maxWorkers,
		protocol:             NewProtocol(),
		ctx:                  ctx,
		cancel:               cancel,
		logger:               cfg.Logger,
		lastBusyTime:           time.Now(),
		scaleDownIdleSecs:      cfg.ScaleDownIdleSecs,
		aggressiveScaleDownSecs: cfg.AggressiveScaleDownSecs,
		backpressureEnabled:  cfg.BackpressureEnabled,
		backpressureMaxQueue: cfg.BackpressureMaxQueue,
		queueTimeoutEnabled:  cfg.QueueTimeoutEnabled,
		queueTimeoutMs:       cfg.QueueTimeoutMs,
	}
}

func (p *WorkerPool) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("pool is already running")
	}

	if err := os.MkdirAll(p.sockDir, 0700); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	p.logger.Info("Starting pool with %d workers (min=%d max=%d, UDS: %s)", p.initialWorkers, p.minWorkers, p.maxWorkers, p.sockDir)

	for i := 0; i < p.initialWorkers; i++ {
		worker, err := p.spawnWorker(i)
		if err != nil {
			p.logger.Error("Failed to create worker %d: %v", i, err)
			continue
		}
		p.workers = append(p.workers, worker)
		p.available <- worker
		atomic.AddInt32(&p.activeWorkers, 1)
	}

	if len(p.workers) == 0 {
		return fmt.Errorf("no workers were started")
	}

	p.nextWorkerID = int32(len(p.workers))
	p.running = true
	p.logger.Info("Pool started with %d active workers", len(p.workers))

	go p.scalerLoop()

	if p.backpressureEnabled {
		p.logger.Info("Backpressure enabled: max_queue=%d", p.backpressureMaxQueue)
	} else if p.queueTimeoutEnabled {
		p.logger.Info("Queue timeout enabled: timeout=%dms", p.queueTimeoutMs)
	} else {
		p.logger.Info("Sem limite de fila (modo padrão)")
	}

	go p.healthMonitor()

	return nil
}

func (p *WorkerPool) spawnWorker(id int) (*Worker, error) {
	sockPath := filepath.Join(p.sockDir, fmt.Sprintf("worker-%03d.sock", id))

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

	cmd := exec.Command(p.phpBinary, p.workerScript, "--sock", sockPath)
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
	}

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
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		listener.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("timeout waiting for worker connection")
	}

	return worker, nil
}

func (p *WorkerPool) GetWorker(ctx context.Context) (*Worker, error) {

	if p.backpressureEnabled {
		currentQueue := atomic.LoadInt32(&p.queuedRequests)
		if int(currentQueue) >= p.backpressureMaxQueue {
			return nil, fmt.Errorf("service unavailable: queue full (%d/%d)", currentQueue, p.backpressureMaxQueue)
		}
	}

	atomic.AddInt32(&p.queuedRequests, 1)
	defer atomic.AddInt32(&p.queuedRequests, -1)

	select {
	case worker := <-p.available:
		if worker == nil {
			return nil, fmt.Errorf("pool stopped")
		}

		worker.mu.Lock()
		isDead := worker.state == WorkerStateDead

		if !isDead && worker.cmd != nil && worker.cmd.ProcessState != nil && worker.cmd.ProcessState.Exited() {
			isDead = true
			worker.state = WorkerStateDead
		}

		if !isDead && worker.conn != nil {
			worker.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
			buf := make([]byte, 1)
			_, err := worker.conn.Read(buf)
			worker.conn.SetReadDeadline(time.Time{})
			if err != nil && !isTimeout(err) {
				isDead = true
				worker.state = WorkerStateDead
			}
		}
		worker.mu.Unlock()

		if isDead {

			go func(w *Worker) {
				newWorker, err := p.respawnWorker(w)
				if err != nil {
					p.logger.Error("Failed to respawn worker %d: %v", w.ID, err)
					return
				}
				p.available <- newWorker
			}(worker)
		} else {
			worker.mu.Lock()
			worker.state = WorkerStateBusy
			worker.lastActiveTime = time.Now()
			worker.mu.Unlock()
			return worker, nil
		}
	default:

	}

	var queueCtx context.Context
	var queueCancel context.CancelFunc

	if p.queueTimeoutEnabled {
		queueCtx, queueCancel = context.WithTimeout(ctx, time.Duration(p.queueTimeoutMs)*time.Millisecond)
		defer queueCancel()
	} else {
		queueCtx = ctx
	}

	p.mu.RLock()
	n := len(p.workers)
	p.mu.RUnlock()
	if n < p.maxWorkers {
		go p.addWorker()
	}

	for {
		select {
		case worker := <-p.available:
			if worker == nil {
				return nil, fmt.Errorf("pool stopped")
			}

			worker.mu.Lock()
			isDead := worker.state == WorkerStateDead

			if !isDead && worker.cmd != nil && worker.cmd.ProcessState != nil && worker.cmd.ProcessState.Exited() {
				isDead = true
				worker.state = WorkerStateDead
			}

			if !isDead && worker.conn != nil {

				worker.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
				buf := make([]byte, 1)
				_, err := worker.conn.Read(buf)
				worker.conn.SetReadDeadline(time.Time{})

				if err != nil && !isTimeout(err) {
					isDead = true
					worker.state = WorkerStateDead
				}
			}
			worker.mu.Unlock()

			if isDead {

				go func(w *Worker) {
					newWorker, err := p.respawnWorker(w)
					if err != nil {
						p.logger.Error("Failed to respawn worker %d: %v", w.ID, err)
						return
					}
					p.available <- newWorker
				}(worker)

				continue
			}

			worker.mu.Lock()
			worker.state = WorkerStateBusy
			worker.lastActiveTime = time.Now()
			worker.mu.Unlock()
			return worker, nil

		case <-queueCtx.Done():

			if p.queueTimeoutEnabled && queueCtx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("service unavailable: queue timeout (%dms)", p.queueTimeoutMs)
			}
			return nil, ctx.Err()
		}
	}
}


func isTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}


func (p *WorkerPool) ReleaseWorker(worker *Worker, resp *Response) {
	worker.mu.Lock()
	currentState := worker.state
	worker.requestCount++
	count := worker.requestCount
	
	if currentState == WorkerStateDead {
		worker.mu.Unlock()
		p.logger.Info("Worker %d is dead, respawning...", worker.ID)
		go func() {
			newWorker, err := p.respawnWorker(worker)
			if err != nil {
				p.logger.Error("Failed to respawn worker %d: %v", worker.ID, err)
				return
			}
			p.available <- newWorker
		}()
		return
	}
	
	worker.state = WorkerStateIdle
	worker.mu.Unlock()

	p.lastBusyTimeMu.Lock()
	p.lastBusyTime = time.Now()
	p.lastBusyTimeMu.Unlock()

	atomic.AddInt64(&p.totalRequests, 1)

	shouldRecycle := false
	if resp != nil && resp.Meta.Recycle {
		shouldRecycle = true
		p.logger.Info("Worker %d requested cooperative recycle", worker.ID)
	}

	if p.maxRequests > 0 && count >= int64(p.maxRequests) {
		shouldRecycle = true
		p.logger.Info("Worker %d reached request limit (%d)", worker.ID, p.maxRequests)
	}

	if shouldRecycle {
		go func() {
			newWorker, err := p.respawnWorker(worker)
			if err != nil {
				p.logger.Error("Failed to recycle worker %d: %v", worker.ID, err)
				return
			}
			p.available <- newWorker
		}()
		return
	}

	p.available <- worker
}

func (p *WorkerPool) respawnWorker(old *Worker) (*Worker, error) {
	if old.conn != nil {
		old.conn.Close()
	}
	if old.listener != nil {
		old.listener.Close()
	}

	if old.cmd != nil && old.cmd.Process != nil {
		old.cmd.Process.Signal(syscall.SIGTERM)

		done := make(chan error, 1)
		go func() { done <- old.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			old.cmd.Process.Kill()
		}
	}

	os.Remove(old.sockPath)
	atomic.AddInt32(&p.activeWorkers, -1)

	newWorker, err := p.spawnWorker(old.ID)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	for i, w := range p.workers {
		if w.ID == old.ID {
			p.workers[i] = newWorker
			break
		}
	}
	p.mu.Unlock()

	atomic.AddInt32(&p.activeWorkers, 1)
	return newWorker, nil
}

func (p *WorkerPool) addWorker() {

	for {
		cur := atomic.LoadInt32(&p.activeWorkers)
		if cur >= int32(p.maxWorkers) {
			return
		}

		if atomic.CompareAndSwapInt32(&p.activeWorkers, cur, cur+1) {
			break
		}

	}

	id := int(atomic.AddInt32(&p.nextWorkerID, 1) - 1)

	worker, err := p.spawnWorker(id)
	if err != nil {
		atomic.AddInt32(&p.activeWorkers, -1)
		p.logger.Error("Scale-up: failed to create worker %d: %v", id, err)
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
	default:

		p.destroyWorker(worker)
		atomic.AddInt32(&p.activeWorkers, -1)
	}
}


func (p *WorkerPool) removeWorker(w *Worker) {

	p.mu.Lock()
	if len(p.workers) <= p.minWorkers {
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

	p.destroyWorker(w)
	newActive := atomic.AddInt32(&p.activeWorkers, -1)
	if newActive < int32(p.minWorkers) {
		atomic.StoreInt32(&p.activeWorkers, int32(p.minWorkers))
		newActive = int32(p.minWorkers)
	}
	p.logger.Info("Scale-down: worker %d (PID %d) removed; active=%d, total_slots=%d", w.ID, w.Pid, newActive, currentTotal)
}

func (p *WorkerPool) destroyWorker(w *Worker) {
	if w.conn != nil {
		w.conn.Close()
	}
	if w.listener != nil {
		w.listener.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		w.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- w.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			w.cmd.Process.Kill()
		}
	}
	os.Remove(w.sockPath)
}

func (p *WorkerPool) scalerLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.lastBusyTimeMu.Lock()
			idleSince := time.Since(p.lastBusyTime)
			p.lastBusyTimeMu.Unlock()

			if p.scaleDownIdleSecs > 0 && idleSince < time.Duration(p.scaleDownIdleSecs)*time.Second {
				continue
			}

			p.mu.RLock()
			n := len(p.workers)
			min := p.minWorkers
			max := p.maxWorkers
			p.mu.RUnlock()
			if n <= min {
				thresholdSec := p.aggressiveScaleDownSecs
				if thresholdSec <= 0 {
					thresholdSec = p.scaleDownIdleSecs
				}
				if thresholdSec > 0 && idleSince >= time.Duration(thresholdSec)*time.Second {
					p.lastStableLogMu.Lock()
					if time.Since(p.lastStableLog) >= 30*time.Second {
						active := atomic.LoadInt32(&p.activeWorkers)
						p.logger.Info("Pool idle and stable: %d active workers (min=%d, max=%d)", active, min, max)
						p.lastStableLog = time.Now()
					}
					p.lastStableLogMu.Unlock()
				}
				continue
			}

			if p.aggressiveScaleDownSecs > 0 && idleSince >= time.Duration(p.aggressiveScaleDownSecs)*time.Second {
				for {
					p.mu.RLock()
					current := len(p.workers)
					p.mu.RUnlock()
					if current <= min {
						break
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
							p.available <- w
						}
					case <-time.After(100 * time.Millisecond):
						break
					}
				}
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
						p.available <- w
					}
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}
}

func (p *WorkerPool) Execute(ctx context.Context, req *Request) (*Response, error) {
	worker, err := p.GetWorker(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker: %w", err)
	}

	internalTimeout := p.workerTimeout + 5*time.Second

	resultCh := make(chan *Response, 1)
	errCh := make(chan error, 1)

	go func() {
		resp, err := worker.Execute(req)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- resp
	}()

	select {
	case resp := <-resultCh:
		p.ReleaseWorker(worker, resp)
		return resp, nil
		
	case err := <-errCh:
		worker.mu.Lock()
		worker.state = WorkerStateDead
		worker.mu.Unlock()
		p.ReleaseWorker(worker, nil)
		return nil, err
		
	case <-ctx.Done():
		go func() {
			select {
			case resp := <-resultCh:
				p.ReleaseWorker(worker, resp)
			case err := <-errCh:
				worker.mu.Lock()
				worker.state = WorkerStateDead
				worker.mu.Unlock()
				p.ReleaseWorker(worker, nil)
				p.logger.Debug("Worker %d error after client closed: %v", worker.ID, err)
			case <-time.After(internalTimeout):
				p.logger.Warn("Worker %d real timeout, will be restarted", worker.ID)
				worker.mu.Lock()
				worker.state = WorkerStateDead
				worker.mu.Unlock()
				if worker.cmd.Process != nil {
					worker.cmd.Process.Kill()
				}
				p.ReleaseWorker(worker, nil)
			}
		}()
		return nil, fmt.Errorf("client cancelled request")
	}
}

func (w *Worker) Execute(req *Request) (*Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return nil, fmt.Errorf("connection not established")
	}

	if req.TimeoutMs > 0 {
		deadline := time.Now().Add(time.Duration(req.TimeoutMs) * time.Millisecond)
		w.conn.SetDeadline(deadline)
	}

	req.WorkerID = w.ID
	req.RuntimeVersion = RuntimeVersion

	if err := w.protocol.SendRequest(w.conn, req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	resp, err := w.protocol.ReceiveResponse(w.conn)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	w.conn.SetDeadline(time.Time{})

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

	var deadWorkers []*Worker

	for _, worker := range workers {
		worker.mu.Lock()
		if worker.cmd != nil && worker.cmd.ProcessState != nil && worker.cmd.ProcessState.Exited() {
			if worker.state != WorkerStateDead {
				worker.state = WorkerStateDead
				p.logger.Warn("Worker %d detected dead by health monitor", worker.ID)
			}
		}
		
		if worker.state == WorkerStateDead {
			deadWorkers = append(deadWorkers, worker)
		}
		worker.mu.Unlock()
	}

	if len(deadWorkers) > 0 {
		p.logger.Info("Respawning %d dead workers...", len(deadWorkers))
		var wg sync.WaitGroup
		for _, worker := range deadWorkers {
			wg.Add(1)
			go func(w *Worker) {
				defer wg.Done()
				newWorker, err := p.respawnWorker(w)
				if err != nil {
					p.logger.Error("Failed to respawn worker %d: %v", w.ID, err)
					return
				}
				p.available <- newWorker
				p.logger.Info("Worker %d respawned successfully", w.ID)
			}(worker)
		}
		wg.Wait()
	}
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
	for _, worker := range p.workers {
		if worker.conn != nil {
			worker.conn.Close()
		}
		if worker.listener != nil {
			worker.listener.Close()
		}
		if worker.cmd != nil && worker.cmd.Process != nil {
			worker.cmd.Process.Signal(syscall.SIGTERM)
		}
		os.Remove(worker.sockPath)
	}
	p.mu.RUnlock()

	time.Sleep(1 * time.Second)

	p.mu.RLock()
	for _, worker := range p.workers {
		if worker.cmd != nil && worker.cmd.Process != nil {
			worker.cmd.Process.Kill()
		}
	}
	p.mu.RUnlock()

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
		MinWorkers:       p.minWorkers,
		MaxWorkers:       p.maxWorkers,
	}
}

type WorkerInfo struct {
	Id           int     `json:"id"`
	Pid          int     `json:"pid"`
	State        string  `json:"state"`
	RequestCount int64   `json:"request_count"`
	StartTime    time.Time `json:"start_time"`
	UptimeSecs   float64 `json:"uptime_secs"`
}

type PoolStats struct {
	TotalRequests    int64        `json:"total_requests"`
	ActiveWorkers    int32        `json:"active_workers"`
	AvailableWorkers int32        `json:"available_workers"`
	MinWorkers       int          `json:"min_workers"`
	MaxWorkers       int          `json:"max_workers"`
	WorkersDetail    []WorkerInfo `json:"workers,omitempty"`
}
