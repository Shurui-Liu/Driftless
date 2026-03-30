#!/usr/bin/env python3
"""main.py — State Observer entry point.

Continuously polls all Raft coordinator nodes, publishes metrics, drives the
terminal dashboard, and fires SNS alerts when the cluster is unhealthy.

Usage
-----
  # Local (against coordinators running on localhost):
  COORDINATOR_ADDRS=localhost:8080,localhost:8081,localhost:8082 python main.py

  # ECS / production:
  COORDINATOR_ADDRS=coordinator-a.raft.local:8080,coordinator-b.raft.local:8080,coordinator-c.raft.local:8080 \
  AWS_REGION=us-east-1 \
  SNS_TOPIC_ARN=arn:aws:sns:us-east-1:123456789012:driftless-alerts \
  CW_NAMESPACE=Driftless/Raft \
  python main.py

Environment variables
---------------------
See config.py for the full list.
"""

from __future__ import annotations

import asyncio
import logging
import signal
import sys

from alerts import AlertManager
from config import Config
from dashboard import Dashboard
from metrics import MetricsPublisher
from raft_client import RaftClusterClient

logging.basicConfig(
    level=logging.WARNING,
    format="%(asctime)s  %(levelname)-8s  %(name)s  %(message)s",
    stream=sys.stderr,
)
log = logging.getLogger(__name__)


async def observe(cfg: Config, stop_event: asyncio.Event) -> None:
    """Main observation loop.  Runs until stop_event is set."""

    client = RaftClusterClient(cfg.coordinator_addrs)
    metrics = MetricsPublisher(
        prometheus_port=cfg.prometheus_port,
        cw_namespace=cfg.cw_namespace,
        aws_region=cfg.aws_region,
        cw_publish_interval_s=cfg.cw_publish_interval_s,
        disable_cloudwatch=cfg.disable_cloudwatch,
    )
    alerter = AlertManager(
        sns_topic_arn=cfg.sns_topic_arn,
        aws_region=cfg.aws_region,
        disable_sns=cfg.disable_sns,
    )

    with Dashboard(cfg.poll_interval_s) as dashboard:
        while not stop_event.is_set():
            try:
                snapshot = await client.poll()
                active_alerts = alerter.check(snapshot)
                metrics.update(snapshot)
                dashboard.update(snapshot, active_alerts)
            except Exception as exc:
                # Never let a poll error crash the observer.
                log.error("poll error: %s", exc, exc_info=True)

            try:
                await asyncio.wait_for(
                    stop_event.wait(), timeout=cfg.poll_interval_s
                )
            except asyncio.TimeoutError:
                pass

    await client.close()


def main() -> None:
    cfg = Config.from_env()
    print(f"State Observer starting — {cfg}", file=sys.stderr)

    loop = asyncio.new_event_loop()
    stop_event = asyncio.Event()

    # Graceful shutdown on SIGTERM / SIGINT.
    def _handle_signal() -> None:
        stop_event.set()

    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, _handle_signal)
        except (NotImplementedError, OSError):
            # Windows does not support add_signal_handler for all signals.
            pass

    try:
        loop.run_until_complete(observe(cfg, stop_event))
    except KeyboardInterrupt:
        pass
    finally:
        loop.close()
        print("State Observer stopped.", file=sys.stderr)


if __name__ == "__main__":
    main()
