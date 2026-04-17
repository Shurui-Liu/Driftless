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
from peers import resolve_addrs
from raft_client import RaftClusterClient

logging.basicConfig(
    level=logging.WARNING,
    format="%(asctime)s  %(levelname)-8s  %(name)s  %(message)s",
    stream=sys.stderr,
)
log = logging.getLogger(__name__)


async def _resolve_initial_addrs(cfg: Config) -> list[str]:
    """If PEERS_TABLE + COORDINATOR_NODE_IDS are set, resolve addresses from
    DynamoDB with a short retry loop. Otherwise fall back to cfg.coordinator_addrs."""
    if not cfg.peers_table or not cfg.coordinator_node_ids:
        return cfg.coordinator_addrs

    loop = asyncio.get_running_loop()
    deadline = loop.time() + 120  # 2 minutes
    while loop.time() < deadline:
        addrs = await loop.run_in_executor(
            None,
            resolve_addrs,
            cfg.peers_table,
            cfg.coordinator_node_ids,
            cfg.aws_region,
        )
        if addrs:
            log.warning("resolved coordinators from peers table: %s", addrs)
            return addrs
        await asyncio.sleep(2)
    log.warning("peers table yielded no coordinators; falling back to static addrs")
    return cfg.coordinator_addrs


async def _refresh_addrs_loop(
    cfg: Config, client: RaftClusterClient, stop_event: asyncio.Event
) -> None:
    """Periodically re-resolve addresses from the peers table so that
    restarted coordinators (new private IPs) get picked up."""
    if not cfg.peers_table or not cfg.coordinator_node_ids:
        return
    loop = asyncio.get_running_loop()
    while not stop_event.is_set():
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=30)
            return
        except asyncio.TimeoutError:
            pass
        try:
            new_addrs = await loop.run_in_executor(
                None,
                resolve_addrs,
                cfg.peers_table,
                cfg.coordinator_node_ids,
                cfg.aws_region,
            )
            if new_addrs and set(new_addrs) != set(client._addrs):
                log.warning("coordinator addresses changed: %s", new_addrs)
                client._addrs = new_addrs
        except Exception as exc:
            log.error("addr refresh failed: %s", exc)


async def observe(cfg: Config, stop_event: asyncio.Event) -> None:
    """Main observation loop.  Runs until stop_event is set."""

    addrs = await _resolve_initial_addrs(cfg)
    client = RaftClusterClient(addrs)
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

    refresh_task = asyncio.create_task(_refresh_addrs_loop(cfg, client, stop_event))

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

    refresh_task.cancel()
    try:
        await refresh_task
    except (asyncio.CancelledError, Exception):
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
