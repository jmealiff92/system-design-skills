package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"itrsrelay/internal/spool"
)

// QueueItem is what flows through the bounded channel between the UDS
// accept loop / spool drainer and the worker pool. Raw is kept as the
// exact original bytes (no re-marshal) so it can be forwarded to the EMS,
// or re-spooled, byte-for-byte.
type QueueItem struct {
	Raw     json.RawMessage
	EventID string
}

type idOnly struct {
	EventID string `json:"event_id"`
}

func parseQueueItem(payload []byte) QueueItem {
	raw := make(json.RawMessage, len(payload))
	copy(raw, payload)
	var id idOnly
	_ = json.Unmarshal(payload, &id) // best-effort; empty Idempotency-Key beats crashing on a malformed frame
	return QueueItem{Raw: raw, EventID: id.EventID}
}

func backoffWithJitter(attempt int) time.Duration {
	base := 50 * time.Millisecond * time.Duration(uint(1)<<uint(attempt-1)) // 50ms, 100ms, 200ms, ...
	return base + time.Duration(rand.Int63n(int64(base)+1))
}

func appendDLQ(cfg Config, item QueueItem, reason string, attempts int) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(item.Raw, &record); err != nil {
		record = map[string]json.RawMessage{}
	}
	if reasonJSON, err := json.Marshal(reason); err == nil {
		record["failure_reason"] = reasonJSON
	}
	if attemptsJSON, err := json.Marshal(attempts); err == nil {
		record["attempts"] = attemptsJSON
	}
	out, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = spool.AtomicAppend(cfg.DLQFile, spool.EncodeRecord(out))
}

// deliver applies the retry/circuit-breaker policy for one event and posts
// it to the EMS. See docs/design/itrs-gateway-event-script.md section 4/6
// for why each piece here (breaker check, per-attempt timeout, capped
// retry+jitter, DLQ on permanent/exhausted failure) is load-bearing.
func deliver(ctx context.Context, cfg Config, client *http.Client, breaker *CircuitBreaker, metrics *Metrics, item QueueItem) {
	if !breaker.AllowRequest() {
		// EMS looks down: don't spend a worker slot retrying into it.
		// Re-spool so the drainer retries this once the breaker's cooldown
		// elapses (naturally rate-limited by the rotate interval, not a
		// tight loop) instead of dropping the event.
		_ = spool.AtomicAppend(cfg.SpoolFile, spool.EncodeRecord(item.Raw))
		metrics.breakerOpenRespooled.Add(1)
		return
	}

	for attempt := 1; ; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, cfg.AttemptTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.EMSURL, bytes.NewReader(item.Raw))
		if err != nil {
			cancel()
			appendDLQ(cfg, item, err.Error(), attempt)
			metrics.dlq.Add(1)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", item.EventID)
		if cfg.EMSToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.EMSToken)
		}

		resp, err := client.Do(req)
		cancel()
		if err != nil {
			breaker.RecordFailure()
			if attempt > cfg.MaxRetries {
				appendDLQ(cfg, item, err.Error(), attempt)
				metrics.dlq.Add(1)
				return
			}
			sleepOrDone(ctx, backoffWithJitter(attempt))
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection returns to the pool
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
			breaker.RecordSuccess()
			metrics.sentOK.Add(1)
			return
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			breaker.RecordFailure() // transient -> retry, then DLQ
			if attempt > cfg.MaxRetries {
				appendDLQ(cfg, item, fmt.Sprintf("http_%d", resp.StatusCode), attempt)
				metrics.dlq.Add(1)
				return
			}
			sleepOrDone(ctx, backoffWithJitter(attempt))
			continue
		default:
			// Any other 4xx is a permanent rejection: retrying it wastes
			// the attempt budget and the EMS will say no again.
			appendDLQ(cfg, item, fmt.Sprintf("http_%d", resp.StatusCode), attempt)
			metrics.dlq.Add(1)
			return
		}
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
