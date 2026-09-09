#!/usr/bin/env python3
"""itrs_notify.py — Geneos Gateway Effect script.

This is the program Geneos forks *per trigger* (rule/alert/action "run a
program" effect). At the target peak of 60,000 triggers/min (1,000/s, see
docs/design/itrs-gateway-event-script.md), this process is spawned up to
~1,000 times a second, possibly by several different rules/samplers/hosts
concurrently. It therefore does the least possible work and NEVER talks to
the network itself:

    read env vars -> build a small JSON envelope -> hand off locally -> exit

The actual HTTP POST to the Event Management System is owned by the
long-running `event_relay.py` daemon (one per Gateway host), which keeps a
pooled/keep-alive connection, retries, and a circuit breaker. That split is
what keeps this script's own wall-time low and independent of the EMS's
latency or availability — see the design doc, section 4.

Stdlib only, on purpose: nothing to install on the Gateway host, and no
import-time cost beyond the interpreter's own startup.

Exit code is always 0 unless something outside the documented fallback path
goes wrong (e.g. the spool disk itself is unwritable) — a non-zero exit here
would make Geneos treat delivery as an effect failure, and at this trigger
rate a Gateway-side effect-retry policy would turn a slow EMS into a
retry storm on top of the one this design already avoids downstream.
"""
from __future__ import annotations

import json
import os
import socket
import struct
import sys
import time
import uuid
from datetime import datetime, timezone

# --- configuration (env-overridable; no config file needed for one script) -
RELAY_SOCK = os.environ.get("ITRS_RELAY_SOCK", "/var/run/itrs-event-relay/relay.sock")
SPOOL_FILE = os.environ.get("ITRS_RELAY_SPOOL", "/var/spool/itrs-event-relay/pending.jsonl")
CONNECT_TIMEOUT_S = float(os.environ.get("ITRS_RELAY_CONNECT_TIMEOUT_S", "0.10"))
ACK_TIMEOUT_S = float(os.environ.get("ITRS_RELAY_ACK_TIMEOUT_S", "0.15"))
# POSIX guarantees a write() of at most PIPE_BUF (commonly 4096B on Linux) is
# atomic when a file is opened O_APPEND — that's what lets many concurrent
# itrs_notify.py processes append to the same spool file with no locking.
MAX_SPOOL_RECORD_BYTES = 4096

# --- Geneos variable mapping ------------------------------------------------
# Best-effort names for the well-known Effect environment variables, based on
# ITRS Geneos's documented convention (underscore-prefixed, mirroring the
# rule-language accessors: rowname()/column()/value()/...). This sandbox's
# network policy blocked docs.itrsgroup.com / support.itrsgroup.com, so this
# table could not be re-verified against the live page at write time —
# verify against a real Gateway (a throwaway effect that runs
# `env > /tmp/env.dump` shows the real names) and correct this table before
# go-live. Nothing is lost in the meantime: every `_`-prefixed var is also
# captured verbatim into `attributes` below regardless of this table.
VAR_MAP = {
    "severity": ("_SEVERITY",),
    "headline": ("_HEADLINE", "_VARIABLE"),
    "message": ("_MESSAGE",),
    "gateway": ("_GATEWAY", "_GATEWAY_NAME"),
    "probe": ("_NETPROBE_HOST", "_PROBE"),
    "probe_port": ("_NETPROBE_PORT",),
    "managed_entity": ("_MANAGED_ENTITY", "_ENTITY"),
    "sampler": ("_SAMPLER",),
    "sampler_type": ("_SAMPLER_TYPE",),
    "dataview": ("_DATAVIEW",),
    "row": ("_ROWNAME", "_ROW"),
    "column": ("_COLUMN",),
    "value": ("_CELL", "_VALUE"),
    "rule": ("_RULE",),
    "triggered_at_raw": ("_TIMESTAMP",),
}


def _first_env(*names: str) -> str | None:
    for name in names:
        val = os.environ.get(name)
        if val:
            return val
    return None


def build_envelope() -> dict:
    """Turn the process's environment into the EMS-bound event envelope.

    See docs/design/itrs-gateway-event-script.md section 3a/3c for the
    contract this builds toward.
    """
    raw_geneos_vars = {k: v for k, v in os.environ.items() if k.startswith("_")}

    envelope = {
        "event_id": str(uuid.uuid4()),  # doubles as the EMS Idempotency-Key
        "source": "geneos",
        "triggered_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ"),
        "attributes": raw_geneos_vars,  # forward-compatible safety net
    }
    for field, candidates in VAR_MAP.items():
        val = _first_env(*candidates)
        if val is not None:
            envelope[field] = val
    return envelope


def _encode_frame(payload: bytes) -> bytes:
    return struct.pack(">I", len(payload)) + payload


def try_handoff_to_daemon(payload: bytes) -> bool:
    """Best-effort local hand-off to the relay daemon over a Unix socket.

    Returns True only on an explicit 'K' (queued) ack from the daemon within
    budget. Any failure (no daemon, socket full, timeout, NACK) returns
    False so the caller falls back to the disk spool — this call must never
    block the script for long: the timeouts here are the whole reason the
    Gateway-visible latency stays low regardless of what the EMS is doing.
    """
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            sock.settimeout(CONNECT_TIMEOUT_S)
            sock.connect(RELAY_SOCK)
            sock.sendall(_encode_frame(payload))
            sock.settimeout(ACK_TIMEOUT_S)
            ack = sock.recv(1)
            return ack == b"K"
    except OSError:
        return False


def spool_to_disk(payload: bytes) -> None:
    """Fallback path when the daemon is unreachable/busy: append one JSON
    line to the local spool file. A background spool-drainer in
    event_relay.py picks these up (see the design doc section 6).

    Deliberately uses a single write() under PIPE_BUF so the append stays
    atomic under concurrent writers without file locking. If the record
    would exceed that, attributes are trimmed rather than risking a torn
    write interleaved with another process's line.
    """
    line = payload + b"\n"
    if len(line) > MAX_SPOOL_RECORD_BYTES:
        envelope = json.loads(payload)
        envelope["attributes"] = {"_truncated": "record exceeded spool record size limit"}
        line = json.dumps(envelope, separators=(",", ":")).encode("utf-8") + b"\n"
        if len(line) > MAX_SPOOL_RECORD_BYTES:
            line = line[: MAX_SPOOL_RECORD_BYTES - 1] + b"\n"

    os.makedirs(os.path.dirname(SPOOL_FILE), exist_ok=True)
    fd = os.open(SPOOL_FILE, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    try:
        os.write(fd, line)
    finally:
        os.close(fd)


def main() -> int:
    start = time.monotonic()
    envelope = build_envelope()
    payload = json.dumps(envelope, separators=(",", ":")).encode("utf-8")

    delivered_locally = try_handoff_to_daemon(payload)
    if not delivered_locally:
        try:
            spool_to_disk(payload)
        except OSError as exc:
            # Truly out of options (e.g. disk full) — drop, don't hang the
            # Gateway effect on it, but say so loudly for the on-call.
            sys.stderr.write(f"itrs_notify: dropped event {envelope['event_id']}: {exc}\n")
            return 0

    if os.environ.get("ITRS_NOTIFY_DEBUG"):
        elapsed_ms = (time.monotonic() - start) * 1000
        sys.stderr.write(
            f"itrs_notify: event_id={envelope['event_id']} "
            f"via={'daemon' if delivered_locally else 'spool'} "
            f"wall_time_ms={elapsed_ms:.1f}\n"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
