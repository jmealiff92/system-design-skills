# ITRS Geneos Gateway → EMS event relay

Working code for `docs/design/itrs-gateway-event-script.md`. Read that first —
this README is setup/ops instructions, not the design rationale.

## What's here

| File | Runs as | Role |
|---|---|---|
| `itrs_notify.py` | Geneos Gateway Effect (forked per trigger) | reads env vars, hands the event to the relay daemon, exits. Stdlib only. |
| `event_relay.py` | a long-running daemon, one per Gateway host | owns the pooled HTTPS connection, retries, circuit breaker, spool/DLQ. Needs `httpx`. |
| `itrs-event-relay.service` | — | example systemd unit for `event_relay.py`. |

## Why two programs and not one script

Geneos forks a fresh OS process per trigger. At the target peak (60,000/min
= 1,000/s), a script that itself opens a TLS connection to the EMS per
invocation can't reuse connections, can't share a circuit breaker across
invocations, and turns a single slow EMS into up to 1,000 independent,
uncoordinated retry decisions per second. `event_relay.py` is the one
long-running process that can hold a connection pool and coordinate that
policy; `itrs_notify.py` stays a thin, fast, stdlib-only hand-off so the
Gateway-visible latency of the effect never depends on the EMS's health.
Full reasoning: design doc sections 2 and 4.

## Setup

1. **Install the daemon's one dependency** (only where `event_relay.py`
   runs — not on every Gateway host):
   ```bash
   pip install -r requirements.txt
   ```
2. **Deploy `event_relay.py`** to each Gateway host (co-located with the
   Gateway, so the hand-off socket is local) and run it as a service:
   ```bash
   cp event_relay.py /opt/itrs-event-relay/
   cp itrs-event-relay.service /etc/systemd/system/
   mkdir -p /etc/itrs-event-relay
   printf 'ITRS_EMS_TOKEN=...\n' > /etc/itrs-event-relay/env
   chmod 600 /etc/itrs-event-relay/env
   useradd --system --no-create-home itrs-relay || true
   systemctl daemon-reload
   systemctl enable --now itrs-event-relay
   ```
3. **Deploy `itrs_notify.py`** anywhere Geneos can invoke it (no install
   step — it's one stdlib file). Point a Gateway Effect at it:
   - **Command:** `/usr/bin/python3`
   - **Arguments:** `/opt/itrs-notify/itrs_notify.py`
   - No extra environment configuration is required *for the hand-off* —
     Geneos already exports the trigger context as env vars, which is all
     the script reads. Set `ITRS_RELAY_SOCK` only if the daemon's socket
     path differs from the default (`/var/run/itrs-event-relay/relay.sock`,
     matched by the systemd unit's `RuntimeDirectory=`).
4. **Verify the real Geneos variable names before relying on severity/
   headline/etc. in the EMS.** This repo's sandbox could not reach
   `docs.itrsgroup.com` to re-verify them (see the note at the top of the
   design doc). Run one throwaway effect with `env > /tmp/env.dump` on a
   real Gateway, compare against `VAR_MAP` in `itrs_notify.py`, and correct
   the table if any name differs — nothing is lost either way, because
   every `_`-prefixed variable also lands verbatim in the event's
   `attributes` field regardless of `VAR_MAP`.

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
| `ITRS_RELAY_HTTP2` | daemon | `0` | set `1` to enable HTTP/2 (needs the `h2` extra, see requirements.txt) |
| `ITRS_NOTIFY_DEBUG` | script | *(unset)* | set `1` to log the script's own wall-time to stderr |

## Trying it locally (no real Gateway needed)

```bash
pip install -r requirements.txt

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
ITRS_RELAY_DLQ=/tmp/dead_letter.jsonl python3 event_relay.py

# terminal 3: simulate a Geneos trigger
ITRS_RELAY_SOCK=/tmp/relay.sock ITRS_NOTIFY_DEBUG=1 \
_SEVERITY=critical _HEADLINE=CPU_Usage _MANAGED_ENTITY=host07 \
python3 itrs_notify.py
```

This exact flow (plus killing the fake EMS mid-stream to exercise the
spool/circuit-breaker/DLQ paths) is how this implementation was validated
during development.
