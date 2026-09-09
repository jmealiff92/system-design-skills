# Design: ITRS Geneos Gateway → Event Management System notifier

Written with the reasoning loop from the `system-design` skill: clarify →
estimate → design → trade-offs → failure modes → iterate. Working code for
this design lives in `examples/itrs-gateway-event-relay/`.

> **Docs access note.** `docs.itrsgroup.com` and `support.itrsgroup.com` are
> blocked by this sandbox's network egress policy, so the exact spelling of
> every Geneos effect environment variable below could not be re-verified
> against the live page (`geneos_rulesactionsalerts_tr` → *Effects*) at
> write time. The design deliberately does not depend on getting that list
> perfectly right (see §3 and §5) — verify the real names with one throwaway
> rule that runs `env > /tmp/env.dump` before go-live, then adjust `varMap`
> in `cmd/itrs-notify/main.go`.

> **Revision note.** The original draft of this design chose a stdlib-only
> Python script for `itrs-notify`, on the reasoning that "no dependency to
> install" was the whole story. A later constraint invalidated that:
> **~80 Gateway hosts across multiple regions**, with no guarantee they run
> the same, or any, Python environment. That's the `system-design` skill's
> "treat the architecture as a hypothesis" discipline in practice — a new
> fact changed a decision, so the decision changed, not just the code. Both
> `itrs-notify` and `event-relay` are now Go, built as static binaries; see
> §6 and the language note at the top of `examples/itrs-gateway-event-relay/README.md`
> for the reasoning. The architecture (§4) did not need to change — only
> the implementation language of the two components it already called for.

## 1. Problem & scope

**Functional requirement:** a Gateway *Effect* — Geneos's mechanism for
running an external program when a rule/alert/action fires — must forward
every trigger to a downstream Event Management System (EMS) as an HTTP POST.

**Non-functional constraints (given):**
- Peak volume **60,000 triggers/min = 1,000/s**.
- **Concurrency expected**, and **multiple different processes** (multiple
  rules, samplers, and/or multiple Gateway hosts) can invoke the script at
  the same time — there is no single caller to serialize behind.
- **Latency < 500 ms.**
- **~80 Gateway hosts, in different regions**, each independently forking
  the trigger program — no shared provisioning is assumed between them.
  This is what rules out relying on a language runtime being present, let
  alone at a consistent version (§6).

**Out of scope:** the EMS's own ingestion design (assumed to be a normal
HTTP API); Geneos Gateway HA/config; historical backfill/replay tooling
beyond the dead-letter file described below.

**Key assumption to make explicit (GUIDE discipline: no number should be
silently assumed):** "latency < 500 ms" is treated as **two separate
budgets**, because a Gateway effect and an HTTP call to a third party are not
the same clock:
1. **Script wall-time** (fork → exec → exit, what the Gateway host pays for
   and what any Gateway-side effect timeout is measured against).
2. **End-to-end delivery latency** (trigger → event acknowledged by the EMS).

Conflating them is what breaks this design under load — see §4.

> **Empirically validated, not just estimated.** The current production
> approach is a shell script invoked per Effect (`curl` + no timeout/retry),
> the exact shape this section's estimate warns about. Rather than rest on
> the math alone, `examples/itrs-gateway-event-relay/benchmark/BENCHMARK.md`
> load-tests a representative reconstruction of it against this design,
> head to head, and gets hard numbers: the naive script's own p99 latency
> alone exceeds the 500ms SLA by 2–10× at peak concurrency (against a
> *healthy* backend), it leaves one OS-level TCP connection in `TIME_WAIT`
> per trigger with no ceiling (≈60,000 at sustained peak — more than the
> host's ephemeral port range), and it hangs its process **indefinitely**
> against a backend that accepts a connection and just doesn't respond —
> an outage in the monitoring pipeline itself, not merely a slow one. Read
> that doc for the reproduction steps if a stakeholder needs to see it
> proven rather than asserted.

## 2. Scale estimates (`back-of-the-envelope`)

| Quantity | Value | Assumption |
|---|---|---|
| Peak rate | 1,000 events/s | 60,000/min given |
| Sustained/average rate | ~500 events/s | standard 2× peak factor |
| Envelope size | ~0.5–2 KB JSON | small structured record, see §3 |
| Peak bandwidth | ~1.5 MB/s (~12 Mbps) | 1,000 × 1.5 KB — trivial, not a driver |
| In-flight concurrency needed at the EMS hop | **N ≈ λ × W ≈ 1,000 × 0.15 s ≈ 150** | Little's Law, per-call budget 150 ms (§4) |
| OS process forks/s on the Gateway host | up to 1,000/s at peak | one OS process per trigger — this **is** a driver, see §6 |

The number that decides the architecture is the last one, not the request
rate itself: **1,000 fresh OS processes/second, each wanting to talk to a
remote HTTPS endpoint**, is the shape of the problem. A naive "the script
does `curl`/`requests.post()` and exits" design forces:
- a fresh DNS lookup + TCP + TLS handshake per event (no connection reuse
  possible across unrelated short-lived processes) — several round trips
  before the POST even starts, blowing the 500 ms budget under any real EMS
  or network latency;
- up to ~1,000 concurrent outbound sockets/ephemeral ports and ~1,000
  process creations per second on the Gateway host, with no shared state to
  rate-limit, retry sanely, or circuit-break against a struggling EMS —
  every process is its own isolated island making its own uncoordinated
  retry decision (the exact retry-storm shape `resilience-failure` warns
  about, just triggered by fan-out instead of a single caller).

So the core design decision (`messaging-streaming`'s framing: *move the
slow/spiky part off the synchronous path*) is: **the Gateway-spawned script
must not be the thing that talks to the network.** It hands the event to a
long-running local process over local IPC and exits; that long-running
process owns the connection pool, retries, and circuit breaker.

## 3. API / contracts (`api-design`)

### 3a. Inbound: env vars Geneos passes to the effect

Geneos exports the triggering context as environment variables (documented
under *Rules, Actions & Alerts → Effects*; conventionally underscore-prefixed,
mirroring the rule-language accessors like `rowname()`/`column()`/`value()`).
Rather than hard-coding an exact, possibly-stale list as required fields, the
script:
1. captures **every** `_`-prefixed env var verbatim into an `attributes` map
   (forward-compatible — nothing is dropped even if a name is wrong or a
   version adds new ones), and
2. additionally lifts the commonly-used ones into named top-level fields via
   a single editable table (`varMap` in `cmd/itrs-notify/main.go`) for the
   fields an EMS integration typically wants to query/alert on: severity,
   headline, managed entity, sampler, dataview, row/column/value, gateway,
   probe, rule/variable name, timestamp.

This is deliberate: the exact spelling is the one fact in this design that
could not be verified against the live docs from this sandbox (see the note
at the top). A table beats hard-coded field access precisely because fixing
a wrong name later is a one-line edit, not a rewrite — and nothing is lost
in the meantime because of point (1).

### 3b. Local hand-off: script → relay daemon

Unix domain socket, length-prefixed JSON, one frame per event:

```
→ 4-byte big-endian length | JSON payload (utf-8)
← 1 byte ack: 'K' = queued for delivery, 'F' = daemon busy/unreachable
```

This is **not** the delivery contract — it is only "accepted for delivery,"
matching the queue's owned semantics (`messaging-streaming`: ack-after-enqueue
≈ at-least-once, not exactly-once). `'F'`, a timeout, or any socket error
means the script falls back to appending straight to the on-disk spool file
instead (§6) — the script never blocks waiting on the network.

### 3c. Outbound: relay daemon → EMS

```
POST /api/v1/events HTTP/1.1
Host: ems.internal
Content-Type: application/json
Idempotency-Key: <event_id>          # UUID4, minted once, at the script
Authorization: Bearer <token>

{
  "event_id": "b6e5...'",            # = Idempotency-Key
  "source": "geneos",
  "gateway": "GW-PROD-1",
  "triggered_at": "2026-09-09T14:03:11.482Z",
  "severity": "critical",
  "headline": "CPU_Usage",
  "managed_entity": "app-host-07",
  "sampler": "cpu",
  "dataview": "cpu",
  "row": "cpu0", "column": "usage", "value": "97",
  "message": "CPU usage above threshold",
  "attributes": { "...every raw _VAR from Geneos, unmapped or not...": "..." }
}

200/202 → success, ack
4xx (not 429) → permanent — do not retry, straight to DLQ
429/5xx → transient — retry per policy (§4), then DLQ
```

`Idempotency-Key` is the load-bearing piece from `api-design`: the script
mints `event_id` once, before any hand-off or retry exists, so every retry —
whether from the relay's own backoff or a later replay out of the disk
spool/DLQ — carries the *same* key and the EMS can dedupe a POST that
actually landed but whose ack was lost.

## 4. High-level design

```
Geneos rule/alert fires (N processes, possibly 80 hosts across regions, up to 1,000/s)
        │  fork+exec
        ▼
 itrs-notify  (per-trigger, static Go binary, no network, no runtime deps)
    - reads env, builds envelope + event_id
    - tries UDS write to local relay daemon (~ms, short timeout)
    - on any failure: appends one line to local spool file instead
    - exits — wall-time dominated by fork/exec, not an interpreter or the network
        │ UDS                                  │ disk (fallback path)
        ▼                                       │
 event-relay  (one long-running daemon per Gateway host, static Go binary)
    - bounded in-memory queue/channel (backpressure: NACK → client spools instead)
    - spool-drainer tails the disk spool (crash recovery + NACK fallback)
    - N worker goroutines, shared pooled HTTP client (keep-alive, stdlib net/http)
    - per-attempt timeout + capped retry+jitter (resilience-failure)
    - circuit breaker around the EMS endpoint
    - exhausted retries / breaker-open → dead-letter file
        │ HTTPS (pooled, persistent connections)
        ▼
                    Event Management System
```

Each component ties back to a number or requirement from §1–§2:
- **Two processes, not one** — the fork-per-trigger process can't hold a
  connection pool (it doesn't live long enough to amortize a TLS handshake);
  the daemon can, because it's long-running. This is what turns "1,000
  independent TLS handshakes/s" into "~150–200 reused, pooled connections."
- **Bounded queue + disk spool, not an unbounded buffer** —
  `messaging-streaming`'s backpressure rule: absorb the burst without hiding
  a backlog. In-memory queue sized for ~20 s of peak (20,000 items) covers
  transient EMS slowness; anything past that (or daemon-down) spills to disk
  rather than blocking the UDS accept loop or growing memory without bound.
- **Circuit breaker** — without it, a slow/down EMS turns 1,000 req/s of
  normal traffic into 1,000 req/s of *retries* piling on top, the retry-storm
  amplifier `resilience-failure` calls out; the breaker fails fast to the
  spool instead once the EMS is clearly unhealthy.
- **One daemon per Gateway host, co-located** — the fast path (script→daemon)
  is a local socket, so it stays fast (~low single-digit ms) regardless of
  what the EMS is doing right now; if Geneos runs an HA pair of Gateways,
  each host runs its own daemon (bulkhead — one host's spool/backlog can't
  affect the other).

### The two latency budgets, sized

1. **Script wall-time** (what the Gateway host/effect timeout sees):
   fork/exec + a static Go binary's own start (measured well under 1 ms,
   vs. tens of ms for a scripting-language interpreter — see the README)
   + env parsing (µs) + UDS write/ack (~1–5 ms, local socket) ≈ **well
   under 500 ms, and independent of the EMS's health** — this is the
   actual win: Gateway never waits on the network call.
2. **End-to-end delivery latency** (trigger → EMS ack), budgeted under
   500 ms P99: per-attempt timeout 150 ms (connect+read) → one retry with
   ~50–150 ms jittered backoff → worst case ~350–400 ms for two attempts,
   leaving margin. A breaker that's already open skips straight to the
   spool instead of spending the budget on a call likely to fail.

Concurrency sizing (Little's Law, §2): **N ≈ 1,000/s × 0.15 s ≈ 150**
in-flight requests needed to sustain peak within the per-attempt budget; the
daemon runs a pooled client with headroom (`ITRS_RELAY_WORKERS = 200`,
`MaxIdleConnsPerHost ≈ 200`).

## 5. Data model

No datastore in the hot path by design (`messaging-streaming`: don't add
persistence the requirement doesn't call for). The only on-disk structures:

- **Spool file** (`pending.jsonl`) — append-only, one JSON record per line,
  each record written with a single `write()` under `PIPE_BUF` (4096 B) so
  POSIX guarantees the append is atomic even with many concurrent writers
  (many `itrs-notify` processes falling back at once need no file locking).
  Truncate the `attributes` blob rather than break this guarantee —
  documented trade-off in the code (`internal/spool`, shared by both
  binaries — safe to share in Go since it's compiled into each static
  binary at build time, not deployed as a sibling file that could drift).
- **DLQ file** (`dead_letter.jsonl`) — same shape plus `failure_reason` and
  `attempts`, for events that exhausted retries or hit a permanent (4xx)
  rejection. Operator-inspectable and replayable.

`event_id` (UUID4, minted in `itrs-notify`) is the natural primary key
across every hop — spool line, queue item, DLQ record, and the
`Idempotency-Key` the EMS sees — so a record can be traced end to end.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| Split "collect" (per-trigger script) from "deliver" (long-running daemon) | Decouples Gateway-visible latency from EMS latency/availability; enables connection reuse, pooling, a shared circuit breaker | Two components to deploy/operate instead of one script; a new local IPC contract | Never, at this volume — the fork-per-trigger issue only worsens as rate grows |
| UDS + disk-spool fallback for the hand-off | Script never blocks on the network; local socket write is ~ms regardless of EMS health | At-least-once only (ack = "queued," not "delivered"); a message can be duplicated across spool-replay + in-flight retry | An EMS-side dedupe on `Idempotency-Key` isn't available — then durability needs a stronger local guarantee |
| Bounded in-memory queue, NACK-and-spool when full | Backpressure — a stalled EMS can't grow memory without bound or block new triggers | A full queue pushes load onto disk I/O; spool-drain adds recovery-path complexity | Sustained rate consistently exceeds spool disk throughput — then the daemon itself needs sharding across hosts |
| Circuit breaker around the EMS call | Fails fast during an EMS outage instead of piling retries onto a dead endpoint | Adds a state machine to tune (open/half-open thresholds); can trip on a real but transient blip | Breaker flaps under normal jitter — loosen the error-rate threshold or lengthen the window |
| Capture all `_`-prefixed env vars into `attributes`, map known ones by a table | Forward-compatible if the exact Geneos variable names are wrong or change across versions (this sandbox couldn't verify them live) | The mapped top-level fields may be empty/wrong until `varMap` is confirmed against a real Gateway | After the first live test — pin `varMap`, keep `attributes` as the safety net |
| Go, static binaries (`CGO_ENABLED=0`), for both `itrs-notify` and `event-relay` | No interpreter/runtime version to match across ~80 hosts in different regions (the deciding factor — see the revision note); sub-ms process start vs. tens of ms for a scripting-language interpreter at up to 1,000 forks/s; stdlib `net/http` gives connection pooling for free, so `event-relay` also needs zero third-party dependencies | A compiled/typed language: a `varMap` fix or schema tweak needs a rebuild + redeploy of the binary, not a hand-edit on a live host; cross-compiling for any non-amd64 region needs a `GOOS`/`GOARCH` build matrix in CI | Never, given the 80-host constraint — the same environment-drift argument only gets stronger with more regions, not weaker |

## 7. Failure modes & degradation

- **EMS is down or slow.** Per-attempt timeout bounds each call; capped
  retry+jitter rides out a blip; the circuit breaker opens on sustained
  failure and skips the network entirely, routing straight to the spool.
  Gateway-visible latency is unaffected either way (§4) — the degradation is
  *invisible upstream* and only shows up as growing spool/DLQ depth, which
  is exactly what's alarmed on (§9).
- **Relay daemon is down or restarting.** `itrs-notify`'s UDS connect fails
  fast (short timeout) and falls back to the disk spool; no events are
  lost, they're just delayed. On restart, the daemon's spool-drainer replays
  the backlog through the same bounded queue/breaker path — no thundering
  herd into the EMS, because replay respects the same backpressure as live
  traffic.
- **Poison event** (a payload the EMS permanently rejects, e.g. malformed
  attribute value → 4xx). Not retried — goes straight to the DLQ with the
  rejection reason, so it can't blocking-loop the worker that picked it up
  or waste retry budget (mirrors `messaging-streaming`'s poison-message
  guidance).
- **Disk fills up** (spool/DLQ growing faster than they drain). This is the
  one true data-loss risk in the design and is deliberately not silently
  papered over: `itrs-notify`'s write to a full disk returns an error, is
  caught, and logged to stderr — the event is dropped rather than hanging
  the Gateway effect. Alarm on spool file size/age (§9) well before this
  point.
- **Gateway host itself is overloaded by process churn** (§8) — the
  daemon/EMS are healthy but the Gateway can't fork fast enough. Choosing
  Go for `itrs-notify` (§6) removes the interpreter-startup share of this
  cost, but not the OS-level fork/exec cost itself — see §8 for what's
  actually left to do if this shows up.

## 8. Scale evolution

Current bottleneck at 1,000 events/s: **process-creation overhead on the
Gateway host**, not the network hop or the EMS (§2) — and choosing Go for
`itrs-notify` (§6) addresses the *interpreter-startup* share of that cost,
not the OS-level fork/exec syscall cost, which any external program pays
regardless of language. At the next order of magnitude (10×, 10,000
events/s):
- 10,000 fork/exec calls/second is still meaningful kernel-side work (page
  table setup, context switches) even with a sub-millisecond static binary.
  If Gateway-host load starts tracking event rate rather than headroom,
  the next lever isn't the trigger program's language (already addressed)
  — it's whether Geneos offers any way to batch or hold a persistent
  channel per Gateway instead of one process per trigger; if not, this
  becomes a host-sizing conversation, not a code change.
- If a single relay daemon's worker pool can't sustain the higher rate,
  raise `ITRS_RELAY_WORKERS`/pool limits first (cheap); only shard into
  multiple daemons per host (e.g. one per NUMA node / CPU group) if a
  single process is provably the ceiling — Go's goroutines scale well
  past the ~200 default before this becomes necessary.
- If the EMS itself becomes the ceiling (rate-limits, 429s dominate), that's
  a capacity conversation with the EMS owner, not something retries can fix
  — the circuit breaker keeps the Gateway host healthy in the meantime by
  shedding to the spool rather than hammering.
- **Rolling out a binary fix across 80 hosts in multiple regions** is
  itself an evolution point this design didn't have to solve when it was
  "edit one Python file in place" — a build pipeline (CI cross-compiles
  per `GOOS`/`GOARCH`, artifacts get pushed per region) becomes necessary
  infrastructure, not optional polish, once a `varMap` or schema fix needs
  to reach every host.

**Signal that it's time to evolve:** sustained (not spiky) spool/DLQ growth,
or Gateway-host load average tracking event rate rather than headroom.

## 9. Observability (`observability`)

RED framing on the relay daemon (it's the request-driven component):
- **Rate** — events accepted (UDS) vs spooled-fallback vs sent-ok vs
  sent-fail vs DLQ, per minute.
- **Errors** — HTTP error class from the EMS (4xx vs 429/5xx), breaker
  state transitions.
- **Duration** — per-attempt latency (p50/p99) and end-to-end
  trigger→ack latency, to check the 500 ms budget in production, not just
  on paper.

USE framing on the two buffers, which is where a slow EMS actually shows up:
**queue depth** and **oldest-message age** in the in-memory queue, and
**spool/DLQ file size and age of oldest un-replayed line** — the single best
early-warning signal per `messaging-streaming`, alarmed well before disk
fills.

## 10. Open questions

- **Exact Geneos env-var names** — verify against a live Gateway (this
  sandbox's network policy blocked `docs.itrsgroup.com` /
  `support.itrsgroup.com`); confirm `varMap` in `cmd/itrs-notify/main.go`.
- **EMS auth scheme and idempotency support** — the contract in §3c assumes
  bearer-token auth and that the EMS honors `Idempotency-Key`; confirm, or
  add an EMS-specific adapter in `cmd/event-relay`.
- **Multiple Gateway hosts** — confirmed one relay daemon per host; if the
  EMS needs a single, globally coordinated rate limit across hosts (rather
  than each host independently pacing itself), that's a `consistency-
  coordination` concern (shared limiter state) not addressed here.
- **Are all 80 hosts `linux/amd64`?** The build in the README assumes it;
  any host on a different OS/architecture (e.g. `arm64`) just needs its own
  `GOOS`/`GOARCH` build in the same CI pipeline — confirm the fleet's
  architecture mix before assuming one build artifact covers all 80.
- ~~Binary distribution mechanism~~ **Resolved: Ansible.** See
  `examples/itrs-gateway-event-relay/ansible/` — a role/playbook that
  deploys both binaries, the systemd unit, and (the part worth reading
  even if Ansible itself is old news) a shared, setgid spool directory
  with the two binaries' different system users both able to write it,
  rolled out in `serial: "10%"` batches rather than to all ~80 hosts/regions
  in one play.
