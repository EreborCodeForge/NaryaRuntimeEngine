// Engine: non-blocking HTTP server. Each request runs in its own goroutine (net/http).
// Dispatcher: pool of N PHP worker processes; no global mutex — per-worker mutex only.
// PHP workers are separate OS processes (exec.Command), not threads, not fork-per-request.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// createOptimizedListener cria um TCP listener com otimizações para high-throughput.
// - TCP_NODELAY: desabilita Nagle algorithm (reduz latência)
// - Keep-Alive: detecta conexões mortas
// Nota: O backlog real é limitado pelo kernel (net.core.somaxconn no Linux).
func createOptimizedListener(addr string, backlog int) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	// Wrap para configurar TCP optimizations em conexões aceitas
	return &tcpKeepAliveListener{listener.(*net.TCPListener)}, nil
}

// tcpKeepAliveListener configura TCP keep-alive e no-delay em conexões aceitas
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln *tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.TCPListener.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(30 * time.Second)
	tc.SetNoDelay(true) // Desabilita Nagle - reduz latência em requests pequenos
	return tc, nil
}

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

type Server struct {
	config        *Config
	pool          *WorkerPool
	runtimeConfig *RuntimeConfig
	httpServer    *http.Server
	logger        *Logger
	metrics       *Metrics
}

type Metrics struct {
	RequestsTotal      int64
	RequestsSuccess    int64
	RequestsError      int64
	RequestsActive     int32
	LastRequestDurMs   int64
}

func NewServer(cfg *Config) (*Server, error) {
	logger := NewLogger(cfg.Logging.Level)
	runtimeConfig := NewRuntimeConfig(
		cfg.Workers.MinWorkers,
		cfg.Workers.MaxWorkers,
		cfg.Workers.Backpressure.Enabled,
		cfg.Workers.Backpressure.MaxQueue,
		cfg.Workers.KeepWarm,
	)

	pool := NewWorkerPool(WorkerPoolConfig{
		Runtime:                runtimeConfig,
		NumWorkers:             cfg.Workers.Count,
		MinWorkers:             cfg.Workers.MinWorkers,
		MaxWorkers:             cfg.Workers.MaxWorkers,
		ScaleDownIdleSecs:       cfg.Workers.ScaleDownIdleSecs,
		AggressiveScaleDownSecs: cfg.Workers.AggressiveScaleDownSecs,
		WarmupStaggerMs:         cfg.Workers.WarmupStaggerMs,
		FastWarmup:              cfg.Workers.FastWarmup,
		MaxRequests:             cfg.Workers.MaxRequests,
		WorkerTimeout:           cfg.Workers.Timeout,
		SocketDir:                cfg.Workers.SocketDir,
		PHPBinary:               cfg.PHP.Binary,
		WorkerScript:            cfg.PHP.WorkerScript,
		Logger:                  logger,
		BackpressureEnabled:     cfg.Workers.Backpressure.Enabled,
		BackpressureMaxQueue:    cfg.Workers.Backpressure.MaxQueue,
		QueueTimeoutEnabled:     cfg.Workers.QueueTimeout.Enabled,
		QueueTimeoutMs:          cfg.Workers.QueueTimeout.TimeoutMs,
	})

	s := &Server{
		config:        cfg,
		pool:          pool,
		runtimeConfig: runtimeConfig,
		logger:        logger,
		metrics:       &Metrics{},
	}

	return s, nil
}

func (s *Server) Start() error {
	s.logger.Info("Mode: UDS + MessagePack")

	// Boot não bloqueante: pool sobe em background, HTTP sobe logo
	go func() {
		if err := s.pool.Start(); err != nil {
			s.logger.Error("Worker pool start error: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRequest)
	mux.HandleFunc("/narya/health", s.handleHealth)
	mux.HandleFunc("/narya/metrics", s.handleMetrics)
	mux.HandleFunc("/narya/debug/workers", s.handleDebugWorkers)
	mux.HandleFunc("/narya/config", s.handleConfig)

	s.httpServer = &http.Server{
		Addr:         s.config.Address(),
		Handler:      mux,
		ReadTimeout:  s.config.Server.ReadTimeout,
		WriteTimeout: s.config.Server.WriteTimeout,
	}

	// Custom listener com backlog aumentado e TCP optimizations
	listener, err := createOptimizedListener(s.config.Address(), s.config.Server.Backlog)
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	s.logger.Info("HTTP server started at http://%s (backlog=%d)", s.config.Address(), s.config.Server.Backlog)
	return s.httpServer.Serve(listener)
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
	defer ReleaseRequest(req)

	ctx, cancel := context.WithTimeout(r.Context(), s.config.Workers.Timeout)
	defer cancel()

	resp, err := s.pool.Execute(ctx, req)
	if err != nil {
		errMsg := err.Error()

		// Backpressure ou fila: retornar 503 (nunca 500)
		if strings.Contains(errMsg, "service unavailable") ||
			strings.Contains(errMsg, "queue full") ||
			strings.Contains(errMsg, "no worker available") ||
			strings.Contains(errMsg, "queue timeout") {
			s.logger.Warn("Service unavailable: %v", err)
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			atomic.AddInt64(&s.metrics.RequestsError, 1)
			return
		}

		if strings.Contains(errMsg, "client cancelled") || strings.Contains(errMsg, "context canceled") {
			s.logger.Debug("Request cancelled: %v", err)
			return
		}
		if strings.Contains(errMsg, "deadline exceeded") {
			s.logger.Warn("Request timeout waiting for worker (overload): %v", err)
			http.Error(w, "timeout waiting for worker", http.StatusServiceUnavailable)
			atomic.AddInt64(&s.metrics.RequestsError, 1)
			return
		}
		if strings.Contains(errMsg, "broken pipe") {
			s.logger.Debug("Worker connection closed (broken pipe), will respawn: %v", err)
		} else if strings.Contains(errMsg, "EOF") {
			s.logger.Debug("Worker closed or died before response (EOF), will respawn: %v", err)
		} else {
			s.logger.Error("Failed to execute request: %v", err)
		}
		// Erros de conexão/resposta do worker → 503 (transiente), resto → 500
		if strings.Contains(errMsg, "failed to read response") || strings.Contains(errMsg, "connection") || strings.Contains(errMsg, "handshake") {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
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
	atomic.StoreInt64(&s.metrics.LastRequestDurMs, time.Since(start).Milliseconds())

	// Debug só se habilitado (evita avaliação de argumentos no hot path)
	if s.logger.IsDebugEnabled() {
		s.logger.Debug("%s %s %d %v", r.Method, r.URL.Path, resp.Status, time.Since(start))
	}
}

// BuildServerParamsInto builds CGI/Apache-style server params from http.Request into pre-allocated map.
func BuildServerParamsInto(r *http.Request, m map[string]string) {
	host := r.Host
	port := ""
	if host != "" {
		var err error
		host, port, err = net.SplitHostPort(r.Host)
		if err != nil {
			if strings.Contains(err.Error(), "missing port") {
				host = r.Host
				if r.TLS != nil {
					port = "443"
				} else {
					port = "80"
				}
			} else {
				host = r.Host
				port = "80"
			}
		}
	}
	if port == "" {
		if r.TLS != nil {
			port = "443"
		} else {
			port = "80"
		}
	}

	scheme := "http"
	https := "off"
	if r.TLS != nil {
		scheme = "https"
		https = "on"
	}

	remoteHost, remotePort := "", ""
	if r.RemoteAddr != "" {
		remoteHost, remotePort, _ = net.SplitHostPort(r.RemoteAddr)
		if remotePort == "" {
			remotePort = "0"
		}
	}

	now := time.Now()

	m["REQUEST_METHOD"] = r.Method
	m["REQUEST_URI"] = r.URL.RequestURI()
	m["QUERY_STRING"] = r.URL.RawQuery
	m["SERVER_PROTOCOL"] = r.Proto
	m["HTTP_HOST"] = r.Host
	m["SERVER_NAME"] = host
	m["SERVER_PORT"] = port
	m["REMOTE_ADDR"] = remoteHost
	m["REMOTE_PORT"] = remotePort
	m["REQUEST_SCHEME"] = scheme
	m["HTTPS"] = https
	m["CONTENT_TYPE"] = r.Header.Get("Content-Type")
	m["CONTENT_LENGTH"] = r.Header.Get("Content-Length")
	m["REQUEST_TIME"] = strconv.FormatInt(now.Unix(), 10)
	m["REQUEST_TIME_FLOAT"] = strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', 6, 64)
	m["SCRIPT_NAME"] = ""
	m["SCRIPT_FILENAME"] = ""
	m["DOCUMENT_ROOT"] = ""
}

// buildFullURI returns the full request URL (scheme + host + path + query) for PSR-7.
func buildFullURI(r *http.Request, scheme, host string) string {
	if host == "" {
		host = r.Host
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	uri := scheme + "://" + host + path
	if r.URL.RawQuery != "" {
		uri += "?" + r.URL.RawQuery
	}
	return uri
}

func (s *Server) httpToRequest(r *http.Request) (*Request, error) {
	req := AcquireRequest()

	if r.Body != nil {
		var err error
		req.Body, err = io.ReadAll(io.LimitReader(r.Body, MaxPayloadSize))
		if err != nil {
			ReleaseRequest(req)
			return nil, fmt.Errorf("failed to read body: %w", err)
		}
		r.Body.Close()
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	// Normalize protocol to "1.1" for PSR-7 (e.g. "HTTP/1.1" -> "1.1")
	protocol := "1.1"
	if p := r.Proto; p != "" {
		if strings.HasPrefix(strings.ToUpper(p), "HTTP/") {
			protocol = strings.TrimPrefix(strings.ToUpper(p), "HTTP/")
		}
	}

	req.ID = NextRequestID()
	req.Method = r.Method
	req.URI = buildFullURI(r, scheme, r.Host)
	req.Path = r.URL.Path
	req.Query = r.URL.RawQuery
	req.Protocol = protocol
	req.RemoteAddr = r.RemoteAddr
	req.Host = r.Host
	req.Scheme = scheme
	req.TimeoutMs = int(s.config.Workers.Timeout.Milliseconds())

	// Copy headers into pre-allocated map
	for k, v := range r.Header {
		req.Headers[k] = v
	}

	// Build server params into pre-allocated map
	BuildServerParamsInto(r, req.Server)

	return req, nil
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

	fmt.Fprintf(w, "# HELP narya_request_duration_ms_last Last request duration (ms)\n")
	fmt.Fprintf(w, "# TYPE narya_request_duration_ms_last gauge\n")
	fmt.Fprintf(w, "narya_request_duration_ms_last %d\n", atomic.LoadInt64(&s.metrics.LastRequestDurMs))
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		min, max, bp, maxq, keepWarm := s.runtimeConfig.Snapshot()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"min_workers":            min,
			"max_workers":            max,
			"backpressure_enabled":   bp,
			"backpressure_max_queue": maxq,
			"keep_warm":              keepWarm,
		})
		return
	case http.MethodPatch, http.MethodPut:
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if err := s.runtimeConfig.Update(body); err != nil {
			http.Error(w, `{"error":"invalid config value"}`, http.StatusBadRequest)
			return
		}
		min, max, bp, maxq, keepWarm := s.runtimeConfig.Snapshot()
		s.logger.Info("Config updated: min_workers=%d max_workers=%d backpressure=%v max_queue=%d keep_warm=%v", min, max, bp, maxq, keepWarm)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true,
			"min_workers": min, "max_workers": max,
			"backpressure_enabled": bp, "backpressure_max_queue": maxq,
			"keep_warm": keepWarm,
		})
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	configFile := flag.String("config", ".nry.yaml", "Config file path")
	host := flag.String("host", "", "Server host (overrides config)")
	port := flag.Int("port", 0, "Server port (overrides config)")
	workers := flag.Int("workers", 0, "Number of workers (overrides config)")
	showVersion := flag.Bool("version", false, "Mostra versão")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Narya Runtime Engine v%s\n", version)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) >= 1 && args[0] == "config" {
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
		os.Exit(runConfigCLI(cfg, args[1:]))
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

// runConfigCLI executa os subcomandos "config get" e "config set" contra o runner em execução.
// Uso: narya -config=.nry.yaml config get | narya -config=.nry.yaml config set min_workers=8 max_workers=16
func runConfigCLI(cfg *Config, subArgs []string) int {
	baseURL := "http://" + configClientAddress(cfg)
	client := &http.Client{Timeout: 10 * time.Second}

	if len(subArgs) == 0 || subArgs[0] == "get" {
		resp, err := client.Get(baseURL + "/narya/config")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get config: %v (is the runner up?)\n", err)
			return 1
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "GET /narya/config failed: %s\n%s\n", resp.Status, string(body))
			return 1
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Read response: %v\n", err)
			return 1
		}
		fmt.Println(string(body))
		return 0
	}

	if subArgs[0] == "set" {
		updates := make(map[string]interface{})
		for _, pair := range subArgs[1:] {
			key, val, ok := strings.Cut(pair, "=")
			if !ok || key == "" {
				fmt.Fprintf(os.Stderr, "Invalid pair (use key=value): %q\n", pair)
				return 1
			}
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)
			switch key {
			case "min_workers", "max_workers", "backpressure_max_queue":
				n, err := strconv.Atoi(val)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Invalid number for %s: %q\n", key, val)
					return 1
				}
				updates[key] = n
			case "backpressure_enabled", "keep_warm":
				updates[key] = val == "true" || val == "1"
			default:
				fmt.Fprintf(os.Stderr, "Unknown key: %q (allowed: min_workers, max_workers, backpressure_enabled, backpressure_max_queue, keep_warm)\n", key)
				return 1
			}
		}
		if len(updates) == 0 {
			fmt.Fprintf(os.Stderr, "Usage: narya -config=.nry.yaml config set min_workers=8 [max_workers=16 ...]\n")
			return 1
		}
		body, _ := json.Marshal(updates)
		req, err := http.NewRequest(http.MethodPatch, baseURL+"/narya/config", strings.NewReader(string(body)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Request: %v\n", err)
			return 1
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to update config: %v (is the runner up?)\n", err)
			return 1
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "PATCH /narya/config failed: %s\n%s\n", resp.Status, string(respBody))
			return 1
		}
		fmt.Println(string(respBody))
		return 0
	}

	if subArgs[0] == "help" || subArgs[0] == "-h" || subArgs[0] == "--help" {
		fmt.Println("Usage:")
		fmt.Println("  narya -config=.nry.yaml config get")
		fmt.Println("  narya -config=.nry.yaml config set min_workers=N [max_workers=N] [backpressure_enabled=true|false] [backpressure_max_queue=N] [keep_warm=true|false]")
		fmt.Println("")
		fmt.Println("Options:")
		fmt.Println("  min_workers           Minimum workers always active")
		fmt.Println("  max_workers           Maximum workers allowed")
		fmt.Println("  backpressure_enabled  Enable/disable backpressure (503 when queue full)")
		fmt.Println("  backpressure_max_queue Max requests in queue (0 = unlimited)")
		fmt.Println("  keep_warm             If true, disables all scale-down (workers always hot)")
		return 0
	}

	fmt.Fprintf(os.Stderr, "Unknown subcommand: %q (use: get, set, help)\n", subArgs[0])
	return 1
}

// configClientAddress retorna host:port para o cliente se conectar (0.0.0.0 vira 127.0.0.1).
func configClientAddress(cfg *Config) string {
	host := cfg.Server.Host
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, cfg.Server.Port)
}
