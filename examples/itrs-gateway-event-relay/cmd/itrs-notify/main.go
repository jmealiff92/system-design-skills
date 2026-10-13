// Command itrs-notify is the program the Geneos Gateway forks per trigger
// (a "run a program" Effect). It builds the event envelope from the
// trigger's environment variables and POSTs it directly to the Event
// Management System (EMS) — no local relay daemon, no Unix-socket
// hand-off, no shared connection pool.
//
// That's a deliberate reversal from an earlier version of this design
// (see docs/design/itrs-gateway-event-script.md's revision history). The
// original 60,000/min figure turned out to be the *fleet-wide* aggregate
// across ~80 hosts, not what any single host sees: real per-host peak is
// ~835/min (~14/s) with the highest average around 14/min — two orders of
// magnitude below what justified a relay. At that rate, Little's Law
// alone settles it (N ≈ λ×W ≈ 14/s × 0.3s ≈ 4 concurrent in-flight even in
// the worst case), so there's no concurrent load to pool connections for,
// no backlog for a breaker to protect against, and nothing for a queue to
// buffer. See benchmark/BENCHMARK.md for the re-measured numbers.
//
// What doesn't change with volume: curl-without-a-timeout's failure mode
// (Test C in the benchmark) was never actually a case *for* a relay, it
// was a missing-timeout bug — fixed here directly with a bounded
// per-attempt timeout and a small retry budget, no second process needed.
// Built as a single static binary (CGO_ENABLED=0) for the same reason as
// before, unaffected by volume: ~80 Gateway hosts across regions can't be
// assumed to carry the same language runtime.
//
// Exit code is always 0 unless the dead-letter fallback itself fails
// (e.g. disk full) — a non-zero exit would make Geneos treat delivery as
// an effect failure, risking a Gateway-side effect-retry on top of the
// retry this program already does itself.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"itrsrelay/internal/spool"
	"itrsrelay/internal/uuid"
)

// Config is built once at the top of main(), after loading the optional
// env file — see loadConfig's doc comment for why this can't be a set of
// package-level `var x = os.Getenv(...)` initializers.
type Config struct {
	EMSURL         string
	EMSToken       string
	AttemptTimeout time.Duration
	MaxRetries     int
	DeadLetterFile string
}

// loadConfig reads configuration from the environment, with a file-based
// fallback for anything not already set.
//
// There's no long-running daemon anymore, so there's no systemd unit with
// an EnvironmentFile= to hold the EMS token — Geneos's own Effect
// configuration can set env vars directly, but that may not be the right
// place for a secret (Gateway setup is often plaintext XML, viewable by
// anyone with config access). So this also reads a plain KEY=VALUE file
// (default /etc/itrs-notify/env, Ansible-deployed at mode 0600 from
// vault) and fills in only the variables Geneos's own environment didn't
// already set — an explicit Effect-level override always wins.
//
// This must run as an explicit function at the top of main(), not as
// package-level `var x = os.Getenv(...)` initializers: Go runs all
// package-level variable initializers before any init() func or main(),
// so a package-level var reading os.Getenv would run before this file
// ever got a chance to populate the environment.
func loadConfig() Config {
	loadEnvFile(getEnv("ITRS_NOTIFY_ENV_FILE", "/etc/itrs-notify/env"))
	return Config{
		EMSURL:         os.Getenv("ITRS_NOTIFY_EMS_URL"),
		EMSToken:       os.Getenv("ITRS_NOTIFY_EMS_TOKEN"),
		AttemptTimeout: getEnvDuration("ITRS_NOTIFY_ATTEMPT_TIMEOUT_S", 150*time.Millisecond),
		MaxRetries:     getEnvInt("ITRS_NOTIFY_MAX_RETRIES", 1),
		DeadLetterFile: getEnv("ITRS_NOTIFY_DEAD_LETTER_FILE", "/var/spool/itrs-notify/dead_letter.jsonl"),
	}
}

// loadEnvFile sets os.Setenv for each KEY=VALUE line in path, skipping
// blank lines and lines starting with '#' — but only for keys not already
// present in the environment, so anything Geneos's Effect config set
// directly always takes precedence. A missing file is not an error (the
// common case when every value comes from the Effect's own environment).
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, already := os.LookupEnv(key); already {
			continue
		}
		_ = os.Setenv(key, strings.TrimSpace(value))
	}
}

func getEnv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func getEnvDuration(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return def
}

func getEnvInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// --- Geneos variable mapping ------------------------------------------------
// Best-effort names for the well-known Effect environment variables, based
// on ITRS Geneos's documented convention (underscore-prefixed, mirroring
// the rule-language accessors: rowname()/column()/value()/...). This
// sandbox's network policy blocked docs.itrsgroup.com / support.itrsgroup.com,
// so this table could not be re-verified against the live page at write
// time — verify against a real Gateway (a throwaway effect that runs
// `env > /tmp/env.dump` shows the real names) and correct this table
// before go-live. Nothing is lost in the meantime: every `_`-prefixed var
// is also captured verbatim into `attributes` below regardless of this
// table.
var varMap = map[string][]string{
	"severity":         {"_SEVERITY"},
	"headline":         {"_HEADLINE", "_VARIABLE"},
	"message":          {"_MESSAGE"},
	"gateway":          {"_GATEWAY", "_GATEWAY_NAME"},
	"probe":            {"_NETPROBE_HOST", "_PROBE"},
	"probe_port":       {"_NETPROBE_PORT"},
	"managed_entity":   {"_MANAGED_ENTITY", "_ENTITY"},
	"sampler":          {"_SAMPLER"},
	"sampler_type":     {"_SAMPLER_TYPE"},
	"dataview":         {"_DATAVIEW"},
	"row":              {"_ROWNAME", "_ROW"},
	"column":           {"_COLUMN"},
	"value":            {"_CELL", "_VALUE"},
	"rule":             {"_RULE"},
	"triggered_at_raw": {"_TIMESTAMP"},
}

func firstEnv(candidates ...string) (string, bool) {
	for _, name := range candidates {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// buildEnvelope turns the process's environment into the EMS-bound event
// envelope (design doc section 3a/3c).
func buildEnvelope() map[string]any {
	attrs := map[string]string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "_") {
			continue
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			attrs[k] = v
		}
	}

	envelope := map[string]any{
		"event_id":     uuid.V4(), // doubles as the EMS Idempotency-Key
		"source":       "geneos",
		"triggered_at": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		"attributes":   attrs, // forward-compatible safety net
	}
	for field, candidates := range varMap {
		if v, ok := firstEnv(candidates...); ok {
			envelope[field] = v
		}
	}
	return envelope
}

func backoffWithJitter(attempt int) time.Duration {
	base := 50 * time.Millisecond * time.Duration(uint(1)<<uint(attempt-1)) // 50ms, 100ms, ...
	return base + time.Duration(rand.Int63n(int64(base)+1))
}

// postOnce makes one bounded-timeout attempt at delivering the event.
// Returns (statusCode, err) — err is only a transport-level failure
// (timeout, connection refused, DNS, ...); a non-2xx response is returned
// as a normal statusCode for the caller to classify as permanent/transient.
func postOnce(cfg Config, client *http.Client, eventID string, payload []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.AttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.EMSURL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", eventID)
	if cfg.EMSToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.EMSToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// deliver runs the retry policy: up to cfg.MaxRetries extra attempts with
// jittered backoff, for transport errors and 429/5xx (transient); any
// other non-2xx is treated as a permanent rejection and not retried. Total
// worst case at the defaults (150ms timeout, 1 retry, ~50-100ms backoff)
// is well under the 500ms SLA.
func deliver(cfg Config, client *http.Client, eventID string, payload []byte) (delivered bool, reason string, attempts int) {
	for attempt := 1; ; attempt++ {
		status, err := postOnce(cfg, client, eventID, payload)
		if err != nil {
			if attempt > cfg.MaxRetries {
				return false, err.Error(), attempt
			}
			time.Sleep(backoffWithJitter(attempt))
			continue
		}
		if status == http.StatusOK || status == http.StatusAccepted {
			return true, "", attempt
		}
		if status == http.StatusTooManyRequests || status >= 500 {
			if attempt > cfg.MaxRetries {
				return false, fmt.Sprintf("http_%d", status), attempt
			}
			time.Sleep(backoffWithJitter(attempt))
			continue
		}
		// Permanent rejection (other 4xx): retrying wastes the budget and
		// the EMS will say no again.
		return false, fmt.Sprintf("http_%d", status), attempt
	}
}

func main() {
	start := time.Now()
	cfg := loadConfig()

	if cfg.EMSURL == "" {
		fmt.Fprintln(os.Stderr, "itrs-notify: ITRS_NOTIFY_EMS_URL is not set, cannot deliver; dropping event")
		os.Exit(0)
	}

	envelope := buildEnvelope()
	eventID, _ := envelope["event_id"].(string)
	payload, err := json.Marshal(envelope)
	if err != nil {
		// Should be unreachable (every value is a string or map[string]string),
		// but never let a marshal bug hang or crash the Gateway effect.
		fmt.Fprintf(os.Stderr, "itrs-notify: failed to encode envelope: %v\n", err)
		os.Exit(0)
	}

	// No connection pooling here on purpose: this process makes at most
	// cfg.MaxRetries+1 requests in its entire lifetime, then exits —
	// there's nothing to amortize a pool across. See the package doc for
	// why that trade-off is fine at this volume.
	client := &http.Client{}

	delivered, reason, attempts := deliver(cfg, client, eventID, payload)
	if !delivered {
		record := map[string]any{}
		for k, v := range envelope {
			record[k] = v
		}
		record["failure_reason"] = reason
		record["attempts"] = attempts
		if line, err := json.Marshal(record); err == nil {
			if err := spool.AtomicAppend(cfg.DeadLetterFile, spool.EncodeRecord(line)); err != nil {
				fmt.Fprintf(os.Stderr, "itrs-notify: dropped event %s: %v\n", eventID, err)
			}
		}
	}

	if os.Getenv("ITRS_NOTIFY_DEBUG") != "" {
		outcome := "dead_letter"
		if delivered {
			outcome = "delivered"
		}
		fmt.Fprintf(os.Stderr, "itrs-notify: event_id=%s outcome=%s attempts=%d wall_time_ms=%.2f\n",
			eventID, outcome, attempts, float64(time.Since(start).Microseconds())/1000)
	}
}
