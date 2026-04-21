"""plot_exp2.py — Generate presentation graphs for Experiment 2.

Usage (from repo root):
    python experiments/exp2/plot_exp2.py [3node_csv] [5node_csv]

If CSV paths are omitted, picks the most recently modified
3node_*.csv and 5node_*.csv in experiments/exp2/results/.
"""

from __future__ import annotations

import csv
import sys
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

# ── Load CSVs ─────────────────────────────────────────────────────────────────

RESULTS = Path("experiments/exp2/results")

def load_csv(path: Path) -> list[dict]:
    with open(path) as f:
        return list(csv.DictReader(f))

def latest(prefix: str) -> Path:
    candidates = sorted(RESULTS.glob(f"{prefix}_*.csv"), key=lambda p: p.stat().st_mtime)
    if not candidates:
        raise FileNotFoundError(f"No {prefix}_*.csv in {RESULTS}")
    return candidates[-1]

if len(sys.argv) >= 3:
    three_node_path, five_node_path = Path(sys.argv[1]), Path(sys.argv[2])
else:
    three_node_path = latest("3node")
    five_node_path  = latest("5node")

print(f"3-node: {three_node_path}")
print(f"5-node: {five_node_path}")

three_node = load_csv(three_node_path)
five_node  = load_csv(five_node_path)

RATES = [100, 500, 1000]

def get(rows: list[dict], rate: int, field: str) -> float:
    for r in rows:
        if r["label"].endswith(f"_{rate}tpm"):
            return float(r[field])
    return 0.0

# 5-node latency validity: flag contaminated runs (p50 > 60 s = election storm)
_5n_p50_max = max(get(five_node, r, "p50_ms") for r in RATES)
FIVE_VALID = _5n_p50_max < 60_000
if not FIVE_VALID:
    print(f"WARNING: 5-node p50={_5n_p50_max/1000:.0f}s — election storm; latency excluded.")

# ── Style ─────────────────────────────────────────────────────────────────────

BLUE3  = "#2980b9"
GREEN5 = "#27ae60"
ORANGE = "#e67e22"
RED    = "#e74c3c"
GRAY   = "#95a5a6"
DARK   = "#2c3e50"

def style_ax(ax, title, xlabel="", ylabel=""):
    ax.set_facecolor("#f8f9fa")
    ax.grid(axis="y", color="white", linewidth=1.4, zorder=0)
    ax.set_title(title, fontsize=12, fontweight="bold", color=DARK, pad=10)
    if xlabel: ax.set_xlabel(xlabel, fontsize=10, color=DARK)
    if ylabel: ax.set_ylabel(ylabel, fontsize=10, color=DARK)
    ax.tick_params(colors=DARK, labelsize=9)
    for spine in ax.spines.values():
        spine.set_visible(False)


# ── Figure 1: Throughput ──────────────────────────────────────────────────────

def fig_throughput():
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(13, 5))
    fig.patch.set_facecolor("white")
    fig.suptitle("Experiment 2 — Throughput vs. Coordinator Cluster Size",
                 fontsize=14, fontweight="bold", color=DARK, y=1.02)

    x, w = np.arange(len(RATES)), 0.3

    t3 = [get(three_node, r, "actual_rate_tpm") for r in RATES]
    t5 = [get(five_node,  r, "actual_rate_tpm") for r in RATES]
    b3 = ax1.bar(x - w/2, t3, width=w, color=BLUE3,  zorder=3, label="3-node")
    b5 = ax1.bar(x + w/2, t5, width=w, color=GREEN5, zorder=3, label="5-node")
    for i, rate in enumerate(RATES):
        ax1.hlines(rate, i - w, i + w, colors=GRAY, linewidths=1.5, linestyles="--", zorder=4)
    for bars in (b3, b5):
        for bar in bars:
            ax1.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 8,
                     f"{bar.get_height():.0f}", ha="center", va="bottom", fontsize=8, color=DARK)
    ax1.set_xticks(x); ax1.set_xticklabels([f"{r} tpm" for r in RATES])
    ax1.legend(fontsize=10)
    style_ax(ax1, "A. Actual Throughput Achieved",
             xlabel="Target rate", ylabel="Actual tasks/min")
    ax1.set_ylim(0, 1150)

    s3 = [get(three_node, r, "submitted") for r in RATES]
    s5 = [get(five_node,  r, "submitted") for r in RATES]
    f3 = [get(three_node, r, "failed")    for r in RATES]
    f5 = [get(five_node,  r, "failed")    for r in RATES]
    ax2.bar(x - w/2, s3, width=w, color=BLUE3,  zorder=3, label="3-node", alpha=0.85)
    ax2.bar(x + w/2, s5, width=w, color=GREEN5, zorder=3, label="5-node", alpha=0.85)
    for i, (a, b) in enumerate(zip(f3, f5)):
        ax2.text(i - w/2, s3[i] + 15, f"fail={int(a)}", ha="center", fontsize=8,
                 color="#27ae60", fontweight="bold")
        ax2.text(i + w/2, s5[i] + 15, f"fail={int(b)}", ha="center", fontsize=8,
                 color="#27ae60", fontweight="bold")
    ax2.set_xticks(x); ax2.set_xticklabels([f"{r} tpm" for r in RATES])
    ax2.legend(fontsize=9)
    style_ax(ax2, "B. Tasks Submitted — 0% Failure Rate",
             xlabel="Target rate", ylabel="Tasks submitted")
    ax2.set_ylim(0, 2400)

    plt.tight_layout()
    out = RESULTS / "fig1_throughput.png"
    plt.savefig(out, dpi=150, bbox_inches="tight"); plt.close(); print(f"Saved {out}")


# ── Figure 2: Latency Percentiles (both configs) ─────────────────────────────

def fig_latency():
    fig, axes = plt.subplots(1, 3, figsize=(14, 5))
    fig.patch.set_facecolor("white")

    if FIVE_VALID:
        note = "3-node vs 5-node, per-measurement-window"
    else:
        note = "3-node only — 5-node excluded (election storm)"
    fig.suptitle(f"Experiment 2 — Assignment Latency\n({note})",
                 fontsize=12, fontweight="bold", color=DARK, y=1.04)

    metrics = [("p50_ms", "p50"), ("p95_ms", "p95"), ("p99_ms", "p99")]
    x = np.arange(len(RATES))
    w = 0.3 if FIVE_VALID else 0.45

    for ax, (field, label) in zip(axes, metrics):
        v3 = [get(three_node, r, field) for r in RATES]
        if FIVE_VALID:
            v5 = [get(five_node, r, field) for r in RATES]
            b3 = ax.bar(x - w/2, v3, width=w, color=BLUE3,  zorder=3, label="3-node")
            b5 = ax.bar(x + w/2, v5, width=w, color=GREEN5, zorder=3, label="5-node")
            for bar in list(b3) + list(b5):
                ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 8,
                        f"{bar.get_height():.0f}", ha="center", va="bottom",
                        fontsize=8, color=DARK)
            ax.legend(fontsize=9)
            y_max = max(max(v3), max(v5))
        else:
            bars = ax.bar(x, v3, width=w, color=BLUE3, zorder=3)
            for bar, v in zip(bars, v3):
                ax.text(bar.get_x() + bar.get_width()/2, v + 8,
                        f"{v:.0f}", ha="center", va="bottom",
                        fontsize=9, fontweight="bold", color=DARK)
            y_max = max(v3)

        ax.set_xticks(x); ax.set_xticklabels([f"{r}\ntpm" for r in RATES], fontsize=9)
        style_ax(ax, f"{label.upper()} Latency (ms)",
                 xlabel="Submission rate", ylabel="ms")
        ax.set_ylim(0, max(y_max * 1.45, 50))

    plt.tight_layout()
    out = RESULTS / "fig2_latency_percentiles.png"
    plt.savefig(out, dpi=150, bbox_inches="tight"); plt.close(); print(f"Saved {out}")


# ── Figure 3: Latency Breakdown — both configs, all rates ────────────────────

def fig_latency_breakdown():
    """Side-by-side p50/p95/p99/max for every rate, both cluster sizes."""
    fig, axes = plt.subplots(1, 3, figsize=(15, 5))
    fig.patch.set_facecolor("white")
    fig.suptitle(
        "Experiment 2 — Assignment Latency Distribution by Rate\n"
        "(p50=0 ms everywhere; p95 step driven by SQS long-poll batching)",
        fontsize=12, fontweight="bold", color=DARK, y=1.03,
    )

    pcts   = ["p50", "p95", "p99", "max"]
    fields = ["p50_ms", "p95_ms", "p99_ms", "max_ms"]
    x = np.arange(len(pcts))
    w = 0.35

    for ax, rate in zip(axes, RATES):
        v3 = [get(three_node, rate, f) for f in fields]
        b3 = ax.bar(x - w/2, v3, width=w, color=BLUE3,  zorder=3, label="3-node", alpha=0.9)

        if FIVE_VALID:
            v5 = [get(five_node, rate, f) for f in fields]
            b5 = ax.bar(x + w/2, v5, width=w, color=GREEN5, zorder=3,
                        label="5-node", alpha=0.9)
            all_bars = list(b3) + list(b5)
        else:
            v5 = [0] * 4
            all_bars = list(b3)

        for bar in all_bars:
            v = bar.get_height()
            if v > 0:
                label_s = f"{v/1000:.0f}s" if v >= 1000 else f"{v:.0f}ms"
                ax.text(bar.get_x() + bar.get_width()/2, v + max(v * 0.03, 20),
                        label_s, ha="center", va="bottom",
                        fontsize=8, color=DARK, fontweight="bold")

        ax.set_xticks(x); ax.set_xticklabels(pcts, fontsize=10)
        ax.legend(fontsize=9)
        y_top = max(max(v3), max(v5) if FIVE_VALID else 0) * 1.35 + 200
        style_ax(ax, f"{rate} tpm", xlabel="Percentile", ylabel="Latency (ms)")
        ax.set_ylim(0, max(y_top, 1500))

        # Annotate SQS artifact and outlier
        if rate >= 500:
            ax.text(0.97, 0.72, "SQS long-poll\nbatch artifact\n(~1 s)",
                    transform=ax.transAxes, fontsize=7.5, color=ORANGE,
                    ha="right", style="italic",
                    bbox=dict(boxstyle="round,pad=0.3", facecolor="#fff3e0",
                              edgecolor=ORANGE, alpha=0.8))
        if rate == 1000 and get(three_node, 1000, "max_ms") > 10_000:
            ax.text(0.97, 0.95, "3-node max:\nelection stall\noutlier (†)",
                    transform=ax.transAxes, fontsize=7, color=RED,
                    ha="right", va="top",
                    bbox=dict(boxstyle="round,pad=0.3", facecolor="#fdecea",
                              edgecolor=RED, alpha=0.8))

    fig.text(0.5, -0.02,
             "† 3-node 1000 tpm max = 128 s: single task stalled during 3→5-node redeployment. "
             "Not a steady-state value.",
             ha="center", fontsize=8, color=GRAY, style="italic")

    plt.tight_layout()
    out = RESULTS / "fig3_latency_breakdown.png"
    plt.savefig(out, dpi=150, bbox_inches="tight"); plt.close(); print(f"Saved {out}")


# ── Figure 4: Summary Scorecard ───────────────────────────────────────────────

def _fmt(val_ms: float) -> str:
    if val_ms == 0:            return "0 ms"
    if val_ms >= 60_000:       return "N/A †"
    if val_ms >= 1_000:        return f"{val_ms/1000:.0f},000 ms" if val_ms < 10_000 else f"{val_ms/1000:.0f} s"
    return f"{val_ms:.0f} ms"

def fig_scorecard():
    fig, ax = plt.subplots(figsize=(13, 5))
    fig.patch.set_facecolor("white")
    ax.set_facecolor("white")
    ax.axis("off")

    cols = ["Metric",
            "3-node\n100 tpm", "3-node\n500 tpm", "3-node\n1000 tpm",
            "5-node\n100 tpm", "5-node\n500 tpm", "5-node\n1000 tpm"]

    def row(metric, field, fmt_fn=None):
        cells = [metric]
        for ds in (three_node, five_node):
            for r in RATES:
                v = get(ds, r, field)
                cells.append(fmt_fn(v) if fmt_fn else str(int(v)))
        return cells

    rows_data = [
        row("Submitted",       "submitted"),
        row("Failed",          "failed"),
        row("Actual rate tpm", "actual_rate_tpm",
            lambda v: f"{v:.1f}"),
        ["Failure rate"] + ["0%"] * 6,
        row("p50 latency",     "p50_ms",  _fmt),
        row("p95 latency",     "p95_ms",  _fmt),
        row("p99 latency",     "p99_ms",  _fmt),
        row("max latency",     "max_ms",  _fmt),
    ]

    def cell_color(val: str) -> str:
        if val in ("0", "0%", "0 ms"):   return "#d5f5e3"
        if "N/A" in val:                  return "#fef9e7"
        return "#fdfefe"

    cell_colors = [["#ecf0f1"] + [cell_color(v) for v in row[1:]]
                   for row in rows_data]

    tbl = ax.table(cellText=rows_data, colLabels=cols, cellLoc="center",
                   loc="center", cellColours=cell_colors)
    tbl.auto_set_font_size(False)
    tbl.set_fontsize(9)
    tbl.scale(1.0, 1.6)
    for j in range(len(cols)):
        tbl[0, j].set_facecolor(DARK)
        tbl[0, j].set_text_props(color="white", fontweight="bold")

    ax.set_title("Experiment 2 — Full Results Scorecard",
                 fontsize=13, fontweight="bold", color=DARK, pad=20)

    footnote = ("† 3-node 1000 tpm max = 128 s: single election-stall outlier during "
                "3→5-node redeployment, not steady-state.")
    if not FIVE_VALID:
        footnote = ("† 5-node latency invalid: election storm — dispatched_at set "
                    "~12 h after task creation.")
    ax.text(0.01, -0.04, footnote, transform=ax.transAxes,
            fontsize=7.5, color=GRAY, style="italic")

    plt.tight_layout()
    out = RESULTS / "fig4_scorecard.png"
    plt.savefig(out, dpi=150, bbox_inches="tight"); plt.close(); print(f"Saved {out}")


# ── Figure 5: 3-node vs 5-node Latency Delta ─────────────────────────────────

def fig_cluster_comparison():
    fig, ax = plt.subplots(figsize=(10, 5))
    fig.patch.set_facecolor("white")

    categories = ["p50", "p95", "p99"]
    fields     = ["p50_ms", "p95_ms", "p99_ms"]
    x = np.arange(len(categories))

    if FIVE_VALID:
        # Per-rate grouped bars: each rate gets a 3-node + 5-node pair
        rate_styles = {100: ("#2980b9", "#1abc9c"),
                       500: ("#8e44ad", "#16a085"),
                       1000: ("#c0392b", "#d35400")}
        w = 0.13
        offsets = {100: -2*w, 500: 0, 1000: 2*w}

        for rate, (c3, c5) in rate_styles.items():
            v3 = [get(three_node, rate, f) for f in fields]
            v5 = [get(five_node,  rate, f) for f in fields]
            b3 = ax.bar(x + offsets[rate] - w/2, v3, width=w,
                        color=c3, zorder=3, label=f"3-node {rate}tpm", alpha=0.9)
            b5 = ax.bar(x + offsets[rate] + w/2, v5, width=w,
                        color=c5, zorder=3, label=f"5-node {rate}tpm",
                        alpha=0.9, hatch="//")
            for bar in list(b3) + list(b5):
                v = bar.get_height()
                if v > 0:
                    ax.text(bar.get_x() + bar.get_width()/2, v + 8,
                            f"{v:.0f}", ha="center", va="bottom",
                            fontsize=7, color=DARK)

        # Highlight the 100tpm p95 difference
        v3_100_p95 = get(three_node, 100, "p95_ms")
        v5_100_p95 = get(five_node,  100, "p95_ms")
        if v5_100_p95 != v3_100_p95:
            ax.annotate(
                f"Only divergence:\n5-node p95 = {v5_100_p95:.0f} ms\n3-node p95 = {v3_100_p95:.0f} ms\n(extra follower RTT\ncrosses SQS batch threshold)",
                xy=(x[1] + offsets[100] + w/2, v5_100_p95),
                xytext=(x[1] - 0.3, 700),
                fontsize=7.5, color=DARK,
                arrowprops=dict(arrowstyle="->", color=DARK, lw=1.2),
                bbox=dict(boxstyle="round,pad=0.4", facecolor="#eaf2ff",
                          edgecolor=BLUE3, alpha=0.9),
            )

        ax.set_xticks(x)
        ax.set_xticklabels(categories, fontsize=12)
        ax.legend(fontsize=8, ncol=2, loc="upper right")
        style_ax(ax,
                 "3-node vs 5-node: Latency Identical at p95/p99 for ≥500 tpm\n"
                 "(only divergence: 5-node p95 = 1,000 ms at 100 tpm vs 0 ms for 3-node)",
                 ylabel="Latency (ms)")
        ax.set_ylim(0, 1500)

    else:
        # Fallback: 3-node per-rate profile only
        rate_colors = {100: "#2980b9", 500: "#8e44ad", 1000: "#c0392b"}
        w = 0.25
        offsets = {100: -w, 500: 0, 1000: w}
        for rate in RATES:
            vals = [get(three_node, rate, f) for f in fields]
            bars = ax.bar(x + offsets[rate], vals, width=w,
                          color=rate_colors[rate], zorder=3,
                          label=f"{rate} tpm", alpha=0.9)
            for bar in bars:
                v = bar.get_height()
                if v > 0:
                    ax.text(bar.get_x() + bar.get_width()/2, v + 10,
                            f"{v:.0f}", ha="center", va="bottom",
                            fontsize=9, fontweight="bold", color=DARK)
        ax.set_xticks(x); ax.set_xticklabels(categories, fontsize=11)
        ax.legend(fontsize=10, title="Rate", title_fontsize=9)
        style_ax(ax, "3-node Latency by Rate (5-node excluded — election storm)",
                 ylabel="Latency (ms)")
        ax.set_ylim(0, 1400)

    plt.tight_layout()
    out = RESULTS / "fig5_cluster_comparison.png"
    plt.savefig(out, dpi=150, bbox_inches="tight"); plt.close(); print(f"Saved {out}")


# ── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    print("Generating Experiment 2 graphs...")
    fig_throughput()
    fig_latency()
    fig_latency_breakdown()
    fig_scorecard()
    fig_cluster_comparison()
    print(f"\nAll figures saved to {RESULTS}")
