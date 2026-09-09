#!/usr/bin/env python3
"""event_relay.py — long-running local daemon, one per Geneos Gateway host.

Owns everything itrs_notify.py deliberately does NOT do: the pooled/
keep-alive HTTP connection to the Event Management System (EMS), retries
with backoff+jitter, a circuit breaker, backpressure, and a dead-letter
file. See docs/design/itrs-gateway-event-script.md for the full reasoning
(sections 4, 6, 7); this file is that design's section-4 diagram made
concrete.

Data flow:

    itrs_notify.py (many, short-lived) --UDS--> [bounded asyncio.Queue] --> worker pool --HTTPS(pooled)--> EMS
                                        \\-on NACK/daemon-down-> disk spool -/  (drained back in, respecting backpressure)
                                                                                 \\-> exhausted retries / permanent 4xx -> DLQ file

Run: `python3 event_relay.py` (needs `httpx`; see requirements.txt).
Config is env-var driven — see the constants below — because this is a
single-purpose daemon, not a multi-tenant service; a config file would be
one more thing to keep in sync across Gateway hosts for no benefit here.
"""
from __future__ import annotations

import asyncio
import glob
import json
import logging
import os
import random
import signal
import struct
import time
from dataclasses import dataclass, field

import httpx

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("event_relay")

# --- configuration -----------------------------------------------------------
RELAY_SOCK = os.environ.get("ITRS_RELAY_SOCK", "/var/run/itrs-event-relay/relay.sock")
SPOOL_FILE = os.environ.get("ITRS_RELAY_SPOOL", "/var/spool/itrs-event-relay/pending.jsonl")
DLQ_FILE = os.environ.get("ITRS_RELAY_DLQ", "/var/spool/itrs-event-relay/dead_letter.jsonl")

EMS_URL = os.environ["ITRS_EMS_URL"]  # e.g. https://ems.internal/api/v1/events
EMS_TOKEN = os.environ.get("ITRS_EMS_TOKEN", "")

# Sized in docs/design/itrs-gateway-event-script.md section 2/4: peak 1,000
# events/s, Little's Law N ~= 1,000 * 0.15s ~= 150 in-flight, +headroom.
WORKER_CONCURRENCY = int(os.environ.get("ITRS_RELAY_WORKERS", "200"))
QUEUE_MAXSIZE = int(os.environ.get("ITRS_RELAY_QUEUE_MAXSIZE", "20000"))  # ~20s of peak buffered in memory

PER_ATTEMPT_TIMEOUT_S = float(os.environ.get("ITRS_RELAY_ATTEMPT_TIMEOUT_S", "0.15"))
MAX_RETRIES = int(os.environ.get("ITRS_RELAY_MAX_RETRIES", "1"))  # 1 retry -> 2 attempts, fits the 500ms budget

BREAKER_FAILURE_THRESHOLD = int(os.environ.get("ITRS_RELAY_BREAKER_THRESHOLD", "20"))
BREAKER_OPEN_DURATION_S = float(os.environ.get("ITRS_RELAY_BREAKER_OPEN_S", "5.0"))

SPOOL_ROTATE_INTERVAL_S = float(os.environ.get("ITRS_RELAY_ROTATE_INTERVAL_S", "2.0"))
METRICS_LOG_INTERVAL_S = float(os.environ.get("ITRS_RELAY_METRICS_INTERVAL_S", "10.0"))

# Same limit and rationale as itrs_notify.py: keep spool/DLQ line writes
# under PIPE_BUF so concurrent appenders (many notify processes, plus this
# daemon re-spooling on a breaker-open path) never need file locking.
MAX_SPOOL_RECORD_BYTES = 4096


# --- shared on-disk record format --------------------------------------------
def encode_jsonl_record(record: dict) -> bytes:
    """Serialize one record to a single spool/DLQ line, trimming rather than
    risking a torn write if it would exceed the atomic-append size limit.
    Kept byte-for-byte compatible with itrs_notify.py's spool_to_disk()."""
    line = json.dumps(record, separators=(",", ":")).encode("utf-8") + b"\n"
    if len(line) > MAX_SPOOL_RECORD_BYTES:
        record = dict(record)
        record["attributes"] = {"_truncated": "record exceeded spool record size limit"}
        line = json.dumps(record, separators=(",", ":")).encode("utf-8") + b"\n"
        if len(line) > MAX_SPOOL_RECORD_BYTES:
            line = line[: MAX_SPOOL_RECORD_BYTES - 1] + b"\n"
    return line


def atomic_append(path: str, line: bytes) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    try:
        os.write(fd, line)
    finally:
        os.close(fd)


def append_to_dlq(envelope: dict, reason: str, attempts: int) -> None:
    record = {**envelope, "failure_reason": reason, "attempts": attempts}
    atomic_append(DLQ_FILE, encode_jsonl_record(record))


# --- circuit breaker (resilience-failure: fail fast, single-probe recovery) --
class CircuitBreaker:
    """Closed -> Open on N consecutive failures; Open -> Half-open after a
    cooldown, admitting exactly one probe call; probe success closes it,
    probe failure reopens it. Deliberately simple (single event-loop thread,
    no locking needed) — see the design doc section 4/6 for why a breaker
    is load-bearing here at all (stop a dead EMS from being hammered by
    every one of ~200 workers at once)."""

    CLOSED, OPEN, HALF_OPEN = "closed", "open", "half_open"

    def __init__(self, failure_threshold: int, open_duration_s: float):
        self.failure_threshold = failure_threshold
        self.open_duration_s = open_duration_s
        self._state = self.CLOSED
        self._consecutive_failures = 0
        self._opened_at = 0.0
        self._probe_in_flight = False

    def allow_request(self) -> bool:
        now = time.monotonic()
        if self._state == self.OPEN:
            if now - self._opened_at < self.open_duration_s:
                return False
            self._state = self.HALF_OPEN
            self._probe_in_flight = False
        if self._state == self.HALF_OPEN:
            if self._probe_in_flight:
                return False
            self._probe_in_flight = True
            return True
        return True  # closed

    def record_success(self) -> None:
        self._consecutive_failures = 0
        if self._state in (self.HALF_OPEN, self.OPEN):
            log.info("circuit breaker closing (probe succeeded)")
        self._state = self.CLOSED
        self._probe_in_flight = False

    def record_failure(self) -> None:
        self._consecutive_failures += 1
        if self._state == self.HALF_OPEN:
            log.warning("circuit breaker re-opening (probe failed)")
            self._state = self.OPEN
            self._opened_at = time.monotonic()
            self._probe_in_flight = False
        elif self._state == self.CLOSED and self._consecutive_failures >= self.failure_threshold:
            log.warning("circuit breaker opening after %d consecutive failures", self._consecutive_failures)
            self._state = self.OPEN
            self._opened_at = time.monotonic()

    @property
    def state(self) -> str:
        return self._state


@dataclass
class Metrics:
    accepted: int = 0
    spooled_fallback: int = 0
    sent_ok: int = 0
    breaker_open_respooled: int = 0
    dlq: int = 0
    dropped: int = 0


def backoff_with_jitter(attempt: int) -> float:
    base = 0.05 * (2 ** (attempt - 1))  # 50ms, 100ms, 200ms, ...
    return base + random.uniform(0, base)


class Relay:
    def __init__(self):
        self.queue: asyncio.Queue[dict] = asyncio.Queue(maxsize=QUEUE_MAXSIZE)
        self.breaker = CircuitBreaker(BREAKER_FAILURE_THRESHOLD, BREAKER_OPEN_DURATION_S)
        self.metrics = Metrics()
        self.client: httpx.AsyncClient | None = None
        self._shutdown = asyncio.Event()

    # -- UDS server: the fast path from itrs_notify.py -----------------------
    async def handle_client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            header = await reader.readexactly(4)
            (length,) = struct.unpack(">I", header)
            payload = await reader.readexactly(length)
            envelope = json.loads(payload)
        except (asyncio.IncompleteReadError, json.JSONDecodeError, struct.error):
            writer.close()
            return

        try:
            self.queue.put_nowait(envelope)
            self.metrics.accepted += 1
            writer.write(b"K")
        except asyncio.QueueFull:
            # Backpressure: reject rather than block the accept loop or grow
            # memory unbounded. The client falls back to the disk spool.
            writer.write(b"F")
        try:
            await writer.drain()
        except (ConnectionResetError, BrokenPipeError):
            pass  # client (itrs_notify.py) already gave up and moved to spool
        writer.close()

    # -- spool drainer: crash recovery + NACK-fallback replay ----------------
    async def spool_drainer(self) -> None:
        """Periodically rotates the live spool file out from under new
        writers (rename is atomic; concurrent appenders just recreate the
        file on their next O_CREAT|O_APPEND open) and replays each rotated
        file into the queue. `queue.put()` (not put_nowait) is used here on
        purpose: it blocks when the queue is full, which *is* the
        backpressure — replay can never outrun live capacity."""
        while not self._shutdown.is_set():
            for path in sorted(glob.glob(f"{SPOOL_FILE}.*.replay")) + self._rotate_spool():
                await self._drain_file(path)
            await asyncio.sleep(SPOOL_ROTATE_INTERVAL_S)

    def _rotate_spool(self) -> list[str]:
        if not os.path.exists(SPOOL_FILE) or os.path.getsize(SPOOL_FILE) == 0:
            return []
        rotated = f"{SPOOL_FILE}.{time.time_ns()}.replay"
        try:
            os.rename(SPOOL_FILE, rotated)
        except OSError:
            return []
        return [rotated]

    async def _drain_file(self, path: str) -> None:
        try:
            with open(path, "r", encoding="utf-8") as fh:
                for line in fh:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        envelope = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    await self.queue.put(envelope)  # blocks -> backpressure
        finally:
            os.remove(path)

    # -- worker pool: owns the pooled HTTP client + retry/breaker policy ----
    async def worker(self, worker_id: int) -> None:
        while True:
            envelope = await self.queue.get()
            try:
                await self._deliver(envelope)
            finally:
                self.queue.task_done()

    async def _deliver(self, envelope: dict) -> None:
        if not self.breaker.allow_request():
            # EMS looks down: don't spend a worker slot retrying into it.
            # Re-spool so the drainer retries this once the breaker's
            # cooldown elapses (naturally rate-limited by the rotate
            # interval, not a tight loop) instead of dropping the event.
            atomic_append(SPOOL_FILE, encode_jsonl_record(envelope))
            self.metrics.breaker_open_respooled += 1
            return

        attempt = 0
        while True:
            attempt += 1
            try:
                resp = await self.client.post(
                    EMS_URL,
                    json=envelope,
                    headers={
                        "Idempotency-Key": envelope["event_id"],
                        **({"Authorization": f"Bearer {EMS_TOKEN}"} if EMS_TOKEN else {}),
                    },
                )
            except (httpx.TimeoutException, httpx.TransportError) as exc:
                self.breaker.record_failure()
                if attempt > MAX_RETRIES:
                    append_to_dlq(envelope, reason=str(exc), attempts=attempt)
                    self.metrics.dlq += 1
                    return
                await asyncio.sleep(backoff_with_jitter(attempt))
                continue

            if resp.status_code in (200, 202):
                self.breaker.record_success()
                self.metrics.sent_ok += 1
                return

            if resp.status_code == 429 or resp.status_code >= 500:
                self.breaker.record_failure()  # transient -> retry, then DLQ
                if attempt > MAX_RETRIES:
                    append_to_dlq(envelope, reason=f"http_{resp.status_code}", attempts=attempt)
                    self.metrics.dlq += 1
                    return
                await asyncio.sleep(backoff_with_jitter(attempt))
                continue

            # Any other 4xx is a permanent rejection: retrying it wastes the
            # attempt budget and the EMS will say no again. Straight to DLQ.
            append_to_dlq(envelope, reason=f"http_{resp.status_code}", attempts=attempt)
            self.metrics.dlq += 1
            return

    # -- observability: RED on the daemon, USE on the two buffers -----------
    async def metrics_reporter(self) -> None:
        while not self._shutdown.is_set():
            await asyncio.sleep(METRICS_LOG_INTERVAL_S)
            m = self.metrics
            log.info(
                "queue_depth=%d breaker=%s accepted=%d sent_ok=%d "
                "breaker_open_respooled=%d dlq=%d dropped=%d",
                self.queue.qsize(), self.breaker.state, m.accepted,
                m.sent_ok, m.breaker_open_respooled, m.dlq, m.dropped,
            )

    async def run(self) -> None:
        os.makedirs(os.path.dirname(RELAY_SOCK), exist_ok=True)
        if os.path.exists(RELAY_SOCK):
            os.remove(RELAY_SOCK)  # stale socket from a prior crash

        limits = httpx.Limits(
            max_connections=WORKER_CONCURRENCY + 50,
            max_keepalive_connections=WORKER_CONCURRENCY,
        )
        self.client = httpx.AsyncClient(
            limits=limits,
            timeout=httpx.Timeout(PER_ATTEMPT_TIMEOUT_S),
            # Connection *reuse* (keep-alive pooling, via `limits` above) is
            # the load-bearing property here, and httpx does that over plain
            # HTTP/1.1 with no extra dependency. HTTP/2 multiplexing is a
            # bonus if the EMS speaks it, but needs the optional `h2`
            # package (see requirements.txt) — don't hard-require it.
            http2=os.environ.get("ITRS_RELAY_HTTP2", "0") == "1",
        )

        server = await asyncio.start_unix_server(self.handle_client, path=RELAY_SOCK)
        os.chmod(RELAY_SOCK, 0o666)  # any Gateway-effect process must be able to write

        workers = [asyncio.create_task(self.worker(i)) for i in range(WORKER_CONCURRENCY)]
        drainer = asyncio.create_task(self.spool_drainer())
        reporter = asyncio.create_task(self.metrics_reporter())

        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, self._shutdown.set)

        log.info("event_relay listening on %s -> %s (workers=%d, queue_max=%d)",
                  RELAY_SOCK, EMS_URL, WORKER_CONCURRENCY, QUEUE_MAXSIZE)

        async with server:
            await self._shutdown.wait()

        log.info("shutting down: draining in-flight queue to disk spool")
        server.close()
        for w in workers:
            w.cancel()
        drainer.cancel()
        reporter.cancel()
        # Don't lose whatever was still queued in memory: persist it so the
        # next startup's spool_drainer replays it.
        while not self.queue.empty():
            atomic_append(SPOOL_FILE, encode_jsonl_record(self.queue.get_nowait()))
        await self.client.aclose()


def main() -> None:
    asyncio.run(Relay().run())


if __name__ == "__main__":
    main()
