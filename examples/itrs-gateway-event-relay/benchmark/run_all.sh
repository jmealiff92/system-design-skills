#!/bin/bash
# run_all.sh — reproduces every test in BENCHMARK.md end to end.
#
# Prereqs: build the two binaries first (see BENCHMARK.md "Reproducing"),
# and point BIN_DIR at them if not /tmp/bench:
#   BIN_DIR=/path/to/binaries ./run_all.sh
set -u
set +m  # silence job-control "[1] Done" noise from the background fan-out

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${BIN_DIR:-/tmp/bench}"
WORK="${WORK:-/tmp/itrs-benchmark-run}"
NOTIFY="$BIN_DIR/itrs-notify"
RELAY="$BIN_DIR/event-relay"
NAIVE="$HERE/itrs_notify_naive.sh"
COUNT_TW="$HERE/count_time_wait.py"
PCTL="$HERE/percentiles.py"

if [[ ! -x "$NOTIFY" || ! -x "$RELAY" ]]; then
  echo "error: build the binaries first, e.g.:" >&2
  echo "  go build -o $NOTIFY ../cmd/itrs-notify" >&2
  echo "  go build -o $RELAY ../cmd/event-relay" >&2
  exit 1
fi

rm -rf "$WORK" && mkdir -p "$WORK"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]}"; do kill -9 "$pid" 2>/dev/null; done
}
trap cleanup EXIT

start_bg() {
  "$@" > "$WORK/$(basename "$1" | tr -d '/').out" 2>&1 &
  PIDS+=("$!")
  disown
}

echo "############################################################"
echo "# Test A: p99 latency under concurrency, healthy backend"
echo "############################################################"
start_bg python3 "$HERE/fake_ems_healthy.py" 8940
sleep 0.5
ITRS_EMS_URL=http://127.0.0.1:8940/api/v1/events \
  ITRS_RELAY_SOCK="$WORK/relay_a.sock" ITRS_RELAY_SPOOL="$WORK/pending_a.jsonl" \
  ITRS_RELAY_DLQ="$WORK/dlq_a.jsonl" ITRS_RELAY_METRICS_INTERVAL_S=3600 \
  start_bg "$RELAY"
sleep 0.5

time_one_naive() {
  local start end
  start=$(date +%s%N)
  EMS_URL="http://127.0.0.1:8940/api/v1/events" LOG_FILE="$WORK/naive.log" bash "$NAIVE"
  end=$(date +%s%N); echo "scale=3; ($end - $start)/1000000" | bc
}
time_one_go() {
  local start end
  start=$(date +%s%N)
  ITRS_RELAY_SOCK="$WORK/relay_a.sock" "$NOTIFY"
  end=$(date +%s%N); echo "scale=3; ($end - $start)/1000000" | bc
}

for C in 10 50 200 500 1000; do
  echo "--- concurrency=$C : naive shell+curl ---"
  rm -f "$WORK/naive_$C.times"
  for i in $(seq 1 "$C"); do ( time_one_naive >> "$WORK/naive_$C.times" ) & done
  wait > /dev/null 2>&1
  python3 "$PCTL" < "$WORK/naive_$C.times"

  echo "--- concurrency=$C : itrs-notify (Go) ---"
  rm -f "$WORK/go_$C.times"
  for i in $(seq 1 "$C"); do ( time_one_go >> "$WORK/go_$C.times" ) & done
  wait > /dev/null 2>&1
  python3 "$PCTL" < "$WORK/go_$C.times"
done

cleanup; PIDS=()
sleep 1

echo
echo "############################################################"
echo "# Test B: TCP connection reuse (TIME_WAIT accumulation)"
echo "############################################################"
start_bg python3 "$HERE/fake_ems_healthy.py" 8950
sleep 0.5
ITRS_EMS_URL=http://127.0.0.1:8950/api/v1/events \
  ITRS_RELAY_SOCK="$WORK/relay_b.sock" ITRS_RELAY_SPOOL="$WORK/pending_b.jsonl" \
  ITRS_RELAY_DLQ="$WORK/dlq_b.jsonl" ITRS_RELAY_METRICS_INTERVAL_S=3600 \
  start_bg "$RELAY"
sleep 0.5

BEFORE=$(python3 "$COUNT_TW" 8950 --client)
for i in $(seq 1 300); do
  ( EMS_URL="http://127.0.0.1:8950/api/v1/events" LOG_FILE="$WORK/naive_tw.log" bash "$NAIVE" ) &
done
wait > /dev/null 2>&1
AFTER=$(python3 "$COUNT_TW" 8950 --client)
echo "naive : client TIME_WAIT to :8950  before=$BEFORE after=$AFTER delta=$((AFTER-BEFORE))  (300 invocations)"

BEFORE=$(python3 "$COUNT_TW" 8950 --client)
for i in $(seq 1 300); do ( ITRS_RELAY_SOCK="$WORK/relay_b.sock" "$NOTIFY" ) & done
wait > /dev/null 2>&1
AFTER=$(python3 "$COUNT_TW" 8950 --client)
echo "Go    : client TIME_WAIT to :8950  before=$BEFORE after=$AFTER delta=$((AFTER-BEFORE))  (300 invocations)"

cleanup; PIDS=()
sleep 1

echo
echo "############################################################"
echo "# Test C: unresponsive backend (accepts, never responds)"
echo "############################################################"
start_bg python3 "$HERE/fake_ems_hung.py" 8951
sleep 0.5

echo "--- naive: 20 concurrent triggers against the hung backend ---"
for i in $(seq 1 20); do
  ( EMS_URL="http://127.0.0.1:8951/api/v1/events" LOG_FILE="$WORK/naive_hang.log" bash "$NAIVE" ) &
done
disown -a 2>/dev/null
sleep 8
echo "still-blocked curl processes after 8s: $(ps aux | grep -c 'curl -s -X POST http://127.0.0.1:8951')"
pkill -9 -f "curl -s -X POST http://127.0.0.1:8951" 2>/dev/null
pkill -9 -f itrs_notify_naive.sh 2>/dev/null

echo "--- Go: same 20 triggers against the same hung backend ---"
ITRS_EMS_URL=http://127.0.0.1:8951/api/v1/events \
  ITRS_RELAY_SOCK="$WORK/relay_c.sock" ITRS_RELAY_SPOOL="$WORK/pending_c.jsonl" \
  ITRS_RELAY_DLQ="$WORK/dlq_c.jsonl" ITRS_RELAY_METRICS_INTERVAL_S=2 \
  start_bg "$RELAY"
sleep 0.5
rm -f "$WORK/go_hang.times"
for i in $(seq 1 20); do
  ( start=$(date +%s%N); ITRS_RELAY_SOCK="$WORK/relay_c.sock" "$NOTIFY"; end=$(date +%s%N)
    echo "scale=2; ($end-$start)/1000000" | bc >> "$WORK/go_hang.times" ) &
done
wait > /dev/null 2>&1
python3 "$PCTL" < "$WORK/go_hang.times"
sleep 6
echo "event-relay's own state (breaker + DLQ) against the hung backend:"
tail -3 "$WORK"/*event-relay*.out 2>/dev/null

echo
echo "Done. Per-run artifacts (logs, raw timing samples, DLQ files) left in $WORK for inspection."
trap - EXIT
cleanup
