#!/usr/bin/env python3
"""
collect.py — Experiment 2 metrics collector.

Queries DynamoDB for assignment latency percentiles (p50/p95/p99) and
fetches per-follower replication lag from the coordinator /state endpoint.
Outputs one CSV row per run.

Usage:
    python collect.py \\
        --table  raft-coordinator-experiment-tasks \\
        --label  "3node_500tpm" \\
        --coordinator http://coordinator-a.raft.local:8080 \\
        --output results/exp2.csv
"""

from __future__ import annotations

import argparse
import csv
import sys
from datetime import datetime, timezone
from pathlib import Path

import boto3
import httpx


# ── Latency collection ────────────────────────────────────────────────────────

def parse_ts(s: str) -> datetime:
    """Parse an RFC3339 / RFC3339Nano timestamp into an aware UTC datetime."""
    s = s.replace("Z", "+00:00")
    # Python's fromisoformat handles .ffffff but not nanoseconds; truncate to 6 decimals.
    if "." in s:
        base, rest = s.split(".", 1)
        tz = rest[-6:] if rest[-6] in ("+", "-") else "+00:00"
        frac = rest[: -6] if rest[-6] in ("+", "-") else rest
        frac = frac[:6].ljust(6, "0")
        s = f"{base}.{frac}{tz}"
    return datetime.fromisoformat(s)


def collect_latencies(table: str) -> list[float]:
    """Return sorted list of dispatched_at - created_at latencies in milliseconds."""
    client = boto3.client("dynamodb")
    paginator = client.get_paginator("scan")
    pages = paginator.paginate(
        TableName=table,
        FilterExpression="attribute_exists(dispatched_at) AND attribute_exists(created_at)",
    )
    latencies: list[float] = []
    for page in pages:
        for item in page["Items"]:
            try:
                t0 = parse_ts(item["created_at"]["S"])
                t1 = parse_ts(item["dispatched_at"]["S"])
                ms = (t1 - t0).total_seconds() * 1000.0
                if ms >= 0:
                    latencies.append(ms)
            except (KeyError, ValueError):
                continue
    return sorted(latencies)


def pct(data: list[float], p: float) -> float:
    if not data:
        return 0.0
    idx = min(int(len(data) * p / 100.0), len(data) - 1)
    return data[idx]


# ── Replication lag ───────────────────────────────────────────────────────────

def collect_replication_lag(coordinator_url: str) -> dict[str, int]:
    """
    Fetch replication lag per follower from the coordinator /state endpoint.
    Returns {peer_id: lag_entries} where lag = commit_index - match_index.
    Only the leader has match_index in its snapshot.
    """
    if not coordinator_url:
        return {}
    try:
        resp = httpx.get(f"{coordinator_url.rstrip('/')}/state", timeout=5.0)
        resp.raise_for_status()
        state = resp.json()
        if state.get("state") != "leader":
            # Try the other coordinators (caller should loop through all)
            return {}
        commit_idx  = state.get("commit_index", 0)
        match_index = state.get("match_index") or {}
        return {peer: commit_idx - mi for peer, mi in match_index.items()}
    except Exception as exc:
        print(f"warning: could not fetch replication lag from {coordinator_url}: {exc}",
              file=sys.stderr)
        return {}


def find_leader_lag(coordinator_addrs: list[str]) -> dict[str, int]:
    """Try each coordinator address in order until one reports as leader."""
    for addr in coordinator_addrs:
        lag = collect_replication_lag(addr)
        if lag is not None and lag != {}:
            return lag
        # Also check if node is leader with no peers (single-node)
        try:
            resp = httpx.get(f"{addr.rstrip('/')}/state", timeout=5.0)
            state = resp.json()
            if state.get("state") == "leader":
                return {}  # leader with no peers = no replication lag
        except Exception:
            continue
    return {}


# ── Main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(description="Collect Experiment 2 metrics")
    parser.add_argument("--table",       required=True, help="DynamoDB tasks table name")
    parser.add_argument("--label",       required=True,
                        help="experiment label, e.g. '3node_500tpm'")
    parser.add_argument("--coordinator", default="",
                        help="coordinator HTTP URL(s), comma-separated")
    parser.add_argument("--submitted",   type=int,   default=0,
                        help="tasks submitted by load generator (from loadgen stdout)")
    parser.add_argument("--failed",      type=int,   default=0,
                        help="tasks that got non-202 responses from load generator")
    parser.add_argument("--actual-rate", type=float, default=0.0, dest="actual_rate",
                        help="actual submission rate achieved (tasks/min)")
    parser.add_argument("--output",      default="-",
                        help="output CSV file path (default: stdout)")
    args = parser.parse_args()

    # Collect latencies from DynamoDB
    print(f"[{args.label}] scanning DynamoDB table '{args.table}'…", file=sys.stderr)
    latencies = collect_latencies(args.table)
    if not latencies:
        print("ERROR: no tasks with dispatched_at found — did the experiment run?",
              file=sys.stderr)
        sys.exit(1)

    # Collect replication lag from coordinator(s)
    coordinator_addrs = [a.strip() for a in args.coordinator.split(",") if a.strip()]
    lag_map = find_leader_lag(coordinator_addrs) if coordinator_addrs else {}

    # Build result row
    row: dict = {
        "label":           args.label,
        "n":               len(latencies),
        "p50_ms":          round(pct(latencies, 50), 2),
        "p95_ms":          round(pct(latencies, 95), 2),
        "p99_ms":          round(pct(latencies, 99), 2),
        "max_ms":          round(max(latencies), 2),
        "min_ms":          round(min(latencies), 2),
        "submitted":       args.submitted,
        "failed":          args.failed,
        "actual_rate_tpm": round(args.actual_rate, 2),
        "collected_at":    datetime.now(timezone.utc).isoformat(),
    }
    for peer, lag in sorted(lag_map.items()):
        row[f"rep_lag_{peer}"] = lag

    # Print to stderr for immediate feedback
    lag_str = "  ".join(f"{p}={v}" for p, v in sorted(lag_map.items()))
    print(
        f"[{args.label}] n={row['n']}  "
        f"p50={row['p50_ms']}ms  p95={row['p95_ms']}ms  p99={row['p99_ms']}ms  "
        f"max={row['max_ms']}ms  "
        f"submitted={row['submitted']} failed={row['failed']} rate={row['actual_rate_tpm']}/min"
        + (f"  rep_lag: {lag_str}" if lag_str else ""),
        file=sys.stderr,
    )

    # Write CSV
    fieldnames = list(row.keys())
    if args.output == "-":
        writer = csv.DictWriter(sys.stdout, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerow(row)
    else:
        out_path = Path(args.output)
        write_header = not out_path.exists() or out_path.stat().st_size == 0
        with out_path.open("a", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=fieldnames, extrasaction="ignore")
            if write_header:
                writer.writeheader()
            writer.writerow(row)
        print(f"[{args.label}] appended to {args.output}", file=sys.stderr)


if __name__ == "__main__":
    main()
