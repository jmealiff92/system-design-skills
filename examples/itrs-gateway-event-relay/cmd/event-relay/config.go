package main

import (
	"os"
	"strconv"
	"time"
)

// Config is env-var driven because this is a single-purpose daemon, not a
// multi-tenant service; a config file would be one more thing to keep in
// sync across 80 Gateway hosts for no benefit here. Sizing defaults come
// from docs/design/itrs-gateway-event-script.md section 2/4 (peak 1,000
// events/s; Little's Law N ~= 1,000 * 0.15s ~= 150 in-flight, + headroom).
type Config struct {
	SockPath  string
	SpoolFile string
	DLQFile   string

	EMSURL   string
	EMSToken string

	Workers  int
	QueueMax int
	HTTP2    bool

	AttemptTimeout time.Duration
	MaxRetries     int

	BreakerThreshold   int
	BreakerOpenSeconds time.Duration

	RotateInterval  time.Duration
	MetricsInterval time.Duration
}

func loadConfig() Config {
	emsURL := os.Getenv("ITRS_EMS_URL")
	if emsURL == "" {
		panic("ITRS_EMS_URL is required (the EMS ingest endpoint)")
	}
	return Config{
		SockPath:           getEnv("ITRS_RELAY_SOCK", "/var/run/itrs-event-relay/relay.sock"),
		SpoolFile:          getEnv("ITRS_RELAY_SPOOL", "/var/spool/itrs-event-relay/pending.jsonl"),
		DLQFile:            getEnv("ITRS_RELAY_DLQ", "/var/spool/itrs-event-relay/dead_letter.jsonl"),
		EMSURL:             emsURL,
		EMSToken:           os.Getenv("ITRS_EMS_TOKEN"),
		Workers:            getEnvInt("ITRS_RELAY_WORKERS", 200),
		QueueMax:           getEnvInt("ITRS_RELAY_QUEUE_MAXSIZE", 20000),
		HTTP2:              getEnv("ITRS_RELAY_HTTP2", "0") == "1",
		AttemptTimeout:     getEnvSeconds("ITRS_RELAY_ATTEMPT_TIMEOUT_S", 150*time.Millisecond),
		MaxRetries:         getEnvInt("ITRS_RELAY_MAX_RETRIES", 1),
		BreakerThreshold:   getEnvInt("ITRS_RELAY_BREAKER_THRESHOLD", 20),
		BreakerOpenSeconds: getEnvSeconds("ITRS_RELAY_BREAKER_OPEN_S", 5*time.Second),
		RotateInterval:     getEnvSeconds("ITRS_RELAY_ROTATE_INTERVAL_S", 2*time.Second),
		MetricsInterval:    getEnvSeconds("ITRS_RELAY_METRICS_INTERVAL_S", 10*time.Second),
	}
}

func getEnv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
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

func getEnvSeconds(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return def
}
