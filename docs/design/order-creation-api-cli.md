# Design: Order Creation API & CLI (Active Directory group orders)

A write-up structured around the reasoning loop (`system-design` skill), composing
`requirements-scoping` → `back-of-the-envelope` → `api-design` → `data-storage` →
`task-scheduling` / `messaging-streaming` → `resilience-failure`.

**Domain note:** an "order" here is a request to **create one Active Directory
(AD) group** — not a physical-goods order. There is no inventory, pricing, or
payment; the downstream dependency is an AD/directory provisioning API. Every
order gets an **order number** the caller can look up either by that number or
by the **group name** it was ordered for.

## 1. Problem & scope

**One sentence:** A service (and a CLI on top of it) that lets a caller order
the creation of AD groups — one at a time, a handful at a time, or up to
~4,500 in a single submission — each order trackable by its order number *or*
its group name, skipping a group that's already been ordered unless the
caller explicitly forces a re-order.

### Functional requirements
1. **Order a single group's creation**, synchronously, and get back an
   **order number** plus final status (`succeeded` / `failed` / `skipped`) in
   one round trip.
2. **Order a small set of groups** (e.g. 2–3) in one call, each evaluated and
   reported independently — one bad group must not block the good ones.
3. **Order a large batch of groups** (up to **4,500** in scope; designed not
   to fall over above that) without forcing the caller to hold a connection
   open for minutes.
4. **Prevalidate order parameters** — group-name policy/format, requester
   permission to own the group, target OU/container validity, and whether the
   group has already been ordered — via dedicated calls the caller can make
   *before* committing to order, for both a single group and a whole batch
   (dry run).
5. **Look up an order by order number *or* by group name**, and see its
   status — `succeeded` (with the order number), `failed` (with an error
   message), or `skipped` (with the order number of the pre-existing order it
   was skipped in favor of).
6. **Skip duplicate orders.** If a group has already been ordered
   (successfully, or is currently in flight), a new order for the same group
   name is **skipped**, not re-attempted — *unless* the caller passes
   `force=true`, in which case a new order is created regardless.
7. **CLI** wraps all of the above: validate, order (single/small-batch),
   submit a bulk file, poll/watch a bulk job, fetch results, get one order by
   number or by group name.

### Non-functional constraints
- **Latency** — single-order create: p99 < 1.5s end-to-end (including the
  prevalidation checks the server itself still runs — see §3). Bulk
  submission is *accepted* in < 2s regardless of batch size; the batch itself
  is processed asynchronously.
- **Throughput / batch ceiling** — up to 4,500 order lines per logical
  submission, and the system should not need a redesign for a moderately
  higher ceiling (see §8).
- **Correctness under retry** — a client that retries a timed-out request (a
  single order, or a whole bulk submission) must never create the same order
  — or the same AD group — twice.
- **No duplicate groups.** The "already ordered → skip" rule (req. 6) must
  hold under concurrent submission of the same group name (two lines in one
  bulk file, two overlapping requests) — an app-level check alone races; see
  §5/§6.
- **Partial-failure isolation** — one malformed, policy-violating, or
  already-ordered line in a 4,500-line batch must not fail the other 4,499.
- **Fairness** — a giant bulk job must not starve single-order latency for
  other callers; they are scheduled independently.
- **Auditability** — every order's outcome (succeeded / failed / skipped, and
  the exact error on failure) must be queryable after the fact by order
  number *or* group name, not just streamed once.

### Out of scope (explicitly)
- The AD/directory provisioning engine itself (the actual LDAP/Graph-API
  calls that create the group object, set its owner, add members) — treated
  as an existing downstream service this API calls.
- Group **lifecycle after creation** — membership changes, renaming,
  deletion, ownership transfer. This design only covers *ordering the
  creation*.
- Detecting a group that was created **directly in AD**, outside this system
  (no order record at all) — the duplicate check in this design is against
  *our own order history*, not a live AD directory scan. Flagged as an open
  question in §9.
- Auth/authz model for who may order which groups — assumed to exist (API
  key / SSO), not designed here.
- Multi-region/multi-tenant data residency — assumed single-region/single-
  directory for this pass (see §8 for what changes).

### Assumptions (stated, revisable)
- **Group name is the natural dedupe key** — it's unique within the target
  AD domain/OU, so "has this group already been ordered" is answered by
  looking up the group name, not a separate client-supplied id. (If multiple
  directories/domains are in play, the key is `(domain, group_name)` — noted
  in §5.)
- Callers are trusted server-to-server integrations (internal tools, service
  catalogs, or the CLI) — not untrusted public browser clients.
- The downstream AD provisioning API is a network call with its own rate
  limit, not infinitely scalable — this shapes §2 and §7.
- Average order line (group name, type, description, owner, optional members)
  is ~500 bytes as JSON.

## 2. Scale estimates

| Quantity | Value | Basis |
|---|---|---|
| Orders per submission | 1 – 4,500 | stated requirement |
| Order line size (JSON) | ~500 B | group_name/type/description/owner/members |
| 4,500-order payload, inline | ~2.3 MB | 4,500 × 500 B |
| Downstream calls per order | 1–2 (create-group; optionally add-members) | pipeline in §3 |
| Downstream call latency | p50 150 ms / p99 400 ms | assumed AD/Graph API SLA |
| Assumed AD-provisioning quota | ~50–100 req/s per tenant/app registration | typical directory-API throttle — **confirm** |
| Sequential time for 1 order | ~150–400 ms | 1 create call, +1 if members set |
| Sequential time for 4,500 orders, 1 worker | ~11–30 min | unacceptable → must parallelize |
| Time for 4,500 orders, bounded by 100 rps quota | ~45 s | 4,500 / 100 rps |
| Concurrent bulk jobs to plan for | ~20 system-wide | assumption, revisit with real traffic |
| In-flight line items at that concurrency | ~90,000 | 20 × 4,500 |

**What the numbers force:**
- A single order must **not** wait behind a bulk job — independent scheduling
  lanes (→ §3/§4, `task-scheduling` priority queues), otherwise a 4,500-line
  job occupying every worker starves single-order latency.
- 4,500 orders cannot run one-at-a-time (11–30 min) — needs a **worker
  pool**, but concurrency is capped by the **AD provisioning quota**, not our
  own compute. Rate-limit workers to that budget rather than free-running
  into 429/throttling responses (→ `resilience-failure`).
- A ~2.3 MB inline JSON body is comfortably within gateway payload caps
  today, but the bulk endpoint still supports a **file-based** ingestion path
  (§3) so a future higher ceiling, or larger group objects (long member
  lists), never forces a limit renegotiation.
- 90,000 in-flight line items at peak means the job/line-item store, and its
  `group_name` lookup index (§5), must be built for that write volume.

## 3. API & CLI (entry points)

Three concerns, three endpoint families: **prevalidate**, **order**, **track**.
All non-idempotent writes require `Idempotency-Key`; all list/results reads
use cursor pagination (§`api-design`).

### 3.1 The order object

Every line — single order, batch line, or bulk-file row — is the same shape:

```json
{
  "group_name": "GRP-Finance-ReadOnly",
  "group_type": "security",            // security | distribution
  "description": "Read-only access to Finance reports",
  "owner": "alice@corp.com",           // AD principal that manages the group
  "parent_ou": "OU=Groups,OU=Finance,DC=corp,DC=com",  // optional, defaults per policy
  "members": ["bob@corp.com"],         // optional initial members
  "force": false                       // optional, default false — see 3.3
}
```

### 3.2 Prevalidation — "would this order work?"

A composite endpoint runs the same checks the create pipeline runs, without
ordering anything:

```
POST /v1/orders:validate
{ "group_name": "GRP-Finance-ReadOnly", "group_type": "security",
  "owner": "alice@corp.com", "parent_ou": "OU=Groups,...,DC=com" }

200 OK
{
  "valid": false,
  "checks": {
    "name_policy":  { "ok": true },                     // naming convention/format/length
    "duplicate":    { "ok": false, "code": "already_ordered",
                       "existing_order_number": "ord_77190", "existing_status": "succeeded" },
    "owner":        { "ok": true },                      // owner is a valid, permitted AD principal
    "parent_ou":    { "ok": true }                        // OU exists and is writable by this caller
  },
  "errors": [
    { "field": "group_name", "code": "already_ordered",
      "message": "GRP-Finance-ReadOnly was already ordered (ord_77190, succeeded)." }
  ]
}
```

- Each check is also independently reachable (`POST /v1/validations/name-policy`,
  `/v1/validations/duplicate`, `/v1/validations/owner`, `/v1/validations/parent-ou`)
  for callers that only need one — e.g. a name field with live validation in a
  request form. `orders:validate` orchestrates all four and is what the CLI's
  `orders validate` and the create pipeline itself both call — **one code
  path**, so "it validated" and "it orders" never drift apart.
- The `duplicate` check here is **advisory**: it reflects our order store at
  read time. The authoritative check is the one the create path performs
  under the DB's unique constraint (§5) — a prevalidation "not a duplicate"
  can still lose a race to a concurrent submission, which is why the create
  response (not just validate) is what a caller must trust.
- Bulk dry-run: `POST /v1/order-jobs?mode=validate_only` (see 3.3) — same
  ingestion shape, no orders are created, results carry per-line `checks`.

### 3.3 Order — one, a few, or 4,500

**Single / small batch (synchronous, ≤ 25 lines):**

```
POST /v1/orders
Idempotency-Key: 3f9a2e7c-...        # required, one per logical order
{ "group_name": "GRP-Finance-ReadOnly", "group_type": "security",
  "owner": "alice@corp.com", "parent_ou": "OU=Groups,...,DC=com" }

201 Created
{ "order_number": "ord_88213", "status": "succeeded", "group_name": "GRP-Finance-ReadOnly" }

200 OK   // group already ordered, force not set — order is skipped, not an error
{ "order_number": "ord_77190", "status": "skipped", "group_name": "GRP-Finance-ReadOnly",
  "reason": "already_ordered" }

422 Unprocessable Entity  // same error envelope as validate — reuses §3.2's checks
{ "order_number": "ord_88214", "status": "failed",
  "error": { "code": "invalid_parent_ou", "message": "OU does not exist or is not writable.",
             "request_id": "req_...", "retryable": false } }
```

Every response — success, skip, or failure — carries an `order_number`: a
failed attempt is still a recorded, lookup-able order (req. 5). `orders`
accepts a top-level array for a small batch:
`POST /v1/orders { "orders": [ {...}, {...}, {...} ] }` → `207 Multi-Status`
with one result per input order, same shape as above, in input order. Capped
at 25 so the call stays synchronous and bounded; above that, use the bulk job
endpoint below (the CLI makes this cutover automatic — see 3.5).

**`force=true`** bypasses the duplicate skip for that line and orders a new
group-creation attempt regardless of order history. It does **not** bypass
AD's own uniqueness: if the group genuinely still exists in AD, the
downstream create call itself rejects it and the order comes back `failed`
with `code: "group_already_exists_in_ad"` — `force` re-attempts, it never
fabricates a group AD would refuse to create.

**Bulk (async, up to the documented ceiling — 4,500 in scope today):**

```
POST /v1/order-jobs
Idempotency-Key: b7e1-...            # scopes the whole job — resubmit-safe
{ "orders": [ {...}, {...} ] }       // inline, capped at 500 lines / ~250KB
# or, above 500 lines / for anything file-sized:
{ "source": { "type": "ndjson", "upload_id": "up_44a1" } }   // from POST /v1/uploads

202 Accepted
{ "job_id": "job_5f11", "status": "queued", "total_lines": 4500 }
```

- `group_name` doubles as each line's dedupe/idempotency key — no separate
  `external_id` is needed (unlike a generic bulk-order API, the natural
  business key *is* the retry key here). Resubmitting the same job — same
  `Idempotency-Key` header — returns the **existing** job; if a job is
  retried/resumed after a partial run, lines whose `group_name` already
  succeeded are returned as-is, not recreated (→ §7).
- `POST /v1/uploads` returns a short-TTL presigned upload URL + `upload_id`
  (the `blob-store` pattern) for the file-ingestion path.
- `POST /v1/order-jobs/{job_id}:cancel` stops dispatch of remaining
  **queued** lines; in-flight and completed lines are unaffected.

### 3.4 Track

```
GET /v1/order-jobs/{job_id}
200 OK
{ "job_id": "job_5f11", "status": "processing",  // queued|processing|completed|completed_with_errors|failed|canceled
  "total_lines": 4500, "succeeded": 3020, "skipped": 100, "failed": 40, "pending": 1340 }

GET /v1/order-jobs/{job_id}/results?limit=200&cursor=eyJ...
200 OK
{ "data": [
    { "line": 1, "group_name": "GRP-Finance-ReadOnly", "status": "succeeded", "order_number": "ord_88213" },
    { "line": 2, "group_name": "GRP-Sales-All",         "status": "skipped",  "order_number": "ord_77190", "reason": "already_ordered" },
    { "line": 3, "group_name": "GRP-Ops-!!invalid",     "status": "failed",   "error": { "code": "invalid_group_name", "...": "..." } }
  ],
  "next_cursor": "eyJ...", "has_more": true }

GET /v1/orders/{order_number}                # lookup by order number
GET /v1/orders?group_name=GRP-Finance-ReadOnly # lookup by group name (req. 5) — returns the latest order for that name
```

`GET /v1/orders?group_name=...` is the same lookup the duplicate check (§3.2)
and the create-path dedupe (§5/§6) use internally — one more place the
"skip if already ordered" rule and "look it up by group name" requirement
share a single implementation.

### 3.5 CLI

The CLI is a thin client over the same contract — no logic the API doesn't
already expose, so CLI and API never disagree on behavior.

```
orders validate --file group.json                 # composite prevalidation, 1 group
orders validate --file groups.ndjson --bulk        # dry-run a whole file, prints a summary + per-line errors

orders create --file group.json [--force] [--idempotency-key <key>]
orders create --file groups.json --batch           # 2..25 groups, synchronous, one result per line

orders bulk submit --file groups.ndjson [--force] [--idempotency-key <key>]
   # ≤500 lines  -> inline POST /v1/order-jobs
   # >500 lines   -> POST /v1/uploads, PUT file, then POST /v1/order-jobs {source}
   # --force applies to every line unless a line sets its own "force" in the file
   # prints job_id; add --wait to block and poll to a terminal state

orders bulk status <job_id> [--watch]              # polls until terminal when --watch
orders bulk results <job_id> [--status failed|skipped|succeeded] [--format table|ndjson] [--out results.ndjson]
orders bulk cancel <job_id>

orders get <order_number>
orders get --group-name GRP-Finance-ReadOnly       # reverse lookup (req. 5)
```

Exit codes (so bulk submission is scriptable in CI): `0` all lines succeeded
or were legitimately skipped, `2` completed with some line **failures**
(`--allow-partial-failure` to keep this from failing the caller's own
script), `1` job-level failure or a validation error on a single/small-batch
call, `130` interrupted locally (the job itself keeps running server-side —
`--watch` can be resumed against the same `job_id`).

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
                            └───────────▶│ Order pipeline    │  (shared code path §3.2/3.3)
                                         │  1. name-policy check
                                         │  2. duplicate check (unique constraint is authoritative)
                                         │  3. AD group-create [+ add members]
                                         │  4. persist order (succeeded/failed/skipped)
                                         └────────┬──────────┘
                                                  ▼
                                     ┌────────────────────────┐
                                     │ orders / order_jobs /   │
                                     │ order_job_items store   │
                                     └────────────────────────┘
                                                  │
                                                  ▼
                                     ┌────────────────────────┐
                                     │ AD / directory          │  (downstream — out of scope, §1)
                                     │ provisioning API        │
                                     └────────────────────────┘
```

- **API gateway** — authn/authz, rate limiting per caller (protects the
  service from a runaway CLI script), routes sync vs. job intake.
- **Order pipeline** is the single implementation both the sync endpoint and
  the bulk workers call per line — this is what makes §3.2's "one code path"
  claim true, and what makes partial-failure isolation (§1) fall out for
  free: each line runs the pipeline independently and reports its own result.
  Step 2 (duplicate check) always re-verifies against the unique constraint
  at persist time, never trusting an earlier prevalidation result (§3.2).
- **Line-item queue** has (at least) two lanes/priorities — sync-path calls
  don't sit behind a 4,500-line job (§2's fairness requirement) — a
  `task-scheduling` priority-queue concern, not a new component.
- **Blob store** exists only for the upload path (>500 lines) — a
  `blob-store` building block, not a bespoke file service.

## 5. Data model

- **`orders`** — PK `order_number`. Columns: group_name, group_type, owner,
  parent_ou, status (`succeeded`|`failed`|`skipped`), error (nullable),
  skipped_order_number (nullable — set when status is `skipped`, pointing at
  the order it deferred to), `job_id` (nullable — set when ordered via a bulk
  job), created_at.
  - **Unique index on `(domain, group_name)` for rows where `status IN
    ('succeeded', 'pending', 'processing')`** (a partial/filtered unique
    index) — this is what makes "skip if already ordered" correct under
    concurrency (req. 6/NFR): two concurrent orders for the same group name
    race on this constraint, and only one wins the insert; the loser's
    pipeline observes the conflict and returns `skipped` pointing at the
    winner, rather than the app-level pre-check (§3.2) being trusted alone.
    A `failed` order does **not** hold the constraint, so a genuinely failed
    attempt (nothing was created) can be retried without being treated as a
    duplicate — this is the default dedupe rule (§9 flags it as confirmable,
    not a hard requirement from the prompt).
  - Unique index on `(tenant_id, idempotency_key)` for the sync path's
    retry-safety dedupe (separate from the group-name dedupe above — one is
    "don't double-execute *this* request," the other is "don't order a group
    that's already ordered," and they can both fire independently).
- **`order_jobs`** — PK `job_id`. Unique index on `(tenant_id, idempotency_key)`
  so a resubmitted job body returns the same job. Columns: status, total_lines,
  succeeded/skipped/failed/pending counters (updated by workers, not
  recomputed by scanning), source (`inline`|`upload_id`), created_at,
  completed_at.
- **`order_job_items`** — PK `(job_id, line_index)`. Unique index on
  `(job_id, group_name)` — this is the row that makes a resumed/retried job
  idempotent per line: on reprocessing, a line whose `group_name` already has
  a `succeeded` (or `skipped`) row is returned as-is, not re-run. Columns:
  status, order_number (nullable), error (nullable), attempt_count.
  `GET .../results` pages on `(job_id, line_index)` — a stable, monotonic
  cursor key.
- **`idempotency_keys`** (sync path) — `(tenant_id, idempotency_key)` →
  stored response, TTL 24h, per the standard `api-design` idempotency
  state-machine (pending/complete, reject on same-key-different-body).

Group-name lookup (`GET /v1/orders?group_name=...`, req. 5) is served by the
`(domain, group_name)` index above — the same index that enforces the
duplicate-skip rule, so "look up by group name" and "detect it's already
ordered" are the same query, not two implementations to keep in sync.

Sharding: not needed at this scale (§2's ~90k in-flight rows is a normal
OLTP write volume); if it becomes one, `job_id` is the natural shard key for
`order_job_items` since every access pattern here is job-scoped.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| Sync endpoint (≤25) + separate async job endpoint (up to 4,500+) | Keeps single-order latency low; lets bulk scale independently | Two code paths to keep behaviorally identical (mitigated: both call the same order pipeline, §4) | The sync cutover (25) is wrong for real traffic → tune, don't redesign |
| `group_name` as the dedupe/business key (no separate `external_id`) | One fewer field to require from callers; "look up by group name" and "already ordered" share one index (§5) | Renaming a group later has no clean story here (out of scope, §1) — the key is fixed at order time | Group names can legitimately repeat across domains → key becomes `(domain, group_name)` (already the plan, §5) |
| Skip-by-default on duplicate, `force=true` to override | Prevents accidental re-ordering of an existing/in-flight group; caller still gets the original order number back (req. 5/6) | A caller who *meant* to retry a `failed` order must know failed orders aren't skipped by default (§5) — needs to be documented, not surprising | Business wants "already ordered" to include failed attempts too → widen the partial-unique-index predicate |
| Duplicate check enforced by a **DB unique constraint**, not just the app-level `orders:validate` pre-check | Correct under concurrency — two racing submissions for the same group can't both succeed | The loser of the race gets its `skipped` result at insert time, not at validate time — slightly less "predictable" from the client's view | Never — this is the correctness backstop; relaxing it reopens the duplicate-group race |
| `force=true` still goes through the real AD create call (not a bypass of AD's own uniqueness) | A forced re-order of a group AD still has fails cleanly with a clear error, never silently duplicates | `force` looks like it "always creates" but sometimes still fails — needs documenting | Never — bypassing AD's own check would let this API create actual duplicate directory objects |
| Inline body (≤500 lines) *or* presigned-upload for bulk | Never hits a gateway payload ceiling as batch size grows | Two ingestion paths in the client/CLI to implement | Gateway payload limits change → could raise the inline threshold instead |
| Idempotency-Key per order **and** per job, `group_name` per line | Safe retries at every granularity (single call, whole job, one line within a resumed job) | Two idempotency scopes plus the business-key dedupe to reason about | Never — this is what makes retries at any level safe |
| Worker pool rate-limited to the AD provisioning quota (not just our own concurrency) | Bulk jobs finish in ~seconds-to-minutes instead of tens of minutes, without throttling storms against the directory API | Job completion time is capped by AD's quota, not ours | AD offers a real batch-create API → call that instead of N single calls |
| Priority lanes for sync vs. bulk line-item dispatch | A 4,500-line job can't starve single-order latency | Scheduler has to be lane-aware, not a plain FIFO queue | Traffic mix makes this moot (e.g. bulk becomes the only path) |

## 7. Failure modes & degradation

- **AD provisioning API slow or down** — each call is wrapped in a timeout +
  circuit breaker (`resilience-failure`). Sync path: the order fails fast
  with a `retryable: true` 5xx rather than hanging. Bulk path: the breaker
  trips, dispatch of *new* lines from that job pauses and backs off with
  jitter; already-succeeded/skipped lines stand; job status surfaces as
  `processing` with a `degraded` flag rather than silently stalling. No line
  is retried past its own attempt-count budget.
- **Client retries a timed-out single order** — same `Idempotency-Key` → the
  stored response is replayed, not re-executed (§5's idempotency table).
- **Two lines for the same `group_name` in one bulk file** (or two overlapping
  bulk jobs) — both hit the pipeline; the DB unique constraint (§5) admits
  the first, the second observes the conflict and is recorded `skipped`
  pointing at the first's order number — correct even though both were
  dispatched concurrently, because the constraint (not app logic) is the
  source of truth (req. 6/NFR).
- **Client resubmits the same bulk file after a network blip** — same job
  `Idempotency-Key` → the existing `job_id` is returned; per-line
  `group_name` dedup (§5's `order_job_items` unique index) means already-
  succeeded/skipped lines are not recreated, so a full-file resubmission is
  safe to just re-run.
- **Worker crashes mid-line** — the queue's visibility timeout requeues the
  line to another worker; because the order pipeline is idempotent on
  `group_name` via the DB constraint, a line that actually completed just
  before the crash (but before ack) is detected as already-succeeded on
  redelivery, not double-created.
- **One malformed name / policy violation / already-ordered line in 4,500**
  — fails or skips independently (`order_job_items.status = failed|skipped`
  with detail), the other 4,499 proceed; the job's final status is
  `completed_with_errors` only when there's a real `failed` line — an
  all-`skipped` outcome is still `completed`, since nothing went wrong.
- **Upload interrupted (large NDJSON)** — the client/CLI retries the PUT and
  resubmits `POST /v1/order-jobs` with the same job `Idempotency-Key`;
  nothing has been processed yet, so this is a clean retry.
- **What the user sees:** a sync call either succeeds, is cleanly skipped
  with the original order number, or comes back with a precise error, in
  ≤1.5s; a bulk job always finishes in a *terminal* state visible via
  `GET /v1/order-jobs/{id}` (never "stuck"), with per-line detail for exactly
  the lines that failed or were skipped — never an all-or-nothing rollback of
  4,500 orders because of one bad group name.

## 8. Scale evolution

**Current bottleneck:** the AD/directory provisioning API's request quota
(§2's ~50–100 rps assumption), not our own compute or storage — a 4,500-line
job is already dominated by that, not by queue throughput.

**At 10× (≈45,000 orders/submission):**
- The inline-body path disappears entirely (mandatory file upload above a
  much lower line count than today's 500).
- A single `order_jobs` row's line count stops being "a job" and becomes "a
  job with shards" — split dispatch across multiple queue partitions keyed by
  `job_id` so one giant job doesn't monopolize the worker pool.
- The per-order downstream call becomes the real limiter — worth checking
  whether the directory API offers a genuine **batch** group-create call
  instead of 45,000 individual calls; if not, this API's own worker-pool
  rate limiting is the only lever, and completion time scales linearly with
  the quota, which becomes the number to renegotiate.
- **Signal to watch:** job completion-time p95 climbing past a documented
  SLA, or throttling responses from the directory API rising despite the
  rate limiter — either means the quota assumption in §2 needs
  renegotiating before volume grows further.

## 9. Open questions

- **Does "already ordered" include failed attempts?** This design's default
  (§5/§6) is: skip only on `succeeded`/`pending`/`processing`, allow retry on
  `failed` without needing `force`. Confirm this matches intent — if a
  failed order should also require `force` to retry, the unique-index
  predicate in §5 changes from excluding `failed` to including everything.
- **Cross-domain group names.** If the same `group_name` can legitimately
  exist as separate groups in two different AD domains/forests, the dedupe
  key must be `(domain, group_name)` (already planned in §5) — confirm
  whether domain is always part of the order, or needs to be inferred from
  `parent_ou`.
- **Drift detection** (§1, out of scope): a group created directly in AD,
  bypassing this API, has no order record — `orders:validate`'s duplicate
  check won't see it, and a subsequent order would attempt creation and get
  a `group_already_exists_in_ad` failure from the downstream call, not a
  clean `skipped`. Worth deciding whether that failure should be
  auto-reclassified as `skipped` once observed, or left as a failure for a
  human to reconcile.
- Real AD provisioning quota — §2's 50–100 rps is an assumption; confirm
  against the actual directory API's throttling limits before sizing the
  worker pool for production.
- Auth model (API key vs. OAuth client-credentials, and how `owner` is
  authorized to receive a new group) — assumed but not designed; doesn't
  change the shapes above either way.

---
### Validation (fill-in gate)
- [x] Every row in §6 has a non-empty **Worsens** and a **breaking point**.
- [x] §2 estimates carry units and state their assumptions.
- [x] §7 names a degradation path per critical dependency (not just "retry").
- [x] Each component in §4 ties back to a requirement/number in §1–§2.
- [x] Coverage sweep: IDs (`order_number`/`job_id`/`group_name`-as-key
      scheme, §5) ✓; media — n/a (no binary assets beyond the NDJSON upload,
      covered by `blob-store`); search — n/a (order lookup is by number or
      exact group name, §3.4, not full-text search); logs/SLOs — deferred to
      `observability`/`distributed-logging`, not re-derived here.

**Weakest dimension:** §9's two behavioral defaults (does "already ordered"
include failed attempts; how cross-domain name collisions are keyed) are this
design's own choices, not confirmed requirements — they're load-bearing on
the unique-index shape in §5 and should be confirmed before implementation,
not just before scale.
