"""leader_tracker.py — observe leader changes during a run.

Polls the peers DynamoDB table for the `is_leader` bit on every coordinator
row. Emits a `leader_observed` event whenever the node holding the bit
changes (or becomes absent). Used by Exp 1 to measure election convergence
time after a `kill_leader` injection.

Polling interval must be shorter than the election timeout (150-300ms) so a
fast re-election isn't under-sampled. Default is 150ms — a single DDB scan
of a ~5-row table sits well under that budget.

Note: the peers table doesn't currently publish `current_term`. A re-election
is detected by node_id flip, which is reliable in a kill_leader scenario
because the just-killed leader cannot re-win before a new node takes over.
"""

from __future__ import annotations

import threading
import time
from typing import Callable, Optional

import boto3

from config import Config


class LeaderTracker:
    def __init__(
        self,
        cfg: Config,
        record: Callable[..., None],
        poll_interval_s: float = 0.15,
    ) -> None:
        self.cfg = cfg
        self._record = record
        self._poll_s = poll_interval_s
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

        session = boto3.session.Session(region_name=cfg.aws_region)
        self._ddb = session.client("dynamodb")

    def start(self) -> None:
        if self._thread is not None:
            raise RuntimeError("already started")
        self._thread = threading.Thread(target=self._run, name="leader-tracker", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=5.0)

    def _run(self) -> None:
        last_leader: str | None = None
        while not self._stop.is_set():
            t0 = time.time()
            leader = self._scan_once()
            if leader != last_leader:
                if leader is not None:
                    self._record(op="leader_observed", node_id=leader)
                else:
                    self._record(op="leader_gap")
                last_leader = leader
            elapsed = time.time() - t0
            if elapsed < self._poll_s:
                self._stop.wait(self._poll_s - elapsed)

    # Ignore peer rows whose heartbeat_ts is older than this. A killed
    # leader's row retains is_leader=True until TTL (~30s), so without a
    # freshness gate the tracker would keep returning the dead node and
    # miss the real re-election. 3s is well above the 500ms heartbeat
    # interval and far under the election timeout.
    STALE_HB_S = 3.0

    def _scan_once(self) -> str | None:
        try:
            resp = self._ddb.scan(
                TableName=self.cfg.peers_table,
                ConsistentRead=True,
            )
        except Exception:
            return None
        now = time.time()
        for item in resp.get("Items", []):
            if not item.get("is_leader", {}).get("BOOL", False):
                continue
            hb_v = item.get("heartbeat_ts", {}).get("N")
            if hb_v is not None:
                try:
                    if now - int(hb_v) > self.STALE_HB_S:
                        continue
                except ValueError:
                    continue
            node_id = item.get("node_id", {}).get("S")
            if node_id:
                return node_id
        return None
