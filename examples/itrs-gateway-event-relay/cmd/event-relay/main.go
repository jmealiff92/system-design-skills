// Command event-relay is the long-running daemon, one per Geneos Gateway
// host, that owns everything itrs-notify deliberately does NOT do: the
// pooled/keep-alive HTTP connection to the Event Management System (EMS),
// retries with backoff+jitter, a circuit breaker, backpressure, and a
// dead-letter file. See docs/design/itrs-gateway-event-script.md for the
// full reasoning (sections 4, 6, 7); this file is that design's section-4
// diagram made concrete.
//
// Data flow:
//
//	itrs-notify (many, short-lived) --UDS--> [bounded chan] --> worker pool --HTTPS(pooled)--> EMS
//	                                 \-on NACK/daemon-down-> disk spool -/  (drained back in, respecting backpressure)
//	                                                                          \-> exhausted retries / permanent 4xx -> DLQ file
//
// Built as a single static binary for the same reason as itrs-notify: 80
// Gateway hosts across regions is 80 places dependency drift can hide.
// Go's stdlib net/http already pools/reuses keep-alive connections, so
// unlike the Python prototype this replaces, the daemon needs zero
// third-party dependencies either — `go build` is the entire install step.
package main

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"itrsrelay/internal/spool"
)

const maxFrameBytes = 1 << 20 // 1 MiB — generous upper bound, guards a corrupt/hostile length prefix

func main() {
	cfg := loadConfig()

	queue := make(chan QueueItem, cfg.QueueMax)
	metrics := &Metrics{}
	breaker := NewCircuitBreaker(cfg.BreakerThreshold, cfg.BreakerOpenSeconds)
	client := newHTTPClient(cfg)

	if err := os.MkdirAll(filepath.Dir(cfg.SockPath), 0o755); err != nil {
		log.Fatalf("event-relay: cannot create socket directory: %v", err)
	}
	_ = os.Remove(cfg.SockPath) // stale socket from a prior crash
	ln, err := net.Listen("unix", cfg.SockPath)
	if err != nil {
		log.Fatalf("event-relay: cannot listen on %s: %v", cfg.SockPath, err)
	}
	if err := os.Chmod(cfg.SockPath, 0o666); err != nil {
		log.Fatalf("event-relay: cannot chmod %s: %v", cfg.SockPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var workers sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			worker(ctx, cfg, queue, client, breaker, metrics)
		}()
	}
	go spoolDrainer(ctx, cfg, queue)
	go metricsReporter(ctx, cfg, queue, breaker, metrics)
	go acceptLoop(ln, queue, metrics)

	log.Printf("event-relay listening on %s -> %s (workers=%d queue_max=%d)",
		cfg.SockPath, cfg.EMSURL, cfg.Workers, cfg.QueueMax)

	waitForShutdownSignal()
	log.Println("shutting down: draining in-flight queue to disk spool")

	cancel()       // tell workers/drainer/reporter to stop
	_ = ln.Close() // stop accepting new triggers

	// Give in-flight deliveries a grace period to finish normally before
	// we give up and persist whatever's left in memory to disk.
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Println("shutdown grace period elapsed; spooling remaining queue")
	}

	drained := 0
drainLoop:
	for {
		select {
		case item := <-queue:
			_ = spool.AtomicAppend(cfg.SpoolFile, spool.EncodeRecord(item.Raw))
			drained++
		default:
			break drainLoop
		}
	}
	if drained > 0 {
		log.Printf("spooled %d in-memory events on shutdown", drained)
	}
	client.CloseIdleConnections()
}

func newHTTPClient(cfg Config) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        cfg.Workers + 50,
		MaxIdleConnsPerHost: cfg.Workers,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   cfg.HTTP2,
	}
	return &http.Client{Transport: transport}
	// Per-attempt timeout is applied via context in deliver.go, not here,
	// so a slow attempt can't outlive the retry loop's own budget.
}

func acceptLoop(ln net.Listener, queue chan QueueItem, metrics *Metrics) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed during shutdown
		}
		go handleConn(conn, queue, metrics)
	}
}

// handleConn reads one length-prefixed JSON frame from an itrs-notify
// client and acks 'K' (queued) or 'F' (queue full — client falls back to
// its disk spool). See docs/design/itrs-gateway-event-script.md section 3b.
func handleConn(conn net.Conn, queue chan QueueItem, metrics *Metrics) {
	defer conn.Close()

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > maxFrameBytes {
		return
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return
	}

	select {
	case queue <- parseQueueItem(payload):
		metrics.accepted.Add(1)
		_, _ = conn.Write([]byte("K"))
	default:
		// Backpressure: reject rather than block the accept loop or grow
		// memory unbounded. The client falls back to the disk spool.
		_, _ = conn.Write([]byte("F"))
	}
}

func worker(ctx context.Context, cfg Config, queue chan QueueItem, client *http.Client, breaker *CircuitBreaker, metrics *Metrics) {
	for {
		select {
		case item := <-queue:
			deliver(ctx, cfg, client, breaker, metrics, item)
		case <-ctx.Done():
			return
		}
	}
}

func waitForShutdownSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}
