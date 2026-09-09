#!/bin/bash
# itrs_notify_naive.sh — a representative reconstruction of the pattern this
# design replaces: one curl POST per Geneos Effect invocation, no timeout,
# no retry, no connection reuse. Modeled on the shape of ITRS's own
# (now-deprecated) pagerduty-integration script and the common "just curl
# it from the effect" approach. Used only as a load-test subject in
# ../benchmark/ — see BENCHMARK.md for the numbers this produces and why
# they're the case against this pattern, not a component of the shipped
# design.
set -u

EMS_URL="${EMS_URL:-http://127.0.0.1:8940/api/v1/events}"
LOG_FILE="${LOG_FILE:-/tmp/itrs-notify-naive.log}"

# String-interpolated JSON: quick to write, and it's also a correctness bug
# (a headline/message containing a `"` or `\` produces invalid JSON the EMS
# will reject) — noted in BENCHMARK.md as a second, independent finding
# alongside the performance ones.
PAYLOAD=$(cat <<JSON
{"severity":"${_SEVERITY:-}","headline":"${_HEADLINE:-}","managed_entity":"${_MANAGED_ENTITY:-}","sampler":"${_SAMPLER:-}","dataview":"${_DATAVIEW:-}","row":"${_ROWNAME:-}","column":"${_COLUMN:-}","value":"${_CELL:-}","gateway":"${_GATEWAY:-}","triggered_at":"$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"}
JSON
)

# No --max-time / --connect-timeout: curl's own defaults apply, and curl
# has NO default response-read timeout at all — a backend that accepts the
# TCP connection and then never replies hangs this process indefinitely.
# No retry: a dropped connection or non-2xx response is silently logged
# and the event is gone. Every invocation is a fresh curl process, so
# there's no TLS/TCP connection reuse across triggers either.
curl -s -X POST "$EMS_URL" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD" >> "$LOG_FILE" 2>&1
