"""dup_scan.py — find tasks dispatched more than once.

Reads task_ids out of a run's history.jsonl, fetches each item from the
tasks DDB table, and reports any with `receive_count > 1`. Those are
duplicate dispatches (either a Raft-layer double-commit or an SQS
redelivery after a visibility timeout).

Uses BatchGetItem in chunks of 100 to keep a 10k-task scan reasonable.
"""

from __future__ import annotations

import json
from dataclasses import dataclass

import boto3

from config import Config


@dataclass
class DupScanResult:
    total_scanned: int
    duplicate_count: int
    duplicate_task_ids: list[str]
    max_receive_count: int

    def to_dict(self) -> dict:
        return {
            "total_scanned": self.total_scanned,
            "duplicate_count": self.duplicate_count,
            "max_receive_count": self.max_receive_count,
            "duplicate_task_ids_sample": self.duplicate_task_ids[:20],
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


def scan(cfg: Config, history_path: str) -> DupScanResult:
    task_ids = _task_ids_from_history(history_path)

    session = boto3.session.Session(region_name=cfg.aws_region)
    ddb = session.client("dynamodb")

    dup_ids: list[str] = []
    max_rc = 0

    # BatchGetItem caps at 100 keys per request.
    for i in range(0, len(task_ids), 100):
        chunk = task_ids[i : i + 100]
        keys = [{"task_id": {"S": tid}} for tid in chunk]

        # Loop to drain UnprocessedKeys (throttling retries).
        to_fetch: dict = {cfg.tasks_table: {"Keys": keys, "ConsistentRead": True}}
        while to_fetch:
            resp = ddb.batch_get_item(RequestItems=to_fetch)
            for item in resp.get("Responses", {}).get(cfg.tasks_table, []):
                rc_attr = item.get("receive_count", {}).get("N")
                if rc_attr is None:
                    continue
                try:
                    rc = int(rc_attr)
                except ValueError:
                    continue
                if rc > max_rc:
                    max_rc = rc
                if rc > 1:
                    dup_ids.append(item["task_id"]["S"])
            unproc = resp.get("UnprocessedKeys") or {}
            to_fetch = unproc if unproc else {}

    return DupScanResult(
        total_scanned=len(task_ids),
        duplicate_count=len(dup_ids),
        duplicate_task_ids=dup_ids,
        max_receive_count=max_rc,
    )


if __name__ == "__main__":
    import sys

    if len(sys.argv) != 2:
        print("usage: python dup_scan.py <history.jsonl>", file=sys.stderr)
        sys.exit(2)
    r = scan(Config.from_env(), sys.argv[1])
    print(json.dumps(r.to_dict(), indent=2))
