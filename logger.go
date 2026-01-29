package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// LogLevel representa o nível de log
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

// Logger estrutura de logging
type Logger struct {
	level  LogLevel
	prefix string
	logger *log.Logger
}

// NewLogger cria um novo logger
func NewLogger(level string) *Logger {
	l := &Logger{
		level:  parseLogLevel(level),
		logger: log.New(os.Stdout, "", 0),
	}
	return l
}

// parseLogLevel converte string para LogLevel
func parseLogLevel(level string) LogLevel {
	switch strings.ToLower(level) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// log formata e escreve a mensagem
func (l *Logger) log(level LogLevel, levelStr, format string, args ...interface{}) {
	if level < l.level {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	l.logger.Printf("[%s] %s: %s", timestamp, levelStr, msg)
}

// Debug loga mensagem de debug
func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LogLevelDebug, "DEBUG", format, args...)
}

// Info loga mensagem informativa
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LogLevelInfo, "INFO", format, args...)
}

// Warn loga mensagem de aviso
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LogLevelWarn, "WARN", format, args...)
}

// Error loga mensagem de erro
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LogLevelError, "ERROR", format, args...)
}

// WithPrefix retorna um logger com prefixo
func (l *Logger) WithPrefix(prefix string) *Logger {
	return &Logger{
		level:  l.level,
		prefix: prefix,
		logger: l.logger,
	}
}
