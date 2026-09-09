# ITRS Geneos Gateway → EMS event relay

Working code for `docs/design/itrs-gateway-event-script.md`. Read that first —
this README is setup/ops instructions, not the design rationale.

## What's here

| Path | Builds/runs as | Role |
|---|---|---|
| `cmd/itrs-notify/` | Geneos Gateway Effect (forked per trigger) | reads env vars, hands the event to the relay daemon, exits. |
| `cmd/event-relay/` | a long-running daemon, one per Gateway host | owns the pooled HTTPS connection, retries, circuit breaker, spool/DLQ. |
| `internal/spool/`, `internal/uuid/` | shared code, compiled into both binaries | atomic spool/DLQ line format; dependency-free UUIDv4. |
| `ansible/` | — | the deployment mechanism — an Ansible role/playbook for the ~80-host fleet. See `ansible/README.md`. |
| `benchmark/` | — | load tests proving *why* this design replaces a naive per-effect shell script, with real measured numbers. See `benchmark/BENCHMARK.md`. |

Both are **single static binaries** (`CGO_ENABLED=0`) with **zero runtime
dependencies** — nothing to `pip install`/`apt install` on any host, and
nothing to install *at all* on the ~80 hosts `itrs-notify` runs on.

## Why Go, and why two programs

**Two programs, not one:** Geneos forks a fresh OS process per trigger. At
the target peak (60,000/min = 1,000/s), a script that itself opens a TLS
connection to the EMS per invocation can't reuse connections, can't share a
circuit breaker across invocations, and turns a single slow EMS into up to
1,000 independent, uncoordinated retry decisions per second. `event-relay`
is the one long-running process that can hold a connection pool and
coordinate that policy; `itrs-notify` stays a thin, fast hand-off so the
Gateway-visible latency of the effect never depends on the EMS's health.

**Go, not a scripting language, given ~80 Gateway hosts across regions:**
two separate reasons stack here, and the second is the deciding one.
- *Startup cost.* `itrs-notify` is forked up to ~1,000 times/second at peak.
  A compiled Go binary starts in well under a millisecond — no interpreter
  boot, no import machinery — vs. tens of milliseconds for even a bare
  interpreter start. That's real CPU/RSS relief on the Gateway host at this
  fork rate.
- *Environment drift across 80 hosts in different regions.* This is the
  bigger one. A script needs a matching, correctly-configured runtime on
  every single host it runs on — a different base image, patch level, or
  whoever-provisioned-it in another region is exactly the kind of drift
  that turns into "works on 78 of 80 Gateways" at 2am. A `CGO_ENABLED=0`
  Go build is one **static binary**: no interpreter version to match, no
  glibc ABI to match either (that's the same drift risk landing at a lower
  layer). Copy the same file to all 80 hosts and it behaves identically —
  there's no "environment" left to differ.

Bonus, not the deciding factor: Go's stdlib `net/http` `Transport` gives
connection pooling/keep-alive for free, so unlike a scripting-language
version of `event-relay`, this daemon needs zero third-party dependencies
either — `go build` is the entire install step.

The architecture (design doc sections 2–4) doesn't depend on the language;
only the implementation does.

## Build

```bash
# From this directory. Fully static — no libc/interpreter dependency on
# the target host. Name matches what ansible/roles/itrs_event_relay expects
# (event-relay-linux-<goarch>, itrs-notify-linux-<goarch>) so the build
# output can be dropped straight into ansible/dist/ and deployed as-is.
mkdir -p ansible/dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ansible/dist/itrs-notify-linux-amd64 ./cmd/itrs-notify
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ansible/dist/event-relay-linux-amd64 ./cmd/event-relay

# A region running arm64 hosts: same source, swap GOARCH=arm64 and build
# again into the *-linux-arm64 filenames — no toolchain needed on the host,
# and ansible/roles/itrs_event_relay picks the right one per host via
# ansible_architecture. See ansible/README.md.
```

Build once in CI. `go vet ./...` and `gofmt -l .` are clean on this tree —
wire them into CI alongside the build, before publishing the artifacts
Ansible deploys.

## Deploying

**Use `ansible/`** — see `ansible/README.md` for the full walkthrough
(inventory, vault-managed EMS token, canary-then-wave rollout across the
~80 hosts). It handles both binaries, the systemd unit, and the shared
spool directory's cross-user permissions (`itrs-notify` and `event-relay`
run as different system users — see that README for why a plain
`chmod 777` isn't the right fix).

For understanding what gets deployed and why (skip if you're only running
the playbook):

1. **`event-relay`** runs as a systemd service, one per Gateway host,
   co-located with the Gateway so the hand-off socket is local:
   `ansible/roles/itrs_event_relay/templates/itrs-event-relay.service.j2`
   is the canonical unit definition — read it rather than hand-rolling one,
   since it encodes the cross-user spool-permission fix above.
2. **`itrs-notify`** is just a file Geneos invokes — no service, no
   install step beyond the binary being present. Point a Gateway Effect at:
   - **Command:** `/opt/itrs-notify/itrs-notify` (the Ansible role's default
     `itrs_notify_install_dir`)
   - **Arguments:** *(none needed)*
   - No extra environment configuration is required *for the hand-off* —
     Geneos already exports the trigger context as env vars, which is all
     the binary reads. Set `ITRS_RELAY_SOCK` only if the daemon's socket
     path differs from the default (`/var/run/itrs-event-relay/relay.sock`,
     matched by the systemd unit's `RuntimeDirectory=`).
3. **Verify the real Geneos variable names before relying on severity/
   headline/etc. in the EMS.** This repo's sandbox could not reach
   `docs.itrsgroup.com` to re-verify them (see the note at the top of the
   design doc). Run one throwaway effect with `env > /tmp/env.dump` on a
   real Gateway, compare against `varMap` in `cmd/itrs-notify/main.go`, and
   correct the table if any name differs — nothing is lost either way,
   because every `_`-prefixed variable also lands verbatim in the event's
   `attributes` field regardless of `varMap`.

## Configuration reference (env vars)

| Variable | Used by | Default | Meaning |
|---|---|---|---|
| `ITRS_EMS_URL` | daemon | *required* | EMS ingest endpoint |
| `ITRS_EMS_TOKEN` | daemon | *(none)* | bearer token, if the EMS needs one |
| `ITRS_RELAY_SOCK` | both | `/var/run/itrs-event-relay/relay.sock` | UDS path for the hand-off |
| `ITRS_RELAY_SPOOL` | both | `/var/spool/itrs-event-relay/pending.jsonl` | disk fallback/replay file |
| `ITRS_RELAY_DLQ` | daemon | `/var/spool/itrs-event-relay/dead_letter.jsonl` | exhausted-retry / permanently-rejected events |
| `ITRS_RELAY_WORKERS` | daemon | `200` | concurrent in-flight POSTs (sized in the design doc, §2/§4) |
| `ITRS_RELAY_QUEUE_MAXSIZE` | daemon | `20000` | in-memory buffer before spilling to disk (~20s of peak) |
| `ITRS_RELAY_ATTEMPT_TIMEOUT_S` | daemon | `0.15` | per-HTTP-attempt timeout |
| `ITRS_RELAY_MAX_RETRIES` | daemon | `1` | retries after the first attempt, before DLQ |
| `ITRS_RELAY_BREAKER_THRESHOLD` | daemon | `20` | consecutive failures before the breaker opens |
| `ITRS_RELAY_BREAKER_OPEN_S` | daemon | `5.0` | cooldown before a single half-open probe |
| `ITRS_RELAY_HTTP2` | daemon | `0` | set `1` to enable HTTP/2 (via Go's stdlib `ForceAttemptHTTP2`, no extra dependency) |
| `ITRS_NOTIFY_DEBUG` | `itrs-notify` | *(unset)* | set to any value to log the binary's own wall-time to stderr |

## Trying it locally (no real Gateway needed)

```bash
go build -o itrs-notify ./cmd/itrs-notify
go build -o event-relay ./cmd/event-relay

# terminal 1: a stand-in EMS that just 202s everything
python3 -c "
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); self.rfile.read(n)
        self.send_response(202); self.end_headers()
HTTPServer(('127.0.0.1', 8940), H).serve_forever()
"

# terminal 2: the relay daemon
ITRS_EMS_URL=http://127.0.0.1:8940/api/v1/events \
ITRS_RELAY_SOCK=/tmp/relay.sock ITRS_RELAY_SPOOL=/tmp/pending.jsonl \
ITRS_RELAY_DLQ=/tmp/dead_letter.jsonl ./event-relay

# terminal 3: simulate a Geneos trigger
ITRS_RELAY_SOCK=/tmp/relay.sock ITRS_NOTIFY_DEBUG=1 \
_SEVERITY=critical _HEADLINE=CPU_Usage _MANAGED_ENTITY=host07 \
./itrs-notify
```

This exact flow (plus killing the fake EMS mid-stream to exercise the
spool/circuit-breaker/DLQ paths, then restarting it to confirm the breaker
self-heals and drains the backlog) is how this implementation was
validated during development — `go build`/`go vet`/`gofmt -l .` are clean,
and the resilience paths (EMS-down spool fallback, breaker trip at the
consecutive-failure threshold, automatic drain on recovery with the
accepted/sent/DLQ counts reconciling exactly) were exercised end to end.
