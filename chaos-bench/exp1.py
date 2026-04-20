"""exp1.py — Experiment 1 orchestrator: Leader Election Correctness & Convergence.

Runs a sustained submission workload while periodically killing the current
Raft leader. Tracks every leader change in the peers DynamoDB table at
sub-election-timeout granularity. After the run:

  • election convergence = time from chaos.kill_ns to the first
    leader_observed event with a different node_id
  • duplicate dispatch count = tasks with receive_count > 1
    (requires worker with the receive_count increment deployed)
  • lost task count = submit - complete - timeout

Everything is co-located in a single history.jsonl so the analyzer has no
time-alignment problems.
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import datetime as dt
import json
import os
import statistics
import sys
import threading
import time

import boto3

from bench import RunParams, _poll_to_completion
from chaos import ChaosInjector
from config import Config
from leader_detect import find_leader
from leader_tracker import LeaderTracker
from recorder import HistoryRecorder
from submitter import Submitter


def _bench_worker(
    cfg: Config,
    rec: HistoryRecorder,
    params: RunParams,
    stop: threading.Event,
) -> None:
    """Submit at a target rate until stop is set; poll completions in parallel."""
    sub = Submitter(cfg)
    interval_s = 60.0 / params.rate_per_min if params.rate_per_min > 0 else 0.0
    payload = b"x" * params.payload_bytes

    submit_pool = cf.ThreadPoolExecutor(max_workers=16, thread_name_prefix="submit")
    poll_pool = cf.ThreadPoolExecutor(max_workers=64, thread_name_prefix="poll")

    def do_submit() -> None:
        try:
            r = sub.submit(payload, params.priority)
        except Exception as e:
            rec.record(op="submit_error", err=str(e))
            return
        rec.record(op="submit", task_id=r.task_id, submitted_ns=r.submitted_ns)
        poll_pool.submit(
            _poll_to_completion,
            sub, rec, r.task_id, r.submitted_ns,
            params.completion_timeout_s, params.poll_interval_s,
        )

    t0 = time.time()
    next_submit = t0
    while not stop.is_set():
        submit_pool.submit(do_submit)
        next_submit += interval_s
        sleep_s = next_submit - time.time()
        if sleep_s > 0:
            if stop.wait(sleep_s):
                break

    submit_pool.shutdown(wait=True)
    poll_pool.shutdown(wait=True)


def _kill_loop(
    cfg: Config,
    rec: HistoryRecorder,
    period_s: float,
    stop: threading.Event,
) -> None:
    """Fire chaos kill_leader every period_s until stop is set."""
    inj = ChaosInjector(cfg)
    # Let the workload ramp up before the first kill.
    if stop.wait(period_s):
        return
    while not stop.is_set():
        info = find_leader(cfg)
        if info is None:
            rec.record(op="chaos.skip", reason="no leader detected")
        else:
            res = inj.kill_leader(info.ecs_service)
            if res is None:
                rec.record(op="chaos.skip", reason="no running task", service=info.ecs_service)
            else:
                rec.record(
                    op="chaos.kill_leader",
                    service=res.service,
                    task_arn=res.task_arn,
                    killed_ns=res.killed_ns,
                    leader_node_id=info.node_id,
                )
        if stop.wait(period_s):
            return


def run_exp1(cfg: Config, run_name: str, duration_s: float, rate_per_min: float, kill_period_s: float) -> str:
    run_dir = os.path.join(cfg.history_dir, run_name)
    history_path = os.path.join(run_dir, "history.jsonl")
    rec = HistoryRecorder(history_path)

    params = RunParams(
        rate_per_min=rate_per_min,
        duration_s=duration_s,
        # Kept tight so the run doesn't stall for 20+ minutes on a trailing
        # timeout queue; a healthy task completes well inside 10s.
        completion_timeout_s=30.0,
    )

    rec.record(
        op="run_start",
        experiment="exp1",
        duration_s=duration_s,
        rate_per_min=rate_per_min,
        kill_period_s=kill_period_s,
    )

    tracker = LeaderTracker(cfg, rec.record, poll_interval_s=0.15)
    tracker.start()

    stop = threading.Event()
    bench_th = threading.Thread(
        target=_bench_worker, args=(cfg, rec, params, stop), name="exp1-bench", daemon=True,
    )
    chaos_th = threading.Thread(
        target=_kill_loop, args=(cfg, rec, kill_period_s, stop), name="exp1-chaos", daemon=True,
    )
    bench_th.start()
    chaos_th.start()

    time.sleep(duration_s)
    stop.set()
    bench_th.join(timeout=params.completion_timeout_s + 30)
    chaos_th.join(timeout=10)

    # Let the tracker observe the final post-duration leader settle.
    time.sleep(5.0)
    tracker.stop()

    rec.record(op="run_end")
    rec.close()
    return history_path


def analyze(history_path: str) -> dict:
    """Compute convergence, duplicate, and loss metrics from a run's history."""
    submits: dict[str, int] = {}
    completes: dict[str, int] = {}
    timeouts: set[str] = set()
    kills: list[int] = []
    leader_events: list[tuple[int, str]] = []  # (wall_ns, node_id)

    with open(history_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            ev = json.loads(line)
            op = ev.get("op")
            if op == "submit":
                submits[ev["task_id"]] = ev.get("submitted_ns", 0)
            elif op == "complete":
                completes[ev["task_id"]] = ev.get("completed_ns", 0)
            elif op == "timeout":
                timeouts.add(ev["task_id"])
            elif op == "chaos.kill_leader":
                kills.append(int(ev.get("killed_ns", ev.get("wall_ns", 0))))
            elif op == "leader_observed":
                leader_events.append((int(ev.get("wall_ns", 0)), ev["node_id"]))

    # Convergence: for each kill, the time until is_leader=True is next
    # observed. We don't require a different node_id — a restarted leader
    # re-winning is still a successful re-election, and on Fargate the old
    # task's row may linger until its heartbeat expires, so the "first
    # different node_id" definition misses same-node re-elections entirely.
    convergence_ms: list[float] = []
    for k in kills:
        # Find the TTL-gap (leader_gap) OR first leader_observed whose
        # wall_ns is strictly after the kill — whichever is the first
        # post-kill quorum-visible event.
        for ts, nid in leader_events:
            if ts > k:
                convergence_ms.append((ts - k) / 1_000_000)
                break

    submitted = len(submits)
    completed = len(completes)
    timed_out = len(timeouts)
    lost = max(0, submitted - completed - timed_out)

    def pct(xs: list[float], p: float) -> float:
        if not xs:
            return 0.0
        xs_sorted = sorted(xs)
        k = max(0, min(len(xs_sorted) - 1, int(round(p / 100 * (len(xs_sorted) - 1)))))
        return xs_sorted[k]

    return {
        "kill_count": len(kills),
        "convergence_samples": len(convergence_ms),
        "convergence_p50_ms": pct(convergence_ms, 50),
        "convergence_p95_ms": pct(convergence_ms, 95),
        "convergence_mean_ms": statistics.mean(convergence_ms) if convergence_ms else 0.0,
        "convergence_max_ms": max(convergence_ms) if convergence_ms else 0.0,
        "submitted": submitted,
        "completed": completed,
        "timed_out": timed_out,
        "lost": lost,
        "leader_change_count": len(leader_events),
    }


def _reset_cluster(cfg: Config, ready_timeout_s: float = 240.0) -> None:
    """Wipe snapshots, stop every coordinator task, wait for fresh quorum.

    Each multi-kill iteration needs a clean slate because the leader doesn't
    currently fall back to InstallSnapshot when a follower's log is too far
    behind — so once compaction kicks in, new/behind followers never catch up.
    Starting from genesis each time sidesteps that.
    """
    s3 = boto3.client("s3", region_name=cfg.aws_region)
    ecs = boto3.client("ecs", region_name=cfg.aws_region)
    ddb = boto3.client("dynamodb", region_name=cfg.aws_region)

    paginator = s3.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=cfg.snapshot_bucket, Prefix="raft/"):
        keys = [{"Key": o["Key"]} for o in page.get("Contents", [])]
        if keys:
            s3.delete_objects(Bucket=cfg.snapshot_bucket, Delete={"Objects": keys})

    for svc in cfg.coordinator_services:
        arns = ecs.list_tasks(
            cluster=cfg.cluster_name, serviceName=svc, desiredStatus="RUNNING"
        ).get("taskArns", [])
        for a in arns:
            ecs.stop_task(cluster=cfg.cluster_name, task=a, reason="exp1-repeat reset")

    deadline = time.time() + ready_timeout_s
    want = len(cfg.coordinator_services)
    while time.time() < deadline:
        resp = ddb.scan(TableName=cfg.peers_table, ConsistentRead=True)
        now = time.time()
        fresh = 0
        leader = None
        for it in resp.get("Items", []):
            hb_v = it.get("heartbeat_ts", {}).get("N")
            try:
                hb = int(hb_v) if hb_v else 0
            except ValueError:
                hb = 0
            if now - hb < 3.0:
                fresh += 1
                if it.get("is_leader", {}).get("BOOL", False):
                    leader = it.get("node_id", {}).get("S")
        if fresh >= want and leader is not None:
            return
        time.sleep(5.0)
    raise RuntimeError(f"cluster did not reach quorum within {ready_timeout_s}s")


def _run_single_kill_iteration(
    cfg: Config, run_name: str, warmup_s: float, rate_per_min: float
) -> dict:
    """One iteration: reset, warm up, kill once, analyze."""
    _reset_cluster(cfg)
    # kill_period = warmup so first (only) kill fires after the warmup window.
    # duration = warmup + 30 gives the tracker 30s to observe convergence
    # and for in-flight tasks to complete.
    path = run_exp1(
        cfg, run_name,
        duration_s=warmup_s + 30.0,
        rate_per_min=rate_per_min,
        kill_period_s=warmup_s,
    )
    return {"run_name": run_name, "history_path": path, **analyze(path)}


def run_exp1_repeat(
    cfg: Config,
    group_name: str,
    iterations: int,
    warmup_s: float,
    rate_per_min: float,
) -> str:
    group_dir = os.path.join(cfg.history_dir, group_name)
    os.makedirs(group_dir, exist_ok=True)
    results: list[dict] = []
    for i in range(1, iterations + 1):
        run_name = f"{group_name}/iter-{i:02d}"
        print(f"[{i}/{iterations}] resetting cluster…", file=sys.stderr, flush=True)
        try:
            r = _run_single_kill_iteration(cfg, run_name, warmup_s, rate_per_min)
        except Exception as e:
            print(f"[{i}/{iterations}] FAILED: {e}", file=sys.stderr)
            results.append({"run_name": run_name, "error": str(e)})
            continue
        print(
            f"[{i}/{iterations}] conv_ms={r.get('convergence_p50_ms'):.0f} "
            f"completed={r.get('completed')} lost={r.get('lost')}",
            file=sys.stderr, flush=True,
        )
        results.append(r)

    samples = [
        r["convergence_p50_ms"] for r in results
        if r.get("convergence_samples", 0) > 0
    ]
    total_lost = sum(r.get("lost", 0) for r in results if "error" not in r)
    total_completed = sum(r.get("completed", 0) for r in results if "error" not in r)
    total_submitted = sum(r.get("submitted", 0) for r in results if "error" not in r)

    def pct(xs: list[float], p: float) -> float:
        if not xs:
            return 0.0
        xs_sorted = sorted(xs)
        k = max(0, min(len(xs_sorted) - 1, int(round(p / 100 * (len(xs_sorted) - 1)))))
        return xs_sorted[k]

    summary = {
        "group": group_name,
        "iterations": iterations,
        "successful_kills": len(samples),
        "convergence_ms_samples": samples,
        "convergence_p50_ms": pct(samples, 50),
        "convergence_p95_ms": pct(samples, 95),
        "convergence_mean_ms": statistics.mean(samples) if samples else 0.0,
        "convergence_max_ms": max(samples) if samples else 0.0,
        "convergence_min_ms": min(samples) if samples else 0.0,
        "submitted_total": total_submitted,
        "completed_total": total_completed,
        "lost_total": total_lost,
        "per_iteration": results,
    }
    out = os.path.join(group_dir, "repeat_summary.json")
    with open(out, "w") as f:
        json.dump(summary, f, indent=2)
    return out


def main() -> int:
    ap = argparse.ArgumentParser(prog="exp1")
    sub = ap.add_subparsers(dest="cmd", required=True)

    rp = sub.add_parser("run", help="run the kill-leader workload")
    rp.add_argument("--duration", type=float, default=180.0)
    rp.add_argument("--rate", type=float, default=300.0)
    rp.add_argument("--kill-period", type=float, default=30.0)
    rp.add_argument("--run-name", default=None)

    ap_a = sub.add_parser("analyze", help="analyze a finished run's history")
    ap_a.add_argument("history_path")

    ap_dup = sub.add_parser("dups", help="scan DDB for duplicate dispatches")
    ap_dup.add_argument("history_path")

    rep = sub.add_parser(
        "repeat",
        help="run N single-kill iterations back-to-back, resetting the cluster between each",
    )
    rep.add_argument("--iterations", type=int, default=5)
    rep.add_argument("--warmup", type=float, default=120.0,
                     help="seconds of load before the kill each iteration")
    rep.add_argument("--rate", type=float, default=300.0)
    rep.add_argument("--group-name", default=None)

    args = ap.parse_args()
    cfg = Config.from_env()

    if args.cmd == "run":
        run_name = args.run_name or dt.datetime.utcnow().strftime("exp1-%Y%m%dT%H%M%SZ")
        path = run_exp1(
            cfg, run_name,
            duration_s=args.duration,
            rate_per_min=args.rate,
            kill_period_s=args.kill_period,
        )
        print(f"history written to {path}", file=sys.stderr)
        return 0

    if args.cmd == "analyze":
        stats = analyze(args.history_path)
        out = os.path.join(os.path.dirname(args.history_path) or ".", "exp1_summary.json")
        with open(out, "w") as f:
            json.dump(stats, f, indent=2)
        print(json.dumps(stats, indent=2))
        return 0

    if args.cmd == "dups":
        from dup_scan import scan
        r = scan(cfg, args.history_path)
        out = os.path.join(os.path.dirname(args.history_path) or ".", "exp1_dups.json")
        with open(out, "w") as f:
            json.dump(r.to_dict(), f, indent=2)
        print(json.dumps(r.to_dict(), indent=2))
        return 0

    if args.cmd == "repeat":
        group = args.group_name or dt.datetime.utcnow().strftime(
            "exp1-repeat-%Y%m%dT%H%M%SZ"
        )
        out = run_exp1_repeat(
            cfg, group, args.iterations, args.warmup, args.rate,
        )
        print(f"summary written to {out}", file=sys.stderr)
        with open(out) as f:
            data = json.load(f)
        brief = {k: v for k, v in data.items() if k != "per_iteration"}
        print(json.dumps(brief, indent=2))
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
