// Command itrs-notify is the program the Geneos Gateway forks per trigger
// (a "run a program" Effect). At the design's target peak — 60,000
// triggers/min = 1,000/s, see docs/design/itrs-gateway-event-script.md —
// this process is spawned up to ~1,000 times a second across up to 80
// Gateway hosts, so it does the least possible work and NEVER talks to
// the network itself:
//
//	read env vars -> build a small JSON envelope -> hand off locally -> exit
//
// The actual HTTP POST to the Event Management System is owned by the
// long-running event-relay daemon (one per Gateway host), which keeps a
// pooled/keep-alive connection, retries, and a circuit breaker. That split
// is what keeps this program's own wall-time low and independent of the
// EMS's latency or availability.
//
// Built as a single static binary (CGO_ENABLED=0) specifically because 80
// Gateway hosts across regions cannot be assumed to carry the same
// language runtime — see the design doc's "Choosing a language" note. Copy
// this one file to every host; nothing else to install, no interpreter
// version or glibc ABI to match.
//
// Exit code is always 0 unless the disk-spool fallback itself fails (e.g.
// disk full) — a non-zero exit would make Geneos treat delivery as an
// effect failure, and at this trigger rate a Gateway-side effect-retry
// policy would turn a slow EMS into a retry storm on top of the one this
// design already avoids downstream.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"itrsrelay/internal/spool"
	"itrsrelay/internal/uuid"
)

// --- configuration (env-overridable; no config file needed for one binary) -
var (
	relaySock      = getEnv("ITRS_RELAY_SOCK", "/var/run/itrs-event-relay/relay.sock")
	spoolFile      = getEnv("ITRS_RELAY_SPOOL", "/var/spool/itrs-event-relay/pending.jsonl")
	connectTimeout = getEnvDuration("ITRS_RELAY_CONNECT_TIMEOUT_S", 100*time.Millisecond)
	ackTimeout     = getEnvDuration("ITRS_RELAY_ACK_TIMEOUT_S", 150*time.Millisecond)
)

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

// tryHandoffToDaemon is a best-effort local hand-off to the relay daemon
// over a Unix socket. Returns true only on an explicit 'K' (queued) ack
// from the daemon within budget; any failure (no daemon, socket full,
// timeout, NACK) returns false so the caller falls back to the disk spool.
// This call must never block for long: the timeouts here are the whole
// reason this program's wall-time stays low regardless of what the EMS is
// doing.
func tryHandoffToDaemon(payload []byte) bool {
	conn, err := net.DialTimeout("unix", relaySock, connectTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()

	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))

	_ = conn.SetWriteDeadline(time.Now().Add(connectTimeout))
	if _, err := conn.Write(header); err != nil {
		return false
	}
	if _, err := conn.Write(payload); err != nil {
		return false
	}

	_ = conn.SetReadDeadline(time.Now().Add(ackTimeout))
	ack := make([]byte, 1)
	n, err := conn.Read(ack)
	return err == nil && n == 1 && ack[0] == 'K'
}

func main() {
	start := time.Now()

	envelope := buildEnvelope()
	payload, err := json.Marshal(envelope)
	if err != nil {
		// Should be unreachable (every value is a string or map[string]string),
		// but never let a marshal bug hang or crash the Gateway effect.
		fmt.Fprintf(os.Stderr, "itrs-notify: failed to encode envelope: %v\n", err)
		os.Exit(0)
	}

	delivered := tryHandoffToDaemon(payload)
	if !delivered {
		if err := spool.AtomicAppend(spoolFile, spool.EncodeRecord(payload)); err != nil {
			// Truly out of options (e.g. disk full) — drop, don't hang the
			// Gateway effect on it, but say so loudly for the on-call.
			fmt.Fprintf(os.Stderr, "itrs-notify: dropped event %v: %v\n", envelope["event_id"], err)
			os.Exit(0)
		}
	}

	if os.Getenv("ITRS_NOTIFY_DEBUG") != "" {
		via := "spool"
		if delivered {
			via = "daemon"
		}
		fmt.Fprintf(os.Stderr, "itrs-notify: event_id=%v via=%s wall_time_ms=%.2f\n",
			envelope["event_id"], via, float64(time.Since(start).Microseconds())/1000)
	}
}
