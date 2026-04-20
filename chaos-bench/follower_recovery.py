"""follower_recovery.py — reconstruct a follower's recovery from CloudWatch logs.

Exp 4 kills one coordinator ECS task cold and measures how long it takes to
rejoin quorum. Since the coordinator `/state` endpoint isn't reachable from
outside the VPC, this module instead filters the coordinator log group for
structured log lines emitted by the coordinator:

  "coordinator started"      — process is up
  "snapshot restored"        — state machine rehydrated from S3 snapshot
  "raft tick role=follower commit_index=N"   — every 2s

Rejoin = first `raft tick` after restart where commit_index matches the
leader's commit_index at kill time (within a small tolerance).

The log group is `/ecs/raft-coordinator-dev/coordinator`. Each coordinator's
stream name is prefixed with its node_id (e.g. `coordinator-b/coordinator/<task-id>`).
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass
from typing import Iterable

import boto3


RAFT_TICK_RE = re.compile(
    r'role=(?P<role>\w+).*?commit_index=(?P<commit>\d+).*?last_applied=(?P<applied>\d+)'
)
SNAPSHOT_RESTORED_RE = re.compile(r'last_included_index=(?P<idx>\d+)')


@dataclass
class RecoveryTimeline:
    node_id: str
    killed_ns: int
    leader_commit_at_kill: int

    restart_ns: int | None = None              # "coordinator started"
    snapshot_restored_ns: int | None = None    # "snapshot restored"
    snapshot_last_index: int | None = None
    catchup_ns: int | None = None              # commit_index matched leader
    final_commit_index: int | None = None

    def to_dict(self) -> dict:
        def ms(end: int | None) -> float | None:
            if end is None:
                return None
            return (end - self.killed_ns) / 1_000_000

        return {
            "node_id": self.node_id,
            "killed_ns": self.killed_ns,
            "leader_commit_at_kill": self.leader_commit_at_kill,
            "restart_ms": ms(self.restart_ns),
            "snapshot_restored_ms": ms(self.snapshot_restored_ns),
            "snapshot_last_index": self.snapshot_last_index,
            "catchup_ms": ms(self.catchup_ns),
            "final_commit_index": self.final_commit_index,
            "used_snapshot": self.snapshot_restored_ns is not None,
        }


class FollowerRecoveryObserver:
    def __init__(
        self,
        region: str,
        log_group: str,
        node_id: str,
        killed_ns: int,
        leader_commit_at_kill: int,
        tolerance: int = 10,
    ) -> None:
        self.log_group = log_group
        self.node_id = node_id
        self.killed_ns = killed_ns
        self.leader_commit = leader_commit_at_kill
        self.tol = tolerance
        session = boto3.session.Session(region_name=region)
        self.logs = session.client("logs")

    def _filter_events(self, start_ms: int, pattern: str) -> Iterable[dict]:
        """Page through filtered log events newer than start_ms."""
        kwargs = dict(
            logGroupName=self.log_group,
            startTime=start_ms,
            logStreamNamePrefix=self.node_id,
            filterPattern=pattern,
        )
        while True:
            resp = self.logs.filter_log_events(**kwargs)
            for ev in resp.get("events", []):
                yield ev
            token = resp.get("nextToken")
            if not token:
                return
            kwargs["nextToken"] = token

    def observe(self, timeout_s: float = 300.0, poll_interval_s: float = 5.0) -> RecoveryTimeline:
        """Poll until all milestones seen or timeout expires."""
        tl = RecoveryTimeline(
            node_id=self.node_id,
            killed_ns=self.killed_ns,
            leader_commit_at_kill=self.leader_commit,
        )
        start_ms = (self.killed_ns // 1_000_000) - 1000  # 1s grace on either side

        deadline = time.time() + timeout_s
        while time.time() < deadline:
            # "coordinator started" — first one after kill.
            if tl.restart_ns is None:
                for ev in self._filter_events(start_ms, '"coordinator started"'):
                    ev_ns = ev["timestamp"] * 1_000_000
                    if ev_ns > self.killed_ns:
                        tl.restart_ns = ev_ns
                        break

            # "snapshot restored" — only present if snapshotter is enabled.
            if tl.snapshot_restored_ns is None:
                for ev in self._filter_events(start_ms, '"snapshot restored"'):
                    ev_ns = ev["timestamp"] * 1_000_000
                    if ev_ns > self.killed_ns:
                        tl.snapshot_restored_ns = ev_ns
                        m = SNAPSHOT_RESTORED_RE.search(ev.get("message", ""))
                        if m:
                            tl.snapshot_last_index = int(m.group("idx"))
                        break

            # Walk raft ticks for the first commit catch-up.
            if tl.catchup_ns is None:
                target = max(0, self.leader_commit - self.tol)
                for ev in self._filter_events(start_ms, '"raft tick"'):
                    ev_ns = ev["timestamp"] * 1_000_000
                    if ev_ns <= self.killed_ns:
                        continue
                    m = RAFT_TICK_RE.search(ev.get("message", ""))
                    if not m:
                        continue
                    ci = int(m.group("commit"))
                    tl.final_commit_index = ci
                    if ci >= target:
                        tl.catchup_ns = ev_ns
                        break

            # Done when we have restart + catchup. Snapshot line is optional.
            if tl.restart_ns is not None and tl.catchup_ns is not None:
                return tl

            time.sleep(poll_interval_s)

        return tl
