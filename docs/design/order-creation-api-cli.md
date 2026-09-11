# Design: Order Creation API & CLI (Active Directory group orders)

A write-up structured around the reasoning loop (`system-design` skill), composing
`requirements-scoping` → `back-of-the-envelope` → `api-design` → `data-storage` →
`caching` → `task-scheduling` / `messaging-streaming` → `resilience-failure`.

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
   **order number** plus final status (`succeeded` / `validation_failed` /
   `failed` / `skipped`) in one round trip — **validation happens as part of
   placing the order**, not as a separate required call. If the parameters
   don't pass, the order response itself comes back `validation_failed` with
   the reason(s); nothing is sent downstream.
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
   process entirely, not just one this system already has an order for). A
   standalone dry-run call (§3.2) is *available* for a caller who wants to
   check before committing, but is optional, not a prerequisite.
5. **Look up an order by order number *or* by group name**, and see its
   status — `succeeded` (with the order number), `validation_failed` (with
   the specific reason, e.g. which field and why), `failed` (with a
   downstream error message), or `skipped` (with the order number of the
   pre-existing order it was skipped in favor of).
6. **Skip duplicate orders.** If a group has already been ordered
   (successfully, or is currently in flight), a new order for the same group
   name is **skipped**, not re-attempted — *unless* the caller passes
   `force=true`, in which case a new order is created regardless. A
   **`failed`** order is not "already ordered" — a new order for that group
   name proceeds normally, no `force` needed, and may carry different
   parameters than the failed attempt (e.g. a different `secondary_owner`
   after a `not_active_fte` rejection) — confirmed intended, not just a side
   effect of the dedupe rule (§9).
7. **CLI** wraps all of the above: validate, order (single/small-batch),
   submit a bulk file, poll/watch a bulk job, fetch results, get one order by
   number or by group name.

### Non-functional constraints
- **Latency** — bounded by the 100/min downstream rate limit (§2), not a flat
  number: a single order should complete in ~1.5s p99 (one rate-limit token
  wait + one AD call); an N-order synchronous batch (N ≤ 25) scales as
  ≈N × 0.6s + AD latency — still one request/response, no polling, but
  callers should expect seconds, not milliseconds, once N > 1. Bulk
  submission is *accepted* in < 2s regardless of batch size; the batch
  itself is processed asynchronously against the same shared rate limit.
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
- **Auditability** — every order's outcome (succeeded / validation_failed /
  failed / skipped, and the exact reason or error) must be queryable after
  the fact by order number *or* group name, not just streamed once.
- **Bounded load on the validation dependencies.** The LDAP group-existence
  check and the active-FTE check are each a network call this design would
  otherwise make once or twice per order — at 4,500 orders/job that's
  thousands of calls to systems whose own capacity isn't stated. Caching
  (§3.2/§5) exists specifically to bound this, not just to shave latency.

### Out of scope (explicitly)
- The AD/directory provisioning engine itself (the actual LDAP/Graph-API
  calls that create the group object and set its `primary_owner`/
  `secondary_owner`) — treated as an existing downstream service this API
  calls.
- Group **lifecycle after creation** — membership changes, renaming,
  deletion, ownership transfer. This design only covers *ordering the
  creation*.
- **Reconciling** a group the LDAP existence check (§3.2) finds already in
  AD with no order record behind it (created outside this process) — this
  design *detects* that case at validation time (in scope, unlike the
  earlier draft), but backfilling an order record for it, or any broader
  drift audit/sync, is not designed here. Flagged in §9.
- The **LDAP existence-check and active-FTE-check APIs themselves** — their
  own availability, latency, and rate limits are assumed, not designed
  (§9); this design only decides how it calls and caches them.
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
- **The AD provisioning system enforces a hard rate limit of 100 requests/
  minute** (confirmed — see §2/§6), shared across every caller of this API,
  not per-tenant. This is the dominant constraint on the whole design, more
  than storage or compute.
- Average order line (`group_name`, `primary_owner`, `secondary_owner`) is
  ~150 bytes as JSON.
- **`primary_owner` and `secondary_owner` must be different AD principals**
  — confirmed requirement, enforced as a blocking validation check (§3.2).
- **A `failed` order does not block a retry, and the retry may carry
  different parameters than the failed attempt** — confirmed: e.g. a caller
  who got `not_active_fte` on `secondary_owner` can resubmit the same
  `group_name` with a different `secondary_owner`, with no `force` needed
  (§5's unique index already excludes `failed`, so this was already how the
  data model worked — this confirms it's intended behavior, not a side
  effect). One consequence worth being explicit about: such a retry must use
  a **new** `Idempotency-Key`, not the failed attempt's — reusing the same
  key with a different body is a `422` key-reuse conflict by the standard
  idempotency contract (`api-design`), since the body has genuinely changed.
- The owner id format (what the regex checks) is a corporate identifier —
  email or employee id — exact pattern owned by policy, not specified here
  (§9).
- "Active full-time employee" is a binary the FTE-check API returns; this
  design treats a `false`/not-found response as a blocking validation
  failure, not a warning.
- The LDAP existence-check and FTE-check APIs are **separate systems** from
  the AD provisioning (group-create) API, with their own latency/rate limits
  — **not** assumed to share the 100/min budget (§2). Worth confirming (§9).
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
| Downstream calls per order | 1 (create-group, owners set at creation) | pipeline in §3 |
| Downstream call latency | p50 150 ms / p99 400 ms | assumed AD/Graph API SLA |
| **AD-provisioning rate limit** | **100 requests/minute, system-wide** (confirmed, not per-tenant) | stated constraint — this API must self-throttle to it, not just retry on 429 |
| Effective downstream throughput | ~1.67 req/s | 100 ÷ 60 |
| Time for 1 order (steady state) | ~0.6 s wait for a token + ~150–400 ms AD call | rate limit, not AD latency, now dominates even a single order |
| Time for a 3-order small batch | **~2–3 s** | ≈3 sequential token waits (~0.6s apart) + AD latency each |
| Time for a 4,500-order bulk job | **~45 minutes** | 4,500 ÷ 100 per min — this is the number that matters, not a compute estimate |
| Concurrent bulk jobs worth planning for | 1–2 | the 100/min budget is shared system-wide; more concurrent jobs don't finish faster, they just interleave against the same ceiling |
| In-flight line items, worst case | ~4,500–9,000 | one or two max-size jobs in flight at once |
| LDAP existence-check calls, uncached, 4,500-order job | up to 9,000 | once at validate (dry-run) + once at order, per line — worth caching even within one job |
| FTE-check calls, uncached, 4,500-order job | up to 9,000 | 2 owners × 4,500 lines, before dedup |
| Distinct owners in a 4,500-order job (assumed) | ~50–200 | owner reuse assumption (§1) — this is the number that makes caching worth building |
| FTE-check calls, **cached** (owner-id → TTL) | ~50–400 | one lookup per distinct owner, not per line — a ~95%+ reduction if the reuse assumption holds |

Neither the LDAP nor the FTE-check API's own rate limit/SLA is stated (§9) —
the estimates above are exactly why this design caches both rather than
assuming either can absorb 9,000 calls per bulk job without being asked to.

**What the numbers force:**
- **The rate limit, not our own compute, is now the entire story.** A worker
  pool doesn't make a 4,500-order job finish faster than ~45 minutes — more
  workers just queue up against the same 100/min ceiling. The design need is
  a **single, shared, accurately-enforced rate limiter** (one token bucket,
  not "N workers each self-limiting," which would either double-count the
  budget or under-use it) gating every call this API makes downstream (→
  `resilience-failure`).
- **A small batch is no longer "fast."** 3 sequential orders spaced ~0.6s
  apart by the rate limit alone take 2–3s — real, but still one request/
  response, no polling needed. The latency NFR below is restated per-item
  rather than as a single flat p99 (see NFR).
- **Sync calls must not queue behind a bulk job**, or a single order could
  wait up to 45 minutes for a token consumed by someone else's big batch.
  This makes the priority lane in §4 a **reserved slice of the 100/min
  budget**, not just dispatch ordering (see §6).
- The payload/storage numbers above are small enough that neither inline-vs-
  upload ingestion (§3) nor the order store (§5) is a scaling concern at this
  volume — the rate limit is the only real bottleneck for AD-provisioning
  calls specifically.
- **The validation-side calls (LDAP existence, FTE) need their own
  bottleneck story, separate from the 100/min limit** — nothing says those
  APIs tolerate 9,000 calls per bulk job. Caching (§3.2/§5) is the mitigation
  that's actually in this design's control, independent of what those APIs'
  real limits turn out to be.

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
else for the caller to specify. Both owners are validated and set on the AD
group at creation (§3.2/§4); which becomes the group's primary `managedBy`
vs. a secondary owner is an AD-provisioning-API detail, not something this
API's contract needs to expose beyond the two fields above.

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
- **`already_ordered`** — found in *our own order store* (fast DB read, no
  API call) → the order will be `skipped`, pointing at the existing order.
- **`exists_in_ad`** — not in our store, but the **LDAP existence-check API**
  says the group is already there (§4) — this is what catches a group
  created outside this process entirely (no longer out of scope, unlike the
  earlier draft). Also `skipped`, but with no `existing_order_number` to
  point to (§3.3) — nothing here ordered it. **Cached** by group name with a
  short TTL (§5), mainly so a validate-then-order round trip for the same
  line doesn't hit LDAP twice.
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
is the one the create path performs — the DB's unique constraint for
`already_ordered` (§5), and the AD create call itself for `exists_in_ad`
(§3.3's `force=true` note) — a dry run showing `not_found` can still lose a
race, which is why the actual order response is what a caller must trust for
the final outcome.

Bulk dry-run: `POST /v1/order-jobs?mode=validate_only` (see 3.3) — same
ingestion shape, no orders are created, results carry per-line `checks`.

### 3.3 Order — one, a few, or 4,500

**Single / small batch (synchronous, ≤ 25 lines):**

```
POST /v1/orders
Idempotency-Key: 3f9a2e7c-...        # required, one per logical order
{ "group_name": "GRP-Finance-ReadOnly",
  "primary_owner": "alice@corp.com", "secondary_owner": "bob@corp.com" }

201 Created
{ "order_number": "ord_88213", "status": "succeeded", "group_name": "GRP-Finance-ReadOnly",
  "ad_object_id": "a1b2c3d4-5e6f-..." }   // from the AD/directory API's create response — §5, §9

200 OK   // found in our own order store, force not set — order is skipped, not an error
{ "order_number": "ord_77190", "status": "skipped", "group_name": "GRP-Finance-ReadOnly",
  "reason": "already_ordered" }

200 OK   // not in our store, but LDAP says the group already exists — created outside this process
{ "order_number": "ord_88219", "status": "skipped", "group_name": "GRP-Legacy-Ops",
  "reason": "exists_in_ad" }   // no existing_order_number — nothing here ordered it (§9)

422 Unprocessable Entity   // step 1 of the pipeline (§4) — no AD call, no FTE-check call was made
{ "order_number": "ord_88214", "status": "validation_failed", "group_name": "GRP-Ops-!!invalid",
  "reason": [
    { "field": "group_name", "code": "invalid_group_name",
      "message": "Group name may not contain '!' and must start with 'GRP-'." },
    { "field": "secondary_owner", "code": "not_active_fte",
      "message": "bob@corp.com is not an active full-time employee." }
  ] }

502 Bad Gateway   // input was valid, the group wasn't a duplicate — the downstream AD call itself failed
{ "order_number": "ord_88215", "status": "failed", "group_name": "GRP-Finance-ReadOnly",
  "error": { "code": "ad_provisioning_timeout", "message": "Directory service did not respond in time.",
             "request_id": "req_...", "retryable": true } }
```

Every response — succeeded, validation_failed, skipped, or failed — carries
an `order_number`: even a rejected attempt is a recorded, lookup-able order
(req. 5). `validation_failed` (client's input, fixable, no downstream call
made, not retryable as-is) and `failed` (our/AD's execution error, often
retryable) are kept as **distinct statuses** on purpose — a caller scripting
against this API can tell "fix your request" apart from "safe to retry"
without parsing the error code. `orders` accepts a top-level array for a
small batch:
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
  "total_lines": 4500, "succeeded": 3020, "skipped": 100, "validation_failed": 15, "failed": 25, "pending": 1340 }

GET /v1/order-jobs/{job_id}/results?limit=200&cursor=eyJ...
200 OK
{ "data": [
    { "line": 1, "group_name": "GRP-Finance-ReadOnly", "status": "succeeded", "order_number": "ord_88213" },
    { "line": 2, "group_name": "GRP-Sales-All",         "status": "skipped",  "order_number": "ord_77190", "reason": "already_ordered" },
    { "line": 3, "group_name": "GRP-Ops-!!invalid",     "status": "validation_failed", "order_number": "ord_88220",
      "reason": [ { "field": "group_name", "code": "invalid_group_name", "message": "..." } ] },
    { "line": 4, "group_name": "GRP-HR-All",            "status": "failed", "order_number": "ord_88221",
      "error": { "code": "ad_provisioning_timeout", "retryable": true, "...": "..." } }
  ],
  "next_cursor": "eyJ...", "has_more": true }

GET /v1/orders/{order_number}                # lookup by order number
GET /v1/orders?group_name=GRP-Finance-ReadOnly # lookup by group name (req. 5) — returns the latest order for that name
```

`GET /v1/orders?group_name=...` is the same lookup the `availability` check's
`already_ordered` branch (§3.2) and the create-path dedupe (§5/§6) use
internally — one more place the "skip if already ordered" rule and "look it
up by group name" requirement share a single implementation.

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
orders bulk results <job_id> [--status validation_failed|failed|skipped|succeeded] [--format table|ndjson] [--out results.ndjson]
orders bulk cancel <job_id>

orders get <order_number>
orders get --group-name GRP-Finance-ReadOnly       # reverse lookup (req. 5)
```

Exit codes (so bulk submission is scriptable in CI): `0` all lines succeeded
or were legitimately skipped, `2` completed with some lines
`validation_failed` and/or `failed` (`--allow-partial-failure` to keep this
from failing the caller's own script; `orders bulk results --status
validation_failed` vs `--status failed` tells the caller which kind), `1`
job-level failure, or a single/small-batch call that came back
`validation_failed`/`failed`, `130` interrupted locally (the job itself keeps
running server-side — `--watch` can be resumed against the same `job_id`).

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
                                         │     check (1/3) fails — no AD
                                         │     call, no token spent
                                         │  4. acquire a token from the
                                         │     shared 100/min rate limiter
                                         │  5. AD group-create (owners set)
                                         │     → failed on downstream error
                                         │  6. persist order (succeeded/
                                         │     validation_failed/failed/skipped)
                                         └───┬─────────┬──────┬──────┘
                                             ▼         ▼      ▼
                          ┌────────────────────┐ ┌──────────┐ ┌───────────────────┐
                          │ orders/order_jobs/  │ │ Cache     │ │ Shared rate limiter │
                          │ order_job_items      │ │ (Redis or │ │ (100 req/min token  │
                          │ store (single         │ │ equiv.):  │ │ bucket, sync-        │
                          │ relational DB, e.g.   │ │ LDAP+FTE  │ │ reserved slice)      │
                          │ PostgreSQL — §5)       │ │ results,  │ │  — §4/§6            │
                          │                        │ │ TTL — §5  │ │                    │
                          └────────────────────┘ └─────┬────┘ └───────────────────┘
                                             │           │ (on miss)
                                             ▼           ▼
                          ┌────────────────────┐ ┌────────────────────────────┐
                          │ AD / directory       │ │ LDAP existence-check API /  │
                          │ provisioning API      │ │ Active-FTE-check API        │
                          │ (out of scope, §1)    │ │ (out of scope, §1)          │
                          └────────────────────┘ └────────────────────────────┘
```

- **API gateway** — authn/authz, rate limiting per caller (protects the
  service from a runaway CLI script), routes sync vs. job intake.
- **Order pipeline** is the single implementation both the sync endpoint and
  the bulk workers call per line — this is what makes §3.2's "one code path"
  claim true, and what makes partial-failure isolation (§1) fall out for
  free: each line runs the pipeline independently and reports its own
  result. Validation (steps 1–3) always runs as part of placing the order,
  not as a prerequisite the caller must remember — a caller who *did* call
  `orders:validate` first gets no special treatment; the order call
  re-validates anyway (against the cache, so this is cheap — see below), so
  "it validated" can never go stale by the time the order is actually
  placed. Steps ordered cheapest-and-most-decisive first: local regex, then
  availability (which can end the request in a `skipped` before any owner
  gets checked), then the FTE calls, then the scarce rate-limited AD call
  last of all — nothing expensive runs for a request that's going to be
  rejected or skipped anyway.
- **Line-item queue** has (at least) two lanes/priorities — sync-path calls
  don't sit behind a 4,500-line job (§2's fairness requirement) — a
  `task-scheduling` priority-queue concern, not a new component.
- **Cache** sits in front of the LDAP existence-check and FTE-check APIs
  (§2/§5/§6) — a `caching` building block, cache-aside, TTL-evicted (neither
  upstream system pushes invalidation events). It's what turns ~9,000
  potential calls per 4,500-order bulk job into a few hundred (§2).
  Logically separate from the rate limiter (one throttles a scarce write
  budget, the other avoids redundant reads) — whether it's a *separate
  physical* component (Redis) or a table in the same Postgres instance is a
  deployment-topology choice, not a design one; see below.
- **Shared rate limiter** is the component §2's numbers actually demand: one
  token bucket, refilled at 100/min, that every downstream AD call (sync or
  bulk) acquires a token from before dispatch — not per-worker limiting,
  which would either over- or under-count the real budget. A small slice of
  the bucket is reserved for the sync lane (§6) so a big bulk job can't starve
  single-order latency. Physically, this can live in Postgres too at this
  volume — see Deployment topology below.
- **Blob store** exists only for the upload path (>500 lines) — a
  `blob-store` building block, not a bespoke file service.
- **Order store** is a single relational database (PostgreSQL or equivalent)
  — see §5 for why SQL, and why one instance is enough at this volume.

### Deployment topology

**An existing Postgres instance is available (confirmed) — use it, don't
stand up a new StatefulSet.** Everything in §2 (≤100 writes/minute by
construction, a handful of stateful components, no sharding story, §8)
already argued for as few moving pieces as possible; an existing instance
takes that further — the topology is now genuinely just the **API pod(s)**,
with storage as a dependency this service connects to rather than one it
operates.

- **API pod(s)** — a stateless Deployment, N replicas for HA and to soak up
  read traffic (lookups, job-status polling) that isn't rate-limited at all
  (only the AD-create call is, §2). This same image is also where bulk-job
  dispatch runs: given the confirmed ceiling (~1.67 req/s), it doesn't need
  a separate worker deployment — one of the replicas (or all of them,
  coordinating via the DB) polling `order_job_items` for pending rows using
  `SELECT ... FOR UPDATE SKIP LOCKED` is enough of a "queue" at this volume
  (`task-scheduling`'s pull-worker pattern, with Postgres as the transport
  instead of a broker).
- **The existing Postgres instance** — no StatefulSet, no new operational
  surface, and its HA/backup posture is inherited for free rather than being
  this design's problem to solve (the "who backs this up" gap a brand-new
  StatefulSet would have opened is moot). Given how small every number in
  this design turned out to be (§2), it can plausibly host more than just
  the order tables:
  - The **rate limiter** (§4/§6) as a single row updated atomically
    (`UPDATE ... SET tokens = tokens - 1 WHERE tokens > 0`) instead of
    standing up Redis — at ~1.67 req/s, no contention concern.
  - The **cache** (§5, LDAP-existence and FTE results) as a table with an
    `expires_at` column instead of a separate cache cluster.
  - Even a bulk-job **upload** (§3.3) — ≤4,500 lines at ~150B each, well
    under 1MB — as a `bytea`/large-object column instead of needing real
    object storage.

  Two things worth confirming precisely *because* the instance is shared,
  not dedicated to this service:
  - **A dedicated schema** (e.g. `order_service.*`) for every table in §5
    plus the rate-limiter/cache tables above — keeps this service's
    migrations, naming, and row-level locking from colliding with whatever
    else already lives on that instance.
  - **Sign-off from whoever owns the instance** that its own capacity
    absorbs this service's load without asking — ≤100 writes/min plus a
    steady low-rate `SELECT ... FOR UPDATE SKIP LOCKED` poll from the bulk-
    job dispatcher (previous bullet) is tiny in isolation, but it's still
    new, continuous load on a resource this design doesn't own.

  Folding rate-limiter/cache/upload-storage into this same instance avoids
  adding Redis, a message broker, or an S3-compatible store — **at this
  confirmed volume.** The trade-off is coupling: an outage on the shared
  instance now takes down order storage, rate limiting, caching, and job
  dispatch together, not just this service's writes — a cost every other
  tenant of that instance already accepts, so it isn't a new risk this
  design introduces, just one it now shares in. It stops making sense to
  keep everything on one instance if any of §8's growth triggers land (a
  much higher rate limit or read volume would start to make a separate
  cache/rate-limiter worth splitting out, regardless of where the order
  tables live).

## 5. Data model

**Storage engine: a single relational database (PostgreSQL or equivalent),
one instance — not sharded, not NoSQL.** This is a deliberate `data-storage`
call, not a default: the two correctness requirements this design leans on
hardest — "don't order an already-ordered group" (req. 6) and "don't
double-execute a retried request" — are both enforced by **unique
constraints checked atomically at insert time** (below), which is exactly
what a relational store gives for free and an eventually-consistent store
would make genuinely hard to get right. And the volume never argues
otherwise: §2 puts the *entire system* at ≤100 writes/minute by construction
(the rate limit gates order creation itself) plus a bounded ~4.5–9k-row bulk
job store per job in flight — trivial OLTP load for one instance, no
sharding story needed, ever, at this design's scale (see §8).

**`order_number` is generated by this system, not returned by AD** — AD has
no concept of an "order," so there's nothing downstream to adopt. It's a
Postgres-native, single-instance concern given §4's topology: `order_number`
is `'ord_' || nextval(...)` off a plain sequence/identity column — no
Snowflake ID, no UUID, no `sequencer` building block needed, because there's
exactly one writer of record (this Postgres instance) and no cross-node
coordination to solve. It's assigned the moment the pipeline persists the
`orders` row (step 6, §4) — for *every* outcome (succeeded, failed,
validation_failed, skipped), which is why even a rejected attempt is
lookup-able (req. 5) and why `order_job_items.order_number` stays `NULL`
until that line is actually processed, not at job-submission time.

**What *does* come from downstream:** on a `succeeded` creation, the AD/
directory provisioning API's response carries its own identifier for the
group object it just created (e.g. an `objectGUID` in classic AD, or the
`id` GUID Microsoft Graph returns for an Entra ID group — which one depends
on which API variant is in play, §9). That has to be recorded, or this
system has no durable link to the actual AD object beyond `group_name` —
worth having independently of the name, since a rename (out of scope, §1)
would otherwise sever the connection entirely.

- **`orders`** — PK `order_number`. Columns: group_name, primary_owner,
  secondary_owner, status (`succeeded`|`validation_failed`|`failed`|
  `skipped`), **`ad_object_id`** (nullable — the downstream identifier above;
  populated only when status is `succeeded`, `NULL` otherwise since nothing
  was created), `validation_errors` (nullable array — field/code/message,
  populated only when status is `validation_failed`), `error` (nullable
  object — code/message/retryable, populated only when status is `failed`),
  skipped_order_number (nullable — set when status is `skipped`, pointing at
  the order it deferred to), `job_id` (nullable — set when ordered via a bulk
  job), created_at.
  - **Unique index on `group_name` for rows where `status IN ('succeeded',
    'pending', 'processing')`** (a partial/filtered unique index — single
    domain, so no qualifier beyond the name itself, §1) — this is what makes
    "skip if already ordered" correct under concurrency (req. 6/NFR): two
    concurrent orders for the same group name race on this constraint, and
    only one wins the insert; the loser's pipeline observes the conflict and
    returns `skipped` pointing at the winner, rather than the app-level
    pre-check (§3.2) being trusted alone. A `failed` order does **not** hold
    the constraint, so a genuinely failed attempt (nothing was created) can
    be retried without being treated as a duplicate, and the retry may carry
    different parameters than the failed attempt — **confirmed** intended
    behavior (§1), not just how the constraint happens to work.
  - Unique index on `(tenant_id, idempotency_key)` for the sync path's
    retry-safety dedupe (separate from the group-name dedupe above — one is
    "don't double-execute *this* request," the other is "don't order a group
    that's already ordered," and they can both fire independently).
- **`order_jobs`** — PK `job_id`. Unique index on `(tenant_id, idempotency_key)`
  so a resubmitted job body returns the same job. Columns: status, total_lines,
  succeeded/skipped/validation_failed/failed/pending counters (updated by
  workers, not recomputed by scanning), source (`inline`|`upload_id`),
  created_at, completed_at.
- **`order_job_items`** — PK `(job_id, line_index)`. Unique index on
  `(job_id, group_name)` — this is the row that makes a resumed/retried job
  idempotent per line: on reprocessing, a line whose `group_name` already has
  a `succeeded` (or `skipped`) row is returned as-is, not re-run — but one
  that's `validation_failed` *is* re-run on reprocessing (the input didn't
  change, but re-validating is cheap and correct, unlike re-calling AD).
  Columns: status (`succeeded`|`validation_failed`|`failed`|`skipped`),
  order_number (nullable), validation_errors (nullable), error (nullable),
  attempt_count. `GET .../results` pages on `(job_id, line_index)` — a
  stable, monotonic cursor key.
- **`idempotency_keys`** (sync path) — `(tenant_id, idempotency_key)` →
  stored response, TTL 24h, per the standard `api-design` idempotency
  state-machine (pending/complete, reject on same-key-different-body).

**Cache** — logically distinct from the tables above regardless of where it
physically lives (a separate Redis, or a table in this same Postgres
instance — a deployment-topology choice, not a design one; see §4's
Deployment topology). Two namespaces, both cache-aside with a TTL (neither
upstream
system offers invalidation events, so TTL expiry is the only eviction
mechanism):
- `fte:{owner_id} → active|inactive`, **TTL 15 minutes (confirmed).** Long
  enough that a 4,500-line job's owner reuse (§2) collapses to ~50–200
  calls; the staleness this trades for — a cached `active` for someone
  terminated moments ago approving an owner the live API would now reject —
  is a **genuine risk, not a self-correcting one** (AD itself doesn't care
  who the owner is), but the 15-minute window has been confirmed acceptable
  for this system.
- `ldap_exists:{group_name} → found|not_found`, **TTL ~60 seconds.** Short —
  its only real job is avoiding a duplicate LDAP round trip between a
  validate-then-order pair for the same line (§3.2), not surviving across a
  whole bulk job. Staleness here is **self-correcting**: a stale `not_found`
  just means the AD create call itself rejects the group as already
  existing (§3.3) — worse UX (`failed` instead of a clean `skipped`), not a
  correctness bug, because AD's own uniqueness is still the backstop.

Group-name lookup (`GET /v1/orders?group_name=...`, req. 5) is served by the
`group_name` index above — the same index that enforces the duplicate-skip
rule, so "look up by group name" and "detect it's already ordered" are the
same query, not two implementations to keep in sync.

Sharding: not needed, and not expected to ever be needed (§2/§8) — the
100/min rate limit caps real order-creation write volume far below what a
single instance handles trivially; `order_job_items`' ~4.5–9k-row bulk-job
bursts are the largest write burst in the system and are still a normal OLTP
load. If this were ever revisited, `job_id` is the natural shard key since
every access pattern here is job-scoped.

## 6. Key decisions & trade-offs

| Decision | Solves | Worsens | Change it when |
|---|---|---|---|
| Validation is step 1 of the order pipeline (runs on every order call), not a required separate step | Callers can't forget to validate; a `validation_failed` reason is always in the order response itself, no extra round trip needed | The order call always pays validation cost, even for a line dry-run-checked moments earlier — no way to present a "trust me, already validated" token | Blocking checks become expensive enough that skipping a repeat check is worth the complexity → add an optional skip token then |
| `validation_failed` and `failed` as distinct order statuses | A caller can branch on "fix the request" vs "safe to retry" without parsing error codes (req. 1/5) | One more status value in every enum/consumer (job counts, CLI filters, data model) | Never — this is exactly the distinction the requirement asks for |
| LDAP existence check added to validation (not just our own order store) | Closes the "group created outside this process" gap — previously out of scope, now actually detected (§1) | A second external dependency validation now waits on, alongside the FTE check | Never — this is the fix for exactly that gap |
| Cache both the LDAP existence check and the FTE check, cache-aside with TTL | Cuts ~9,000 potential calls/4,500-order job to a few hundred for FTE (owner reuse, §2), and avoids a duplicate LDAP call on validate-then-order for the same line | Two staleness windows to reason about — bounded but real for FTE (§5), self-correcting for LDAP (§5) | The FTE TTL's staleness risk becomes unacceptable to the business → shorten it or drop caching for that check specifically, not both |
| Validation steps ordered cheapest/most-decisive first (regex → availability → FTE → rate-limited AD call) | A request that's going to be skipped or rejected never spends an FTE-check call or a scarce rate-limit token on itself | The pipeline has more sequential stages, and their order is now a meaningful design choice, not incidental | Never — reordering would spend the scarcest resources (rate-limit tokens, and calls to APIs whose own limits are unstated, §9) on doomed requests |
| Sync endpoint (≤25) + separate async job endpoint (up to 4,500+) | Keeps single-order latency low; lets bulk scale independently | Two code paths to keep behaviorally identical (mitigated: both call the same order pipeline, §4) | The sync cutover (25) is wrong for real traffic → tune, don't redesign |
| `group_name` as the dedupe/business key (no separate `external_id`) | One fewer field to require from callers; "look up by group name" and "already ordered" share one index (§5) | Renaming a group later has no clean story here (out of scope, §1) — the key is fixed at order time | A second domain is ever added → key must become `(domain, group_name)`, since §1 confirms single-domain today |
| A single shared rate limiter (one 100/min token bucket), not per-worker limiting | Correctly enforces the real, confirmed constraint (§2) regardless of worker-pool size — adding workers can't accidentally over-spend the budget | Every call path (sync and bulk) now depends on one shared piece of state — it must be fast and available, or nothing can place an order (→ §7) | Never, while the limit is a single system-wide number; if AD ever exposes per-tenant quotas, the bucket becomes per-tenant |
| A reserved slice of the 100/min budget for the sync lane (not just dispatch-order priority) | A big bulk job literally cannot starve single-order latency down to zero, even under the confirmed low ceiling (§2) | Reserved sync capacity is bulk capacity not spent — a max bulk job takes a little longer than the raw 45 min math (§2) | The real traffic mix shows sync calls are rare enough that a reservation wastes bulk throughput → shrink or drop it |
| PostgreSQL (single relational instance) for the order store, not NoSQL | Atomic unique constraints are what make the group-name and idempotency-key dedupe (§5) correct under concurrency; the confirmed 100/min limit keeps volume trivial for one instance | A single writer to keep available (standard HA replica, not a novel problem here) | Never, at this design's scale (§8) — revisit only if `data-storage` scale numbers actually demand a distributed store |
| Skip-by-default on duplicate, `force=true` to override; a `failed` order never blocks a retry (confirmed — §1) | Prevents accidental re-ordering of an existing/in-flight group; a failed attempt can be freely resubmitted, including with different owners, with no `force` needed (req. 5/6) | A retry after `failed` must use a new `Idempotency-Key` (§1) — reusing the old one with different owners is a key-reuse conflict, not a silent success | Never, as confirmed — only if "already ordered" is later redefined to include failed attempts would the unique-index predicate need to widen |
| Duplicate check enforced by a **DB unique constraint**, not just the app-level `orders:validate` pre-check | Correct under concurrency — two racing submissions for the same group can't both succeed | The loser of the race gets its `skipped` result at insert time, not at validate time — slightly less "predictable" from the client's view | Never — this is the correctness backstop; relaxing it reopens the duplicate-group race |
| `force=true` still goes through the real AD create call (not a bypass of AD's own uniqueness) | A forced re-order of a group AD still has fails cleanly with a clear error, never silently duplicates | `force` looks like it "always creates" but sometimes still fails — needs documenting | Never — bypassing AD's own check would let this API create actual duplicate directory objects |
| Inline body (≤500 lines) *or* presigned-upload for bulk | Never hits a gateway payload ceiling as batch size grows | Two ingestion paths in the client/CLI to implement | Gateway payload limits change → could raise the inline threshold instead |
| Idempotency-Key per order **and** per job, `group_name` per line | Safe retries at every granularity (single call, whole job, one line within a resumed job) | Two idempotency scopes plus the business-key dedupe to reason about | Never — this is what makes retries at any level safe |
| Worker-pool size decoupled from throughput (concurrency just hides non-AD-call latency: validation, duplicate check, persist) | No wasted complexity sizing a large pool the rate limiter would throttle anyway | A 4,500-order job still takes ~45 min regardless of pool size (§2) — that's the confirmed limit, not a tuning knob | AD offers a real batch-create API → call that instead of N single calls, which is the only lever that actually changes completion time (§8) |
| Priority lanes for sync vs. bulk line-item dispatch | A 4,500-line job can't starve single-order latency | Scheduler has to be lane-aware, not a plain FIFO queue | Traffic mix makes this moot (e.g. bulk becomes the only path) |

## 7. Failure modes & degradation

- **Invalid input** — bad group-name format, a malformed owner id (regex),
  or an owner who **is not an active full-time employee** — caught at steps
  1–3 of the pipeline. A format failure never reaches the FTE-check API at
  all (cheapest check first, §6); an FTE failure means the check *ran* and
  came back negative, which is why it's `validation_failed` (fixable,
  non-retryable) and not `failed` — the distinction only holds because the
  check actually completed. Returns the specific field/code/message
  immediately — no AD call, no rate-limit token spent either way.
- **The FTE-check or LDAP existence-check API errors outright** (times out,
  5xx — as opposed to a definitive "not active" / "exists" answer) — this is
  **not** the same as a validation failure, because the check never actually
  ran to completion: the order comes back `failed` with `retryable: true`,
  same treatment as an AD provisioning error (`resilience-failure`: timeout +
  circuit breaker per dependency). Conflating "the check said no" with "the
  check couldn't run" would misclassify a transient outage as the caller's
  fault.
- **The cache is unavailable** — fail **open**, not closed: the pipeline
  calls the LDAP/FTE APIs directly rather than blocking orders on a cache
  outage, since the cache is a load-reduction optimization (§5/§6), not a
  correctness mechanism. The cost is degraded — higher latency, and the full
  ~9,000-call load (§2) lands on APIs whose own capacity is unconfirmed
  (§9) — worth alerting on, not silently absorbing indefinitely.
- **The rate limiter itself is unavailable** (§4's shared token bucket is
  down or unreachable) — this is now a real dependency every order goes
  through, sync or bulk (§6). Fail closed: orders return a `retryable: true`
  5xx rather than bypassing the limiter and risking a burst past the
  confirmed 100/min ceiling. Run it as a small, highly-available component
  (e.g. a replicated in-memory store) rather than folding it into the order
  DB, so an order-store blip and a rate-limiter blip aren't the same failure.
- **The sync lane's reserved token slice is exhausted** (a burst of small
  batches) — `429` + `Retry-After`, same contract as any other rate limit
  (`api-design`); the caller backs off and retries, it does not silently
  fall back to consuming bulk-reserved tokens.
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
  — resolves independently (`order_job_items.status = validation_failed|
  failed|skipped` with detail), the other 4,499 proceed; the job's final
  status is `completed_with_errors` when there's a real `failed` line (a
  downstream/execution problem worth flagging) — an all-`skipped` or
  all-`validation_failed` outcome is still `completed`, since the pipeline
  did exactly what it should with cleanly-rejected input, nothing "went
  wrong" operationally.
- **Upload interrupted (large NDJSON)** — the client/CLI retries the PUT and
  resubmits `POST /v1/order-jobs` with the same job `Idempotency-Key`;
  nothing has been processed yet, so this is a clean retry.
- **What the user sees:** a sync call either succeeds, is cleanly skipped
  with the original order number, comes back `validation_failed` with the
  exact reason (their input, fixable, no AD call attempted), or comes back
  `failed` with a retryable/non-retryable downstream error — in ~1.5s for a
  single order, scaling predictably with batch size for a small batch (§1's
  NFR), never an open-ended hang. A bulk job always finishes in a *terminal*
  state visible via
  `GET /v1/order-jobs/{id}` (never "stuck"), with per-line detail for exactly
  the lines that were skipped, failed validation, or failed downstream —
  never an all-or-nothing rollback of 4,500 orders because of one bad group
  name.

## 8. Scale evolution

**Current bottleneck:** the confirmed **100 requests/minute** AD-provisioning
rate limit (§2) — not our own compute or storage, and not even close. A
4,500-order job is a **~45-minute** job purely from that number; nothing
about workers, queues, or the database changes that.

**At 10× (≈45,000 orders/submission):** the arithmetic stops being tolerable
— 45,000 ÷ 100/min is **~7.5 hours** for one job. This is not a "split into
shards and add workers" problem, because concurrency was never the
constraint (§2/§6):
- The inline-body path disappears entirely (mandatory file upload above a
  much lower line count than today's 500) — but that's a payload-size fix,
  not a throughput fix.
- The **only** lever that actually changes completion time is the rate limit
  itself: renegotiate a higher quota, or — better — get a genuine **batch**
  group-create call from the directory API (create N groups in one request,
  counted as one unit against the limit rather than N). Splitting dispatch
  across more queue partitions or workers does **nothing** here, unlike a
  typical bulk-processing bottleneck — worth stating explicitly so a future
  reader doesn't reach for "add more workers" first.
- A job whose expected completion is hours, not minutes, also changes the
  CLI/UX story (§3.5): `--wait` blocking a terminal for 7.5 hours is not
  reasonable — `--watch` polling with a persisted `job_id` to resume against
  becomes the primary flow, not a fallback for the interrupted case.
- **Signal to watch:** job completion-time p95 climbing past a documented
  SLA, or the rate limiter's queue depth growing without bound — either means
  the 100/min ceiling itself needs renegotiating before volume grows further,
  not a scaling fix on this API's side.
- **The caching assumption also needs re-checking at 10×.** §2's ~50–200
  distinct-owner estimate for a 4,500-line job is an assumption about owner
  reuse (§1) — a 45,000-line job doesn't necessarily have 10× the reuse; if
  distinct owners scale with order count instead, the FTE-check cache's hit
  rate (and its protection of that API) degrades exactly when it's needed
  most. Watch cache hit rate as its own metric, not just job completion time.

## 9. Open questions

- **Should an `exists_in_ad` skip backfill an order record?** (§1's now-
  narrower out-of-scope item) The LDAP check catches a group created outside
  this process at validation time, and it's cleanly `skipped` (§3.2/§3.3) —
  but no `order_number` exists for it to point to. Worth deciding whether to
  synthesize a historical order record at that point (so a later
  `GET ?group_name=...` finds *something*), or leave it as a `skipped` order
  with no back-reference, as designed here.
- **What are the LDAP existence-check and FTE-check APIs' own rate limits/
  SLAs?** Unstated (§1/§2) — the caching design (§5/§6) is this system's own
  mitigation regardless of the answer, but the cache TTLs and whether a
  circuit breaker is even needed depend on it.
- **Is the 100/min limit truly one global ceiling**, or actually per
  app-registration/service-principal in a way that would let a second
  registration double the effective budget? This design assumes one shared
  system-wide bucket (§2/§5/§6) as the safe/conservative reading — worth
  confirming, since a per-registration limit would change the rate-limiter
  design (multiple buckets, one per registration) and the §8 scale story.
- **Does AD's own `owners` semantics distinguish "primary" from
  "secondary"?**, or is that distinction purely this system's metadata
  (§3.1) — `primary_owner != secondary_owner` itself is now confirmed
  (§1/§3.2), this is only about how the two map onto AD's owner model.
- **Exact owner-id regex** — assumed to be an email/employee-id shape (§1);
  the real pattern (and whether `primary_owner`/`secondary_owner` must be
  the same *kind* of identifier) needs the actual policy.
- **Which directory API variant, exactly** — classic AD (LDAP,
  `objectGUID`) vs. Microsoft Graph/Entra ID (`id`) — decides `ad_object_id`'s
  actual shape (§5) and the concrete request/response fields in §3.3's
  downstream calls, which are written generically ("AD/directory
  provisioning API") pending this.
- Auth model (API key vs. OAuth client-credentials, and how `primary_owner`/
  `secondary_owner` are authorized to receive a new group) — assumed but not
  designed; doesn't change the shapes above either way.

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

**Weakest dimension:** the two APIs this design now depends on for
validation — LDAP existence-check and active-FTE-check — have **unstated
capacity** (§1/§2/§9). Every number in §2 for them (9,000 uncached calls,
~50–200 cached) is this design's own estimate of the load it will offer, not
a confirmed budget those systems can absorb; the caching strategy (§5/§6) is
built to reduce that load regardless, but whether it reduces it *enough*
can't be confirmed until those APIs' real limits are known. §9's other open
questions (whether the 100/min limit is truly global, the `exists_in_ad`
backfill decision, the exact owner-id regex) are the design's next-most
load-bearing choices, but all are second to this one.
