package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// Metrics: RED framing on the daemon (accepted/sent/dlq = rate+errors),
// USE framing on the two buffers (queue depth here; spool/DLQ file
// size+age is left to an external file-age check — see the design doc
// section 9 and the README's monitoring note). atomic.Int64 because
// multiple worker/handler goroutines update these concurrently.
type Metrics struct {
	accepted             atomic.Int64
	sentOK               atomic.Int64
	breakerOpenRespooled atomic.Int64
	dlq                  atomic.Int64
}

func metricsReporter(ctx context.Context, cfg Config, queue chan QueueItem, breaker *CircuitBreaker, m *Metrics) {
	ticker := time.NewTicker(cfg.MetricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf(
				"queue_depth=%d breaker=%s accepted=%d sent_ok=%d breaker_open_respooled=%d dlq=%d",
				len(queue), breaker.State(), m.accepted.Load(), m.sentOK.Load(),
				m.breakerOpenRespooled.Load(), m.dlq.Load(),
			)
		}
	}
}
