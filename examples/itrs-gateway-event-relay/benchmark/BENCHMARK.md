# Benchmark: do we need the relay at the real measured volume?

Revised after the real per-host numbers came in: **peak 835 events/min/host
(~14/s), highest average across hosts 14 events/min/host (~0.23/s)** — two
orders of magnitude below the ~1,000/s aggregate this design originally
assumed per host (that figure turned out to be the *fleet-wide* total
across ~80 hosts, not a per-host number). See
`docs/design/itrs-gateway-event-script.md`'s revision history for the
full reasoning; this file is the empirical check on that reasoning, not
just the arithmetic.

**Bottom line: no, the relay isn't needed at this volume.** `itrs-notify`
now POSTs directly to the EMS itself — bounded per-attempt timeout, a
small retry budget, a local dead-letter file on exhausted/permanent
failure — no daemon, no Unix-socket hand-off, no connection pool, no
circuit breaker, no queue. The three tests below check that decision
empirically rather than by arithmetic alone, and still keep the one
naive-script comparison that matters regardless of volume: a missing
timeout is a bug, not something that needed a relay to work around.

**Environment caveat.** Numbers came from a shared sandbox VM — don't cite
the absolute milliseconds in a stakeholder doc without re-running
(`run_all.sh`) on real Gateway hardware first. The *shape* (naive
degrades and eventually hangs; the hardened single binary stays bounded)
is what's structural, not the hardware artifact.

## Test A — latency across concurrency, healthy backend

Real per-host peak (14/s) implies, by Little's Law, an expected in-flight
concurrency around **N ≈ λ×W ≈ 14 × 0.3s ≈ 4** even at sustained peak.
Tested 1, 4, 14 (the actual measured range), then 50 and 140 as stress
margins (3.5× and 10× past the measured peak) to see where each approach
actually breaks, not just where the math says it should be fine.

| Concurrency | naive p50 | naive p99 | naive max | notify p50 | notify p99 | notify max |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 51ms | 51ms | 51ms | 11ms | 11ms | 11ms |
| 4 | 25ms | 26ms | 26ms | 11ms | 11ms | 11ms |
| 14 (= measured peak) | 72ms | 80ms | 80ms | 38ms | 56ms | 56ms |
| 50 (3.5× peak) | 146ms | **1,153ms** | 1,153ms | 90ms | 298ms | 298ms |
| 140 (10× peak) | 274ms | **1,398ms** | 1,436ms | 350ms | 481ms | 485ms |

**Finding:** across the entire *actually measured* range (1–14), both
approaches are fast and nowhere near the 500ms SLA, exactly as Little's
Law predicted — there's no concurrent load here for a pool or a queue to
help with. The gap only opens up once concurrency is pushed well past
anything measured: at 10× peak, naive's p99 already exceeds the SLA by
nearly 3×, while the single hardened binary (no pooling, no daemon — just
a bounded timeout) stays under it, with room to spare. The relay's old
job — keeping tail latency flat under heavy concurrency — isn't needed at
the real number, and the plain binary already has enough headroom to
survive a 10× surprise without it.

## Test B — TCP `TIME_WAIT`, sustained at the real peak rate

Fired the naive script at ~14/s for 10 seconds (140 invocations total) —
sustaining the measured *peak* rate for longer than a real peak likely
lasts, rather than the burst-all-at-once shape Test A already covers.

```
naive : client TIME_WAIT to :8950  before=0 after=140  delta=140
```

**Finding:** 140 sockets in `TIME_WAIT`, against a Linux ephemeral port
range commonly ~28,000–30,000 — under 0.5%. This was the original design's
loudest argument for a relay (at the *assumed* 1,000/s it projected to
~60,000 `TIME_WAIT` sockets, more than the whole port range); at the real
per-host rate it isn't a concern at all, with or without connection
pooling. (And this test already uses the worst-case assumption — sustained
peak for 10 whole seconds; the measured *average* is ~60× lower than the
peak, so even this is a pessimistic input.)

## Test C — an unresponsive backend (the one finding that doesn't change)

Same test as the earlier draft, same fixture (`fake_ems_hung.py`: accepts
the TCP connection, never reads or writes again). This finding isn't about
volume — it's about whether the trigger-side program has *any* bound on
how long it'll wait, and that's true at 1 trigger or 1,000.

```
naive (no timeout): 5 triggers -> all 5 curl+bash processes still
  BLOCKED after 8+ seconds (indefinitely, in production)

itrs-notify (bounded timeout + 1 retry, no relay): same 5 triggers
  wall-time: p50=399ms p99=405ms max=405ms (under the 500ms SLA)
  5/5 recorded in the dead-letter file with
    failure_reason="context deadline exceeded", attempts=2
  0 leftover OS processes
```

**Finding:** the fix was always a timeout on the HTTP call, not a second
long-running process. `itrs-notify` carries that fix directly now — no
relay required to get it.

## What this changes vs. the earlier draft

- **Drop:** `event-relay`, the Unix-socket hand-off protocol, the
  in-memory queue, the circuit breaker, connection pooling.
- **Keep, because they're not about volume:** a bounded per-attempt
  timeout + small retry budget (this test's Test C), safe JSON encoding
  (not string interpolation — see the note below), an `Idempotency-Key`
  per event, a local dead-letter file instead of silently dropping a
  failed delivery, and Go as the implementation language (the
  environment-consistency argument across ~80 regionally different hosts
  has nothing to do with request volume).
- **Revisit if a future number changes this:** see
  `docs/design/itrs-gateway-event-script.md` §8 for the specific
  threshold (sustained per-host rate, not a one-off burst) that would
  justify bringing the relay back.

## A secondary, independent finding: correctness

`itrs_notify_naive.sh` (kept only as the "what not to do" baseline here)
builds its JSON body by string interpolation. A headline or message
containing a `"` or `\` produces invalid JSON the EMS will reject
outright. `itrs-notify` uses `encoding/json` and never had this problem —
noted again here since it's unrelated to the volume question and would
matter even at the lowest possible trigger rate.

## Reproducing this

```bash
cd examples/itrs-gateway-event-relay
go build -o /tmp/bench/itrs-notify ./cmd/itrs-notify
cd benchmark
./run_all.sh    # runs Tests A, B, and C in sequence, printing each result
```

Test C's cleanup step force-kills the still-hung `curl` processes it just
proved are stuck; the resulting `line 32: <pid> Killed curl ...` lines in
the output are bash reporting that cleanup, not a test failure.
