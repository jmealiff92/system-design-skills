#!/bin/bash
# run_all.sh — reproduces every test in BENCHMARK.md end to end.
#
# Prereq: build itrs-notify first (see BENCHMARK.md "Reproducing"), and
# point BIN_DIR at it if not /tmp/bench:
#   BIN_DIR=/path/to/binary ./run_all.sh
set -u
set +m  # silence job-control "[1] Done" noise from the background fan-out

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${BIN_DIR:-/tmp/bench}"
WORK="${WORK:-/tmp/itrs-benchmark-run}"
NOTIFY="$BIN_DIR/itrs-notify"
NAIVE="$HERE/itrs_notify_naive.sh"
COUNT_TW="$HERE/count_time_wait.py"
PCTL="$HERE/percentiles.py"

if [[ ! -x "$NOTIFY" ]]; then
  echo "error: build the binary first, e.g.:" >&2
  echo "  go build -o $NOTIFY ../cmd/itrs-notify" >&2
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
echo "# Test A: latency at the REAL measured concurrency, healthy backend"
echo "# (peak 835/min/host = ~14/s; highest average 14/min/host = ~0.23/s —"
echo "#  i.e. N (Little's Law) is ~4 even at sustained peak. Testing up to"
echo "#  10x that burst as a stress margin, well past anything measured.)"
echo "############################################################"
start_bg python3 "$HERE/fake_ems_healthy.py" 8940
sleep 0.5

time_one_naive() {
  local start end
  start=$(date +%s%N)
  EMS_URL="http://127.0.0.1:8940/api/v1/events" LOG_FILE="$WORK/naive.log" bash "$NAIVE"
  end=$(date +%s%N); echo "scale=3; ($end - $start)/1000000" | bc
}
time_one_notify() {
  local start end
  start=$(date +%s%N)
  ITRS_NOTIFY_EMS_URL="http://127.0.0.1:8940/api/v1/events" \
    ITRS_NOTIFY_DEAD_LETTER_FILE="$WORK/dlq_a.jsonl" "$NOTIFY"
  end=$(date +%s%N); echo "scale=3; ($end - $start)/1000000" | bc
}

for C in 1 4 14 50 140; do
  echo "--- concurrency=$C : naive shell+curl (no timeout) ---"
  rm -f "$WORK/naive_$C.times"
  for i in $(seq 1 "$C"); do ( time_one_naive >> "$WORK/naive_$C.times" ) & done
  wait > /dev/null 2>&1
  python3 "$PCTL" < "$WORK/naive_$C.times"

  echo "--- concurrency=$C : itrs-notify (hardened single binary, no relay) ---"
  rm -f "$WORK/notify_$C.times"
  for i in $(seq 1 "$C"); do ( time_one_notify >> "$WORK/notify_$C.times" ) & done
  wait > /dev/null 2>&1
  python3 "$PCTL" < "$WORK/notify_$C.times"
done

cleanup; PIDS=()
sleep 1

echo
echo "############################################################"
echo "# Test B: TCP TIME_WAIT at the real PEAK rate (worst case: 13.9/s"
echo "# sustained for a full 60s, which the real traffic rarely does —"
echo "# the measured average is ~60x lower)"
echo "############################################################"
start_bg python3 "$HERE/fake_ems_healthy.py" 8950
sleep 0.5

BEFORE=$(python3 "$COUNT_TW" 8950 --client)
# 14/s for 10s ~= the peak rate, sustained briefly, rather than firing
# everything at once (which would overstate instantaneous concurrency
# Test A already covers).
for i in $(seq 1 10); do
  for j in $(seq 1 14); do
    ( EMS_URL="http://127.0.0.1:8950/api/v1/events" LOG_FILE="$WORK/naive_tw.log" bash "$NAIVE" ) &
  done
  sleep 1
done
wait > /dev/null 2>&1
AFTER=$(python3 "$COUNT_TW" 8950 --client)
echo "naive (no timeout) : client TIME_WAIT to :8950  before=$BEFORE after=$AFTER delta=$((AFTER-BEFORE))  (140 invocations @ ~14/s)"
echo "(expected: ~140, nowhere near a ~28-30k ephemeral port range either way)"

cleanup; PIDS=()
sleep 1

echo
echo "############################################################"
echo "# Test C: unresponsive backend (accepts, never responds) — the one"
echo "# finding that doesn't change with volume: a missing timeout, not"
echo "# a case for a relay."
echo "############################################################"
start_bg python3 "$HERE/fake_ems_hung.py" 8951
sleep 0.5

echo "--- naive (no timeout): 5 triggers against the hung backend ---"
for i in $(seq 1 5); do
  ( EMS_URL="http://127.0.0.1:8951/api/v1/events" LOG_FILE="$WORK/naive_hang.log" bash "$NAIVE" ) &
done
disown -a 2>/dev/null
sleep 8
echo "still-blocked curl processes after 8s: $(ps aux | grep -c 'curl -s -X POST http://127.0.0.1:8951')"
pkill -9 -f "curl -s -X POST http://127.0.0.1:8951" 2>/dev/null
pkill -9 -f itrs_notify_naive.sh 2>/dev/null

echo "--- itrs-notify (bounded timeout+retry, no relay): same hung backend ---"
rm -f "$WORK/notify_hang.times"
for i in $(seq 1 5); do
  ( start=$(date +%s%N)
    ITRS_NOTIFY_EMS_URL="http://127.0.0.1:8951/api/v1/events" \
      ITRS_NOTIFY_DEAD_LETTER_FILE="$WORK/dlq_c.jsonl" "$NOTIFY"
    end=$(date +%s%N)
    echo "scale=2; ($end-$start)/1000000" | bc >> "$WORK/notify_hang.times" ) &
done
wait > /dev/null 2>&1
python3 "$PCTL" < "$WORK/notify_hang.times"
echo "dead-letter entries recorded: $(wc -l < "$WORK/dlq_c.jsonl" 2>/dev/null || echo 0)"
echo "leftover itrs-notify processes: $(ps aux | grep -c '[i]trs-notify')"

echo
echo "Done. Per-run artifacts (logs, raw timing samples, dead-letter files) left in $WORK for inspection."
trap - EXIT
cleanup
