#!/usr/bin/env python3
"""Count sockets in TIME_WAIT by reading /proc/net/tcp directly (this
sandbox has no `ss`/`netstat` binary). TCP state 06 = TIME_WAIT per
include/net/tcp_states.h.

Usage: count_time_wait.py <port> [--client|--server]
  --client (default): count TIME_WAIT entries where the *remote* port
      matches — i.e. sockets on THIS host left behind by connections it
      opened to <port>. This is the number that matters for the Gateway
      host's own ephemeral-port budget, and the one the naive-vs-relay
      comparison is about.
  --server: count TIME_WAIT entries where the *local* port matches — left
      behind by the process listening on <port> (here, the fake EMS).
      Included for completeness; not the metric this benchmark cares about.
"""
import sys

port = int(sys.argv[1])
mode = sys.argv[2] if len(sys.argv) > 2 else "--client"
target_hex = f"{port:04X}"

count = 0
for path in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        with open(path) as f:
            next(f)  # header
            for line in f:
                fields = line.split()
                local_port = fields[1].split(":")[-1]
                remote_port = fields[2].split(":")[-1]
                state = fields[3]
                if state != "06":
                    continue
                if mode == "--server" and local_port == target_hex:
                    count += 1
                elif mode == "--client" and remote_port == target_hex:
                    count += 1
    except FileNotFoundError:
        continue
print(count)
