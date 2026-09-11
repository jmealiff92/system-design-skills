# Design: Order Creation API & CLI (Active Directory group orders)

A write-up structured around the reasoning loop (`system-design` skill), composing
`requirements-scoping` → `back-of-the-envelope` → `api-design` → `data-storage` →
`caching` → `task-scheduling` / `messaging-streaming` → `resilience-failure`.

**Domain note:** an "order" here is a request to **create one Active Directory
(AD) group** — not a physical-goods order. There is no inventory, pricing, or
payment. **This system does not call AD directly** — it submits the order to
a **Marketplace (mktplace) order API**, which accepts orders **asynchronously**
and provisions the group in AD on its own time. This system records mktplace's
own order id (`mktplace_order_number`) the moment mktplace accepts the
submission, then **reconciles** completion later by re-using the LDAP
existence-check API (§3.2) to confirm the group actually exists — confirmed
mechanism, §9 for the open parameters. Every order this system creates gets
its own **order number**, lookup-able by number or by **group name**, whose
`status` moves from `submitted` to `completed` as that reconciliation happens.

## 1. Problem & scope

**One sentence:** A service (and a CLI on top of it) that lets a caller order
the creation of AD groups — one at a time, a handful at a time, or up to
~4,500 in a single submission — each order trackable by its order number *or*
its group name, skipping a group that's already been ordered unless the
caller explicitly forces a re-order, with its status reflecting mktplace's
asynchronous fulfillment rather than a synchronous yes/no.

### Functional requirements
1. **Order a single group's creation** and get back an **order number** in
   one round trip: `submitted` (accepted by mktplace, `mktplace_order_number`
   recorded — provisioning not yet confirmed), `validation_failed`,
   `failed` (mktplace rejected the submission outright), or `skipped` — all
   four determined synchronously. **`completed`** (the group is confirmed to
   exist in AD) is reached later, asynchronously, by reconciliation (§4) —
   never returned by the initial call. Validation still happens as part of
   placing the order (§3.2), not as a separate required call.
2. **Order a small set of groups** (e.g. 2–3) in one call, each evaluated and
   reported independently — one bad group must not block the good ones.
3. **Order a large batch of groups** (up to **4,500** in scope; designed not
   to fall over above that) without forcing the caller to hold a connection
   open for minutes.
4. **Validate order parameters inline, on every order** — as the first step
   of placing the order (single, small-batch, or each line of a bulk job):
   group-name policy/format; `primary_owner`/`secondary_owner` id format
   (regex) and that they're distinct; both owners are **active full-time
   employees** (an API call); and the group **doesn't already exist in AD**
   (an LDAP lookup — this is what catches a group created outside this
   process entirely, not just one this system already has an order for, and
   is the same check reconciliation reuses, §4). A standalone dry-run call
   (§3.2) is *available* for a caller who wants to check before committing,
   but is optional, not a prerequisite.
5. **Look up an order by order number *or* by group name**, and see its
   current status — `submitted` (with the order number and
   `mktplace_order_number`), `completed` (confirmed created),
   `validation_failed` (with the specific reason), `failed` (with a
   downstream error message), or `skipped` (with the order number of the
   pre-existing order it was skipped in favor of). For a `submitted` order,
   this is a live value — the same lookup, re-polled, is how a caller learns
   it later became `completed`.
6. **Skip duplicate orders.** If a group has already been ordered — its
   order is `completed`, or still `submitted` and not yet confirmed either
   way — a new order for the same group name is **skipped**, not
   re-attempted — *unless* the caller passes `force=true`, in which case a
   new order is created regardless. A **`failed`** order is not "already
   ordered" — a new order for that group name proceeds normally, no `force`
   needed, and may carry different parameters than the failed attempt (e.g.
   a different `secondary_owner` after a `not_active_fte` rejection) —
   confirmed intended, not just a side effect of the dedupe rule (§9).
7. **CLI** wraps all of the above: validate, order (single/small-batch),
   submit a bulk file, poll/watch a bulk job, fetch results, get one order by
   number or by group name — including re-polling a `submitted` order until
   it reconciles to `completed`.

### Non-functional constraints
- **Latency of *acceptance*** — bounded by the 100/min mktplace rate limit
  (§2): a single order reaches `submitted` in ~1.5s p99 (one rate-limit
  token wait + one mktplace call); an N-order synchronous batch (N ≤ 25)
  scales as ≈N × 0.6s + mktplace latency. **Latency of *completion*
  (`submitted` → `completed`) is a separate, currently unbounded number** —
  it depends on mktplace's own provisioning time plus this system's
  reconciliation poll interval, neither of which is stated (§9). Do not
  conflate "the order call returned quickly" with "the group exists" — they
  are now two different claims.
- **Throughput / batch ceiling** — up to 4,500 order lines per logical
  submission, and the system should not need a redesign for a moderately
  higher ceiling (see §8).
- **Correctness under retry** — a client that retries a timed-out request (a
  single order, or a whole bulk submission) must never create the same order
  — or submit the same group-creation order to mktplace — twice.
- **No duplicate groups.** The "already ordered → skip" rule (req. 6) must
  hold under concurrent submission of the same group name (two lines in one
  bulk file, two overlapping requests) — an app-level check alone races; see
  §5/§6. It must also hold while an order is merely `submitted`, not just
  once `completed` — two callers racing to order the same group while the
  first is still awaiting reconciliation must not both succeed.
- **Partial-failure isolation** — one malformed, policy-violating, or
  already-ordered line in a 4,500-line batch must not fail the other 4,499.
- **Fairness** — a giant bulk job must not starve single-order latency for
  other callers; they are scheduled independently.
- **Auditability** — every order's outcome and current status (submitted /
  completed / validation_failed / failed / skipped, and the exact reason or
  error) must be queryable after the fact by order number *or* group name,
  not just streamed once — including watching a `submitted` order progress
  to `completed`.
- **Bounded load on the validation *and* reconciliation dependencies.** The
  LDAP group-existence check now serves **two** purposes — a one-off
  validation-time check, and a **repeated** reconciliation poll per
  `submitted` order until it resolves — and the active-FTE check is another
  network call this design would otherwise make once or twice per order. At
  4,500 orders/job that's thousands of calls to systems whose own capacity
  isn't stated. Caching (§3.2/§5) and a deliberate poll cadence (§4/§9)
  exist specifically to bound this, not just to shave latency.

### Out of scope (explicitly)
- **The Marketplace's own order-fulfillment process** — however mktplace
  actually provisions the group in AD (its own LDAP/Graph-API calls,
  retries, internal queueing, timing) is entirely mktplace's concern; this
  design only decides how it submits an order and how it later confirms the
  outcome via LDAP (§3.3, §4, §9).
- Group **lifecycle after creation** — membership changes, renaming,
  deletion, ownership transfer. This design only covers *ordering the
  creation*.
- **Reconciling** a group the LDAP existence check (§3.2) finds already in
  AD with no order record behind it (created outside this process) — this
  design *detects* that case at validation time (in scope), but backfilling
  an order record for it, or any broader drift audit/sync, is not designed
  here. Flagged in §9. (Not to be confused with **this design's own**
  reconciliation of `submitted` → `completed`, which *is* in scope, §4.)
- **The mktplace order API, LDAP existence-check API, and active-FTE-check
  API themselves** — their own availability, latency, and rate limits are
  assumed, not designed (§9); this design only decides how it calls, polls,
  and caches them.
- Auth/authz model for who may order which groups — assumed to exist (API
  key / SSO), not designed here.
- Multi-region/multi-tenant data residency — assumed single-region/single-
  directory for this pass (see §8 for what changes).

### Assumptions (stated, revisable)
- **Single AD domain.** Everything in this design targets one directory —
  `group_name` alone is the dedupe/lookup key (no domain qualifier needed).
- **Group name is the natural dedupe key** — it's unique in that domain, so
  "has this group already been ordered" is answered by looking up the group
  name, not a separate client-supplied id.
- Callers are trusted server-to-server integrations (internal tools, service
  catalogs, or the CLI) — not untrusted public browser clients.
- **Marketplace (mktplace) enforces a hard rate limit of 100 requests/
  minute** (confirmed — see §2/§6) on order submissions, shared across every
  caller of this API, not per-tenant.
- **This system submits an order to mktplace; it never calls AD or LDAP to
  create anything.** mktplace is a distinct system from the LDAP
  existence-check and FTE-check APIs — three separate downstream
  dependencies in total.
- **Mktplace is asynchronous (confirmed):** its response to a submission
  only confirms acceptance (`submitted`, with `mktplace_order_number`) —
  never that the AD group exists. This system reconciles completion itself,
  by re-checking LDAP existence for `submitted` orders (confirmed mechanism)
  on some poll cadence (§9 — the cadence itself is not yet specified).
- **There is no confirmed failure signal for a `submitted` order** — only a
  success signal (the group eventually appears in AD). What should happen
  to an order that never resolves (mktplace silently drops it, fails
  internally, etc.) is an open, unresolved question (§9) this design does
  not yet answer — flagged as the design's biggest current gap.
- Average order line (`group_name`, `primary_owner`, `secondary_owner`) is
  ~150 bytes as JSON.
- **`primary_owner` and `secondary_owner` must be different AD principals**
  — confirmed requirement, enforced as a blocking validation check (§3.2).
- **A `failed` order does not block a retry, and the retry may carry
  different parameters than the failed attempt** — confirmed: e.g. a caller
  who got `not_active_fte` on `secondary_owner` can resubmit the same
  `group_name` with a different `secondary_owner`, with no `force` needed
  (§5's unique index excludes `failed`). One consequence worth being
  explicit about: such a retry must use a **new** `Idempotency-Key`, not the
  failed attempt's — reusing the same key with a different body is a `422`
  key-reuse conflict by the standard idempotency contract (`api-design`).
- The owner id format (what the regex checks) is a corporate identifier —
  email or employee id — exact pattern owned by policy, not specified here
  (§9).
- "Active full-time employee" is a binary the FTE-check API returns; this
  design treats a `false`/not-found response as a blocking validation
  failure, not a warning.
- The LDAP existence-check and FTE-check APIs are **separate systems** from
  mktplace, with their own latency/rate limits — **not** assumed to share
  the 100/min budget (§2). Worth confirming (§9).
- **Owner reuse across a bulk job is high** — the same handful of managers/
  teams order most groups in a given submission, so the distinct-owner count
  in a 4,500-line job is assumed to be a small fraction of 9,000 (2 owners ×
  4,500). This is exactly what makes caching the FTE check valuable (§2/§6);
  flagged as an assumption because the real distribution isn't known.

## 2. Scale estimates

| Quantity | Value | Basis |
|---|---|---|
| Orders per submission | 1 – 4,500 | stated requirement |
| Order line size (JSON) | ~150 B | `group_name` + `primary_owner` + `secondary_owner` |
| 4,500-order payload, inline | ~0.7 MB | 4,500 × 150 B |
| Downstream calls per order at submission | 1 (submit order to mktplace) | pipeline in §3 |
| Mktplace order-submission latency | p50 150 ms / p99 400 ms | assumed SLA for the *accept* call — **confirm** |
| **Mktplace order-submission rate limit** | **100 requests/minute, system-wide** (confirmed) | this API must self-throttle to it |
| Effective downstream throughput (submission) | ~1.67 req/s | 100 ÷ 60 |
| Time to `submitted`, 1 order (steady state) | ~0.6 s wait for a token + ~150–400 ms mktplace call | rate limit dominates even a single order |
| Time to `submitted`, 3-order small batch | **~2–3 s** | ≈3 sequential token waits + mktplace latency each |
| Time to fully dispatch a 4,500-order bulk job | **~45 minutes** | 4,500 ÷ 100 per min |
| Time from `submitted` to `completed`, 1 order | **unknown — unstated (§9)** | depends on mktplace's own AD-provisioning time |
| Reconciliation poll interval (assumed) | ~60 s | not yet specified by the business (§9); used for the estimates below |
| Reconciliation LDAP calls per `submitted` order (assumed provisioning time ~2–5 min) | ~2–5 | one per poll cycle until confirmed, at the assumed 60s cadence |
| Reconciliation LDAP calls, 4,500-order bulk job | **up to ~9,000–22,500 additional** | 4,500 orders × ~2–5 polls each, on top of validation-time LDAP calls below |
| LDAP existence-check calls, validation only, uncached, 4,500-order job | up to 9,000 | once at validate (dry-run) + once at order, per line |
| FTE-check calls, uncached, 4,500-order job | up to 9,000 | 2 owners × 4,500 lines, before dedup |
| Distinct owners in a 4,500-order job (assumed) | ~50–200 | owner reuse assumption (§1) |
| FTE-check calls, **cached** (owner-id → TTL) | ~50–400 | one lookup per distinct owner |

Reconciliation roughly **doubles or triples** the LDAP existence-check's real
load on top of the validation-time estimate already flagged as
unconfirmed-capacity (§9) — worth re-reading that row with this addition in
mind. Neither the LDAP nor the FTE-check API's own rate limit/SLA is stated
(§9); mktplace's own typical provisioning time (the biggest unknown in this
table) isn't either.

**What the numbers force:**
- **The rate limit still governs *acceptance* throughput** — a 4,500-order
  job is still ~45 minutes to fully dispatch, for the same reasons as
  before (§2's original analysis holds unchanged for the `submitted` phase).
- **Reconciliation is a second, independent load source on LDAP**, driven by
  *how many orders are currently `submitted`* × *how long each takes to
  resolve* × *poll frequency* — none of which this design controls (mktplace
  controls the middle one). A naive "poll every submitted order every N
  seconds forever" approach could turn into unbounded LDAP load if
  mktplace's real provisioning time is much longer than assumed, or if
  orders never resolve (§1's flagged gap) and pile up as permanently-polled
  "zombies." The reconciliation design (§4) needs backoff and a cap, not a
  flat-interval sweep, once real numbers are known.
- **Sync calls must not queue behind a bulk job's *dispatch*** (still true,
  §6) — reconciliation is a separate background concern and doesn't compete
  with the rate-limited submission path at all, since LDAP isn't part of the
  100/min budget.
- The payload/storage numbers are small enough that neither inline-vs-upload
  ingestion (§3) nor the order store (§5) is a scaling concern — the rate
  limit and the *unknown* reconciliation load are the two real bottlenecks.

## 3. API & CLI (entry points)

Three concerns, three endpoint families: **validate**, **order**, **track**.
Validation is not a separate step a caller must perform first — it's the
first stage of the order pipeline itself (§4), run on every order call, and
also independently reachable for a dry run. All non-idempotent writes require
`Idempotency-Key`; all list/results reads use cursor pagination (§`api-design`).

### 3.1 The order object

Every line — single order, batch line, or bulk-file row — is the same shape:

```json
{
  "group_name": "GRP-Finance-ReadOnly",
  "primary_owner": "alice@corp.com",     // AD principal, primary owner of the group
  "secondary_owner": "bob@corp.com",     // AD principal, backup owner — must differ from primary_owner
  "force": false                         // optional, default false — see 3.3
}
```

Just the three business fields plus the `force` control flag — no
`group_type`, `description`, `parent_ou`, or `members`: with a single target
domain (§1) and a fixed system-wide placement/type policy, there's nothing
else for the caller to specify. Both owners are included in the order
submitted to mktplace (§3.2/§4); how mktplace maps them onto the AD group's
`managedBy` vs. a secondary owner is mktplace's own detail, not something
this API's contract needs to expose beyond the two fields above.

### 3.2 Validation — runs on every order; also callable standalone

Validation is **step 1 of the order pipeline** (§4) — every call to
`POST /v1/orders` or a bulk job line runs it automatically before anything
else happens, and a failing check comes back as part of the order response
itself (`status: "validation_failed"`, see §3.3), no separate call required.
The same logic is also exposed standalone for a caller who wants to check
*before* committing (e.g. a request form validating as the user types):

```
POST /v1/orders:validate
{ "group_name": "GRP-Finance-ReadOnly",
  "primary_owner": "alice@corp.com", "secondary_owner": "bob@corp.com" }

200 OK
{
  "valid": false,
  "checks": {
    "name_policy":     { "ok": true },                       // regex/format/length — local, no API call
    "primary_owner":   { "ok": true, "source": "cache" },     // format regex, then active-FTE check
    "secondary_owner": { "ok": false, "code": "not_active_fte", "source": "api" }
  },
  "errors": [
    { "field": "secondary_owner", "code": "not_active_fte",
      "message": "bob@corp.com is not an active full-time employee." }
  ],
  "availability": { "status": "not_found" }   // not in our order store, not in LDAP — informational, would proceed to order
}
```

Each owner check is really **two checks run in order**, cheapest first:
1. **Format** — a regex over the id (email/employee-id shape, §1) — local,
   free, no API call, no caching needed.
2. **Active FTE** — only run if the format passed; calls the FTE-check API,
   **cached** by owner id (§5) so 4,500 lines sharing ~50–200 distinct owners
   (§2) cost ~50–200 API calls, not ~9,000.

`name_policy` and both owner checks are the **blocking** validation checks —
any failing one is what the order pipeline reports as `validation_failed`;
the `errors` array (field/code/message) is exactly what that order response
carries.

`availability` replaces the earlier "duplicate" check and now has three
possible outcomes, checked cheapest-first so a request that's going to be
skipped never pays for an owner/FTE check it doesn't need:
- **`already_ordered`** — found in *our own order store*, either
  `completed` or still `submitted` (fast DB read, no API call) → the order
  will be `skipped`, pointing at the existing order.
- **`exists_in_ad`** — not in our store, but the **LDAP existence-check API**
  says the group is already there (§4) — this is what catches a group
  created outside this process entirely. Also `skipped`, but with no
  `existing_order_number` to point to (§3.3) — nothing here ordered it.
  **Cached** by group name with a short TTL (§5), mainly so a validate-then-
  order round trip for the same line doesn't hit LDAP twice — this is the
  **same LDAP call reconciliation (§4) also uses**, just triggered at a
  different point in the order's lifecycle.
- **`not_found`** — neither store has it; the order can proceed.

`availability` is reported separately from the blocking checks and is **not**
a validation failure — ordering an already-existing group isn't invalid
input, it's a request the pipeline *skips* (req. 6, §3.3).

Each blocking check is also independently reachable
(`POST /v1/validations/name-policy`, `/v1/validations/primary-owner`,
`/v1/validations/secondary-owner`) for callers that only need one.
`orders:validate` and the order pipeline's step 1 are **the same
implementation** — "it validated" and "it orders" can't drift apart.

The `availability` check here is **advisory**: it reflects a point-in-time
read (possibly cached) of our order store and LDAP. The authoritative check
for `already_ordered` is the DB's unique constraint (§5); for `exists_in_ad`,
it's whatever mktplace's own order processing does with a duplicate
submission, *plus* this system's own reconciliation loop, which would in any
case eventually find the group and mark an in-flight duplicate `completed`
rather than leave it inconsistent. A dry run showing `not_found` can still
lose a race, which is why the actual order response is what a caller must
trust for the outcome at that moment.

Bulk dry-run: `POST /v1/order-jobs?mode=validate_only` (see 3.3) — same
ingestion shape, no orders are created, results carry per-line `checks`.

### 3.3 Order — one, a few, or 4,500

**Single / small batch (synchronous, ≤ 25 lines):**

```
POST /v1/orders
Idempotency-Key: 3f9a2e7c-...        # required, one per logical order
{ "group_name": "GRP-Finance-ReadOnly",
  "primary_owner": "alice@corp.com", "secondary_owner": "bob@corp.com" }

202 Accepted   // mktplace accepted the order — NOT yet confirmed in AD (§1/§4)
{ "order_number": "ord_88213", "status": "submitted", "group_name": "GRP-Finance-ReadOnly",
  "mktplace_order_number": "MKT-2026-771102" }

200 OK   // found in our own order store (completed or still submitted), force not set — skipped, not an error
{ "order_number": "ord_77190", "status": "skipped", "group_name": "GRP-Finance-ReadOnly",
  "reason": "already_ordered" }

200 OK   // not in our store, but LDAP says the group already exists — created outside this process
{ "order_number": "ord_88219", "status": "skipped", "group_name": "GRP-Legacy-Ops",
  "reason": "exists_in_ad" }   // no existing_order_number — nothing here ordered it (§9)

422 Unprocessable Entity   // step 1 of the pipeline (§4) — no mktplace call, no FTE-check call was made
{ "order_number": "ord_88214", "status": "validation_failed", "group_name": "GRP-Ops-!!invalid",
  "reason": [
    { "field": "group_name", "code": "invalid_group_name",
      "message": "Group name may not contain '!' and must start with 'GRP-'." },
    { "field": "secondary_owner", "code": "not_active_fte",
      "message": "bob@corp.com is not an active full-time employee." }
  ] }

502 Bad Gateway   // input was valid, not a duplicate — mktplace rejected the submission itself, synchronously
{ "order_number": "ord_88215", "status": "failed", "group_name": "GRP-Finance-ReadOnly",
  "error": { "code": "mktplace_order_timeout", "message": "Marketplace did not respond in time.",
             "request_id": "req_...", "retryable": true } }
```

**Later — the same order, re-fetched after reconciliation (§4):**

```
GET /v1/orders/ord_88213
200 OK
{ "order_number": "ord_88213", "status": "completed", "group_name": "GRP-Finance-ReadOnly",
  "mktplace_order_number": "MKT-2026-771102", "completed_at": "2026-09-11T14:32:07Z" }
```

`202 Accepted` (not `201 Created`) on the happy path reflects what's actually
true: the order was *accepted for processing*, not *finished* — `201` would
overclaim. Every response — submitted, validation_failed, skipped, or
failed — still carries an `order_number`: even a rejected attempt is a
recorded, lookup-able order (req. 5). `validation_failed` (client's input,
fixable, no downstream call made) and `failed` (mktplace's own synchronous
rejection, often retryable) are kept as **distinct statuses** on purpose — a
caller can tell "fix your request" apart from "safe to retry" without
parsing the error code. `orders` accepts a top-level array for a small
batch: `POST /v1/orders { "orders": [ {...}, {...}, {...} ] }` →
`207 Multi-Status` with one result per input order, same shape as above, in
input order. Capped at 25 so the call stays synchronous and bounded; above
that, use the bulk job endpoint below (the CLI makes this cutover automatic
— see 3.5).

**`force=true`** bypasses the duplicate skip for that line (against both
`completed` and `submitted` orders) and submits a new group-creation order
to mktplace regardless of our own order history. It does **not** bypass
mktplace's own duplicate handling: if the group genuinely still exists in
AD, mktplace's own processing rejects it — synchronously as a `failed`
order if mktplace checks at submission time, or, if not, this system's own
reconciliation loop will simply find the group already exists and mark the
order `completed` anyway (a forced re-order of a group that already exists
converges to the same `completed` state, it just doesn't *create* a second
group) — `force` never fabricates a group mktplace would refuse to create.

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

**Job-level `status` tracks *dispatch* completion — every line has been
submitted, skipped, or rejected — not whether every `submitted` line has
since reconciled to `completed`.** Those two things are now different
claims (§1): a job can be `completed` (fully dispatched) while some of its
lines are still `submitted`, quietly reconciling in the background. Re-poll
`.../results` after job completion to see lines finish moving to
`completed`.

```
GET /v1/order-jobs/{job_id}
200 OK
{ "job_id": "job_5f11", "status": "processing",  // queued|processing|completed|completed_with_errors|failed|canceled — dispatch status
  "total_lines": 4500, "submitted": 2980, "completed": 40, "skipped": 100, "validation_failed": 15, "failed": 25, "pending": 1340 }

GET /v1/order-jobs/{job_id}/results?limit=200&cursor=eyJ...
200 OK
{ "data": [
    { "line": 1, "group_name": "GRP-Finance-ReadOnly", "status": "submitted", "order_number": "ord_88213",
      "mktplace_order_number": "MKT-2026-771102" },
    { "line": 2, "group_name": "GRP-Sales-All",         "status": "skipped",  "order_number": "ord_77190", "reason": "already_ordered" },
    { "line": 3, "group_name": "GRP-Ops-!!invalid",     "status": "validation_failed", "order_number": "ord_88220",
      "reason": [ { "field": "group_name", "code": "invalid_group_name", "message": "..." } ] },
    { "line": 4, "group_name": "GRP-HR-All",            "status": "failed", "order_number": "ord_88221",
      "error": { "code": "mktplace_order_timeout", "retryable": true, "...": "..." } }
  ],
  "next_cursor": "eyJ...", "has_more": true }

GET /v1/orders/{order_number}                # lookup by order number
GET /v1/orders?group_name=GRP-Finance-ReadOnly # lookup by group name (req. 5) — returns the latest order for that name
```

`GET /v1/orders?group_name=...` is the same lookup the `availability`
check's `already_ordered` branch (§3.2) and the create-path dedupe (§5/§6)
use internally.

### 3.5 CLI

The CLI is a thin client over the same contract — no logic the API doesn't
already expose, so CLI and API never disagree on behavior.

```
orders validate --file group.json                 # composite prevalidation, 1 group
orders validate --file groups.ndjson --bulk        # dry-run a whole file, prints a summary + per-line errors

orders create --file group.json [--force] [--idempotency-key <key>]
   # returns as soon as status is "submitted" (or a terminal rejection) — add --wait-for-completion
   # to keep polling until "completed" (§4) if the caller actually needs AD-confirmed creation

orders create --file groups.json --batch           # 2..25 groups, synchronous, one result per line

orders bulk submit --file groups.ndjson [--force] [--idempotency-key <key>]
   # ≤500 lines  -> inline POST /v1/order-jobs
   # >500 lines   -> POST /v1/uploads, PUT file, then POST /v1/order-jobs {source}
   # --force applies to every line unless a line sets its own "force" in the file
   # prints job_id; add --wait to block until dispatch is done (not full reconciliation, §3.4)

orders bulk status <job_id> [--watch]              # polls until terminal (dispatch) status when --watch
orders bulk results <job_id> [--status submitted|completed|validation_failed|failed|skipped] [--format table|ndjson] [--out results.ndjson]
orders bulk cancel <job_id>

orders get <order_number> [--wait-for-completion]
orders get --group-name GRP-Finance-ReadOnly       # reverse lookup (req. 5)
```

Exit codes (so bulk submission is scriptable in CI): `0` all lines
`submitted`/`completed` or legitimately skipped, `2` completed with some
lines `validation_failed` and/or `failed` (`--allow-partial-failure` to keep
this from failing the caller's own script), `1` job-level failure or a
single/small-batch call that came back `validation_failed`/`failed`, `130`
interrupted locally (the job/reconciliation keeps running server-side —
`--watch` can be resumed).

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
                                         │  1. name-policy + owner id-format
                                         │     regex (local, no API call)
                                         │  2. availability: our order store,
                                         │     then LDAP existence check
                                         │     (cache-backed) → skipped
                                         │  3. FTE check: primary_owner,
                                         │     secondary_owner (cache-backed)
                                         │     → validation_failed if either
                                         │     check (1/3) fails
                                         │  4. acquire a token from the
                                         │     shared 100/min rate limiter
                                         │  5. submit order to mktplace
                                         │     → status=submitted + record
                                         │     mktplace_order_number, or
                                         │     failed on synchronous rejection
                                         └───┬─────────┬──────┬──────┘
                                             ▼         ▼      ▼
                          ┌────────────────────┐ ┌──────────┐ ┌───────────────────┐
                          │ orders/order_jobs/  │ │ Cache     │ │ Shared rate limiter │
                          │ order_job_items      │ │ (Redis or │ │ (100 req/min token  │
                          │ store (single         │ │ equiv.):  │ │ bucket, sync-        │
                          │ relational DB, e.g.   │ │ LDAP+FTE  │ │ reserved slice)      │
                          │ PostgreSQL — §5)       │ │ results,  │ │  — §4/§6            │
                          │        ▲               │ │ TTL — §5  │ │                    │
                          └────────┼───────────┘ └─────┬────┘ └───────────────────┘
                                   │                     │ (on miss)
                     6. mark completed                   ▼
                                   │           ┌────────────────────────────┐
                          ┌────────┴────────┐  │ LDAP existence-check API /  │
                          │ Reconciliation    │─▶│ Active-FTE-check API        │
                          │ poller (sweeps     │  │ (out of scope, §1)          │
                          │ status=submitted,  │  └────────────────────────────┘
                          │ polls LDAP — §9    │
                          │ for cadence)        │
                          └────────────────────┘
                                             │
                                             ▼ (step 5 above)
                          ┌────────────────────────┐
                          │ Marketplace (mktplace)   │
                          │ order API — async, out   │
                          │ of scope (§1)             │
                          └────────────────────────┘
```

Three distinct downstream dependencies, not one: **mktplace** (accepts the
order asynchronously; the only one rate-limited at 100/min; out of scope
§1), the **LDAP existence-check API** (read-only — used *twice* in this
design: at validation, §3.2, and by the reconciliation poller below), and
the **FTE-check API** (read-only, "is this owner active," §3.2). This
system never talks to AD or LDAP-for-writes directly — only mktplace does.

- **API gateway** — authn/authz, rate limiting per caller (protects the
  service from a runaway CLI script), routes sync vs. job intake.
- **Order pipeline** is the single implementation both the sync endpoint and
  the bulk workers call per line — this is what makes §3.2's "one code path"
  claim true, and what makes partial-failure isolation (§1) fall out for
  free. Validation (steps 1–3) always runs as part of placing the order.
  Steps ordered cheapest-and-most-decisive first: local regex, then
  availability (which can end the request in a `skipped` before any owner
  gets checked), then the FTE calls, then the scarce rate-limited mktplace
  submission last of all. **The pipeline's own job ends at step 5**
  (`submitted` or a synchronous rejection) — it does not wait for AD
  confirmation.
- **Reconciliation poller** — a new component, this design's answer to
  "how does `submitted` ever become `completed`." A recurring background
  job (`task-scheduling`) that sweeps `orders` where `status = 'submitted'`,
  and for each, calls the **same LDAP existence-check API** validation uses
  — if the group is now found, marks the order `completed` (and stamps
  `completed_at`); if not, leaves it `submitted` for the next sweep. Runs
  independently of the rate-limited submission path (LDAP isn't part of the
  100/min budget) and independently of job dispatch (§3.4). Poll cadence,
  backoff, and a cap on how long an order is polled before being treated as
  stalled are **not yet specified** — §9's most consequential open question,
  since a naive fixed-interval-forever sweep risks unbounded LDAP load
  exactly when mktplace is running slow (§2).
- **Line-item queue** has (at least) two lanes/priorities — sync-path calls
  don't sit behind a 4,500-line job (§2's fairness requirement) — a
  `task-scheduling` priority-queue concern, not a new component.
- **Cache** sits in front of the LDAP existence-check and FTE-check APIs
  (§2/§5/§6) — a `caching` building block, cache-aside, TTL-evicted. Note
  the reconciliation poller's LDAP calls are **not** served from the
  `ldap_exists` cache (§5) — its whole purpose is a fresh read, and its
  ~60s TTL would be stale against a poller checking on roughly that same
  cadence anyway.
- **Shared rate limiter** — one token bucket, refilled at 100/min, that
  every mktplace order submission (sync or bulk) acquires a token from
  before dispatch. A small slice is reserved for the sync lane (§6) so a
  big bulk job can't starve single-order acceptance latency. Physically,
  this can live in Postgres too at this volume — see Deployment topology.
- **Blob store** exists only for the upload path (>500 lines) — a
  `blob-store` building block, not a bespoke file service.
- **Order store** is a single relational database (PostgreSQL or equivalent)
  — see §5 for why SQL, and why one instance is enough at this volume.

### Deployment topology

**An existing Postgres instance is available (confirmed) — use it, don't
stand up a new StatefulSet.** Everything in §2 (order-store volume trivial
by construction, a handful of stateful components, no sharding story, §8)
already argued for as few moving pieces as possible; an existing instance
takes that further — the topology is now genuinely just the **API pod(s)**,
with storage as a dependency this service connects to rather than one it
operates.

- **API pod(s)** — a stateless Deployment, N replicas for HA and to soak up
  read traffic (lookups, job-status polling) that isn't rate-limited at all
  (only the mktplace order-submission call is, §2). This same image also
  runs: bulk-job dispatch (`SELECT ... FOR UPDATE SKIP LOCKED` over
  `order_job_items`, `task-scheduling`'s pull-worker pattern with Postgres
  as the transport), and now the **reconciliation poller** — a recurring
  loop over `orders WHERE status = 'submitted'`, similarly not needing a
  separate worker deployment at this volume.
- **The existing Postgres instance** — no StatefulSet, no new operational
  surface, its HA/backup posture inherited for free. Given how small every
  *known* number in this design turned out to be (§2 — reconciliation load
  is the exception, still unconfirmed), it can plausibly host more than
  just the order tables:
  - The **rate limiter** (§4/§6) as a single row updated atomically instead
    of standing up Redis — at ~1.67 req/s, no contention concern.
  - The **cache** (§5, LDAP-existence and FTE results) as a table with an
    `expires_at` column instead of a separate cache cluster.
  - Even a bulk-job **upload** (§3.3) — ≤4,500 lines at ~150B each, well
    under 1MB — as a `bytea`/large-object column instead of needing real
    object storage.

  Two things worth confirming precisely *because* the instance is shared:
  - **A dedicated schema** (e.g. `order_service.*`) for every table in §5
    plus the rate-limiter/cache tables above.
  - **Sign-off from whoever owns the instance** that its own capacity
    absorbs this service's load — including the reconciliation poller's
    sweep query, which runs continuously and whose row count is currently
    unbounded (§2/§9) until a poll cadence and stall policy are set.

  The trade-off is coupling: an outage on the shared instance now takes
  down order storage, rate limiting, caching, job dispatch, *and*
  reconciliation together. For a system whose own numbers say "small,
  internal, low-stakes" (§1/§2), that's a reasonable price for one stateful
  component instead of several — reconsider if §8's growth triggers land.

## 5. Data model

**Storage engine: a single relational database (PostgreSQL or equivalent),
one instance — not sharded, not NoSQL.** The two correctness requirements
this design leans on hardest — "don't order an already-ordered group" (req.
6) and "don't double-execute a retried request" — are both enforced by
**unique constraints checked atomically at insert time** (below), which a
relational store gives for free. The volume never argues otherwise (§2/§8).

**`order_number` is generated by this system, not returned by mktplace** —
a plain Postgres sequence/identity column (`'ord_' || nextval(...)`) — no
Snowflake ID, no UUID, no `sequencer` building block needed, given one
writer of record and no cross-node coordination to solve. It's assigned the
moment the pipeline persists the `orders` row (step 5, §4) — for *every*
outcome (`submitted`, `failed`, `validation_failed`, `skipped`), which is
why even a rejected attempt is lookup-able (req. 5).

**`mktplace_order_number`** is recorded at the same moment, for `submitted`
orders only — mktplace's own tracking id for the order it accepted. It is
**not** proof the AD group exists (§1/§4) — that's what reconciliation is
for, and it's why `orders` needs a `status` transition, not just this field,
to represent the order's real lifecycle.

- **`orders`** — PK `order_number`. Columns: group_name, primary_owner,
  secondary_owner, **status** (`submitted`|`completed`|`validation_failed`|
  `failed`|`skipped`), **`mktplace_order_number`** (nullable — populated
  when status becomes `submitted`, stays set through `completed`),
  `validation_errors` (nullable array, populated only when
  `validation_failed`), `error` (nullable object, populated only when
  `failed`), skipped_order_number (nullable, set when `skipped`), `job_id`
  (nullable, set when ordered via a bulk job), `created_at`,
  **`completed_at`** (nullable — set by the reconciliation poller when
  status moves to `completed`; also the field a future stall/timeout policy,
  §9, would key off of, e.g. "still `submitted` and `created_at` is over
  N hours old").
  - **Unique index on `group_name` for rows where `status IN ('completed',
    'submitted')`** (a partial/filtered unique index — single domain, no
    qualifier beyond the name itself, §1) — this is what makes "skip if
    already ordered" correct under concurrency (req. 6/NFR), and now
    explicitly covers the in-flight `submitted` state too, not just
    `completed`: two concurrent orders for the same group name race on this
    constraint, and only one wins the insert; the loser's pipeline observes
    the conflict and returns `skipped` pointing at the winner. A `failed`
    order does **not** hold the constraint, so a genuinely failed attempt
    can be retried without being treated as a duplicate, and the retry may
    carry different parameters — **confirmed** intended behavior (§1).
  - Unique index on `(tenant_id, idempotency_key)` for the sync path's
    retry-safety dedupe (separate from the group-name dedupe above).
- **`order_jobs`** — PK `job_id`. Unique index on `(tenant_id, idempotency_key)`.
  Columns: status (dispatch status, §3.4), total_lines, submitted/completed/
  skipped/validation_failed/failed/pending counters (updated by workers and
  by the reconciliation poller, not recomputed by scanning), source
  (`inline`|`upload_id`), created_at, completed_at (dispatch completion,
  distinct from any individual order's `completed_at`).
- **`order_job_items`** — PK `(job_id, line_index)`. Unique index on
  `(job_id, group_name)` — on reprocessing, a line whose `group_name`
  already has a `completed` (or `submitted`, or `skipped`) row is returned
  as-is, not re-run — but one that's `validation_failed` *is* re-run (cheap
  to re-validate, unlike re-submitting to mktplace). Columns: status
  (mirrors `orders.status`), order_number (nullable), validation_errors
  (nullable), error (nullable), attempt_count. `GET .../results` pages on
  `(job_id, line_index)`.
- **`idempotency_keys`** (sync path) — `(tenant_id, idempotency_key)` →
  stored response, TTL 24h, per the standard `api-design` idempotency
  state-machine.

**Cache** — logically distinct from the tables above regardless of where it
physically lives (a separate Redis, or a table in this same Postgres
instance; see §4's Deployment topology). Two namespaces, both cache-aside
with a TTL:
- `fte:{owner_id} → active|inactive`, **TTL 15 minutes (confirmed).**
  Collapses a 4,500-line job's owner reuse (§2) to ~50–200 calls; a
  **genuine, bounded staleness risk** (a just-terminated employee could
  stay "active" in cache for up to 15 minutes), confirmed acceptable.
- `ldap_exists:{group_name} → found|not_found`, **TTL ~60 seconds**,
  used only for the **validation-time** check (§3.2) — the reconciliation
  poller (§4) always reads LDAP live, bypassing this cache, since its whole
  job is a fresh answer. Staleness in the validation-time cache is
  self-correcting: a stale `not_found` there just means mktplace (or
  reconciliation, once it runs) ends up handling an unexpected duplicate,
  not a data-loss bug.

Group-name lookup (`GET /v1/orders?group_name=...`, req. 5) is served by the
`group_name` index above — the same index that enforces the duplicate-skip
rule.

Sharding: not needed for the order tables (§2/§8's *known* numbers); the
reconciliation poller's own load is the one part of this design whose
volume isn't yet bounded (§9) — that's a poll-cadence/backoff problem to
solve first, not a sharding one.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| `submitted` as a distinct intermediate status, reconciled to `completed` later by re-polling LDAP | Matches reality: mktplace is confirmed asynchronous (§1) — this design no longer claims a fact (the group exists) it can't actually know synchronously | Every consumer (CLI, job counters, the duplicate-skip index) now has two "in-progress-ish" states to handle (`submitted` is both "not done" and "blocks a duplicate") instead of one clean succeeded/failed | Never, given mktplace's confirmed async behavior — this is the correctness fix for that fact, not a preference |
| Reconciliation reuses the **existing** LDAP existence-check API rather than a new mktplace status endpoint | No new integration to build; the same call already used at validation time does double duty | It's a *confirmation-only* signal — it can prove `completed`, it can never prove `failed` (no such thing as "LDAP says this will never exist") — the stalled-order gap in §9 is a direct consequence of this choice | Mktplace exposes an order-status API or webhook → prefer it, since it could give a real failure signal this design currently lacks entirely |
| Reconciliation is a separate poller, not folded into the order pipeline that handles submission | Its load and cadence can be tuned independently of the rate-limited submission path — a slow reconciliation sweep never blocks a new order from being placed | One more background component to run and monitor (§4/§7) | Never — coupling it to the submission pipeline would make reconciliation's (currently unbounded, §2) load compete with the 100/min-gated path for no reason |
| Validation is step 1 of the order pipeline (runs on every order call), not a required separate step | Callers can't forget to validate; a `validation_failed` reason is always in the order response itself | The order call always pays validation cost, even for a line dry-run-checked moments earlier | Blocking checks become expensive enough that skipping a repeat check is worth the complexity → add an optional skip token then |
| `validation_failed` and `failed` as distinct order statuses | A caller can branch on "fix the request" vs "safe to retry" without parsing error codes (req. 1/5) | One more status value in every enum/consumer | Never — this is exactly the distinction the requirement asks for |
| LDAP existence check added to validation (not just our own order store) | Closes the "group created outside this process" gap | A second external dependency validation now waits on, alongside the FTE check | Never — this is the fix for exactly that gap |
| Cache both the LDAP existence check and the FTE check, cache-aside with TTL | Cuts ~9,000 potential calls/4,500-order job to a few hundred for FTE, and avoids a duplicate LDAP call on validate-then-order | Two staleness windows to reason about | The FTE TTL's staleness risk becomes unacceptable to the business → shorten it, not both caches |
| Validation steps ordered cheapest/most-decisive first (regex → availability → FTE → rate-limited mktplace call) | A request that's going to be skipped or rejected never spends an FTE-check call or a scarce rate-limit token on itself | The pipeline has more sequential stages, and their order is a meaningful design choice | Never — reordering would spend scarce resources on doomed requests |
| Sync endpoint (≤25) + separate async job endpoint (up to 4,500+) | Keeps single-order *acceptance* latency low; lets bulk scale independently | Two code paths to keep behaviorally identical (mitigated: both call the same order pipeline, §4) | The sync cutover (25) is wrong for real traffic → tune, don't redesign |
| `group_name` as the dedupe/business key (no separate `external_id`) | One fewer field to require from callers | Renaming a group later has no clean story here (out of scope, §1) | A second domain is ever added → key must become `(domain, group_name)` |
| A single shared rate limiter (one 100/min token bucket), not per-worker limiting | Correctly enforces the confirmed submission constraint regardless of worker-pool size | Every call path now depends on one shared piece of state — must be fast and available (→ §7) | Never, while the limit is one system-wide number |
| A reserved slice of the 100/min budget for the sync lane | A big bulk job can't starve single-order acceptance latency | Reserved sync capacity is bulk capacity not spent | Sync calls prove rare enough that a reservation wastes bulk throughput → shrink it |
| PostgreSQL (single relational instance) for the order store, not NoSQL | Atomic unique constraints make the group-name and idempotency-key dedupe correct under concurrency | A single writer to keep available (standard HA replica) | Never, at this design's scale (§8) |
| Skip-by-default on duplicate (against `completed` *and* `submitted`), `force=true` to override; `failed` never blocks a retry | Prevents re-ordering an existing or in-flight group; a failed attempt can be freely resubmitted, including with different owners | A retry after `failed` must use a new `Idempotency-Key` | Never, as confirmed |
| Duplicate check enforced by a **DB unique constraint**, not just the app-level pre-check | Correct under concurrency | The loser of the race gets its `skipped` result at insert time, not at validate time | Never — the correctness backstop |
| `force=true` still goes through the real mktplace order call | A forced re-order of a group that still genuinely exists converges to `completed` rather than silently duplicating | `force` looks like it "always creates" but sometimes just confirms what already exists | Never — bypassing this would risk actual duplicate directory objects |
| Inline body (≤500 lines) *or* presigned-upload for bulk | Never hits a gateway payload ceiling as batch size grows | Two ingestion paths to implement | Gateway payload limits change → raise the inline threshold instead |
| Idempotency-Key per order **and** per job, `group_name` per line | Safe retries at every granularity | Two idempotency scopes plus the business-key dedupe to reason about | Never |
| Worker-pool size decoupled from submission throughput | No wasted complexity sizing a pool the rate limiter would throttle anyway | A 4,500-order job's dispatch still takes ~45 min regardless of pool size | Mktplace offers a real batch-order API → the only lever that changes dispatch time (§8) |
| Priority lanes for sync vs. bulk line-item dispatch | A 4,500-line job can't starve single-order acceptance latency | Scheduler has to be lane-aware, not a plain FIFO queue | Traffic mix makes this moot |

## 7. Failure modes & degradation

- **An order sits in `submitted` and never reconciles** — the design's
  biggest open gap (§1/§9): there is currently no confirmed way to
  distinguish "mktplace is just slow" from "mktplace silently dropped or
  failed this order." Today, such an order polls forever. Until a
  stall/timeout policy is set (§9), this is a real degradation risk, not a
  handled case — flagged rather than papered over with an invented answer.
- **Invalid input** — bad group-name format, a malformed owner id, or an
  owner who is not an active FTE — caught at steps 1–3 of the pipeline, no
  mktplace call, no rate-limit token spent. `validation_failed`, not
  `failed`, because the check *ran and answered*.
- **The FTE-check or LDAP existence-check API errors outright** (times out,
  5xx) — distinct from a definitive negative answer: the order comes back
  `failed` with `retryable: true` (`resilience-failure`: timeout + circuit
  breaker per dependency), not `validation_failed`.
- **The cache is unavailable** — fail **open**: call the LDAP/FTE APIs
  directly rather than blocking orders, since the cache is a load-reduction
  optimization, not a correctness mechanism. Degraded, not broken — worth
  alerting on.
- **The rate limiter itself is unavailable** — fail **closed**: orders
  return `retryable: true` 5xx rather than risking a burst past the
  confirmed 100/min ceiling. Run it as a small, highly-available component
  (or a row in the shared Postgres instance, §4) separate from order
  storage, so the two blips aren't the same failure.
- **The sync lane's reserved token slice is exhausted** — `429` +
  `Retry-After`, standard contract; the caller backs off rather than
  silently consuming bulk-reserved tokens.
- **Mktplace's order API is slow or down (at submission time)** — timeout +
  circuit breaker. Sync path: fails fast with `retryable: true`. Bulk path:
  breaker trips, dispatch of *new* lines pauses and backs off with jitter;
  already-submitted/completed/skipped lines stand; job status surfaces as
  `processing` with a `degraded` flag.
- **The reconciliation poller falls behind or is down** — self-healing, not
  a correctness bug: `submitted` orders simply stay `submitted` longer than
  usual, and catch up once the poller resumes. Distinguishable from the
  "never reconciles" gap above only by duration — worth a metric on oldest
  unresolved `submitted` order's age, not just poller uptime.
- **Client retries a timed-out single order** — same `Idempotency-Key` → the
  stored response is replayed, not re-executed (§5's idempotency table).
- **Two lines for the same `group_name` in one bulk file** (or two
  overlapping bulk jobs, or one racing a still-`submitted` earlier order) —
  the DB unique constraint (§5, now covering `submitted` too) admits the
  first, the second observes the conflict and is recorded `skipped`.
- **Client resubmits the same bulk file after a network blip** — same job
  `Idempotency-Key` → the existing `job_id` is returned; per-line
  `group_name` dedup means already-submitted/completed/skipped lines are
  not recreated.
- **Worker crashes mid-line** — the queue's visibility timeout requeues the
  line; the pipeline is idempotent on `group_name` via the DB constraint, so
  a line that actually completed submission just before the crash is
  detected as already-submitted on redelivery, not double-submitted.
- **One malformed name / policy violation / already-ordered line in 4,500**
  — resolves independently, the other 4,499 proceed; the job's dispatch
  status is `completed_with_errors` only when there's a real `failed` line.
- **Upload interrupted (large NDJSON)** — the client/CLI retries the PUT and
  resubmits with the same job `Idempotency-Key`; nothing has been processed
  yet, so this is a clean retry.
- **What the user sees:** a sync call is accepted (`submitted`) or cleanly
  rejected (`skipped`/`validation_failed`/`failed`) in ~1.5s; *when the
  group actually exists* is a separate question the same lookup answers
  later, once reconciliation catches up (§1/§4 — timing currently unbounded,
  §9). A bulk job's dispatch always finishes in a terminal state visible via
  `GET /v1/order-jobs/{id}`, with per-line detail — and per-line status
  keeps updating afterward as reconciliation runs.

## 8. Scale evolution

**Current bottleneck:** two, not one — the confirmed **100 requests/minute**
mktplace *submission* rate limit (dispatch takes ~45 min for a 4,500-order
job regardless of workers, unchanged from before), and the **unbounded
reconciliation load** (§2/§9), which has no confirmed ceiling at all yet.

**At 10× (≈45,000 orders/submission):**
- Dispatch: 45,000 ÷ 100/min ≈ **7.5 hours** — the same "add workers doesn't
  help, only a real batch-order API or a higher quota does" story as before
  (§6's worker-pool row).
- Reconciliation: 10× the orders sitting `submitted` at once means 10× the
  poller's per-sweep LDAP load, with **no reason to expect** mktplace's own
  provisioning throughput scales the same way — this could become the
  larger bottleneck of the two, and there's no data yet to say either way.
- The inline-body path disappears entirely (mandatory file upload above a
  much lower line count than today's 500) — a payload-size fix, not a
  throughput fix.
- `--wait`/`--wait-for-completion` blocking a terminal for hours stops being
  reasonable at any scale beyond today's — `--watch` polling against a
  persisted id becomes the only sane flow.
- **Signal to watch:** dispatch completion-time p95 (as before), **plus** the
  age of the oldest still-`submitted` order and reconciliation sweep
  duration — both need baselines that don't exist yet (§9).

## 9. Open questions

- **What poll cadence, backoff, and stall/timeout policy should the
  reconciliation poller use?** (§2/§4/§7 — this design's biggest open gap)
  Right now there is no cap on how long an order stays `submitted`, and no
  distinction between "mktplace is just slow" and "this will never
  complete." Needs: a poll interval (this doc assumed ~60s for its
  estimates, not a real number), a backoff curve so old `submitted` orders
  are polled less frequently rather than forever at a flat rate, and a
  decision on what happens to an order past some age threshold — surfaced
  as `failed` for manual investigation, left `submitted` indefinitely with
  an alert, or something else. This one decision reshapes §2's load
  estimates, §5's `completed_at`/stall-detection story, and §7's biggest
  named risk.
- **Does mktplace expose any status/webhook API beyond order submission?**
  If so, that's likely a better reconciliation signal than blind LDAP
  polling — it's the only thing that could give this design an actual
  *failure* signal, which LDAP existence-checking structurally cannot (§6).
  Worth asking mktplace's team directly rather than assuming polling-only
  is final.
- **Should an `exists_in_ad` skip backfill an order record?** (§1's
  narrower out-of-scope item) The LDAP check catches a group created
  outside this process at validation time, cleanly `skipped` — but no
  `order_number` exists for it to point to.
- **What are mktplace's, the LDAP existence-check's, and the FTE-check's own
  rate limits/SLAs?** Unstated beyond mktplace's confirmed 100/min — the
  caching and polling design is this system's own mitigation regardless,
  but the real numbers decide whether it's *enough*.
- **Is the 100/min limit truly one global ceiling**, or per app-registration/
  service-principal? This design assumes one shared system-wide bucket as
  the conservative reading.
- **Does mktplace's order schema distinguish "primary" from "secondary"
  owner**, or is that purely this system's own metadata that mktplace's API
  doesn't natively support? `primary_owner != secondary_owner` itself is
  confirmed (§1/§3.2).
- **Exact owner-id regex** — assumed to be an email/employee-id shape (§1);
  the real pattern needs the actual policy.
- Auth model (API key vs. OAuth client-credentials) — assumed but not
  designed; doesn't change the shapes above either way.

---
### Validation (fill-in gate)
- [x] Every row in §6 has a non-empty **Worsens** and a **breaking point**.
- [x] §2 estimates carry units and state their assumptions.
- [x] §7 names a degradation path per critical dependency (not just "retry")
      — with one named exception (the stalled-`submitted` gap) left
      explicitly unresolved rather than fabricated.
- [x] Each component in §4 ties back to a requirement/number in §1–§2.
- [x] Coverage sweep: IDs (`order_number`/`job_id`/`mktplace_order_number`/
      `group_name`-as-key scheme, §5) ✓; media — n/a; search — n/a (lookup
      by number or exact group name only, §3.4); logs/SLOs — deferred to
      `observability`/`distributed-logging`, not re-derived here.

**Weakest dimension:** there is **no confirmed failure signal** for an order
stuck in `submitted` (§7/§9) — this design can prove success (the group
eventually appears in AD) but has no way to prove failure short of a
policy-driven timeout it hasn't set yet. That's a correctness-of-the-model
gap, not a tuning one: until it's resolved, a caller cannot fully trust that
a `submitted` order will ever resolve one way or the other. Everything else
in §9 (poll cadence specifics, unstated third-party capacity, the
`exists_in_ad` backfill decision) is real but secondary to this one.
