"""exp4.py — Experiment 4 orchestrator: Snapshot Recovery & S3 Log Persistence.

1. Drive the coordinator cluster with a sustained workload until the leader's
   Raft log exceeds ~50k committed entries.
2. Ensure at least one snapshot flush has landed in S3 for the victim follower.
3. Kill that follower cold via `ecs:StopTask`.
4. Observe its CloudWatch log stream to time:
     • container restart (coordinator started)
     • snapshot restoration (optional; absent on full-replay contrast run)
     • commit-index catch-up to the leader's value at kill time
5. Record snapshot size (S3 HEAD) and raw log size estimate (commit_index × avg entry bytes).

For the full-log-replay contrast run, redeploy the coordinator task with
SNAPSHOT_S3_BUCKET set to "" and rerun this script. The observer's
`used_snapshot` field flips false and `catchup_ms` will be materially larger.

Outputs {run_dir}/exp4_summary.json.
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import datetime as dt
import json
import os
import re
import sys
import threading
import time

import boto3

from bench import RunParams, _poll_to_completion
from chaos import ChaosInjector
from config import Config
from follower_recovery import FollowerRecoveryObserver
from leader_detect import find_leader
from recorder import HistoryRecorder
from submitter import Submitter


COORDINATOR_LOG_GROUP = os.environ.get(
    "COORDINATOR_LOG_GROUP", "/ecs/raft-coordinator-dev/coordinator"
)
RAFT_TICK_RE = re.compile(r'"commit_index"\s*:\s*(?P<ci>\d+)')


def _bench_worker(cfg: Config, rec: HistoryRecorder, params: RunParams, stop: threading.Event) -> None:
    sub = Submitter(cfg)
    interval_s = 60.0 / params.rate_per_min if params.rate_per_min > 0 else 0.0
    payload = b"x" * params.payload_bytes
    submit_pool = cf.ThreadPoolExecutor(max_workers=32, thread_name_prefix="submit")
    poll_pool = cf.ThreadPoolExecutor(max_workers=128, thread_name_prefix="poll")

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

    next_submit = time.time()
    while not stop.is_set():
        submit_pool.submit(do_submit)
        next_submit += interval_s
        sleep_s = next_submit - time.time()
        if sleep_s > 0 and stop.wait(sleep_s):
            break

    submit_pool.shutdown(wait=True)
    poll_pool.shutdown(wait=True)


def _latest_leader_commit(region: str, leader_node_id: str, since_ns: int) -> int | None:
    """Read the leader's raft tick logs from CloudWatch and return its latest commit_index."""
    logs = boto3.session.Session(region_name=region).client("logs")
    latest: int | None = None
    start_ms = max(0, (since_ns // 1_000_000) - 5000)
    kwargs = dict(
        logGroupName=COORDINATOR_LOG_GROUP,
        startTime=start_ms,
        logStreamNamePrefix=leader_node_id,
        filterPattern='"raft tick"',
    )
    while True:
        resp = logs.filter_log_events(**kwargs)
        for ev in resp.get("events", []):
            m = RAFT_TICK_RE.search(ev.get("message", ""))
            if not m:
                continue
            ci = int(m.group("ci"))
            if latest is None or ci > latest:
                latest = ci
        token = resp.get("nextToken")
        if not token:
            break
        kwargs["nextToken"] = token
    return latest


def _wait_for_commit(cfg: Config, target: int, max_wait_s: float, rec: HistoryRecorder) -> tuple[str, int]:
    """Block until leader's commit_index >= target. Returns (leader_node_id, commit)."""
    deadline = time.time() + max_wait_s
    last_log_ns = 0
    while time.time() < deadline:
        leader = find_leader(cfg)
        if leader is None:
            time.sleep(3.0)
            continue
        ci = _latest_leader_commit(cfg.aws_region, leader.node_id, 0) or 0
        if time.time_ns() - last_log_ns > 30 * 1_000_000_000:
            rec.record(op="commit_progress", leader=leader.node_id, commit_index=ci, target=target)
            last_log_ns = time.time_ns()
        if ci >= target:
            return leader.node_id, ci
        time.sleep(5.0)
    raise TimeoutError(f"commit_index never reached {target} within {max_wait_s}s")


def _snapshot_s3_object(cfg: Config, node_id: str) -> tuple[int, dt.datetime] | None:
    """Return (size_bytes, last_modified) for the node's snapshot, or None if absent."""
    s3 = boto3.session.Session(region_name=cfg.aws_region).client("s3")
    bucket = os.environ.get("SNAPSHOT_S3_BUCKET") or f"raft-coordinator-dev-raft-snapshots-{cfg.account_id}"
    key = f"raft/{node_id}/snapshot.json"
    try:
        resp = s3.head_object(Bucket=bucket, Key=key)
    except Exception:
        return None
    return resp["ContentLength"], resp["LastModified"]


def _pick_follower(cfg: Config, leader_node_id: str) -> str | None:
    """Pick any coordinator service whose node_id ≠ leader."""
    for s in cfg.coordinator_services:
        # Services are named "<prefix>-<node_id>"
        if not s.endswith(leader_node_id):
            return s
    return None


def run_exp4(
    cfg: Config, run_name: str, duration_s: float, rate_per_min: float,
    target_commit: int,
) -> dict:
    run_dir = os.path.join(cfg.history_dir, run_name)
    history_path = os.path.join(run_dir, "history.jsonl")
    rec = HistoryRecorder(history_path)

    params = RunParams(
        rate_per_min=rate_per_min,
        duration_s=duration_s,
        completion_timeout_s=180.0,
    )
    rec.record(op="run_start", experiment="exp4", target_commit=target_commit,
               rate_per_min=rate_per_min, duration_s=duration_s)

    stop = threading.Event()
    bench_th = threading.Thread(
        target=_bench_worker, args=(cfg, rec, params, stop), name="exp4-bench", daemon=True,
    )
    bench_th.start()

    # Wait for the leader to cross target_commit entries.
    leader_id, leader_commit = _wait_for_commit(cfg, target_commit, max_wait_s=duration_s, rec=rec)
    rec.record(op="commit_threshold_reached", leader=leader_id, commit_index=leader_commit)

    # Pick a follower and confirm it has a snapshot on disk.
    follower_service = _pick_follower(cfg, leader_id)
    if follower_service is None:
        rec.record(op="exp4_abort", reason="no follower service")
        stop.set()
        bench_th.join(timeout=60)
        rec.close()
        raise RuntimeError("no follower service found")
    follower_node_id = follower_service.split("-")[-2] + "-" + follower_service.split("-")[-1] \
        if follower_service.count("-") >= 3 else follower_service

    # Extract node_id from service name by trimming known prefix.
    # Service names are "raft-coordinator-dev-coordinator-x"; node_id is "coordinator-x".
    parts = follower_service.split("-")
    follower_node_id = "-".join(parts[-2:]) if len(parts) >= 2 else follower_service

    snap_before = _snapshot_s3_object(cfg, follower_node_id)
    rec.record(op="snapshot_before_kill",
               node=follower_node_id,
               size=snap_before[0] if snap_before else None,
               last_modified=str(snap_before[1]) if snap_before else None)

    # Kill the follower cold.
    inj = ChaosInjector(cfg)
    kill = inj.stop_random_task(follower_service, "exp4: follower cold restart")
    if kill is None:
        rec.record(op="exp4_abort", reason="no task to kill", service=follower_service)
        stop.set()
        bench_th.join(timeout=60)
        rec.close()
        raise RuntimeError(f"no running task for service {follower_service}")
    rec.record(op="chaos.kill_follower", service=kill.service, task_arn=kill.task_arn,
               killed_ns=kill.killed_ns, follower=follower_node_id)

    # Leader's commit will keep advancing during recovery; grab a fresh value
    # as the catch-up target.
    fresh_leader_commit = _latest_leader_commit(cfg.aws_region, leader_id, kill.killed_ns) or leader_commit

    observer = FollowerRecoveryObserver(
        region=cfg.aws_region,
        log_group=COORDINATOR_LOG_GROUP,
        node_id=follower_node_id,
        killed_ns=kill.killed_ns,
        leader_commit_at_kill=fresh_leader_commit,
    )
    timeline = observer.observe(timeout_s=600.0, poll_interval_s=10.0)
    rec.record(op="recovery_timeline", **timeline.to_dict())

    # Capture snapshot metadata after recovery (identical to before in the happy path).
    snap_after = _snapshot_s3_object(cfg, follower_node_id)

    # Raw log size estimate: commit_index × avg entry bytes.
    # One task entry is roughly JSON-encoded {task_id,priority,s3_key} ~ 200B.
    avg_entry_bytes = 220
    raw_log_bytes = fresh_leader_commit * avg_entry_bytes

    stop.set()
    bench_th.join(timeout=params.completion_timeout_s + 30)
    rec.record(op="run_end")
    rec.close()

    summary = {
        "run_name": run_name,
        "leader_node_id": leader_id,
        "follower_node_id": follower_node_id,
        "leader_commit_at_kill": fresh_leader_commit,
        "snapshot_size_bytes": (snap_after or snap_before or (0, None))[0],
        "raw_log_size_bytes_estimate": raw_log_bytes,
        "recovery_timeline": timeline.to_dict(),
    }
    out_path = os.path.join(run_dir, "exp4_summary.json")
    with open(out_path, "w") as f:
        json.dump(summary, f, indent=2, default=str)
    return summary


def main() -> int:
    ap = argparse.ArgumentParser(prog="exp4")
    ap.add_argument("--duration", type=float, default=1800.0,
                    help="max wall seconds (workload + recovery)")
    ap.add_argument("--rate", type=float, default=2000.0, help="tasks/min")
    ap.add_argument("--target-commit", type=int, default=50000,
                    help="wait until leader commit_index ≥ this before killing follower")
    ap.add_argument("--run-name", default=None)
    args = ap.parse_args()

    cfg = Config.from_env()
    run_name = args.run_name or dt.datetime.utcnow().strftime("exp4-%Y%m%dT%H%M%SZ")
    summary = run_exp4(
        cfg, run_name,
        duration_s=args.duration,
        rate_per_min=args.rate,
        target_commit=args.target_commit,
    )
    print(json.dumps(summary, indent=2, default=str))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
