"""raft_client.py — Async HTTP client that polls coordinator /state endpoints.

Each coordinator exposes GET /state which returns a NodeSnapshot JSON:
  {
    "node_id": "coordinator-a",
    "state": "leader" | "follower" | "candidate",
    "current_term": 42,
    "voted_for": "coordinator-a",
    "commit_index": 17,
    "last_applied": 17,
    "log_length": 18,
    "match_index": {"peer-A": 17, "peer-B": 16},   // leader only
    "next_index":  {"peer-A": 18, "peer-B": 17}     // leader only
  }
"""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass, field
from typing import Dict, List, Optional

import httpx


@dataclass
class NodeState:
    """Parsed view of a single coordinator's Raft state at a point in time."""

    node_id: str
    state: str              # "leader" | "follower" | "candidate" | "unreachable"
    current_term: int
    voted_for: str
    commit_index: int
    last_applied: int
    log_length: int
    match_index: Optional[Dict[str, int]] = None   # populated when state == "leader"
    next_index: Optional[Dict[str, int]] = None    # populated when state == "leader"

    # Metadata added by the client (not from the wire)
    addr: str = ""
    reachable: bool = True
    error: Optional[str] = None
    polled_at: float = field(default_factory=time.monotonic)

    @classmethod
    def from_dict(cls, d: dict, addr: str) -> "NodeState":
        return cls(
            node_id=d.get("node_id", addr),
            state=d.get("state", "unknown"),
            current_term=d.get("current_term", 0),
            voted_for=d.get("voted_for", ""),
            commit_index=d.get("commit_index", 0),
            last_applied=d.get("last_applied", 0),
            log_length=d.get("log_length", 0),
            match_index=d.get("match_index"),
            next_index=d.get("next_index"),
            addr=addr,
        )

    @classmethod
    def unreachable(cls, addr: str, error: str) -> "NodeState":
        return cls(
            node_id=addr,
            state="unreachable",
            current_term=0,
            voted_for="",
            commit_index=0,
            last_applied=0,
            log_length=0,
            addr=addr,
            reachable=False,
            error=error,
        )

    @property
    def is_leader(self) -> bool:
        return self.state == "leader"

    def replication_lags(self) -> Dict[str, int]:
        """Returns {peer_id: lag} where lag = commit_index - match_index[peer].

        Only meaningful when this node is the leader and match_index is populated.
        """
        if not self.is_leader or not self.match_index:
            return {}
        return {
            peer: max(0, self.commit_index - match)
            for peer, match in self.match_index.items()
        }


@dataclass
class ClusterSnapshot:
    """All node states collected in a single poll round."""

    nodes: List[NodeState]
    polled_at: float = field(default_factory=time.monotonic)

    @property
    def leaders(self) -> List[NodeState]:
        return [n for n in self.nodes if n.is_leader]

    @property
    def reachable_count(self) -> int:
        return sum(1 for n in self.nodes if n.reachable)

    @property
    def leader(self) -> Optional[NodeState]:
        """Returns the single leader, or None if there is none (or more than one)."""
        ls = self.leaders
        return ls[0] if len(ls) == 1 else None

    @property
    def current_term(self) -> int:
        """Highest term observed across all reachable nodes."""
        terms = [n.current_term for n in self.nodes if n.reachable]
        return max(terms) if terms else 0

    def split_brain_message(self) -> Optional[str]:
        """Returns a human-readable alert message if split-brain is detected.

        Split-brain = two or more nodes simultaneously report state == "leader"
        at the same term.  One leader per term is the Raft safety guarantee;
        two leaders means something has gone seriously wrong.
        """
        leaders_by_term: Dict[int, List[str]] = {}
        for n in self.nodes:
            if n.is_leader:
                leaders_by_term.setdefault(n.current_term, []).append(n.node_id)

        for term, leaders in leaders_by_term.items():
            if len(leaders) > 1:
                return (
                    f"SPLIT-BRAIN DETECTED: term={term} "
                    f"multiple leaders: {', '.join(sorted(leaders))}"
                )
        return None


class RaftClusterClient:
    """Polls all coordinators concurrently and returns a ClusterSnapshot."""

    def __init__(self, addrs: List[str], timeout: float = 1.5):
        self._addrs = addrs
        self._client = httpx.AsyncClient(timeout=timeout)

    async def poll(self) -> ClusterSnapshot:
        tasks = [self._poll_one(addr) for addr in self._addrs]
        nodes = await asyncio.gather(*tasks)
        return ClusterSnapshot(nodes=list(nodes))

    async def _poll_one(self, addr: str) -> NodeState:
        url = f"http://{addr}/state"
        try:
            resp = await self._client.get(url)
            resp.raise_for_status()
            return NodeState.from_dict(resp.json(), addr)
        except Exception as exc:
            return NodeState.unreachable(addr, str(exc))

    async def close(self) -> None:
        await self._client.aclose()
