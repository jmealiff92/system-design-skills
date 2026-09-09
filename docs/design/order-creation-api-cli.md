# Design: Order Creation API & CLI

A write-up structured around the reasoning loop (`system-design` skill), composing
`requirements-scoping` → `back-of-the-envelope` → `api-design` → `data-storage` →
`task-scheduling` / `messaging-streaming` → `resilience-failure`.

## 1. Problem & scope

**One sentence:** A service (and a CLI on top of it) that lets a caller create
orders — one at a time, a handful at a time, or up to ~4,500 in a single
submission — with a way to check that an order *would* succeed before it is
placed.

### Functional requirements
1. **Create a single order**, synchronously, and get back its final state
   (created / rejected) in one round trip.
2. **Create a small set of orders** (e.g. 2–3) in one call, each evaluated and
   reported independently — one bad order must not block the good ones.
3. **Create a large batch of orders** (up to **4,500** in scope; designed not
   to fall over above that) without forcing the caller to hold a connection
   open for minutes.
4. **Prevalidate order parameters** — inventory availability, computed
   price/tax, shipping-address deliverability, payment-method validity — via
   dedicated calls the caller can make *before* committing to create, for both
   a single order and a whole batch (dry run).
5. **Track and retrieve results**: per-order status, error detail on failure,
   and the created order id on success — for both the sync and async paths.
6. **CLI** wraps all of the above: validate, create (single/small-batch),
   submit a bulk file, poll/watch a bulk job, fetch results, get one order.

### Non-functional constraints
- **Latency** — single-order create: p99 < 1.5s end-to-end (including
  prevalidation the server itself must still do — see §3). Bulk submission is
  *accepted* in < 2s regardless of batch size; the batch itself is processed
  asynchronously.
- **Throughput / batch ceiling** — up to 4,500 order lines per logical
  submission, and the system should not need a redesign for a moderately
  higher ceiling (see §8).
- **Correctness under retry** — a client that retries a timed-out request (a
  single create, or a whole bulk submission) must never create the same order
  twice.
- **Partial-failure isolation** — one malformed or unfulfillable line in a
  4,500-line batch must not fail the other 4,499.
- **No oversell** — inventory reservation must be atomic per line; a
  prevalidation "looks available" is advisory, not a hold (see §3, validation
  token).
- **Fairness** — a giant bulk job must not starve single-order latency for
  other callers; they are scheduled independently.
- **Auditability** — every order's outcome (success or the exact validation
  failure) must be queryable after the fact, not just streamed once.

### Out of scope (explicitly)
- Payment **capture**/settlement, refunds, chargebacks — this design only
  reaches "payment method authorized", i.e. `data-storage`/`resilience-failure`
  concerns of a payments service are assumed, not designed here.
- Fulfillment/warehouse routing, shipping-label generation, returns.
- The internal design of the inventory, pricing/tax, address-validation, and
  payment services — they're treated as existing dependencies this API calls.
- Multi-region/multi-tenant data residency — assumed single-region for this
  pass (see §8 for what changes).
- Order **editing** after creation (cancel/modify) — only creation is in scope.

### Assumptions (stated, revisable)
- Callers are trusted server-to-server integrations (merchant backends,
  internal tools, or the CLI) — not untrusted public browser clients — so auth
  is an API key/OAuth client-credential concern, not designed in depth here.
- Downstream dependencies (inventory, pricing, payment) are network calls with
  their own rate limits, not infinitely scalable — this shapes §2 and §7.
- Average order line is ~5 items, ~800 bytes as JSON.

## 2. Scale estimates

| Quantity | Value | Basis |
|---|---|---|
| Orders per submission | 1 – 4,500 | stated requirement |
| Order line size (JSON) | ~800 B | avg ~5 items/order |
| 4,500-order payload, inline | ~3.6 MB | 4,500 × 800 B |
| Downstream calls per order | 3 (inventory reserve, price/tax, payment auth) | pipeline in §3 |
| Downstream call latency | p50 150 ms / p99 400 ms | assumed dependency SLA |
| Assumed inventory-service quota | ~100 req/s per tenant | typical partner-API ceiling — **confirm** |
| Sequential time for 1 order | ~450 ms – 1.2 s | 3 calls, partially parallel |
| Sequential time for 4,500 orders, 1 worker | ~45–90 min | unacceptable → must parallelize |
| Time for 4,500 orders, bounded by 100 rps quota | ~45 s (inventory step alone) | 4,500 / 100 rps |
| Concurrent bulk jobs to plan for | ~20 system-wide | assumption, revisit with real traffic |
| In-flight line items at that concurrency | ~90,000 | 20 × 4,500 |

**What the numbers force:**
- A single order must **not** wait behind a bulk job — they need independent
  scheduling lanes (→ §3/§4, `task-scheduling` priority queues), otherwise a
  4,500-line job occupying every worker starves single-order latency.
- 4,500 orders cannot run one-at-a-time (45–90 min) — needs a **worker pool**,
  but concurrency is capped by the **slowest downstream quota**, not by our own
  compute. Rate-limit workers to each downstream's budget rather than letting
  the pool free-run into 429s (→ `resilience-failure`).
- A 3.6 MB inline JSON body is within most gateway payload caps today but
  leaves no headroom if line size or the ceiling grows — the bulk endpoint
  should support a **file-based** ingestion path so the inline-body limit
  never has to be renegotiated when the ceiling moves (→ §3).
- 90,000 in-flight line items at peak means the job/line-item store and its
  indexes (§5) must be built for that write volume, not just "a few rows."

## 3. API & CLI (entry points)

Three concerns, three endpoint families: **prevalidate**, **create**, **track**.
All non-idempotent writes require `Idempotency-Key`; all list/results reads use
cursor pagination (§`api-design`).

### 3.1 Prevalidation — "would this order work?"

A composite endpoint runs the same checks the create pipeline runs, without
reserving anything:

```
POST /v1/orders:validate
{
  "customer_id": "cus_9f2",
  "shipping_address": { "line1": "...", "city": "...", "postal_code": "...", "country": "US" },
  "payment_method_id": "pm_781",
  "items": [ { "sku": "SKU-1001", "quantity": 2 } ]
}

200 OK
{
  "valid": false,
  "checks": {
    "inventory":       { "ok": true },
    "pricing":         { "ok": true, "subtotal": 4200, "tax": 336, "total": 4536, "currency": "usd" },
    "address":         { "ok": true, "normalized": { "...": "..." } },
    "payment_method":  { "ok": false, "code": "payment_method_expired" }
  },
  "errors": [
    { "field": "payment_method_id", "code": "payment_method_expired", "message": "Card expired 2025-11" }
  ],
  "validation_token": "vt_5e1c...",   // present only when valid == true
  "expires_at": "2026-09-09T12:03:00Z"  // 60s TTL
}
```

- Each check (`inventory`, `pricing`, `address`, `payment_method`) is also
  independently reachable (`POST /v1/validations/inventory`,
  `/v1/validations/pricing`, `/v1/validations/address`,
  `/v1/validations/payment-method`) for callers that only need one — e.g. an
  address-autocomplete widget. `orders:validate` orchestrates all four and is
  what the CLI's `orders validate` and the create pipeline itself both call —
  **one code path**, so "it validated" and "it creates" never drift apart.
- **`validation_token`** is an optimization, not a trust boundary: presenting
  it to create *skips re-running the checks* but the create path still does its
  own atomic reserve — inventory can change in the 60s window, so a stale
  token degrades to a normal recheck-and-reserve rather than being trusted
  blindly (TOCTOU is a `consistency-coordination` concern the reservation step
  owns, not something a client-side check can promise).
- Bulk dry-run: `POST /v1/order-jobs?mode=validate_only` (see 3.2) — same
  ingestion shape, no orders are created, results carry per-line `checks`.

### 3.2 Create — one, a few, or 4,500

**Single / small batch (synchronous, ≤ 25 lines):**

```
POST /v1/orders
Idempotency-Key: 3f9a2e7c-...        # required, one per logical order
{
  "customer_id": "cus_9f2",
  "shipping_address": { "...": "..." },
  "payment_method_id": "pm_781",
  "items": [ { "sku": "SKU-1001", "quantity": 2 } ],
  "validation_token": "vt_5e1c..."    // optional, from §3.1
}

201 Created
{ "order_id": "ord_88213", "status": "created", "total": 4536, "currency": "usd" }

422 Unprocessable Entity  // same error envelope as validate — reuses §3.1's checks
{ "error": { "code": "insufficient_inventory", "message": "...", "request_id": "req_...", "retryable": false } }
```

`items` accepts a top-level array of order objects for a small batch:
`POST /v1/orders { "orders": [ {...}, {...}, {...} ] }` → `207 Multi-Status`
with one result per input order, same shape as a single result, in input
order. Capped at 25 so the call stays synchronous and bounded; above that,
use the bulk job endpoint below (the CLI makes this cutover automatic — see
3.3).

**Bulk (async, up to the documented ceiling — 4,500 in scope today):**

```
POST /v1/order-jobs
Idempotency-Key: b7e1-...            # scopes the whole job — resubmit-safe
{
  "orders": [ {...}, {...} ]         // inline, capped at 500 lines / ~500KB
}
# or, above 500 lines / for anything file-sized:
{
  "source": { "type": "ndjson", "upload_id": "up_44a1" }   // from POST /v1/uploads
}

202 Accepted
{ "job_id": "job_5f11", "status": "queued", "total_lines": 4500 }
```

- Each order line carries its own client-supplied `external_id` (the line's
  idempotency key). Resubmitting the same job — same `Idempotency-Key` header
  — returns the **existing** job rather than starting a second one; if a job
  is retried/resumed after a partial run, lines whose `external_id` already
  succeeded are returned as-is, not recreated (→ §7).
- `POST /v1/uploads` returns a short-TTL presigned upload URL + `upload_id`
  (the `blob-store` pattern) — the client (or CLI) PUTs the NDJSON file
  directly to storage, then references it in `POST /v1/order-jobs`. This is
  why the bulk path never has to renegotiate an inline-body size ceiling as
  volume grows (§2).
- `POST /v1/order-jobs/{job_id}:cancel` stops dispatch of remaining
  **queued** lines; in-flight and completed lines are unaffected.

### 3.3 Track

```
GET /v1/order-jobs/{job_id}
200 OK
{
  "job_id": "job_5f11", "status": "processing",   // queued|processing|completed|completed_with_errors|failed|canceled
  "total_lines": 4500, "succeeded": 3120, "failed": 40, "pending": 1340
}

GET /v1/order-jobs/{job_id}/results?limit=200&cursor=eyJ...
200 OK
{
  "data": [
    { "line": 1, "external_id": "po-2026-000001", "status": "succeeded", "order_id": "ord_88213" },
    { "line": 2, "external_id": "po-2026-000002", "status": "failed", "error": { "code": "insufficient_inventory", "...": "..." } }
  ],
  "next_cursor": "eyJ...", "has_more": true
}

GET /v1/orders/{order_id}   // single-order lookup, used by both paths
```

### 3.4 CLI

The CLI is a thin client over the same contract — no logic the API doesn't
already expose, so CLI and API never disagree on behavior.

```
orders validate --file order.json                 # composite prevalidation, 1 order
orders validate --file orders.ndjson --bulk        # dry-run a whole file, prints a summary + per-line errors

orders create --file order.json [--idempotency-key <key>]
orders create --file orders.json --batch           # 2..25 orders, synchronous, one result per line

orders bulk submit --file orders.ndjson [--idempotency-key <key>]
   # ≤500 lines  -> inline POST /v1/order-jobs
   # >500 lines   -> POST /v1/uploads, PUT file, then POST /v1/order-jobs {source}
   # prints job_id; add --wait to block and poll to a terminal state

orders bulk status <job_id> [--watch]              # polls until terminal when --watch
orders bulk results <job_id> [--failed-only] [--format table|ndjson] [--out results.ndjson]
orders bulk cancel <job_id>

orders get <order_id>
```

Exit codes (so bulk submission is scriptable in CI): `0` all lines succeeded,
`2` completed with some line failures (`--allow-partial-failure` to keep this
from being treated as an error by the caller's own script), `1` job-level
failure or a validation error on a single/small-batch call, `130` interrupted
locally (the job itself keeps running server-side — `--watch` can be resumed
against the same `job_id`).

## 4. High-level design

```
                         ┌──────────────┐
 CLI / caller ──POST────▶│ API gateway  │
                         └──────┬───────┘
        single/small (≤25) │            │ bulk (POST /v1/order-jobs)
                            ▼            ▼
                    ┌───────────────┐  ┌────────────────────┐
                    │ Order service │  │ Job intake          │──▶ blob store (NDJSON uploads)
                    │ (sync path)   │  │ (writes job + lines) │
                    └───────┬───────┘  └─────────┬───────────┘
                            │                     ▼
                            │            ┌──────────────────┐
                            │            │ Line-item queue    │  (priority: sync lane ≠ bulk lane)
                            │            └─────────┬─────────┘
                            │                      ▼
                            │            ┌──────────────────┐
                            └───────────▶│ Order pipeline    │  (shared code path §3.1/3.2)
                                         │  1. inventory check/reserve
                                         │  2. pricing/tax
                                         │  3. payment auth
                                         │  4. persist order
                                         └────────┬──────────┘
                                                  ▼
                                     ┌────────────────────────┐
                                     │ orders / order_jobs /   │
                                     │ order_job_items store   │
                                     └────────────────────────┘
```

- **API gateway** — one line: authn/authz, rate limiting per caller (protects
  the service from a runaway CLI script), routes sync vs. job intake.
- **Order pipeline** is the single implementation both the sync endpoint and
  the bulk workers call per line — this is what makes §3.1's "one code path"
  claim true, and what makes partial-failure isolation (§1) fall out for
  free: each line runs the pipeline independently and reports its own result.
- **Line-item queue** has (at least) two lanes/priorities — sync-path calls
  don't sit behind a 4,500-line job (§2's fairness requirement) — this is a
  `task-scheduling` priority-queue concern, not a new component.
- **Blob store** exists only for the upload path (>500 lines) — a `blob-store`
  building block, not a bespoke file service.

## 5. Data model

- **`orders`** — PK `order_id`. Unique index on `(tenant_id, idempotency_key)`
  for the sync path's dedupe. Columns: customer_id, status, totals, currency,
  `job_id` (nullable — set when created via a bulk job), created_at.
- **`order_jobs`** — PK `job_id`. Unique index on `(tenant_id, idempotency_key)`
  so a resubmitted job body returns the same job. Columns: status, total_lines,
  succeeded/failed/pending counters (updated by workers, not recomputed by
  scanning), source (`inline`|`upload_id`), created_at, completed_at.
- **`order_job_items`** — PK `(job_id, line_index)`. Unique index on
  `(job_id, external_id)` — this is the row that makes a resumed/retried job
  idempotent per line: on reprocessing, a line whose `external_id` already has
  a `succeeded` row is skipped, not re-run. Columns: status, order_id
  (nullable), error (nullable), attempt_count. `GET .../results` pages on
  `(job_id, line_index)` — a stable, monotonic cursor key.
- **`idempotency_keys`** (sync path) — `(tenant_id, idempotency_key)` →
  stored response, TTL 24h, per the standard `api-design` idempotency
  state-machine (pending/complete, reject on same-key-different-body).

Sharding: not needed at this scale (§2's ~90k in-flight rows is a normal
OLTP write volume); if it becomes one, `job_id` is the natural shard key for
`order_job_items` since every access pattern here is job-scoped.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| Sync endpoint (≤25) + separate async job endpoint (up to 4,500+) | Keeps single-order latency low; lets bulk scale independently | Two code paths to keep behaviorally identical (mitigated: both call the same order pipeline, §4) | The sync cutover (25) is wrong for real traffic → tune, don't redesign |
| Inline body (≤500 lines) *or* presigned-upload for bulk | Never hits a gateway payload ceiling as batch size grows | Two ingestion paths in the client/CLI to implement | Gateway payload limits change → could raise the inline threshold instead |
| Composite `orders:validate` reused by the create pipeline | Prevalidation and creation can't silently drift apart | One more endpoint family to design/document | Checks diverge in cost so much that create needs a cheaper subset → split then |
| `validation_token` (60s TTL) as an optimization, not a trust boundary | Cheap re-check on create when nothing changed | A stale token still forces a full recheck — no real "skip" under contention | Inventory volatility is low enough to trust a longer TTL → extend it |
| Idempotency-Key per order **and** per job, `external_id` per line | Safe retries at every granularity (single call, whole job, one line within a resumed job) | Three idempotency scopes to reason about instead of one | Never — this is what makes retries at any level safe |
| Worker pool rate-limited to each downstream's quota (not just our own concurrency) | Bulk jobs finish in ~seconds-to-minutes instead of hours, without 429-storming a dependency | Job completion time is capped by someone else's quota, not ours | A downstream offers a real batch/bulk API → call that instead of N single calls |
| Priority lanes for sync vs. bulk line-item dispatch | A 4,500-line job can't starve single-order latency | Scheduler has to be lane-aware, not a plain FIFO queue | Traffic mix makes this moot (e.g. bulk becomes the only path) |

## 7. Failure modes & degradation

- **Downstream (inventory/pricing/payment) slow or down** — each call is
  wrapped in a timeout + circuit breaker (`resilience-failure`). Sync path:
  the order fails fast with a `retryable: true` 5xx rather than hanging.
  Bulk path: the breaker trips, dispatch of *new* lines from that job pauses
  and backs off with jitter; already-succeeded lines stand; job status
  surfaces as `processing` with a `degraded` flag rather than silently
  stalling. No line is retried past its own attempt-count budget.
- **Client retries a timed-out single create** — same `Idempotency-Key` →
  the stored response is replayed, not re-executed (§5's idempotency table).
- **Client resubmits the same bulk file after a network blip** — same job
  `Idempotency-Key` → the existing `job_id` is returned; if the first
  submission had already progressed, per-line `external_id` dedup means
  already-succeeded lines are not recreated (§5's `order_job_items` unique
  index), so a full-file resubmission is safe to just re-run.
- **Worker crashes mid-line** — the queue's visibility timeout requeues the
  line to another worker; because the order pipeline itself is idempotent on
  `external_id`, a line that actually completed just before the crash (but
  before ack) is detected as already-succeeded on the redelivery, not
  double-created.
- **One malformed/unfulfillable line in 4,500** — fails independently
  (`order_job_items.status = failed` with its `error`), the other 4,499
  proceed; the job's final status is `completed_with_errors`, never `failed`,
  for a partial outcome — a whole-job `failed` status is reserved for the
  job never being able to run at all (e.g. bad `source.upload_id`).
- **Upload interrupted (large NDJSON)** — the client/CLI simply retries the
  PUT and resubmits `POST /v1/order-jobs` with the same job `Idempotency-Key`;
  nothing has been processed yet, so this is a clean retry, not a special case.
- **What the user sees:** a sync call either succeeds or comes back with a
  precise, actionable error in ≤1.5s; a bulk job always finishes in a
  *terminal* state visible via `GET /v1/order-jobs/{id}` (never "stuck"), with
  per-line detail available for exactly the lines that failed — never an
  all-or-nothing rollback of 4,500 orders because of one bad SKU.

## 8. Scale evolution

**Current bottleneck:** the slowest downstream dependency's request quota
(§2's ~100 rps assumption), not our own compute or storage — a 4,500-line job
is already dominated by that, not by queue throughput.

**At 10× (≈45,000 orders/submission):**
- The inline-body path disappears entirely (mandatory file upload above a
  much lower line count than today's 500).
- A single `order_jobs` row's line count stops being "a job" and becomes "a
  job with shards" — split dispatch across multiple queue partitions keyed by
  `job_id` so one giant job doesn't monopolize the worker pool the way a
  single-partition queue would.
- The per-order downstream fan-out (3 calls/order) becomes the real limiter —
  worth negotiating actual **batch** endpoints with the inventory/pricing/
  payment providers (reserve 100 SKUs in one call) instead of 45,000×3
  individual calls; that's a `service-decomposition`/contract conversation
  with those teams, not something this API alone can fix.
- **Signal to watch:** job completion-time p95 climbing past a documented SLA,
  or downstream 429 rate on the worker pool rising despite the rate limiter —
  either means the quota assumption in §2 needs renegotiating before volume
  grows further.

## 9. Open questions

- Real downstream quotas (inventory/pricing/payment) — §2's 100 rps is an
  assumption; confirm against actual partner/service SLAs before sizing the
  worker pool for production.
- Multi-tenant fairness: should the priority lanes in §4 be per-tenant as well
  as sync-vs-bulk, so one tenant's 4,500-line job can't starve another
  tenant's bulk jobs (not just their sync calls)?
- Whether `validation_token` should be widened to also pin a *price* quote
  (e.g. for a checkout flow where price must not drift between validate and
  create) — deferred here since this design's create path always recomputes
  price authoritatively.
- Auth model (API key vs. OAuth client-credentials) — assumed but not
  designed; doesn't change the shapes above either way.

---
### Validation (fill-in gate)
- [x] Every row in §6 has a non-empty **Worsens** and a **breaking point**.
- [x] §2 estimates carry units and state their assumptions.
- [x] §7 names a degradation path per critical dependency (not just "retry").
- [x] Each component in §4 ties back to a requirement/number in §1–§2.
- [x] Coverage sweep: IDs (`order_id`/`job_id`/`external_id` scheme, §5) ✓;
      media — n/a (no binary assets beyond the NDJSON upload, covered by
      `blob-store`); search — n/a (no order search UI in scope, §1);
      logs/SLOs — deferred to `observability`/`distributed-logging`, not
      re-derived here.

**Weakest dimension:** §9's unconfirmed downstream quotas — the entire worker-
pool sizing and completion-time story (§2, §8) rests on an assumed 100 rps
ceiling that needs validating against the real inventory/pricing/payment
services before this is production-sized.
