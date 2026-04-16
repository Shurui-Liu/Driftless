"""alerts.py — Split-brain detection and SNS alerting.

Split-brain definition
----------------------
Two or more coordinator nodes simultaneously report state == "leader" at the
same Raft term.  Raft guarantees at most one leader per term; if two nodes both
believe they are leader in term T, the safety invariant has been violated.

Deduplication
-------------
We only send one SNS alert per unique incident.  An incident is identified by
(term, frozenset(leader_ids)).  Once an alert is fired, we suppress duplicates
for COOLDOWN_S seconds so that a cluster flap doesn't flood the SNS topic.

Other alertable conditions
--------------------------
  - No leader elected and cluster has been leaderless for > NO_LEADER_TIMEOUT_S
  - Majority of nodes unreachable (< quorum reachable)
"""

from __future__ import annotations

import logging
import time
from typing import Optional, TYPE_CHECKING

import boto3
from botocore.exceptions import BotoCoreError, ClientError

if TYPE_CHECKING:
    from raft_client import ClusterSnapshot

log = logging.getLogger(__name__)

_COOLDOWN_S = 120.0          # suppress duplicate alerts for 2 minutes
_NO_LEADER_TIMEOUT_S = 10.0  # alert if leaderless for longer than this


class AlertManager:
    """Detects anomalies and fires SNS notifications with deduplication."""

    def __init__(
        self,
        *,
        sns_topic_arn: str,
        aws_region: str,
        disable_sns: bool,
    ):
        self._topic_arn = sns_topic_arn
        self._disable_sns = disable_sns or not sns_topic_arn
        self._last_sent: dict[str, float] = {}   # alert_key → monotonic timestamp
        self._leaderless_since: Optional[float] = None

        if not self._disable_sns:
            self._sns = boto3.client("sns", region_name=aws_region)
            log.info("SNS alerts enabled (topic=%s)", sns_topic_arn)
        else:
            self._sns = None
            reason = "no SNS_TOPIC_ARN set" if not sns_topic_arn else "SNS disabled"
            log.info("SNS alerts disabled (%s)", reason)

    # ── Public API ────────────────────────────────────────────────────────────

    def check(self, snapshot: "ClusterSnapshot") -> list[str]:
        """Inspect the snapshot for anomalies.  Returns a list of alert messages
        (may be empty).  Fires SNS for any new, non-duplicate alert.
        """
        alerts: list[str] = []

        self._check_split_brain(snapshot, alerts)
        self._check_no_leader(snapshot, alerts)
        self._check_quorum_loss(snapshot, alerts)

        return alerts

    # ── Detectors ─────────────────────────────────────────────────────────────

    def _check_split_brain(
        self, snap: "ClusterSnapshot", alerts: list[str]
    ) -> None:
        msg = snap.split_brain_message()
        if not msg:
            return

        # Build a stable key so we deduplicate across consecutive poll ticks.
        leaders_by_term: dict[int, list[str]] = {}
        for n in snap.nodes:
            if n.is_leader:
                leaders_by_term.setdefault(n.current_term, []).append(n.node_id)

        for term, leaders in leaders_by_term.items():
            if len(leaders) > 1:
                key = f"split-brain:term={term}:leaders={','.join(sorted(leaders))}"
                subject = f"[Driftless] SPLIT-BRAIN DETECTED in term {term}"
                body = (
                    f"{msg}\n\n"
                    f"This means the Raft safety invariant (at most one leader per term) "
                    f"has been violated.  Immediate investigation required.\n\n"
                    f"Nodes queried: {[n.node_id for n in snap.nodes]}"
                )
                fired = self._maybe_send(key, subject, body)
                alerts.append(msg + (" [alert sent]" if fired else " [suppressed]"))

    def _check_no_leader(
        self, snap: "ClusterSnapshot", alerts: list[str]
    ) -> None:
        if snap.leader is not None:
            # Leader present — reset the leaderless timer.
            self._leaderless_since = None
            return

        now = time.monotonic()
        if self._leaderless_since is None:
            self._leaderless_since = now
            return

        elapsed = now - self._leaderless_since
        if elapsed < _NO_LEADER_TIMEOUT_S:
            return  # still within grace period

        reachable = [n.node_id for n in snap.nodes if n.reachable]
        key = f"no-leader:{snap.current_term}"
        subject = "[Driftless] No Raft leader elected"
        body = (
            f"No leader has been elected for {elapsed:.1f}s.\n"
            f"Reachable nodes: {reachable}\n"
            f"Highest term seen: {snap.current_term}\n\n"
            f"Possible causes: network partition, all nodes crashed, "
            f"persistent split-vote."
        )
        msg = f"No leader for {elapsed:.0f}s (term={snap.current_term})"
        fired = self._maybe_send(key, subject, body)
        alerts.append(msg + (" [alert sent]" if fired else " [suppressed]"))

    def _check_quorum_loss(
        self, snap: "ClusterSnapshot", alerts: list[str]
    ) -> None:
        total = len(snap.nodes)
        if total == 0:
            return
        quorum = total // 2 + 1
        reachable = snap.reachable_count
        if reachable >= quorum:
            return

        key = f"quorum-loss:{reachable}/{total}"
        subject = "[Driftless] Quorum loss — cluster cannot make progress"
        body = (
            f"Only {reachable}/{total} nodes are reachable; "
            f"quorum requires {quorum}.\n"
            f"The cluster cannot commit new log entries until "
            f"enough nodes are restored."
        )
        msg = f"Quorum loss: {reachable}/{total} nodes up (need {quorum})"
        fired = self._maybe_send(key, subject, body)
        alerts.append(msg + (" [alert sent]" if fired else " [suppressed]"))

    # ── SNS dispatch with cooldown ─────────────────────────────────────────────

    def _maybe_send(self, key: str, subject: str, body: str) -> bool:
        """Send an SNS notification unless the same key was sent recently.

        Returns True if the message was actually published.
        """
        now = time.monotonic()
        last = self._last_sent.get(key, 0.0)
        if now - last < _COOLDOWN_S:
            return False

        self._last_sent[key] = now
        log.warning("ALERT: %s", subject)

        if self._disable_sns or self._sns is None:
            return False  # alert detected, but not sent (SNS disabled)

        try:
            self._sns.publish(
                TopicArn=self._topic_arn,
                Subject=subject[:100],  # SNS subject limit
                Message=body,
            )
            log.info("SNS alert sent: %s", key)
            return True
        except (BotoCoreError, ClientError) as exc:
            log.error("SNS publish failed: %s", exc)
            return False
