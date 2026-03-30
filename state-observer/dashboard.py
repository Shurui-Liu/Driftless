"""dashboard.py — Real-time terminal dashboard using Rich.

Layout (updates every poll tick):
┌──────────────────────────────────────────────────────────────────┐
│  DRIFTLESS  ·  Raft Cluster Monitor          refreshed HH:MM:SS  │
├──────────────────────────────────────────────────────────────────┤
│  CLUSTER SUMMARY                                                  │
│  Leader: coordinator-a  ·  Term: 42  ·  3/3 nodes up            │
├──────────────────────────────────────────────────────────────────┤
│  NODE STATUS                                                      │
│  ┌─────────────────┬────────────┬──────┬─────────┬──────────┐   │
│  │ Node            │ State      │ Term │ Commit  │ Status   │   │
│  └─────────────────┴────────────┴──────┴─────────┴──────────┘   │
├──────────────────────────────────────────────────────────────────┤
│  REPLICATION LAG  (from leader coordinator-a)                     │
│  ┌─────────────────┬────────────┬──────────┐                     │
│  │ Peer            │ Match Idx  │ Lag      │                     │
│  └─────────────────┴────────────┴──────────┘                     │
├──────────────────────────────────────────────────────────────────┤
│  THROUGHPUT  ·  commit rate: 3.2 entries/s                       │
├──────────────────────────────────────────────────────────────────┤
│  ALERTS                                                           │
│  ✓ No active alerts                                               │
└──────────────────────────────────────────────────────────────────┘
"""

from __future__ import annotations

import time
from collections import deque
from datetime import datetime
from typing import List, Optional, TYPE_CHECKING

from rich.console import Console
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.table import Table
from rich.text import Text

if TYPE_CHECKING:
    from raft_client import ClusterSnapshot

_THROUGHPUT_WINDOW = 10  # seconds of history used to compute commit rate


class Dashboard:
    """Renders a Rich Live terminal dashboard from ClusterSnapshot objects."""

    def __init__(self, poll_interval_s: float):
        self._poll_interval = poll_interval_s
        self._console = Console()
        self._live = Live(
            renderable=self._render(None, []),
            console=self._console,
            refresh_per_second=4,
            screen=True,
        )
        # Ring buffer of (monotonic_time, commit_index) samples for throughput calc.
        self._commit_history: deque[tuple[float, int]] = deque(maxlen=64)
        self._latest_snapshot: Optional["ClusterSnapshot"] = None
        self._latest_alerts: List[str] = []

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def __enter__(self) -> "Dashboard":
        self._live.__enter__()
        return self

    def __exit__(self, *args) -> None:
        self._live.__exit__(*args)

    # ── Public API ────────────────────────────────────────────────────────────

    def update(
        self, snapshot: "ClusterSnapshot", alerts: List[str]
    ) -> None:
        """Called on each poll tick to refresh the display."""
        self._latest_snapshot = snapshot
        self._latest_alerts = alerts

        # Track commit index for throughput calculation.
        if snapshot.leader:
            self._commit_history.append(
                (time.monotonic(), snapshot.leader.commit_index)
            )

        self._live.update(self._render(snapshot, alerts))

    # ── Rendering ─────────────────────────────────────────────────────────────

    def _render(
        self, snap: Optional["ClusterSnapshot"], alerts: List[str]
    ) -> Panel:
        layout = Layout()
        layout.split_column(
            Layout(name="header", size=3),
            Layout(name="summary", size=4),
            Layout(name="nodes"),
            Layout(name="replication"),
            Layout(name="throughput", size=3),
            Layout(name="alerts_section", size=max(3, len(alerts) + 2)),
        )

        layout["header"].update(self._header())
        layout["summary"].update(self._summary(snap))
        layout["nodes"].update(self._node_table(snap))
        layout["replication"].update(self._replication_table(snap))
        layout["throughput"].update(self._throughput_panel())
        layout["alerts_section"].update(self._alerts_panel(alerts))

        return Panel(
            layout,
            title="[bold cyan]DRIFTLESS[/bold cyan]  ·  Raft Cluster Monitor",
            border_style="bright_blue",
            padding=(0, 1),
        )

    def _header(self) -> Text:
        now = datetime.now().strftime("%H:%M:%S")
        t = Text(justify="right")
        t.append("refreshed ", style="dim")
        t.append(now, style="bold white")
        t.append(f"  ·  poll every {self._poll_interval:.1f}s", style="dim")
        return t

    def _summary(self, snap: Optional["ClusterSnapshot"]) -> Panel:
        if snap is None:
            return Panel(Text("Waiting for first poll…", style="dim"))

        leader = snap.leader
        if leader:
            leader_text = Text()
            leader_text.append("Leader: ", style="dim")
            leader_text.append(leader.node_id, style="bold green")
            leader_text.append("  ·  Term: ", style="dim")
            leader_text.append(str(leader.current_term), style="bold white")
        else:
            leaders = snap.leaders
            if len(leaders) > 1:
                leader_text = Text("⚠  SPLIT-BRAIN — multiple leaders", style="bold red")
            else:
                leader_text = Text("⚠  No leader elected", style="bold yellow")

        up_total = f"{snap.reachable_count}/{len(snap.nodes)}"
        up_color = "green" if snap.reachable_count == len(snap.nodes) else "yellow"
        up_text = Text()
        up_text.append("  ·  Nodes up: ", style="dim")
        up_text.append(up_total, style=f"bold {up_color}")

        combined = Text()
        combined.append_text(leader_text)
        combined.append_text(up_text)

        return Panel(combined, title="Cluster Summary", border_style="blue", padding=(0, 1))

    def _node_table(self, snap: Optional["ClusterSnapshot"]) -> Panel:
        table = Table(
            show_header=True,
            header_style="bold cyan",
            box=None,
            pad_edge=False,
            expand=True,
        )
        table.add_column("Node", min_width=18)
        table.add_column("State", min_width=12)
        table.add_column("Term", justify="right", min_width=6)
        table.add_column("Commit", justify="right", min_width=8)
        table.add_column("Applied", justify="right", min_width=8)
        table.add_column("Log Len", justify="right", min_width=8)
        table.add_column("Status", min_width=8)

        if snap is None:
            return Panel(table, title="Node Status", border_style="blue")

        for n in sorted(snap.nodes, key=lambda x: x.node_id):
            state_text, state_style = _format_state(n.state)
            status_text = (
                Text("✓ up", style="green") if n.reachable else Text("✗ down", style="red")
            )
            table.add_row(
                Text(n.node_id, style="bold" if n.is_leader else ""),
                Text(state_text, style=state_style),
                str(n.current_term),
                str(n.commit_index),
                str(n.last_applied),
                str(n.log_length),
                status_text,
            )

        return Panel(table, title="Node Status", border_style="blue")

    def _replication_table(self, snap: Optional["ClusterSnapshot"]) -> Panel:
        if snap is None or snap.leader is None:
            empty = Text("─  No leader; replication lag unavailable", style="dim")
            return Panel(empty, title="Replication Lag", border_style="blue")

        leader = snap.leader
        lags = leader.replication_lags()

        if not lags:
            empty = Text("─  No peer replication data (single-node cluster?)", style="dim")
            return Panel(empty, title=f"Replication Lag  (leader: {leader.node_id})", border_style="blue")

        table = Table(show_header=True, header_style="bold cyan", box=None, pad_edge=False, expand=True)
        table.add_column("Peer", min_width=18)
        table.add_column("Match Index", justify="right", min_width=12)
        table.add_column("Lag", justify="right", min_width=6)
        table.add_column("", min_width=16)  # visual bar

        for peer_id in sorted(lags.keys()):
            lag = lags[peer_id]
            match = (leader.match_index or {}).get(peer_id, 0)
            lag_style = "green" if lag == 0 else ("yellow" if lag <= 3 else "red")
            bar = "█" * min(lag, 20)
            table.add_row(
                peer_id,
                str(match),
                Text(str(lag), style=lag_style),
                Text(bar, style=lag_style),
            )

        return Panel(
            table,
            title=f"Replication Lag  (leader: {leader.node_id})",
            border_style="blue",
        )

    def _throughput_panel(self) -> Panel:
        rate = self._commit_rate()
        text = Text()
        text.append("Commit rate: ", style="dim")
        if rate is None:
            text.append("calculating…", style="dim italic")
        else:
            color = "green" if rate > 0 else "dim"
            text.append(f"{rate:.1f} entries/s", style=f"bold {color}")
        return Panel(text, title="Throughput", border_style="blue", padding=(0, 1))

    def _alerts_panel(self, alerts: List[str]) -> Panel:
        if not alerts:
            text = Text("✓  No active alerts", style="green")
        else:
            text = Text()
            for alert in alerts:
                color = "red" if "SPLIT-BRAIN" in alert or "Quorum" in alert else "yellow"
                text.append(f"⚠  {alert}\n", style=color)

        return Panel(text, title="Alerts", border_style="red" if alerts else "blue", padding=(0, 1))

    # ── Throughput ────────────────────────────────────────────────────────────

    def _commit_rate(self) -> Optional[float]:
        """Returns entries/second over the trailing window, or None if insufficient data."""
        if len(self._commit_history) < 2:
            return None

        now = time.monotonic()
        cutoff = now - _THROUGHPUT_WINDOW
        window = [(t, c) for t, c in self._commit_history if t >= cutoff]

        if len(window) < 2:
            return None

        dt = window[-1][0] - window[0][0]
        dc = window[-1][1] - window[0][1]
        if dt <= 0:
            return None
        return max(0.0, dc / dt)


# ── Helpers ───────────────────────────────────────────────────────────────────

def _format_state(state: str) -> tuple[str, str]:
    """Returns (display_text, rich_style) for a Raft role string."""
    return {
        "leader": ("★ LEADER", "bold green"),
        "candidate": ("◆ candidate", "bold yellow"),
        "follower": ("  follower", ""),
        "unreachable": ("  unreachable", "dim red"),
    }.get(state, (state, "dim"))
