package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// spoolDrainer periodically rotates the live spool file out from under new
// writers (os.Rename is atomic; concurrent appenders just recreate the
// file on their next O_CREATE|O_APPEND open) and replays each rotated file
// into the queue. Sending on `queue` (not a non-blocking send) is
// deliberate: it blocks when the queue is full, which *is* the
// backpressure — replay can never outrun live capacity. Runs once
// immediately on startup for crash recovery, then on RotateInterval.
func spoolDrainer(ctx context.Context, cfg Config, queue chan QueueItem) {
	drainOnce(ctx, cfg, queue)
	ticker := time.NewTicker(cfg.RotateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drainOnce(ctx, cfg, queue)
		}
	}
}

func drainOnce(ctx context.Context, cfg Config, queue chan QueueItem) {
	matches, _ := filepath.Glob(cfg.SpoolFile + ".*.replay")
	sort.Strings(matches) // oldest first (timestamp-suffixed names)
	if rotated := rotateSpool(cfg.SpoolFile); rotated != "" {
		matches = append(matches, rotated)
	}
	for _, path := range matches {
		drainFile(ctx, path, queue)
	}
}

func rotateSpool(spoolFile string) string {
	info, err := os.Stat(spoolFile)
	if err != nil || info.Size() == 0 {
		return ""
	}
	rotated := fmt.Sprintf("%s.%d.replay", spoolFile, time.Now().UnixNano())
	if err := os.Rename(spoolFile, rotated); err != nil {
		return ""
	}
	return rotated
}

func drainFile(ctx context.Context, path string, queue chan QueueItem) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		select {
		case queue <- parseQueueItem(line): // blocks -> backpressure
		case <-ctx.Done():
			// Shutting down mid-replay: leave the rest of this file where
			// it is rather than half-consume it; safest is to not delete
			// it below in that case.
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("spool drainer: error reading %s: %v", path, err)
		return // don't delete a file we couldn't fully read
	}
	_ = os.Remove(path)
}
