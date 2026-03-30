"""metrics.py — Prometheus and CloudWatch metric publishing.

Prometheus
----------
Exposes a /metrics endpoint (prometheus_client HTTP server) on PROMETHEUS_PORT.
The Prometheus server scrapes this endpoint; metrics are updated on every poll.

CloudWatch
----------
Publishes a batch of custom metrics via PutMetricData every CW_PUBLISH_INTERVAL_S
seconds.  Batching keeps API call count low (one call per interval, regardless of
cluster size).

Metric catalogue
----------------
Per-node (dimension: NodeId):
  raft_current_term          – current Raft term
  raft_commit_index          – highest log index known to be committed
  raft_log_length            – total log entries (incl. dummy entry at index 0)
  raft_is_leader             – 1 if leader, 0 otherwise
  raft_node_up               – 1 if reachable, 0 otherwise

Per-peer replication (dimensions: LeaderId, PeerId):
  raft_replication_lag       – commit_index − match_index for each follower

Cluster-level (no extra dimensions):
  raft_has_leader            – 1 if exactly one leader, 0 otherwise
  raft_split_brain           – 1 if split-brain detected, 0 otherwise
  raft_leader_term           – current leader's term (0 if leaderless)
  raft_nodes_up              – count of reachable nodes
"""

from __future__ import annotations

import logging
import time
from typing import TYPE_CHECKING, List

import boto3
from botocore.exceptions import BotoCoreError, ClientError
from prometheus_client import Gauge, start_http_server

if TYPE_CHECKING:
    from raft_client import ClusterSnapshot

log = logging.getLogger(__name__)

# ── Prometheus gauge registry ─────────────────────────────────────────────────

_p_term = Gauge("raft_current_term", "Current Raft term", ["node_id"])
_p_commit = Gauge("raft_commit_index", "Highest committed log index", ["node_id"])
_p_log_len = Gauge("raft_log_length", "Total log entries", ["node_id"])
_p_is_leader = Gauge("raft_is_leader", "1 if node is current leader", ["node_id"])
_p_node_up = Gauge("raft_node_up", "1 if node is reachable", ["node_id"])
_p_rep_lag = Gauge(
    "raft_replication_lag",
    "Entries not yet replicated to peer (commit_index - match_index)",
    ["leader_id", "peer_id"],
)
_p_has_leader = Gauge("raft_has_leader", "1 if cluster has exactly one leader")
_p_split_brain = Gauge("raft_split_brain", "1 if split-brain is detected")
_p_leader_term = Gauge("raft_leader_term", "Current leader term (0 if leaderless)")
_p_nodes_up = Gauge("raft_nodes_up", "Number of reachable coordinator nodes")


class MetricsPublisher:
    """Updates Prometheus gauges and batches CloudWatch PutMetricData calls."""

    def __init__(
        self,
        *,
        prometheus_port: int,
        cw_namespace: str,
        aws_region: str,
        cw_publish_interval_s: float,
        disable_cloudwatch: bool,
    ):
        self._namespace = cw_namespace
        self._cw_interval = cw_publish_interval_s
        self._disable_cw = disable_cloudwatch
        self._last_cw_publish: float = 0.0
        self._pending_metrics: List[dict] = []

        # Start Prometheus HTTP exposition server in background thread.
        start_http_server(prometheus_port)
        log.info("Prometheus /metrics available on port %d", prometheus_port)

        if not disable_cloudwatch:
            self._cw = boto3.client("cloudwatch", region_name=aws_region)
            log.info("CloudWatch publisher ready (namespace=%s)", cw_namespace)
        else:
            self._cw = None
            log.info("CloudWatch publishing disabled")

    # ── Public API ────────────────────────────────────────────────────────────

    def update(self, snapshot: "ClusterSnapshot") -> None:
        """Update all metrics from the latest cluster snapshot.

        Called on every poll tick.  Prometheus gauges are updated immediately;
        CloudWatch data is buffered and flushed on the slower publish interval.
        """
        self._update_prometheus(snapshot)
        if not self._disable_cw:
            self._buffer_cloudwatch(snapshot)
            if time.monotonic() - self._last_cw_publish >= self._cw_interval:
                self._flush_cloudwatch()

    # ── Prometheus ────────────────────────────────────────────────────────────

    def _update_prometheus(self, snap: "ClusterSnapshot") -> None:
        for n in snap.nodes:
            nid = n.node_id
            _p_term.labels(node_id=nid).set(n.current_term)
            _p_commit.labels(node_id=nid).set(n.commit_index)
            _p_log_len.labels(node_id=nid).set(n.log_length)
            _p_is_leader.labels(node_id=nid).set(1 if n.is_leader else 0)
            _p_node_up.labels(node_id=nid).set(1 if n.reachable else 0)

            for peer_id, lag in n.replication_lags().items():
                _p_rep_lag.labels(leader_id=nid, peer_id=peer_id).set(lag)

        has_leader = snap.leader is not None
        _p_has_leader.set(1 if has_leader else 0)
        _p_split_brain.set(1 if snap.split_brain_message() else 0)
        _p_leader_term.set(snap.leader.current_term if has_leader else 0)
        _p_nodes_up.set(snap.reachable_count)

    # ── CloudWatch ────────────────────────────────────────────────────────────

    def _buffer_cloudwatch(self, snap: "ClusterSnapshot") -> None:
        """Stage metric data points for the next flush call."""
        ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

        for n in snap.nodes:
            nid = n.node_id
            dim = [{"Name": "NodeId", "Value": nid}]

            self._pending_metrics += [
                _cw_datum("CurrentTerm", n.current_term, "Count", dim, ts),
                _cw_datum("CommitIndex", n.commit_index, "Count", dim, ts),
                _cw_datum("LogLength", n.log_length, "Count", dim, ts),
                _cw_datum("IsLeader", 1 if n.is_leader else 0, "Count", dim, ts),
                _cw_datum("NodeUp", 1 if n.reachable else 0, "Count", dim, ts),
            ]

            for peer_id, lag in n.replication_lags().items():
                rep_dim = [
                    {"Name": "LeaderId", "Value": nid},
                    {"Name": "PeerId", "Value": peer_id},
                ]
                self._pending_metrics.append(
                    _cw_datum("ReplicationLag", lag, "Count", rep_dim, ts)
                )

        has_leader = snap.leader is not None
        self._pending_metrics += [
            _cw_datum("HasLeader", 1 if has_leader else 0, "Count", [], ts),
            _cw_datum("SplitBrain", 1 if snap.split_brain_message() else 0, "Count", [], ts),
            _cw_datum(
                "LeaderTerm",
                snap.leader.current_term if has_leader else 0,
                "Count",
                [],
                ts,
            ),
            _cw_datum("NodesUp", snap.reachable_count, "Count", [], ts),
        ]

    def _flush_cloudwatch(self) -> None:
        """Send all buffered metric data to CloudWatch in batches of 1000."""
        if not self._pending_metrics:
            return

        batch_size = 1000  # CloudWatch maximum per PutMetricData call
        batches = [
            self._pending_metrics[i : i + batch_size]
            for i in range(0, len(self._pending_metrics), batch_size)
        ]
        self._pending_metrics = []
        self._last_cw_publish = time.monotonic()

        for batch in batches:
            try:
                self._cw.put_metric_data(
                    Namespace=self._namespace,
                    MetricData=batch,
                )
                log.debug("CloudWatch: published %d metrics", len(batch))
            except (BotoCoreError, ClientError) as exc:
                log.warning("CloudWatch publish failed: %s", exc)


# ── Helpers ───────────────────────────────────────────────────────────────────

def _cw_datum(
    metric_name: str,
    value: float,
    unit: str,
    dimensions: list,
    timestamp: str,
) -> dict:
    d: dict = {
        "MetricName": metric_name,
        "Value": float(value),
        "Unit": unit,
        "Timestamp": timestamp,
    }
    if dimensions:
        d["Dimensions"] = dimensions
    return d
