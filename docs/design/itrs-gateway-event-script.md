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

> **Revision history.** This design has reversed itself twice so far — kept
> here deliberately rather than quietly edited away, since the *reasoning*
> behind each reversal is as load-bearing as the current answer.
>
> 1. **Python → Go** (per-trigger script). The original draft chose a
>    stdlib-only Python script, reasoning "no dependency to install" was
>    the whole story. ~80 Gateway hosts across regions, with no guarantee
>    of a shared provisioning process, invalidated that — no language
>    runtime can be assumed present or at a consistent version. Both
>    programs (at the time) became Go static binaries.
> 2. **Two users → one** (deployment). A separate dedicated system account
>    for the relay daemon, on general least-privilege grounds, turned out
>    to be solving a problem this org didn't have: Geneos's own runtime
>    account was already minimal and dedicated. Collapsed to one account,
>    dropping a setgid shared-directory workaround along with it.
> 3. **Two processes → one** (this revision, the big one). The 60,000/min
>    figure that justified a whole relay daemon — connection pool, circuit
>    breaker, queue — turned out to be the *fleet-wide* aggregate across
>    ~80 hosts, not what any single host sees. Real per-host peak is
>    **835/min (~14/s)**, highest average **14/min (~0.23/s)**. At that
>    rate, Little's Law alone settles it (see §2) — there's no concurrent
>    load to pool connections for, no backlog for a breaker, nothing for a
>    queue to buffer. `event-relay` is gone; `itrs-notify` POSTs directly.
>    Revision 2's question (does the daemon need its own user) is now moot
>    — there's only one process, so it was never a live question in the
>    first place, it just didn't know that yet.
>
> The pattern across all three: a number or a constraint changed, and the
> *architecture* changed with it rather than being defended past the point
> it stopped fitting — the `system-design` skill's "treat the architecture
> as a hypothesis" discipline, applied for real rather than as a slogan.

## 1. Problem & scope

**Functional requirement:** a Gateway *Effect* — Geneos's mechanism for
running an external program when a rule/alert/action fires — must forward
every trigger to a downstream Event Management System (EMS) as an HTTP POST.

**Non-functional constraints (given, as currently understood):**
- Peak volume **per Gateway host: 835 events/min (~14/s)**. Highest
  *average* across hosts: **14 events/min (~0.23/s)** — i.e. a ~60×
  peak-to-average ratio; this is a bursty alerting workload, not a steady
  telemetry stream, and the vast majority of each host's time is spent
  near-idle. (An earlier draft read "60,000/min" as a per-host number;
  it's the fleet-wide aggregate across ~80 hosts instead — see the
  revision history above and §2.)
- **Concurrency expected**, and **multiple different processes** (multiple
  rules, samplers) can invoke the program on the same host at the same
  time — there is no single caller to serialize behind, even though the
  *rate* is low.
- **Latency < 500 ms.**
- **~80 Gateway hosts, in different regions**, each independently forking
  the trigger program — no shared provisioning is assumed between them.
  This is what rules out relying on a language runtime being present, let
  alone at a consistent version (§6) — independent of the volume question.

**Out of scope:** the EMS's own ingestion design (assumed to be a normal
HTTP API, and sized for the *fleet-wide* aggregate — not this design's
concern, since nothing here pools or throttles across hosts); Geneos
Gateway HA/config; historical backfill/replay tooling beyond the
dead-letter file described below.

**Key assumption to make explicit (GUIDE discipline: no number should be
silently assumed):** "latency < 500 ms" is the end-to-end time from
trigger to EMS acknowledgment, for a single `itrs-notify` process (fork →
exec → the HTTP call → exit). With no second process in this design,
there's only one clock to budget against now — see §4.

> **Empirically validated, not just estimated.** The current production
> approach is a shell script invoked per Effect (`curl` + no timeout/retry).
> Rather than rest on arithmetic alone,
> `examples/itrs-gateway-event-relay/benchmark/BENCHMARK.md` load-tests a
> representative reconstruction of it against this design, head to head,
> **at the real measured volume** (not the originally-assumed one) and
> gets hard numbers: across the actually-measured concurrency range (1–14),
> both the naive script and this design are fast and nowhere near the SLA
> — exactly what Little's Law predicts at this rate, with no help from
> pooling or a relay needed. The gap only opens at a 10× stress margin past
> the measured peak, where naive's p99 exceeds the SLA by ~3× while the
> hardened single binary (still no pooling, no daemon — just a bounded
> timeout) stays under it. The one failure mode that doesn't improve with
> lower volume — curl's lack of a default response-read timeout hanging a
> process **indefinitely** against a stuck backend — was always a
> missing-timeout bug, not a case for a relay; `itrs-notify` fixes it
> directly. Read that doc for the reproduction steps if a stakeholder needs
> to see any of this proven rather than asserted.

## 2. Scale estimates (`back-of-the-envelope`)

| Quantity | Value | Assumption |
|---|---|---|
| Peak rate, per host | 14 events/s | 835/min given |
| Highest average rate, per host | 0.23 events/s | 14/min given |
| Peak-to-average ratio | ~60× | bursty alerting workload, not steady telemetry |
| Envelope size | ~0.5–2 KB JSON | small structured record, see §3 |
| Peak bandwidth, per host | ~21 KB/s | 14 × 1.5 KB — trivial, never a driver |
| **In-flight concurrency at peak (Little's Law, N ≈ λ×W)** | **N ≈ 14 × 0.3 s ≈ 4** | per-attempt-plus-retry budget ≈ 300 ms (§4) |
| In-flight concurrency, 10× stress margin | N ≈ 140 × 0.3 s ≈ 42 | sanity check past anything measured — see benchmark Test A |
| OS process forks/s, per host | up to ~14/s at peak | one process per trigger — trivial at this rate for *any* language |
| `TIME_WAIT` sockets at sustained peak | ≈ 835 (14/s × ~60 s default lifetime) | vs. a ~28,000–30,000 ephemeral port range — under 3% |

**None of these numbers forces a relay.** This is the opposite conclusion
from the original (fleet-aggregate) estimate, and it's worth walking
through exactly why, since the original reasoning wasn't wrong *given its
input* — the input was wrong:

- **Concurrency.** At N≈4 even under a full-timeout worst case, there's
  nothing for a connection pool to amortize or a queue to buffer — Little's
  Law's whole point is that concurrency, not raw rate, is what resource
  planning answers to, and at this rate concurrency rounds to "a handful."
- **Process-fork cost.** ~14 forks/s is trivial for literally any runtime,
  including a slow interpreter — this was never going to be the
  bottleneck at this rate (contrast the original 1,000/s assumption, where
  it was the single biggest driver).
- **`TIME_WAIT`/port exhaustion.** ~835 sockets at sustained peak is under
  3% of a typical ephemeral port range. The original estimate's loudest
  argument for pooling — ~60,000 `TIME_WAIT` sockets, more than the whole
  port range, at the assumed 1,000/s — simply isn't the number that
  applies here.

**What doesn't change with volume:** a backend that accepts a TCP
connection and never responds hangs a caller with no read timeout
**indefinitely**, regardless of trigger rate. That's a missing
`--max-time`-equivalent, fixed directly in `itrs-notify` with a bounded
per-attempt timeout and a small retry budget — never actually a case for
a relay, even under the original (wrong) volume assumption.

## 3. API / contracts (`api-design`)

### 3a. Inbound: env vars Geneos passes to the effect

Geneos exports the triggering context as environment variables (documented
under *Rules, Actions & Alerts → Effects*; conventionally underscore-prefixed,
mirroring the rule-language accessors like `rowname()`/`column()`/`value()`).
Rather than hard-coding an exact, possibly-stale list as required fields, the
program:
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

### 3b. Outbound: `itrs-notify` → EMS

The only network call in this design — no local hand-off, no daemon.

```
POST /api/v1/events HTTP/1.1
Host: ems.internal
Content-Type: application/json
Idempotency-Key: <event_id>          # UUID4, minted once, at process start
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

200/202 → success, exit 0
4xx (not 429) → permanent — do not retry, straight to the dead-letter file
429/5xx → transient — retry per policy (§4), then the dead-letter file
```

`Idempotency-Key` is the load-bearing piece from `api-design`: `event_id`
is minted once, before the first attempt, so a retry within the same
process carries the *same* key and the EMS can dedupe a POST that
actually landed but whose ack was lost to a network blip.

## 4. High-level design

```
Geneos rule/alert fires (N processes, possibly 80 hosts across regions, up to ~14/s per host)
        │  fork+exec
        ▼
 itrs-notify  (per-trigger, static Go binary, no runtime deps)
    - reads env, builds envelope + event_id
    - POSTs directly to the EMS: bounded per-attempt timeout, up to 1 retry+jitter
    - on exhausted/permanent failure: appends one line to a local dead-letter file
    - exits — wall-time dominated by fork/exec + the HTTP call, nothing else
        │ HTTPS (one connection per process; no pooling — see below)
        ▼
                    Event Management System
```

One component, one hop. Each piece ties back to a number from §1–§2:

- **One process, not two.** At N≈4 in-flight (§2), there's no concurrent
  load to justify a long-running process holding a connection pool — the
  fork-per-trigger cost that made pooling necessary at the *assumed*
  1,000/s doesn't apply at the *real* ~14/s. No pool, no breaker, no
  queue, no local IPC protocol to maintain.
- **No connection pooling.** Each `itrs-notify` process makes at most 2
  requests (1 try + 1 retry) in its entire lifetime, then exits — nothing
  to amortize a persistent connection across. The TLS/TCP handshake cost
  per invocation is the one thing this design gives up vs. a pooled
  daemon, and at ~14/s it's immaterial (§2's `TIME_WAIT` math).
- **Bounded timeout + small retry budget, not a breaker.** A circuit
  breaker's job is protecting a *shared* resource (a pool, a worker budget)
  from being exhausted by a sick dependency — there's no shared resource
  here to protect. Each process independently bounds its own worst case
  to ~300–400ms (below) and moves on; at N≈4, even every single in-flight
  request timing out simultaneously is a non-event for the host.
- **Local dead-letter file, not a live spool/replay queue.** Nothing
  needs draining in real time — there's no daemon to crash or restart,
  and no backlog to build (§2: concurrency never gets high enough to
  create one). The file is a last-resort, operator-inspectable record of
  what failed and why; see §7 for why automatic replay usually isn't even
  necessary.

### The latency budget, sized

One clock now, not two: fork/exec + a static Go binary's own start
(sub-millisecond — see the README) + env parsing (µs) + the HTTP attempt(s)
≈ **well under 500 ms**. Per-attempt timeout 150 ms (connect+read) → one
retry with ~50–100 ms jittered backoff → worst case ~350–400 ms for two
full-timeout attempts, leaving margin under the SLA — same numbers the
original design used for its per-attempt budget, just now the *only*
budget instead of one of two.

Concurrency sizing (Little's Law, §2): **N ≈ 14/s × 0.3 s ≈ 4** at
sustained peak, ≈42 even at a 10× stress margin — both trivially below any
level where pooling, a worker limit, or backpressure would change the
outcome. See `benchmark/BENCHMARK.md` Test A for the measured latency
curve confirming this holds in practice, not just in the formula.

## 5. Data model

No datastore in the hot path (`messaging-streaming`: don't add persistence
the requirement doesn't call for) — and now no live queue/spool either,
since there's no daemon to crash-recover or backlog to buffer. The one
on-disk structure:

- **Dead-letter file** (`dead_letter.jsonl`) — append-only, one JSON
  record per line (envelope + `failure_reason` + `attempts`), each record
  written with a single `write()` under `PIPE_BUF` (4096 B) so POSIX
  guarantees the append is atomic even with many concurrent `itrs-notify`
  processes writing at once, with no file locking needed. Truncate the
  `attributes` blob rather than break this guarantee — documented
  trade-off in the code (`internal/spool`).

`event_id` (UUID4, minted in `itrs-notify`) is the natural primary key —
dead-letter record and the `Idempotency-Key` the EMS sees are the same
value, so a record can be traced end to end even without a central store.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| One program, direct POST — no relay daemon, no connection pool, no queue, no breaker | Nothing to deploy/operate beyond one binary; no local IPC protocol; lowest possible latency path (no extra local hop) | Each process pays its own TLS/TCP handshake (no reuse across invocations); no shared backpressure if the EMS degrades fleet-wide | Sustained (not spiky) per-host rate climbs into the hundreds/sec, or burst analysis shows sustained high concurrency — see §8 for the actual threshold |
| Bounded per-attempt timeout + 1 retry, local dead-letter file on failure | Fixes the one failure mode that doesn't improve with lower volume: an unresponsive backend hanging the process indefinitely | At-least-once only; a dead-letter record needs a human (or a Geneos re-trigger — see §7) to resolve, nothing drains it automatically | Automatic replay becomes necessary — then a lightweight periodic reader of the dead-letter file is cheaper to add than reintroducing a daemon |
| Capture all `_`-prefixed env vars into `attributes`, map known ones by a table | Forward-compatible if the exact Geneos variable names are wrong or change across versions (this sandbox couldn't verify them live) | The mapped top-level fields may be empty/wrong until `varMap` is confirmed against a real Gateway | After the first live test — pin `varMap`, keep `attributes` as the safety net |
| Go, a single static binary (`CGO_ENABLED=0`) | No interpreter/runtime version to match across ~80 hosts in different regions — unaffected by the volume correction, since it was never a throughput argument | A compiled/typed language: a `varMap` fix or schema tweak needs a rebuild + redeploy, not a hand-edit on a live host; cross-compiling for any non-amd64 region needs a `GOOS`/`GOARCH` build matrix in CI | Never, given the 80-host constraint — the same environment-drift argument only gets stronger with more regions, not weaker |

## 7. Failure modes & degradation

- **EMS is down or slow.** Per-attempt timeout bounds each call; one
  capped retry+jitter rides out a blip; on exhausted retries or a
  permanent rejection, the event lands in the dead-letter file and the
  process exits normally. No breaker, because there's no shared resource
  (pool, worker budget) a sick EMS could exhaust at this concurrency (§4)
  — each process's worst case is bounded on its own.
- **A Geneos alert condition that keeps re-evaluating true will typically
  re-fire the Effect on the next cycle anyway** — so a delivery dropped
  during a brief EMS blip often self-heals without any replay mechanism,
  for exactly the kind of persistent condition (CPU high, disk full) this
  design exists to report. This is a real mitigating factor specific to
  Geneos's rule-evaluation model, not a universal excuse to skip
  durability — confirm it holds for the rules this integration actually
  covers (some are edge-triggered, not level-triggered) before relying on
  it over the dead-letter file.
- **Poison event** (a payload the EMS permanently rejects, e.g. malformed
  attribute value → 4xx). Not retried — straight to the dead-letter file
  with the rejection reason, so it doesn't waste the retry budget on a
  request that will fail the same way every time.
- **Disk fills up** (dead-letter file growing faster than anyone clears
  it). The one true data-loss risk in this design, deliberately not
  papered over: `itrs-notify`'s write to a full disk returns an error, is
  caught, and logged to stderr — the event is dropped rather than hanging
  the Gateway effect. Alarm on the dead-letter file's size/age (§9) well
  before this point; a sustained climb (not a brief spike) is the signal
  something is actually wrong downstream, worth escalating past "a
  Geneos rule will probably re-fire."
- **Gateway host process-fork cost.** Not a concern at the real per-host
  rate (§2: ~14 forks/s is trivial for any runtime) — kept here only to
  note explicitly that it was reasoned about and dismissed, not missed.

## 8. Scale evolution

This section now runs in the opposite direction from a typical "what
breaks at 10×" write-up: the interesting threshold is the one that would
bring back the complexity this revision removed, not one that adds more
of the same.

**Bring back pooling/a relay/a breaker if:**
- **Sustained per-host rate**, not a brief spike, climbs enough that
  Little's Law's N stops rounding to "a handful" — roughly, once a
  sustained rate implies **N ≳ 50–100** at the same ~300ms budget (so
  sustained rates in the hundreds/sec per host), connection reuse and a
  shared breaker start earning their complexity back. A one-off 10× burst
  (§2, §4) is already covered without this — it's *sustained* rate that
  matters, not a spike.
- **Burst concurrency analysis says otherwise.** §2's Little's-Law math
  assumes triggers are reasonably spread within a peak minute. If a single
  correlated incident (one shared dependency tripping many rules on one
  host in the same evaluation cycle) can realistically produce, say,
  hundreds of simultaneous triggers on one host — not just 835 spread
  across a minute — that's a different concurrency profile than this
  design was checked against. Confirm the burst shape, not just the
  per-minute aggregate, before ruling this out entirely (open question, §10).
- **Automatic replay of the dead-letter file becomes a real requirement**
  (§6, §7) — but try a lightweight periodic reader first; that's a much
  smaller addition than a live daemon.

**Signal that it's time to evolve:** the dead-letter file growing
persistently rather than in brief bursts tied to a known incident, or
Gateway-host load tracking event rate instead of headroom — both
observable without new instrumentation (§9).

**Rolling out a binary fix across 80 hosts in multiple regions** remains
real infrastructure regardless of which side of this design wins — a
build pipeline (CI cross-compiles per `GOOS`/`GOARCH`, Ansible deploys
per host/region) is already in place (`examples/itrs-gateway-event-relay/ansible/`).

## 9. Observability (`observability`)

Much lighter than a daemon would need, since there's no live process
state (queue depth, breaker state, worker saturation) to expose:

- **Rate** — dead-letter writes per host per day/hour. At the real
  volume, this should be rare; a sudden climb is the primary signal.
- **Errors** — `failure_reason` values in the dead-letter file (transport
  error vs. which HTTP status) — enough to tell "EMS was unreachable" from
  "EMS rejected the payload" without needing a live metrics pipeline.
- **Duration** — `ITRS_NOTIFY_DEBUG=1`'s per-invocation wall-time log, if
  ever profiling against the 500ms SLA in production rather than just in
  the benchmark.

**The one thing worth alarming on:** dead-letter file size/age — age of
the oldest unresolved entry is the best early-warning signal (same
principle `messaging-streaming` gives for any durable-failure record,
applied here to a file instead of a DLQ topic).

## 10. Open questions

- **Exact Geneos env-var names** — verify against a live Gateway (this
  sandbox's network policy blocked `docs.itrsgroup.com` /
  `support.itrsgroup.com`); confirm `varMap` in `cmd/itrs-notify/main.go`.
- **EMS auth scheme and idempotency support** — the contract in §3b assumes
  bearer-token auth and that the EMS honors `Idempotency-Key`; confirm, or
  add an EMS-specific adapter in `cmd/itrs-notify`.
- **Burst concurrency shape, not just the per-minute peak.** §8 names the
  specific scenario (a correlated incident firing many rules on one host
  within the same evaluation cycle) that would matter more than the
  835/min aggregate — worth checking against how this org's rules are
  actually structured before treating this design as settled at scale.
- **Are all 80 hosts `linux/amd64`?** The build in the README assumes it;
  any host on a different OS/architecture (e.g. `arm64`) just needs its own
  `GOOS`/`GOARCH` build in the same CI pipeline — confirm the fleet's
  architecture mix before assuming one build artifact covers all 80.
- ~~Binary distribution mechanism~~ **Resolved: Ansible.** See
  `examples/itrs-gateway-event-relay/ansible/` — a role/playbook that
  deploys the binary and a small secrets file, rolled out in
  `serial: "10%"` batches rather than to all ~80 hosts/regions in one play.
- ~~Does the relay daemon need its own dedicated system user?~~
  **Moot.** There's no relay daemon anymore (this revision) — see the
  revision history at the top. `itrs-notify` simply runs as whatever
  account Geneos's Effects already run as; no second account was ever a
  live question once there's only one process.
- **Does a persistent condition reliably re-fire the Effect on Geneos's
  next evaluation cycle**, the way §7 assumes for self-healing a dropped
  delivery during a brief EMS blip? True for level-triggered rules in the
  general case, but confirm it for whichever specific rules this
  integration covers — an edge-triggered rule wouldn't get the same
  safety net, and would lean more on the dead-letter file actually being
  checked.
