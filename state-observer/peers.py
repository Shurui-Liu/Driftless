"""peers.py - Resolve coordinator HTTP addresses from the DynamoDB peers table.

Replaces Cloud Map DNS lookup in sandbox environments that deny
servicediscovery:* actions. Coordinators register their private IP into
<PEERS_TABLE> on startup; this module reads them back out.
"""

from __future__ import annotations

import logging
from typing import List

import boto3

log = logging.getLogger(__name__)


def resolve_addrs(table_name: str, node_ids: List[str], region: str) -> List[str]:
    """Return the http_addr field for each node_id that resolves.

    Silently skips rows that are missing, malformed, or error out - the caller
    will mark any unreachable node as "unreachable" in the next poll, which is
    the observer's normal behavior for dead coordinators.
    """
    client = boto3.client("dynamodb", region_name=region)
    addrs: List[str] = []
    for nid in node_ids:
        try:
            resp = client.get_item(
                TableName=table_name,
                Key={"node_id": {"S": nid}},
                ConsistentRead=True,
            )
        except Exception as exc:
            log.warning("peers: get_item %s failed: %s", nid, exc)
            continue
        item = resp.get("Item")
        if not item:
            continue
        http_addr = item.get("http_addr", {}).get("S")
        if http_addr:
            addrs.append(http_addr)
    return addrs
