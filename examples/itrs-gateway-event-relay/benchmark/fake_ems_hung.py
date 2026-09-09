#!/usr/bin/env python3
"""A stand-in for the worst realistic EMS failure: it accepts the TCP
connection (so nothing fails fast — no connection-refused, no RST) and then
never reads or writes another byte. This is what a firewall black-hole, an
overloaded reverse proxy that accepted but never dispatched the connection,
or a backend stuck mid-GC looks like from the caller's side.

curl has no default response-read timeout, so a caller that didn't set
--max-time hangs forever against this — that's exactly what
BENCHMARK.md's "Test C" demonstrates.
"""
import socket
import sys

port = int(sys.argv[1]) if len(sys.argv) > 1 else 8941
srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(1024)

conns = []
while True:
    conn, _ = srv.accept()
    conns.append(conn)  # keep it open; never read, never write, never close
