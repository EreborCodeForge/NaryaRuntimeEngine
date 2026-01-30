package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	version = "2.0.0"
	banner  = `
 _   _    _    ______   __    _    
| \ | |  / \  |  _ \ \ / /   / \   
|  \| | / _ \ | |_) \ V /   / _ \  
| |\  |/ ___ \|  _ < | |   / ___ \ 
|_| \_/_/   \_\_| \_\|_|  /_/   \_\
                                   
Runtime Engine v%s
The Runtime that Ignites and Scales Applications
`
)

// RuntimeVersion is exposed so the worker pool can attach it to every request for PHP SDK traceability.
var RuntimeVersion = version

type Server struct {
	config     *Config
	pool       *WorkerPool
	httpServer *http.Server
	logger     *Logger
	metrics    *Metrics
}

type Metrics struct {
	RequestsTotal   int64
	RequestsSuccess int64
	RequestsError   int64
	RequestsActive  int32
}

func NewServer(cfg *Config) (*Server, error) {
	logger := NewLogger(cfg.Logging.Level)

	pool := NewWorkerPool(WorkerPoolConfig{
		NumWorkers:              cfg.Workers.Count,
		MinWorkers:              cfg.Workers.MinWorkers,
		MaxWorkers:              cfg.Workers.MaxWorkers,
		ScaleDownIdleSecs:       cfg.Workers.ScaleDownIdleSecs,
		AggressiveScaleDownSecs: cfg.Workers.AggressiveScaleDownSecs,
		MaxRequests:             cfg.Workers.MaxRequests,
		WorkerTimeout:           cfg.Workers.Timeout,
		PHPBinary:               cfg.PHP.Binary,
		WorkerScript:            cfg.PHP.WorkerScript,
		Logger:                  logger,
		BackpressureEnabled:     cfg.Workers.Backpressure.Enabled,
		BackpressureMaxQueue:    cfg.Workers.Backpressure.MaxQueue,
		QueueTimeoutEnabled:     cfg.Workers.QueueTimeout.Enabled,
		QueueTimeoutMs:          cfg.Workers.QueueTimeout.TimeoutMs,
	})

	s := &Server{
		config:  cfg,
		pool:    pool,
		logger:  logger,
		metrics: &Metrics{},
	}

	return s, nil
}

func (s *Server) Start() error {
	s.logger.Info("Mode: UDS + MessagePack")

	if err := s.pool.Start(); err != nil {
		return fmt.Errorf("failed to start worker pool: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRequest)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/debug/workers", s.handleDebugWorkers)

	s.httpServer = &http.Server{
		Addr:         s.config.Address(),
		Handler:      mux,
		ReadTimeout:  s.config.Server.ReadTimeout,
		WriteTimeout: s.config.Server.WriteTimeout,
	}

	go func() {
		s.logger.Info("HTTP server started at http://%s", s.config.Address())
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			s.logger.Error("HTTP server error: %v", err)
		}
	}()

	return nil
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.metrics.RequestsTotal, 1)
	atomic.AddInt32(&s.metrics.RequestsActive, 1)
	defer atomic.AddInt32(&s.metrics.RequestsActive, -1)

	start := time.Now()

	req, err := s.httpToRequest(r)
	if err != nil {
		s.logger.Error("Failed to convert request: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		atomic.AddInt64(&s.metrics.RequestsError, 1)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Workers.Timeout)
	defer cancel()

	resp, err := s.pool.Execute(ctx, req)
	if err != nil {
		errMsg := err.Error()
		
		if strings.Contains(errMsg, "service unavailable") {
			s.logger.Warn("Service unavailable: %v", err)
			http.Error(w, errMsg, http.StatusServiceUnavailable)
			atomic.AddInt64(&s.metrics.RequestsError, 1)
			return
		}
		
		if strings.Contains(errMsg, "client cancelled") || strings.Contains(errMsg, "context canceled") {
			s.logger.Debug("Request cancelled: %v", err)
			return
		}
		
		s.logger.Error("Failed to execute request: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		atomic.AddInt64(&s.metrics.RequestsError, 1)
		return
	}

	if resp.Error != "" {
		s.logger.Error("Worker error: %s", resp.Error)
		http.Error(w, resp.Error, http.StatusInternalServerError)
		atomic.AddInt64(&s.metrics.RequestsError, 1)
		return
	}

	for name, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	w.WriteHeader(resp.Status)
	w.Write(resp.Body)

	atomic.AddInt64(&s.metrics.RequestsSuccess, 1)

	s.logger.Debug("%s %s %d %v", r.Method, r.URL.Path, resp.Status, time.Since(start))
}

func (s *Server) httpToRequest(r *http.Request) (*Request, error) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxPayloadSize))
		if err != nil {
			return nil, fmt.Errorf("failed to read body: %w", err)
		}
		r.Body.Close()
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	timeoutMs := int(s.config.Workers.Timeout.Milliseconds())

	return &Request{
		ID:         NextRequestID(),
		Method:     r.Method,
		URI:        r.RequestURI,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Headers:    r.Header,
		Body:       body,
		RemoteAddr: r.RemoteAddr,
		Host:       r.Host,
		Scheme:     scheme,
		TimeoutMs:  timeoutMs,
	}, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()

	if stats.ActiveWorkers == 0 {
		http.Error(w, "No active workers", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","workers":%d,"active":%d}`,
		stats.ActiveWorkers, stats.AvailableWorkers)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "# HELP narya_requests_total Total de requests processados\n")
	fmt.Fprintf(w, "# TYPE narya_requests_total counter\n")
	fmt.Fprintf(w, "narya_requests_total %d\n", atomic.LoadInt64(&s.metrics.RequestsTotal))

	fmt.Fprintf(w, "# HELP narya_requests_success Requests com sucesso\n")
	fmt.Fprintf(w, "# TYPE narya_requests_success counter\n")
	fmt.Fprintf(w, "narya_requests_success %d\n", atomic.LoadInt64(&s.metrics.RequestsSuccess))

	fmt.Fprintf(w, "# HELP narya_requests_error Requests com erro\n")
	fmt.Fprintf(w, "# TYPE narya_requests_error counter\n")
	fmt.Fprintf(w, "narya_requests_error %d\n", atomic.LoadInt64(&s.metrics.RequestsError))

	fmt.Fprintf(w, "# HELP narya_requests_active Requests ativos\n")
	fmt.Fprintf(w, "# TYPE narya_requests_active gauge\n")
	fmt.Fprintf(w, "narya_requests_active %d\n", atomic.LoadInt32(&s.metrics.RequestsActive))

	fmt.Fprintf(w, "# HELP narya_workers_active Workers ativos\n")
	fmt.Fprintf(w, "# TYPE narya_workers_active gauge\n")
	fmt.Fprintf(w, "narya_workers_active %d\n", stats.ActiveWorkers)

	fmt.Fprintf(w, "# HELP narya_workers_available Workers disponíveis\n")
	fmt.Fprintf(w, "# TYPE narya_workers_available gauge\n")
	fmt.Fprintf(w, "narya_workers_available %d\n", stats.AvailableWorkers)

	fmt.Fprintf(w, "# HELP narya_pool_requests_total Total de requests no pool\n")
	fmt.Fprintf(w, "# TYPE narya_pool_requests_total counter\n")
	fmt.Fprintf(w, "narya_pool_requests_total %d\n", stats.TotalRequests)

	fmt.Fprintf(w, "# HELP narya_workers_min Mínimo de workers configurado\n")
	fmt.Fprintf(w, "# TYPE narya_workers_min gauge\n")
	fmt.Fprintf(w, "narya_workers_min %d\n", stats.MinWorkers)
	fmt.Fprintf(w, "# HELP narya_workers_max Máximo de workers configurado\n")
	fmt.Fprintf(w, "# TYPE narya_workers_max gauge\n")
	fmt.Fprintf(w, "narya_workers_max %d\n", stats.MaxWorkers)
}

func (s *Server) handleDebugWorkers(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]interface{}{
		"min_workers":       stats.MinWorkers,
		"max_workers":       stats.MaxWorkers,
		"active_workers":    stats.ActiveWorkers,
		"available_workers": stats.AvailableWorkers,
		"total_requests":   stats.TotalRequests,
		"workers":           stats.WorkersDetail,
	})
}

func (s *Server) Stop() error {
	s.logger.Info("Starting graceful shutdown...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.Error("Failed to stop HTTP server: %v", err)
	}

	s.pool.Stop()

	s.logger.Info("Server stopped successfully")
	return nil
}

func main() {
	configFile := flag.String("config", ".rr.yaml", "Config file path")
	host := flag.String("host", "", "Server host (overrides config)")
	port := flag.Int("port", 0, "Server port (overrides config)")
	workers := flag.Int("workers", 0, "Number of workers (overrides config)")
	showVersion := flag.Bool("version", false, "Mostra versão")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Narya Runtime Engine v%s\n", version)
		os.Exit(0)
	}

	fmt.Printf(banner, version)

	cfg, err := LoadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if *host != "" {
		cfg.Server.Host = *host
	}
	if *port > 0 {
		cfg.Server.Port = *port
	}
	if *workers > 0 {
		cfg.Workers.Count = *workers
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\n", err)
		os.Exit(1)
	}

	server, err := NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Erro ao criar servidor: %v\n", err)
		os.Exit(1)
	}

	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start server: %v\n", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	if err := server.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop server: %v\n", err)
		os.Exit(1)
	}
}
