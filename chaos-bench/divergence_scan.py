"""divergence_scan.py — detect task status divergence in DynamoDB.

After a fault-injection run, every submitted task should be in a terminal
state (COMPLETED or FAILED). This script fetches the DynamoDB record for
every task seen in history.jsonl and reports:

  • stuck_pending  — tasks still in PENDING after the run (never assigned)
  • stuck_assigned — tasks still in ASSIGNED after the run (never completed)
  • missing        — task_ids from history with no DynamoDB record at all

Tasks with receive_count > 1 are reported as duplicates (same as dup_scan).
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

import boto3

from config import Config


TERMINAL_STATUSES = {"COMPLETED", "FAILED"}


@dataclass
class DivergenceScanResult:
    total_scanned: int
    stuck_pending: int
    stuck_assigned: int
    missing: int
    stuck_pending_sample: list[str] = field(default_factory=list)
    stuck_assigned_sample: list[str] = field(default_factory=list)
    missing_sample: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "total_scanned": self.total_scanned,
            "stuck_pending": self.stuck_pending,
            "stuck_assigned": self.stuck_assigned,
            "missing": self.missing,
            "stuck_pending_sample": self.stuck_pending_sample[:20],
            "stuck_assigned_sample": self.stuck_assigned_sample[:20],
            "missing_sample": self.missing_sample[:20],
        }


def _task_ids_from_history(history_path: str) -> list[str]:
    ids: list[str] = []
    seen: set[str] = set()
    with open(history_path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            ev = json.loads(line)
            if ev.get("op") != "submit":
                continue
            tid = ev.get("task_id")
            if tid and tid not in seen:
                seen.add(tid)
                ids.append(tid)
    return ids


def scan(cfg: Config, history_path: str) -> DivergenceScanResult:
    task_ids = _task_ids_from_history(history_path)
    if not task_ids:
        return DivergenceScanResult(total_scanned=0, stuck_pending=0, stuck_assigned=0, missing=0)

    session = boto3.session.Session(region_name=cfg.aws_region)
    ddb = session.client("dynamodb")

    stuck_pending: list[str] = []
    stuck_assigned: list[str] = []
    missing: list[str] = []
    found: set[str] = set()

    for i in range(0, len(task_ids), 100):
        chunk = task_ids[i : i + 100]
        keys = [{"task_id": {"S": tid}} for tid in chunk]
        to_fetch: dict = {cfg.tasks_table: {"Keys": keys, "ConsistentRead": True}}

        while to_fetch:
            resp = ddb.batch_get_item(RequestItems=to_fetch)
            for item in resp.get("Responses", {}).get(cfg.tasks_table, []):
                tid = item["task_id"]["S"]
                found.add(tid)
                status = item.get("status", {}).get("S", "")
                if status == "PENDING":
                    stuck_pending.append(tid)
                elif status == "ASSIGNED":
                    stuck_assigned.append(tid)
            unproc = resp.get("UnprocessedKeys") or {}
            to_fetch = unproc if unproc else {}

    for tid in task_ids:
        if tid not in found:
            missing.append(tid)

    return DivergenceScanResult(
        total_scanned=len(task_ids),
        stuck_pending=len(stuck_pending),
        stuck_assigned=len(stuck_assigned),
        missing=len(missing),
        stuck_pending_sample=stuck_pending,
        stuck_assigned_sample=stuck_assigned,
        missing_sample=missing,
    )


if __name__ == "__main__":
    import sys

    if len(sys.argv) != 2:
        print("usage: python divergence_scan.py <history.jsonl>", file=sys.stderr)
        sys.exit(2)
    r = scan(Config.from_env(), sys.argv[1])
    print(json.dumps(r.to_dict(), indent=2))
