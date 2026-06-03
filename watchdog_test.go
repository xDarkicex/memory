package memory

import (
	"sync"
	"testing"
	"time"
)

func TestNewWatchdog(t *testing.T) {
	called := make(chan MemStats, 1)
	wd := NewWatchdog(0, func(s MemStats) {
		select {
		case called <- s:
		default:
		}
	})
	if wd == nil {
		t.Fatal("NewWatchdog returned nil")
	}
	if wd.threshold != 0 {
		t.Errorf("threshold = %d, want 0", wd.threshold)
	}
	if wd.stop == nil {
		t.Fatal("stop channel is nil")
	}
}

func TestWatchdogStartStop(t *testing.T) {
	var mu sync.Mutex
	var fired bool
	wd := NewWatchdog(0, func(s MemStats) {
		mu.Lock()
		fired = true
		mu.Unlock()
	})

	wd.Start()

	// Wait for at least one tick (1s) plus a small buffer.
	time.Sleep(1200 * time.Millisecond)
	wd.Stop()

	mu.Lock()
	triggered := fired
	mu.Unlock()

	if !triggered {
		t.Error("callback was not triggered within 1.2s with threshold=0")
	}
}

func TestWatchdogStopIdempotent(t *testing.T) {
	wd := NewWatchdog(^uint64(0), func(s MemStats) {}) // never fires

	wd.Start()
	wd.Stop()
	// Second stop should not panic (sync.Once)
	wd.Stop()
}

func TestWatchdogBelowThreshold(t *testing.T) {
	var mu sync.Mutex
	var fired bool
	// Set threshold impossibly high so callback never fires.
	wd := NewWatchdog(^uint64(0), func(s MemStats) {
		mu.Lock()
		fired = true
		mu.Unlock()
	})

	wd.Start()
	time.Sleep(100 * time.Millisecond)
	wd.Stop()

	mu.Lock()
	triggered := fired
	mu.Unlock()

	if triggered {
		t.Error("callback fired but threshold was impossibly high")
	}
}

func TestRegisterMemoryPressureCallback(t *testing.T) {
	var mu sync.Mutex
	var count int
	stop := RegisterMemoryPressureCallback(0, func(s MemStats) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	time.Sleep(1200 * time.Millisecond)
	stop()

	mu.Lock()
	c := count
	mu.Unlock()

	if c == 0 {
		t.Error("callback never fired")
	}
}

func TestRegisterMemoryPressureCallbackReplacement(t *testing.T) {
	var mu sync.Mutex
	var firstFired, secondFired bool

	stop1 := RegisterMemoryPressureCallback(0, func(s MemStats) {
		mu.Lock()
		firstFired = true
		mu.Unlock()
	})

	// Replace with a second callback.
	stop2 := RegisterMemoryPressureCallback(0, func(s MemStats) {
		mu.Lock()
		secondFired = true
		mu.Unlock()
	})

	time.Sleep(1200 * time.Millisecond)

	stop2()
	_ = stop1

	mu.Lock()
	f1, f2 := firstFired, secondFired
	mu.Unlock()

	if f1 {
		t.Error("first callback fired after replacement — should have been stopped")
	}
	if !f2 {
		t.Error("second callback never fired after replacement")
	}
}
