package main

import (
	"strconv"
	"sync"
)

// RuntimeConfig mantém parâmetros alteráveis em tempo de execução (min/max workers, backpressure, keep_warm).
// Leitura com RLock; escrita só via PATCH /narya/config. Impacto em performance é desprezível.
type RuntimeConfig struct {
	mu                   sync.RWMutex
	MinWorkers           int  `json:"min_workers"`
	MaxWorkers           int  `json:"max_workers"`
	BackpressureEnabled  bool `json:"backpressure_enabled"`
	BackpressureMaxQueue int  `json:"backpressure_max_queue"`
	KeepWarm             bool `json:"keep_warm"` // Se true, desabilita scale-down (workers sempre quentes)
}

func NewRuntimeConfig(min, max int, backpressure bool, maxQueue int, keepWarm bool) *RuntimeConfig {
	if max <= 0 {
		max = min
	}
	if min <= 0 {
		min = 1
	}
	return &RuntimeConfig{
		MinWorkers:           min,
		MaxWorkers:           max,
		BackpressureEnabled:  backpressure,
		BackpressureMaxQueue: maxQueue,
		KeepWarm:             keepWarm,
	}
}

func (r *RuntimeConfig) GetMinWorkers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.MinWorkers
}

func (r *RuntimeConfig) GetMaxWorkers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.MaxWorkers
}

func (r *RuntimeConfig) GetBackpressureEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.BackpressureEnabled
}

func (r *RuntimeConfig) GetBackpressureMaxQueue() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.BackpressureMaxQueue
}

func (r *RuntimeConfig) GetKeepWarm() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.KeepWarm
}

// Update aplica apenas os campos presentes no mapa (ex: {"min_workers": 8}). Retorna erro se validação falhar.
func (r *RuntimeConfig) Update(updates map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if v, ok := updates["min_workers"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 {
			return errInvalidConfig
		}
		r.MinWorkers = n
	}
	if v, ok := updates["max_workers"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 {
			return errInvalidConfig
		}
		r.MaxWorkers = n
	}
	if v, ok := updates["backpressure_enabled"]; ok {
		r.BackpressureEnabled = toBool(v)
	}
	if v, ok := updates["backpressure_max_queue"]; ok {
		n, err := toInt(v)
		if err != nil || n < 0 {
			return errInvalidConfig
		}
		r.BackpressureMaxQueue = n
	}
	if v, ok := updates["keep_warm"]; ok {
		r.KeepWarm = toBool(v)
	}

	if r.MinWorkers > r.MaxWorkers {
		r.MaxWorkers = r.MinWorkers
	}
	return nil
}

func (r *RuntimeConfig) Snapshot() (min, max int, backpressure bool, maxQueue int, keepWarm bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.MinWorkers, r.MaxWorkers, r.BackpressureEnabled, r.BackpressureMaxQueue, r.KeepWarm
}

func toInt(v interface{}) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case float64:
		return int(x), nil
	case string:
		return strconv.Atoi(x)
	default:
		return 0, errInvalidConfig
	}
}

func toBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	default:
		return false
	}
}

var errInvalidConfig = &configError{msg: "invalid config value"}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }
