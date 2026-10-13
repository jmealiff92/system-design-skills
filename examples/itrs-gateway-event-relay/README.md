# ITRS Geneos Gateway → EMS notifier

Working code for `docs/design/itrs-gateway-event-script.md`. Read that first —
this README is setup/ops instructions, not the design rationale.

## What's here

| Path | Builds/runs as | Role |
|---|---|---|
| `cmd/itrs-notify/` | Geneos Gateway Effect (forked per trigger) | reads env vars, POSTs the event to the EMS directly, exits. |
| `internal/spool/`, `internal/uuid/` | shared code, compiled into the binary | atomic dead-letter line format; dependency-free UUIDv4. |
| `ansible/` | — | the deployment mechanism — an Ansible role/playbook for the ~80-host fleet. See `ansible/README.md`. |
| `benchmark/` | — | load tests checking the volume-based design decisions empirically, with real measured numbers. See `benchmark/BENCHMARK.md`. |

One **single static binary** (`CGO_ENABLED=0`) with **zero runtime
dependencies** — nothing to install on any of the ~80 hosts this runs on.

If you're looking at an older checkout with a `cmd/event-relay/`
directory or an `itrs_event_relay` Ansible role: that was an earlier
version of this design built around a since-corrected volume assumption.
See "Why one binary, not two" below and the design doc's revision history.

## Why one binary, not two

An earlier version of this design split the work across two programs — a
thin per-trigger script handing off to a long-running relay daemon that
owned a connection pool, a circuit breaker, and a queue — reasoning from a
peak of 60,000 triggers/min. That number turned out to be the *fleet-wide*
aggregate across ~80 hosts, not what any single host sees. The real
per-host numbers are **peak 835/min (~14/s), highest average 14/min
(~0.23/s)** — and at that rate, Little's Law alone settles it:
N ≈ λ×W ≈ 14/s × 0.3s ≈ **4** concurrent in-flight requests even in the
worst case. There's no concurrent load to pool connections for, no
backlog for a breaker to protect against, and nothing for a queue to
buffer — see `benchmark/BENCHMARK.md` for the re-measured proof, not just
the arithmetic.

What the relay was built to fix that *doesn't* evaporate with lower
volume: a backend that accepts a TCP connection and never responds hangs
a caller with no read timeout **indefinitely**, at any trigger rate. But
that was always a missing-`--max-time`-flag bug, not a case for a second
long-running process — `itrs-notify` carries the fix itself now (a
bounded per-attempt timeout + a small retry budget), with no daemon
required to get it.

**Still Go, unaffected by the volume correction:** ~80 Gateway hosts
across regions can't be assumed to carry a matching, correctly-configured
language runtime — a different base image, patch level, or
whoever-provisioned-it in another region is exactly the kind of drift
that turns into "works on 78 of 80 Gateways" at 2am. A `CGO_ENABLED=0` Go
build is one **static binary**: no interpreter version to match, no
glibc ABI to match either. That argument never depended on throughput, so
the volume correction doesn't touch it.

The architecture (design doc sections 2–4) is now just: one program, one
HTTP call, one local dead-letter file on failure. Simpler than the
two-program version not because simpler is a virtue in the abstract, but
because the number that justified the complexity was wrong.

## Build

```bash
# From this directory. Fully static — no libc/interpreter dependency on
# the target host. Name matches what ansible/roles/itrs_notify expects
# (itrs-notify-linux-<goarch>) so the build output can be dropped straight
# into ansible/dist/ and deployed as-is.
mkdir -p ansible/dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ansible/dist/itrs-notify-linux-amd64 ./cmd/itrs-notify

# A region running arm64 hosts: same source, swap GOARCH=arm64 and build
# again into the *-linux-arm64 filename — no toolchain needed on the
# host, and ansible/roles/itrs_notify picks the right one per host via
# ansible_facts['architecture']. See ansible/README.md.
```

Build once in CI. `go vet ./...` and `gofmt -l .` are clean on this tree —
wire them into CI alongside the build, before publishing the artifact
Ansible deploys.

## Deploying

**Use `ansible/`** — see `ansible/README.md` for the full walkthrough
(inventory, vault-managed EMS token, canary-then-wave rollout across the
~80 hosts).

For understanding what gets deployed and why (skip if you're only running
the playbook):

1. **`itrs-notify`** is just a file Geneos invokes — no service, no
   daemon, no install step beyond the binary and a small secrets file
   being present. Point a Gateway Effect at:
   - **Command:** `/opt/itrs-notify/itrs-notify` (the Ansible role's
     default `itrs_notify_install_dir`)
   - **Arguments:** *(none needed)*
   - No extra environment configuration is required for the EMS URL/token
     specifically — the Ansible role writes `/etc/itrs-notify/env`
     (mode `0600`) and the binary reads it at startup, filling in only
     what Geneos's own Effect environment didn't already set. An
     Effect-level override still wins if you ever need one per-rule.
2. **Verify the real Geneos variable names before relying on severity/
   headline/etc. in the EMS.** This repo's sandbox could not reach
   `docs.itrsgroup.com` to re-verify them (see the note at the top of the
   design doc). Run one throwaway effect with `env > /tmp/env.dump` on a
   real Gateway, compare against `varMap` in `cmd/itrs-notify/main.go`, and
   correct the table if any name differs — nothing is lost either way,
   because every `_`-prefixed variable also lands verbatim in the event's
   `attributes` field regardless of `varMap`.

## Configuration reference (env vars)

All of these can come from the Effect's own environment or from the
`ITRS_NOTIFY_ENV_FILE` fallback (default `/etc/itrs-notify/env`,
Ansible-managed) — an explicit Effect-level value always wins.

| Variable | Default | Meaning |
|---|---|---|
| `ITRS_NOTIFY_EMS_URL` | *required* | EMS ingest endpoint |
| `ITRS_NOTIFY_EMS_TOKEN` | *(none)* | bearer token, if the EMS needs one |
| `ITRS_NOTIFY_ATTEMPT_TIMEOUT_S` | `0.15` | per-HTTP-attempt timeout |
| `ITRS_NOTIFY_MAX_RETRIES` | `1` | retries after the first attempt, before the dead-letter file |
| `ITRS_NOTIFY_DEAD_LETTER_FILE` | `/var/spool/itrs-notify/dead_letter.jsonl` | where a delivery that exhausted retries or hit a permanent (4xx) rejection is recorded |
| `ITRS_NOTIFY_ENV_FILE` | `/etc/itrs-notify/env` | fallback config file (see above) |
| `ITRS_NOTIFY_DEBUG` | *(unset)* | set to any value to log outcome/attempts/wall-time to stderr |

## Trying it locally (no real Gateway needed)

```bash
go build -o itrs-notify ./cmd/itrs-notify

# terminal 1: a stand-in EMS that just 202s everything
python3 -c "
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); self.rfile.read(n)
        self.send_response(202); self.end_headers()
HTTPServer(('127.0.0.1', 8940), H).serve_forever()
"

# terminal 2: simulate a Geneos trigger
ITRS_NOTIFY_DEBUG=1 ITRS_NOTIFY_EMS_URL=http://127.0.0.1:8940/api/v1/events \
_SEVERITY=critical _HEADLINE=CPU_Usage _MANAGED_ENTITY=host07 \
./itrs-notify
```

Try it again pointed at a backend that accepts the connection and never
responds (swap the Python one-liner for `benchmark/fake_ems_hung.py`) to
see the bounded-timeout behavior: `itrs-notify` still exits in well under
500ms and writes a dead-letter record instead of hanging. This exact flow
(plus the naive-vs-hardened comparison at the real measured volume) is
how this implementation was validated — see `benchmark/BENCHMARK.md`.
