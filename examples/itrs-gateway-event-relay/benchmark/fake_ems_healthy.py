#!/usr/bin/env python3
"""A stand-in EMS that accepts every POST immediately with 202. Used as the
"healthy backend" baseline in the latency/TIME_WAIT benchmarks — the naive
shell script's problems show up even when the EMS is behaving perfectly."""
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    # BaseHTTPRequestHandler defaults to HTTP/1.0, which the server closes
    # after every response regardless of what the client wants — that would
    # confound the TIME_WAIT benchmark (it measures whether the *client*
    # reuses connections, not whether the test fixture forces a close).
    # HTTP/1.1 lets a persistent client (the Go relay's pooled http.Client)
    # actually keep the connection open across requests.
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(202)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    # Threaded so this stand-in isn't itself the bottleneck under
    # concurrent load — the benchmark is measuring the *client's* overhead
    # (shell+curl fork/TLS vs. a pooled daemon connection), not this
    # server's.
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8940
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
