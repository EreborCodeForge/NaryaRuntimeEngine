package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Verifica valores padrão
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("Default host: expected 0.0.0.0, got %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 8888 {
		t.Errorf("Default port: expected 8888, got %d", cfg.Server.Port)
	}
	if cfg.Workers.Count != 4 {
		t.Errorf("Workers padrão: esperado 4, obtido %d", cfg.Workers.Count)
	}
	if cfg.Workers.MaxRequests != 1000 {
		t.Errorf("Default MaxRequests: expected 1000, got %d", cfg.Workers.MaxRequests)
	}
	if cfg.Workers.Timeout != 30*time.Second {
		t.Errorf("Default timeout: expected 30s, got %v", cfg.Workers.Timeout)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid config",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "porta inválida (0)",
			modify:  func(c *Config) { c.Server.Port = 0 },
			wantErr: true,
		},
		{
			name:    "invalid port (high)",
			modify:  func(c *Config) { c.Server.Port = 70000 },
			wantErr: true,
		},
		{
			name:    "workers inválido",
			modify:  func(c *Config) { c.Workers.Count = 0 },
			wantErr: true,
		},
		{
			name:    "invalid timeout",
			modify:  func(c *Config) { c.Workers.Timeout = 500 * time.Millisecond },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigAddress(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 9000

	addr := cfg.Address()
	if addr != "127.0.0.1:9000" {
		t.Errorf("Address: expected 127.0.0.1:9000, got %s", addr)
	}
}

func TestLoadConfigNonExistent(t *testing.T) {
	cfg, err := LoadConfig("nao_existe.yaml")
	if err != nil {
		t.Fatalf("LoadConfig should not return error for non-existent file: %v", err)
	}

	// Deve usar defaults
	if cfg.Server.Port != 8888 {
		t.Errorf("Should use default port 8888, got %d", cfg.Server.Port)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	// Cria arquivo temporário
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.yaml")

	content := `server:
  host: "127.0.0.1"
  port: 9000
workers:
  count: 8
  max_requests: 500
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Erro ao carregar config: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Host: expected 127.0.0.1, got %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 9000 {
		t.Errorf("Port: expected 9000, got %d", cfg.Server.Port)
	}
	if cfg.Workers.Count != 8 {
		t.Errorf("Workers: expected 8, got %d", cfg.Workers.Count)
	}
}
