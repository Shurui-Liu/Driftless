"""bench.py — orchestrate a workload run.

A "run" submits tasks at a target rate for a fixed duration, records every
submit + completion event into a JSONL history, and reports basic stats.
Each run lives in its own subdirectory of HISTORY_DIR so chaos events from
a run stay alongside their workload.
"""

from __future__ import annotations

import concurrent.futures as cf
import os
import time
from dataclasses import dataclass

from config import Config
from recorder import HistoryRecorder
from submitter import Submitter


@dataclass
class RunParams:
    rate_per_min: float       # target submissions per minute
    duration_s: float         # total wall time
    payload_bytes: int = 128
    priority: int = 5
    poll_interval_s: float = 0.5
    completion_timeout_s: float = 60.0


def _poll_to_completion(
    sub: Submitter,
    rec: HistoryRecorder,
    task_id: str,
    submitted_ns: int,
    timeout_s: float,
    poll_interval_s: float,
) -> None:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        status = sub.poll_status(task_id)
        if status in ("COMPLETED", "FAILED"):
            rec.record(
                op="complete",
                task_id=task_id,
                submitted_ns=submitted_ns,
                completed_ns=time.time_ns(),
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


def run_bench(cfg: Config, params: RunParams, run_name: str) -> str:
    run_dir = os.path.join(cfg.history_dir, run_name)
    history_path = os.path.join(run_dir, "history.jsonl")
    rec = HistoryRecorder(history_path)
    sub = Submitter(cfg)

    interval_s = 60.0 / params.rate_per_min if params.rate_per_min > 0 else 0.0
    payload = b"x" * params.payload_bytes

    rec.record(op="run_start", params=params.__dict__)

    # One pool submits, another polls — decoupling keeps submit rate honest
    # even when completions lag under load.
    submit_pool = cf.ThreadPoolExecutor(max_workers=16, thread_name_prefix="submit")
    poll_pool = cf.ThreadPoolExecutor(max_workers=64, thread_name_prefix="poll")

    def do_submit() -> None:
        r = sub.submit(payload, params.priority)
        rec.record(op="submit", task_id=r.task_id, submitted_ns=r.submitted_ns)
        poll_pool.submit(
            _poll_to_completion,
            sub, rec, r.task_id, r.submitted_ns,
            params.completion_timeout_s, params.poll_interval_s,
        )

    t0 = time.time()
    next_submit = t0
    while time.time() - t0 < params.duration_s:
        submit_pool.submit(do_submit)
        next_submit += interval_s
        sleep_s = next_submit - time.time()
        if sleep_s > 0:
            time.sleep(sleep_s)

    submit_pool.shutdown(wait=True)
    # Give completions some slack after submits stop.
    poll_pool.shutdown(wait=True)

    rec.record(op="run_end")
    rec.close()
    return history_path
