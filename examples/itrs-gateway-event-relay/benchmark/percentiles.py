#!/usr/bin/env python3
"""Read one number (milliseconds) per line from stdin, print
count/min/p50/p95/p99/max/mean. Used to summarize the per-invocation
wall-time samples the benchmark scripts collect."""
import sys

vals = sorted(float(l) for l in sys.stdin if l.strip())
if not vals:
    print("no samples")
    sys.exit(1)


def pct(p):
    idx = min(len(vals) - 1, int(len(vals) * p))
    return vals[idx]


print(
    f"n={len(vals)} min={vals[0]:.1f}ms p50={pct(0.50):.1f}ms "
    f"p95={pct(0.95):.1f}ms p99={pct(0.99):.1f}ms max={vals[-1]:.1f}ms "
    f"mean={sum(vals)/len(vals):.1f}ms"
)
