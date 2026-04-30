// Package memory — Watchdog: heap pressure monitor.
//
// Provides a process-wide memory pressure watchdog that monitors Go heap
// metrics (HeapInuse), not the off-heap mmap'd memory managed by this package.
// When HeapInuse exceeds the configured threshold, the callback fires.

package memory

import (
	"sync"
	"sync/atomic"
	"time"
)

// Watchdog monitors memory pressure and triggers callbacks.
// Singleton with CAS-based replacement.
var globalWatchdog atomic.Pointer[Watchdog]

// Watchdog monitors system memory pressure.
type Watchdog struct {
	threshold uint64
	action    func(MemStats)
	stop      chan struct{}
	stopOnce  sync.Once
}

// NewWatchdog creates a new memory watchdog.
func NewWatchdog(threshold uint64, action func(MemStats)) *Watchdog {
	return &Watchdog{
		threshold: threshold,
		action:    action,
		stop:      make(chan struct{}),
	}
}

// Start begins memory monitoring.
func (w *Watchdog) Start() {
	go w.run()
}

// Stop stops monitoring safely - idempotent via sync.Once.
func (w *Watchdog) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

func (w *Watchdog) run() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			stats := ReadMemStats()
			if stats.Used > w.threshold {
				w.action(stats)
			}
		}
	}
}

// RegisterMemoryPressureCallback sets the threshold callback.
// Uses actual CAS loop for atomic watchdog replacement.
// Returns a stop function to cleanly shut down the watchdog.
func RegisterMemoryPressureCallback(threshold uint64, fn func(MemStats)) func() {
	wd := NewWatchdog(threshold, fn)

	// CAS loop for atomic replacement
	for {
		old := globalWatchdog.Load()

		// Try to atomically replace old with new
		if globalWatchdog.CompareAndSwap(old, wd) {
			if old != nil {
				old.Stop()
			}
			break
		}
		// CAS failed: another goroutine replaced it, retry
	}

	wd.Start()
	return wd.Stop
}
