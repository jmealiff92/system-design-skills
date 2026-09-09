package main

import (
	"sync"
	"time"
)

// CircuitBreaker: closed -> open on N consecutive failures; open -> half-open
// after a cooldown, admitting exactly one probe call; probe success closes
// it, probe failure reopens it (resilience-failure: fail fast, single-probe
// recovery — stop a dead EMS from being hammered by every one of ~200
// worker goroutines at once).
//
// Unlike the Python prototype this replaces (a single-threaded asyncio
// event loop, where no locking was needed), Go's worker pool is real
// concurrency, so every method here is mutex-guarded.
type CircuitBreaker struct {
	mu                  sync.Mutex
	state               breakerState
	consecutiveFailures int
	openedAt            time.Time
	probeInFlight       bool
	failureThreshold    int
	openDuration        time.Duration
}

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

func (s breakerState) String() string {
	switch s {
	case open:
		return "open"
	case halfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

func NewCircuitBreaker(failureThreshold int, openDuration time.Duration) *CircuitBreaker {
	return &CircuitBreaker{state: closed, failureThreshold: failureThreshold, openDuration: openDuration}
}

// AllowRequest reports whether the caller may attempt the EMS call now. A
// true from the half-open state reserves the single probe slot for this
// caller until RecordSuccess/RecordFailure releases it.
func (b *CircuitBreaker) AllowRequest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == open {
		if time.Since(b.openedAt) < b.openDuration {
			return false
		}
		b.state = halfOpen
		b.probeInFlight = false
	}
	if b.state == halfOpen {
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	}
	return true // closed
}

func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.state = closed
	b.probeInFlight = false
}

func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures++
	switch b.state {
	case halfOpen:
		b.state = open
		b.openedAt = time.Now()
		b.probeInFlight = false
	case closed:
		if b.consecutiveFailures >= b.failureThreshold {
			b.state = open
			b.openedAt = time.Now()
		}
	}
}

func (b *CircuitBreaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}
