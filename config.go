package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config representa a configuração do Narya
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Workers WorkersConfig `yaml:"workers"`
	PHP     PHPConfig     `yaml:"php"`
	Logging LoggingConfig `yaml:"logging"`
}

// ServerConfig configuração do servidor HTTP
type ServerConfig struct {
	Host         string        `yaml:"host"`
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"-"`
	WriteTimeout time.Duration `yaml:"-"`
	EnableHTTP2  bool          `yaml:"enable_http2"`
	// Campos para parsing YAML (inteiros em segundos)
	ReadTimeoutSecs  int `yaml:"read_timeout"`
	WriteTimeoutSecs int `yaml:"write_timeout"`
}

// WorkersConfig configuração dos workers
type WorkersConfig struct {
	Count       int           `yaml:"count"`
	MinWorkers  int           `yaml:"min_workers"`  // Mínimo sempre ativos (warmup); 0 = usa count
	MaxWorkers  int           `yaml:"max_workers"`  // Teto de workers; 0 = usa count
	ScaleDownIdleSecs int     `yaml:"scale_down_idle_secs"` // Segundos ociosos para scale-down suave; 0 = desligado
	AggressiveScaleDownSecs int `yaml:"aggressive_scale_down_secs"` // Após Xs ocioso, derruba tudo até o mínimo
	MaxRequests int           `yaml:"max_requests"`
	Timeout     time.Duration `yaml:"-"`
	// Campo para parsing YAML (inteiro em segundos)
	TimeoutSecs int `yaml:"timeout"`

	// Estratégias de overflow (mutuamente exclusivas)
	// backpressure: rejeita imediatamente com 503 se fila cheia
	// queue_timeout: espera até X segundos na fila, depois 503
	Backpressure BackpressureConfig `yaml:"backpressure"`
	QueueTimeout QueueTimeoutConfig `yaml:"queue_timeout"`
}

// BackpressureConfig configuração de backpressure
// Rejeita requests imediatamente se a fila exceder o limite
type BackpressureConfig struct {
	Enabled  bool `yaml:"enabled"`
	MaxQueue int  `yaml:"max_queue"` // Máximo de requests na fila (0 = sem fila)
}

// QueueTimeoutConfig configuração de timeout de fila
// Requests esperam até timeout_ms na fila, depois são rejeitadas
type QueueTimeoutConfig struct {
	Enabled   bool `yaml:"enabled"`
	TimeoutMs int  `yaml:"timeout_ms"` // Tempo máximo de espera na fila
}

// PHPConfig configuração do PHP
type PHPConfig struct {
	Binary       string `yaml:"binary"`
	WorkerScript string `yaml:"worker_script"`
}

// LoggingConfig configuração de logging
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DefaultConfig retorna a configuração padrão
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:             "0.0.0.0",
			Port:             8888,
			ReadTimeout:      60 * time.Second,
			WriteTimeout:     60 * time.Second,
			ReadTimeoutSecs:  60,
			WriteTimeoutSecs: 60,
			EnableHTTP2:      true,
		},
		Workers: WorkersConfig{
			Count:       4,
			MinWorkers:  4,
			MaxWorkers:  4,
			MaxRequests: 1000,
			Timeout:     30 * time.Second,
			TimeoutSecs: 30,
		},
		PHP: PHPConfig{
			Binary:       "php",
			WorkerScript: "worker.php",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// LoadConfig carrega configuração de um arquivo YAML
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // Usa configuração padrão se arquivo não existe
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Converte valores inteiros de segundos para time.Duration
	if cfg.Server.ReadTimeoutSecs > 0 {
		cfg.Server.ReadTimeout = time.Duration(cfg.Server.ReadTimeoutSecs) * time.Second
	}
	if cfg.Server.WriteTimeoutSecs > 0 {
		cfg.Server.WriteTimeout = time.Duration(cfg.Server.WriteTimeoutSecs) * time.Second
	}
	if cfg.Workers.TimeoutSecs > 0 {
		cfg.Workers.Timeout = time.Duration(cfg.Workers.TimeoutSecs) * time.Second
	}

	// Normaliza min/max: 0 = usar count (comportamento fixo)
	if cfg.Workers.MinWorkers <= 0 {
		cfg.Workers.MinWorkers = cfg.Workers.Count
	}
	if cfg.Workers.MaxWorkers <= 0 {
		cfg.Workers.MaxWorkers = cfg.Workers.Count
	}

	return cfg, nil
}

// Validate valida a configuração
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("porta inválida: %d", c.Server.Port)
	}

	if c.Workers.Count < 1 {
		return fmt.Errorf("number of workers must be at least 1")
	}
	if c.Workers.MinWorkers < 1 || c.Workers.MaxWorkers < 1 {
		return fmt.Errorf("min_workers e max_workers devem ser >= 1")
	}
	if c.Workers.MinWorkers > c.Workers.Count || c.Workers.Count > c.Workers.MaxWorkers {
		return fmt.Errorf("must be min_workers <= count <= max_workers (current: min=%d count=%d max=%d)",
			c.Workers.MinWorkers, c.Workers.Count, c.Workers.MaxWorkers)
	}

	if c.Workers.Timeout < 1*time.Second {
		return fmt.Errorf("timeout de worker muito baixo: %v", c.Workers.Timeout)
	}

	// Validação: backpressure e queue_timeout são mutuamente exclusivos
	if c.Workers.Backpressure.Enabled && c.Workers.QueueTimeout.Enabled {
		return fmt.Errorf("backpressure and queue_timeout are mutually exclusive - enable only one")
	}

	// Validação de valores
	if c.Workers.Backpressure.Enabled && c.Workers.Backpressure.MaxQueue < 0 {
		return fmt.Errorf("backpressure.max_queue deve ser >= 0")
	}

	if c.Workers.QueueTimeout.Enabled && c.Workers.QueueTimeout.TimeoutMs <= 0 {
		return fmt.Errorf("queue_timeout.timeout_ms must be > 0")
	}

	return nil
}

// Address retorna o endereço completo do servidor
func (c *Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}
