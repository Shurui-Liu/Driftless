"""exp3.py — Experiment 3: Exactly-Once Semantics Under Faults.

Submits 10 000 tasks while injecting three fault types concurrently:
  • kill_leader   — crash the current Raft leader every 30 s
  • kill_follower — stop a random follower every 45 s
  • sqs_redeliver — reset visibility on in-flight ingest messages every 20 s

Every status observation is written to history.jsonl as a `poll_observe` event
so the Porcupine linearizability checker can verify monotone transitions.

After the run, three analysis passes run automatically:
  • dup_scan       — DynamoDB receive_count > 1 (duplicate dispatches)
  • divergence_scan — tasks stuck in PENDING / ASSIGNED after the run
  • lincheck       — Porcupine linearizability check (binary in experiments/lincheck)

Metrics:
  • Linearizability violations (target: 0)
  • Duplicate dispatches (target: 0)
  • Task status divergence (target: 0)
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import datetime as dt
import json
import os
import subprocess
import sys
import threading
import time

import boto3

from bench import RunParams
from chaos import ChaosInjector
from config import Config
from divergence_scan import scan as divergence_scan
from dup_scan import scan as dup_scan
from leader_detect import find_leader
from leader_tracker import LeaderTracker
from recorder import HistoryRecorder
from submitter import Submitter

# ── Traced poll (records every intermediate observation for Porcupine) ────────

def _poll_to_completion_traced(
    sub: Submitter,
    rec: HistoryRecorder,
    task_id: str,
    submitted_ns: int,
    timeout_s: float,
    poll_interval_s: float,
) -> None:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        call_ns = time.time_ns()
        status = sub.poll_status(task_id)
        return_ns = time.time_ns()
        if status:
            rec.record(
                op="poll_observe",
                task_id=task_id,
                call_ns=call_ns,
                return_ns=return_ns,
                status=status,
            )
            if status in ("COMPLETED", "FAILED"):
                rec.record(
                    op="complete",
                    task_id=task_id,
                    submitted_ns=submitted_ns,
                    completed_ns=return_ns,
                    status=status,
                )
                return
        time.sleep(poll_interval_s)
    rec.record(
        op="timeout",
        task_id=task_id,
        submitted_ns=submitted_ns,
        completed_ns=time.time_ns(),
        status="TIMEOUT",
    )


# ── Bench thread (count-based, not duration-based) ────────────────────────────

def _bench_worker(
    cfg: Config,
    rec: HistoryRecorder,
    params: RunParams,
    target_count: int,
    stop: threading.Event,
    done: threading.Event,
) -> None:
    sub = Submitter(cfg)
    interval_s = 60.0 / params.rate_per_min if params.rate_per_min > 0 else 0.0
    payload = b"x" * params.payload_bytes

    submit_pool = cf.ThreadPoolExecutor(max_workers=16, thread_name_prefix="submit")
    poll_pool = cf.ThreadPoolExecutor(max_workers=128, thread_name_prefix="poll")

    def do_submit() -> None:
        try:
            r = sub.submit(payload, params.priority)
        except Exception as e:
            rec.record(op="submit_error", err=str(e))
            return
        rec.record(op="submit", task_id=r.task_id, submitted_ns=r.submitted_ns)
        poll_pool.submit(
            _poll_to_completion_traced,
            sub, rec, r.task_id, r.submitted_ns,
            params.completion_timeout_s, params.poll_interval_s,
        )

    submitted = 0
    next_submit = time.time()
    while submitted < target_count and not stop.is_set():
        submit_pool.submit(do_submit)
        submitted += 1
        next_submit += interval_s
        sleep_s = next_submit - time.time()
        if sleep_s > 0:
            if stop.wait(sleep_s):
                break

    submit_pool.shutdown(wait=True)
    poll_pool.shutdown(wait=True)
    done.set()


# ── Fault injection threads ───────────────────────────────────────────────────

def _kill_leader_loop(
    cfg: Config,
    rec: HistoryRecorder,
    period_s: float,
    stop: threading.Event,
) -> None:
    inj = ChaosInjector(cfg)
    if stop.wait(period_s):
        return
    while not stop.is_set():
        try:
            info = find_leader(cfg)
            if info is None:
                rec.record(op="chaos.skip", fault="kill_leader", reason="no leader detected")
            else:
                res = inj.kill_leader(info.ecs_service)
                if res is None:
                    rec.record(op="chaos.skip", fault="kill_leader", reason="no running task")
                else:
                    rec.record(
                        op="chaos.kill_leader",
                        service=res.service,
                        task_arn=res.task_arn,
                        killed_ns=res.killed_ns,
                        leader_node_id=info.node_id,
                    )
        except Exception as e:
            rec.record(op="chaos.error", fault="kill_leader", err=str(e))
        if stop.wait(period_s):
            return


def _kill_follower_loop(
    cfg: Config,
    rec: HistoryRecorder,
    period_s: float,
    stop: threading.Event,
) -> None:
    inj = ChaosInjector(cfg)
    if stop.wait(period_s * 0.7):  # offset so it doesn't fire at same time as leader kill
        return
    while not stop.is_set():
        try:
            info = find_leader(cfg)
            if info is None:
                rec.record(op="chaos.skip", fault="kill_follower", reason="no leader detected")
            else:
                followers = [s for s in cfg.coordinator_services if s != info.ecs_service]
                res = inj.kill_follower(followers)
                if res is None:
                    rec.record(op="chaos.skip", fault="kill_follower", reason="no follower tasks")
                else:
                    rec.record(
                        op="chaos.kill_follower",
                        service=res.service,
                        task_arn=res.task_arn,
                        killed_ns=res.killed_ns,
                    )
        except Exception as e:
            rec.record(op="chaos.error", fault="kill_follower", err=str(e))
        if stop.wait(period_s):
            return


def _sqs_redeliver_loop(
    cfg: Config,
    rec: HistoryRecorder,
    period_s: float,
    stop: threading.Event,
) -> None:
    inj = ChaosInjector(cfg)
    if stop.wait(period_s * 0.4):  # different offset from the kill loops
        return
    while not stop.is_set():
        try:
            n = inj.inject_sqs_redelivery(cfg.ingest_queue_url, count=5)
            rec.record(op="chaos.sqs_redeliver", redelivered=n, queue=cfg.ingest_queue_url)
        except Exception as e:
            rec.record(op="chaos.error", fault="sqs_redeliver", err=str(e))
        if stop.wait(period_s):
            return


# ── Orchestrator ──────────────────────────────────────────────────────────────

def run_exp3(
    cfg: Config,
    run_name: str,
    target_tasks: int,
    rate_per_min: float,
    leader_kill_period_s: float = 30.0,
    follower_kill_period_s: float = 45.0,
    sqs_redeliver_period_s: float = 20.0,
) -> str:
    run_dir = os.path.join(cfg.history_dir, run_name)
    history_path = os.path.join(run_dir, "history.jsonl")
    rec = HistoryRecorder(history_path)

    params = RunParams(
        rate_per_min=rate_per_min,
        duration_s=0,  # unused — count-based termination
        completion_timeout_s=120.0,
        poll_interval_s=0.5,
    )

    rec.record(
        op="run_start",
        experiment="exp3",
        target_tasks=target_tasks,
        rate_per_min=rate_per_min,
        leader_kill_period_s=leader_kill_period_s,
        follower_kill_period_s=follower_kill_period_s,
        sqs_redeliver_period_s=sqs_redeliver_period_s,
    )

    tracker = LeaderTracker(cfg, rec.record, poll_interval_s=0.15)
    tracker.start()

    stop = threading.Event()
    bench_done = threading.Event()

    bench_th = threading.Thread(
        target=_bench_worker,
        args=(cfg, rec, params, target_tasks, stop, bench_done),
        name="exp3-bench", daemon=True,
    )
    leader_kill_th = threading.Thread(
        target=_kill_leader_loop,
        args=(cfg, rec, leader_kill_period_s, stop),
        name="exp3-kill-leader", daemon=True,
    )
    follower_kill_th = threading.Thread(
        target=_kill_follower_loop,
        args=(cfg, rec, follower_kill_period_s, stop),
        name="exp3-kill-follower", daemon=True,
    )
    sqs_redeliver_th = threading.Thread(
        target=_sqs_redeliver_loop,
        args=(cfg, rec, sqs_redeliver_period_s, stop),
        name="exp3-sqs-redeliver", daemon=True,
    )

    bench_th.start()
    leader_kill_th.start()
    follower_kill_th.start()
    sqs_redeliver_th.start()

    # Wait until all tasks submitted + completions drained.
    bench_done.wait()
    stop.set()

    leader_kill_th.join(timeout=10)
    follower_kill_th.join(timeout=10)
    sqs_redeliver_th.join(timeout=10)

    time.sleep(5.0)
    tracker.stop()

    rec.record(op="run_end")
    rec.close()
    return history_path


# ── Analysis ──────────────────────────────────────────────────────────────────

def analyze(cfg: Config, history_path: str, lincheck_bin: str | None = None) -> dict:
    run_dir = os.path.dirname(history_path)

    # 1. Duplicate dispatches (receive_count > 1 in DynamoDB).
    print("[exp3] scanning for duplicate dispatches…", file=sys.stderr, flush=True)
    dup_result = dup_scan(cfg, history_path)
    dup_path = os.path.join(run_dir, "exp3_dups.json")
    with open(dup_path, "w") as f:
        json.dump(dup_result.to_dict(), f, indent=2)

    # 2. Status divergence (tasks stuck in PENDING/ASSIGNED).
    print("[exp3] scanning for status divergence…", file=sys.stderr, flush=True)
    div_result = divergence_scan(cfg, history_path)
    div_path = os.path.join(run_dir, "exp3_divergence.json")
    with open(div_path, "w") as f:
        json.dump(div_result.to_dict(), f, indent=2)

    # 3. Porcupine linearizability check.
    lin_violations = -1  # -1 = skipped
    lin_path = os.path.join(run_dir, "exp3_lincheck.json")
    if lincheck_bin and os.path.isfile(lincheck_bin):
        print("[exp3] running Porcupine linearizability check…", file=sys.stderr, flush=True)
        try:
            result = subprocess.run(
                [lincheck_bin, "-history", history_path, "-out", lin_path],
                capture_output=True, text=True, timeout=300,
            )
            if result.returncode == 0:
                with open(lin_path) as f:
                    lin_data = json.load(f)
                lin_violations = lin_data.get("violations", 0)
            else:
                print(f"[exp3] lincheck stderr: {result.stderr}", file=sys.stderr)
        except subprocess.TimeoutExpired:
            print("[exp3] lincheck timed out (>5 min)", file=sys.stderr)
    else:
        print(
            f"[exp3] lincheck binary not found at {lincheck_bin!r}; skipping Porcupine check.",
            file=sys.stderr,
        )

    # 4. Parse chaos counts from history.jsonl.
    leader_kills = 0
    follower_kills = 0
    sqs_redeliveries = 0
    submitted = 0
    completed = 0
    timed_out = 0
    with open(history_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            ev = json.loads(line)
            op = ev.get("op")
            if op == "submit":
                submitted += 1
            elif op == "complete":
                completed += 1
            elif op == "timeout":
                timed_out += 1
            elif op == "chaos.kill_leader":
                leader_kills += 1
            elif op == "chaos.kill_follower":
                follower_kills += 1
            elif op == "chaos.sqs_redeliver":
                sqs_redeliveries += ev.get("redelivered", 0)

    return {
        "submitted": submitted,
        "completed": completed,
        "timed_out": timed_out,
        "lost": max(0, submitted - completed - timed_out),
        "leader_kills": leader_kills,
        "follower_kills": follower_kills,
        "sqs_redeliveries_injected": sqs_redeliveries,
        "duplicate_dispatches": dup_result.duplicate_count,
        "max_receive_count": dup_result.max_receive_count,
        "stuck_pending": div_result.stuck_pending,
        "stuck_assigned": div_result.stuck_assigned,
        "linearizability_violations": lin_violations,
    }


# ── CLI ───────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(prog="exp3")
    sub = ap.add_subparsers(dest="cmd", required=True)

    rp = sub.add_parser("run", help="run the exactly-once fault experiment")
    rp.add_argument("--tasks", type=int, default=10_000, help="total tasks to submit")
    rp.add_argument("--rate", type=float, default=500.0, help="task submissions per minute")
    rp.add_argument("--leader-kill-period", type=float, default=30.0)
    rp.add_argument("--follower-kill-period", type=float, default=45.0)
    rp.add_argument("--sqs-redeliver-period", type=float, default=20.0)
    rp.add_argument("--run-name", default=None)

    ap_a = sub.add_parser("analyze", help="analyze a finished run")
    ap_a.add_argument("history_path")
    ap_a.add_argument(
        "--lincheck",
        default=None,
        help="path to compiled lincheck binary (experiments/lincheck/lincheck)",
    )

    args = ap.parse_args()
    cfg = Config.from_env()

    if args.cmd == "run":
        run_name = args.run_name or dt.datetime.utcnow().strftime("exp3-%Y%m%dT%H%M%SZ")
        print(f"[exp3] starting run '{run_name}', target={args.tasks} tasks @ {args.rate}/min",
              file=sys.stderr, flush=True)
        path = run_exp3(
            cfg, run_name,
            target_tasks=args.tasks,
            rate_per_min=args.rate,
            leader_kill_period_s=args.leader_kill_period,
            follower_kill_period_s=args.follower_kill_period,
            sqs_redeliver_period_s=args.sqs_redeliver_period,
        )
        print(f"[exp3] history written to {path}", file=sys.stderr)
        return 0

    if args.cmd == "analyze":
        stats = analyze(cfg, args.history_path, lincheck_bin=args.lincheck)
        run_dir = os.path.dirname(args.history_path)
        out = os.path.join(run_dir, "exp3_summary.json")
        with open(out, "w") as f:
            json.dump(stats, f, indent=2)
        print(json.dumps(stats, indent=2))
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
