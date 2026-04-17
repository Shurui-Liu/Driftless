"""leader_detect.py — find the current Raft leader via coordinator /state.

Reads every coordinator's address out of the DynamoDB peers table, polls
`http://addr/state`, and returns the node whose state is "leader".
Maps the node_id back to its ECS service name so the chaos injector knows
which service to call StopTask against.
"""

from __future__ import annotations

import urllib.error
import urllib.request
from dataclasses import dataclass

import boto3

from config import Config

STATE_POLL_TIMEOUT_S = 3.0


@dataclass
class LeaderInfo:
    node_id: str
    http_addr: str          # "http://10.0.11.170:8080"
    ecs_service: str        # "raft-coordinator-dev-coordinator-a"
    term: int


def _ecs_service_for(node_id: str, known_services: list[str]) -> str | None:
    """Map node_id like "coordinator-a" to its ECS service name.

    Terraform names services "<prefix>-<node_id>" so we match by suffix.
    """
    for s in known_services:
        if s.endswith(node_id):
            return s
    return None


def _fetch_state(http_addr: str) -> dict | None:
    try:
        with urllib.request.urlopen(http_addr + "/state", timeout=STATE_POLL_TIMEOUT_S) as r:
            import json
            return json.loads(r.read())
    except (urllib.error.URLError, TimeoutError, ValueError):
        return None


def find_leader(cfg: Config) -> LeaderInfo | None:
    """Find the current leader.

    Preferred path (works from outside the VPC): scan the peers DynamoDB
    table and read the `is_leader` attribute published on each heartbeat.

    Fallback: if no row is marked leader (older image without the bit),
    poll each node's /state endpoint — only works from inside the VPC.
    """
    session = boto3.session.Session(region_name=cfg.aws_region)
    ddb = session.client("dynamodb")

    resp = ddb.scan(TableName=cfg.peers_table, ConsistentRead=True)
    rows = []
    for item in resp.get("Items", []):
        node_id = item.get("node_id", {}).get("S")
        http_addr = item.get("http_addr", {}).get("S")
        if not node_id or not http_addr:
            continue
        is_leader = item.get("is_leader", {}).get("BOOL", False)
        rows.append((node_id, http_addr, is_leader))
        if is_leader:
            svc = _ecs_service_for(node_id, cfg.coordinator_services)
            if svc:
                return LeaderInfo(
                    node_id=node_id, http_addr=f"http://{http_addr}",
                    ecs_service=svc, term=0,
                )

    # Fallback: HTTP /state polling (in-VPC only).
    for node_id, http_addr, _ in rows:
        state = _fetch_state(f"http://{http_addr}")
        if state and state.get("state") == "leader":
            svc = _ecs_service_for(node_id, cfg.coordinator_services)
            if svc:
                return LeaderInfo(
                    node_id=node_id, http_addr=f"http://{http_addr}",
                    ecs_service=svc, term=int(state.get("current_term", 0)),
                )
    return None
