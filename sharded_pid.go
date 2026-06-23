// Package memory - process-wide PID scheduler for ShardedFreeList.

package memory

import (
	"sync"
	"time"
)

const (
	shardedPIDInterval        = 100 * time.Millisecond
	shardedPIDKp              = 2.0
	shardedPIDKi              = 0.5
	shardedPIDIntegralLimit   = 100.0
	shardedPIDTargetFreeRatio = 0.20
	shardedPIDMaxThreshold    = hyalineK + 1
)

var defaultShardedFreeListPID = newSharedPIDController(shardedPIDInterval)

type sharedPIDController struct {
	mu       sync.Mutex
	entries  map[*ShardedFreeList]*sharedPIDEntry
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	running  bool
	stopping bool
}

type sharedPIDEntry struct {
	sfl      *ShardedFreeList
	integral float64
}

func newSharedPIDController(interval time.Duration) *sharedPIDController {
	return &sharedPIDController{
		entries:  make(map[*ShardedFreeList]*sharedPIDEntry),
		interval: interval,
	}
}

func (p *sharedPIDController) register(sfl *ShardedFreeList) {
	p.mu.Lock()
	for p.stopping {
		done := p.done
		p.mu.Unlock()
		<-done
		p.mu.Lock()
	}

	p.entries[sfl] = &sharedPIDEntry{sfl: sfl}
	if !p.running {
		p.startLocked()
	}
	p.mu.Unlock()
}

func (p *sharedPIDController) unregister(sfl *ShardedFreeList) {
	p.mu.Lock()
	delete(p.entries, sfl)

	if len(p.entries) == 0 && p.running && !p.stopping {
		stop := p.stop
		done := p.done
		p.stopping = true
		close(stop)
		p.mu.Unlock()

		<-done
		return
	}

	p.mu.Unlock()
}

func (p *sharedPIDController) startLocked() {
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	p.running = true
	go p.run(p.stop, p.done)
}

func (p *sharedPIDController) run(stop <-chan struct{}, done chan<- struct{}) {
	defer func() {
		p.mu.Lock()
		if p.done == done {
			p.stop = nil
			p.done = nil
			p.running = false
			p.stopping = false
		}
		p.mu.Unlock()
		close(done)
	}()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

func (p *sharedPIDController) tick() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.entries {
		entry.tick()
	}
}

func (e *sharedPIDEntry) tick() {
	stats := e.sfl.Stats()
	if stats.SlotSize == 0 || stats.Reserved == 0 {
		return
	}

	totalSlots := float64(stats.Reserved / stats.SlotSize)
	allocatedSlots := float64(stats.Allocated / stats.SlotSize)
	currentDepth := totalSlots - allocatedSlots
	targetDepth := totalSlots * shardedPIDTargetFreeRatio
	err := targetDepth - currentDepth

	e.integral += err
	if e.integral > shardedPIDIntegralLimit {
		e.integral = shardedPIDIntegralLimit
	} else if e.integral < -shardedPIDIntegralLimit {
		e.integral = -shardedPIDIntegralLimit
	}

	adjustment := shardedPIDKp*err + shardedPIDKi*e.integral
	newThreshold := float64(shardedPIDMaxThreshold) - adjustment

	var clamped uint64
	if newThreshold > float64(shardedPIDMaxThreshold) {
		clamped = shardedPIDMaxThreshold
	} else if newThreshold < 1 {
		clamped = 1
	} else {
		clamped = uint64(newThreshold)
	}

	e.sfl.hyHeader.threshold.Store(clamped)
}
