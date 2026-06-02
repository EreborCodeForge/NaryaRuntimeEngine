package main

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var errno syscall.Errno
		if errors.As(opErr.Err, &errno) {
			return errno == syscall.EADDRINUSE
		}
	}
	return false
}

const unixBindRetries = 10

func (p *WorkerPool) listenUnix(sockPath string) (net.Listener, error) {
	var lastErr error

	for attempt := 0; attempt < unixBindRetries; attempt++ {
		if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
			p.logger.Warn("Error removing old socket %s: %v", sockPath, err)
		}

		listener, err := net.Listen("unix", sockPath)
		if err == nil {
			if chmodErr := os.Chmod(sockPath, 0600); chmodErr != nil {
				listener.Close()
				return nil, chmodErr
			}
			return listener, nil
		}

		lastErr = err
		if !isAddrInUse(err) {
			return nil, err
		}

		atomicAddSpawnBindError(p)
		if p.logger != nil {
			p.logger.Debug("UDS bind retry %d/%d for %s: %v", attempt+1, unixBindRetries, sockPath, err)
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}

	atomicAddSpawnBindError(p)
	return nil, lastErr
}

func atomicAddSpawnBindError(p *WorkerPool) {
	if p != nil {
		atomic.AddInt64(&p.spawnBindErrorsTotal, 1)
	}
}
