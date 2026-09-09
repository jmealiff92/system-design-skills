# Benchmark: why the per-effect shell script is the wrong design

Empirical evidence for the argument in `docs/design/itrs-gateway-event-script.md`
§2 — not "shell is slow" as an assertion, but three specific, reproducible
failure shapes, measured against a representative reconstruction of the
current approach (`itrs_notify_naive.sh` — no real production script was
available to this session; see that file's header for what it models and
why it's a fair stand-in). All three tests compare it directly against
this design's `itrs-notify` + `event-relay`, under identical load.

**Environment caveat.** These numbers came from a shared sandbox VM, not
the target Gateway hardware — don't cite the absolute millisecond values in
a stakeholder doc without re-running on (or close to) real Gateway
hardware first (`run_all.sh` reproduces every test below). What will not
change on different hardware is the *shape* of each result: unbounded vs.
bounded resource growth, and hang vs. no-hang under a stuck backend — those
are structural, not a hardware artifact.

## Test A — p99 latency under concurrency, healthy backend

Fired C concurrent invocations of each approach against a perfectly
healthy, zero-latency, localhost stand-in EMS (`fake_ems_healthy.py`) and
timed each invocation's own wall-clock (fork → exit). `run_test_a.sh`.

| Concurrency | naive p50 | naive p99 | naive max | Go p50 | Go p99 | Go max |
|---:|---:|---:|---:|---:|---:|---:|
| 10 | 34ms | 40ms | 40ms | 13ms | 16ms | 16ms |
| 50 | 165ms | 1,359ms | 1,359ms | 56ms | 81ms | 81ms |
| 200 | 1,212ms | 1,477ms | 1,541ms | 74ms | 143ms | 152ms |
| 500 | 593ms | 3,346ms | 4,393ms | 61ms | 117ms | 131ms |
| 1000 | **466ms** | **5,189ms** | **6,184ms** | 58ms | 116ms | 141ms |

(`run_all.sh`'s output, captured on a shared sandbox VM — a second run of
the same script produced different absolute numbers, e.g. 1000-concurrency
naive p50 as low as 466ms one run and 597ms another, both times with a
long tail well past a second; run-to-run *variance* is expected on shared
hardware, but naive's p99/max blowing through the 500ms budget by 2–10× at
peak concurrency, against Go staying under ~150ms throughout, reproduced
identically every time this was run.)

**Finding:** naive's tail latency (p99/max) is the number that matters for
an SLA, and it degrades sharply and unpredictably with concurrency — into
multiple *seconds* at the stated peak — against a backend with zero real
network latency and zero processing time. Every millisecond of real EMS
latency, TLS handshake to a real (non-localhost) host, or network round
trip in production stacks on top of this, not instead of it. The Go
numbers here aren't even the relevant ones for the SLA (see Test C —
`itrs-notify`'s own wall-time doesn't depend on the EMS at all); they're
shown for a fair apples-to-apples comparison under the same synthetic
concurrency, and they stay flat and sub-150ms where naive does not.

## Test B — TCP connection reuse (`TIME_WAIT` accumulation)

Every `itrs_notify_naive.sh` invocation is a separate OS process running a
separate `curl`, so there is categorically no way for it to reuse a TCP
connection across triggers, no matter how the server behaves. Measured the
number of client-side sockets left in `TIME_WAIT` (`count_time_wait.py
<port> --client` — counts by *remote* port, i.e. connections *this* host
made outward, since that's the Gateway host's own ephemeral-port budget,
not the fake EMS's).

```
naive : 300 invocations -> before=0   after=300  (delta=300, exactly 1:1)
Go    : 300 invocations -> before=300 after=300  (delta=0)
Go    : 300 more        -> before=300 after=300  (delta=0, pool stays warm)
```

**Finding:** every naive invocation leaves exactly one socket in
`TIME_WAIT`; the Go daemon's pooled, kept-alive connections leave none. The
naive number is not "slow," it's **unbounded and directly proportional to
trigger volume** — which turns into a hard number at the stated peak:

> 1,000 new outbound connections/s × ~60s default Linux `TIME_WAIT` (2×MSL)
> ≈ **up to 60,000 sockets parked in `TIME_WAIT` at any given moment**, on
> a single Gateway host, from this one integration.

The default Linux ephemeral port range (`net.ipv4.ip_local_port_range`) is
commonly ~28,000 ports. The naive design would exhaust the Gateway host's
entire outbound port space before accounting for anything else that host
does — new connections (this integration's and any other process's) start
failing with `EADDRNOTAVAIL`. That's not a performance complaint, it's an
outage the monitoring pipeline causes on its own host — and with ~80
Gateway hosts, it's 80 independent occurrences of it, one per host/region,
each hitting whenever that host's trigger rate sustains near peak.

## Test C — an unresponsive backend (the sharpest failure mode)

`fake_ems_hung.py` accepts the TCP connection and then never reads or
writes another byte — no refusal, no RST, nothing a caller can fail fast
on. `curl` has **no default response-read timeout**; without an explicit
`--max-time`/`--connect-timeout` (absent from the naive script, and easy to
forget in a quickly-written effect script), this hangs the calling process
indefinitely.

```
naive: 20 concurrent triggers -> all 20 curl+bash processes still BLOCKED
       after 28+ seconds (checked twice; would still be blocked now)

Go:    the same 20 triggers, same hung backend
       itrs-notify wall-time: p50=26ms p99=32ms max=32ms (unaffected —
         it never touches the network; see design doc §4)
       event-relay's own log: breaker opens after the configured
         consecutive-failure threshold; all 20 land in the DLQ with
         reason="context deadline exceeded", attempts=2 — inspectable,
         not lost, and zero OS processes left behind
```

**Finding:** the naive design has no ceiling on this failure — every
trigger against a stuck EMS is one more permanently blocked process, at
the actual trigger rate (up to 1,000/s at peak). Given enough time (or a
short outage during a busy period), the Gateway host runs out of process
table entries, file descriptors, or memory **because of this integration**,
regardless of how good the EMS's actual uptime is otherwise — a single
slow response or misbehaving proxy is enough to trigger it, not just a
full EMS outage. The Go daemon's per-attempt timeout, retry cap, and
circuit breaker together put a hard, configured ceiling on this
(`ITRS_RELAY_WORKERS` × `ITRS_RELAY_ATTEMPT_TIMEOUT_S` bounds the total
resource any EMS misbehavior can ever consume), and the trigger-side
program is never blocked on the network at all.

## A secondary, independent finding: correctness

`itrs_notify_naive.sh` builds its JSON body by string interpolation
(`"headline":"${_HEADLINE}"`). A headline or message containing a `"` or
`\` — plausible free text from a monitored application — produces invalid
JSON the EMS will reject outright, silently, with no retry. This is
unrelated to the performance findings above and would exist even at low
volume; worth raising alongside them since it's the same root cause
(a script that was fast to write, not designed to the contract).

## Reproducing this

```bash
cd examples/itrs-gateway-event-relay
go build -o /tmp/bench/itrs-notify ./cmd/itrs-notify
go build -o /tmp/bench/event-relay ./cmd/event-relay
cd benchmark
./run_all.sh    # runs Tests A, B, and C in sequence, printing each result
```

Each test starts and tears down its own fake EMS / relay daemon — see
`run_all.sh` for the exact sequencing if you want to run one test in
isolation or against a real (non-localhost) EMS endpoint instead. Test C's
cleanup step force-kills the still-hung `curl` processes it just proved are
stuck; the resulting `line 32: <pid> Killed curl ...` lines in the output
are bash reporting that cleanup, not a test failure.
