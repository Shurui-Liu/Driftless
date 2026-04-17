"""plot.py — read a history JSONL and emit summary stats + plots.

Outputs (next to the input file):
  • summary.json   — count, p50/p95/p99 end-to-end latency (ms)
  • latency.png    — CDF of submit→complete latency
  • throughput.png — completions per second over wall time
"""

from __future__ import annotations

import json
import os
import sys
from collections import defaultdict

import matplotlib.pyplot as plt
import numpy as np


def load(path: str) -> list[dict]:
    events: list[dict] = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                events.append(json.loads(line))
    return events


def analyze(events: list[dict]) -> dict:
    latencies_ms: list[float] = []
    completions_by_sec: dict[int, int] = defaultdict(int)
    run_start_ns = None
    status_counts: dict[str, int] = defaultdict(int)

    for e in events:
        op = e.get("op")
        if op == "run_start":
            run_start_ns = e.get("wall_ns")
        elif op in ("complete", "timeout"):
            status_counts[e.get("status", "?")] += 1
            if op == "complete":
                d_ns = e["completed_ns"] - e["submitted_ns"]
                latencies_ms.append(d_ns / 1_000_000)
                if run_start_ns is not None:
                    s = int((e["completed_ns"] - run_start_ns) / 1_000_000_000)
                    completions_by_sec[s] += 1

    lat = np.array(latencies_ms) if latencies_ms else np.array([0.0])
    return {
        "completed_count": int(status_counts.get("COMPLETED", 0)),
        "failed_count": int(status_counts.get("FAILED", 0)),
        "timeout_count": int(status_counts.get("TIMEOUT", 0)),
        "p50_ms": float(np.percentile(lat, 50)),
        "p95_ms": float(np.percentile(lat, 95)),
        "p99_ms": float(np.percentile(lat, 99)),
        "mean_ms": float(np.mean(lat)),
        "completions_by_sec": dict(completions_by_sec),
        "latencies_ms": latencies_ms,
    }


def render(path: str) -> None:
    events = load(path)
    stats = analyze(events)
    out_dir = os.path.dirname(path) or "."

    summary = {k: v for k, v in stats.items() if k not in ("latencies_ms", "completions_by_sec")}
    with open(os.path.join(out_dir, "summary.json"), "w") as f:
        json.dump(summary, f, indent=2)

    if stats["latencies_ms"]:
        xs = sorted(stats["latencies_ms"])
        ys = np.arange(1, len(xs) + 1) / len(xs)
        plt.figure()
        plt.plot(xs, ys)
        plt.xlabel("submit→complete latency (ms)")
        plt.ylabel("CDF")
        plt.title("End-to-end task latency")
        plt.grid(True)
        plt.savefig(os.path.join(out_dir, "latency.png"), dpi=120)
        plt.close()

    if stats["completions_by_sec"]:
        secs = sorted(stats["completions_by_sec"].keys())
        counts = [stats["completions_by_sec"][s] for s in secs]
        plt.figure()
        plt.bar(secs, counts)
        plt.xlabel("seconds since run_start")
        plt.ylabel("completions / s")
        plt.title("Throughput")
        plt.grid(True, axis="y")
        plt.savefig(os.path.join(out_dir, "throughput.png"), dpi=120)
        plt.close()

    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: python plot.py <history.jsonl>", file=sys.stderr)
        sys.exit(2)
    render(sys.argv[1])
