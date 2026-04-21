"""plot_exp2.py — Generate presentation graphs for Experiment 2.

Usage (from repo root):
    python experiments/exp2/plot_exp2.py [3node_csv] [5node_csv]

If CSV paths are omitted, the script picks the most recently modified
3node_*.csv and 5node_*.csv in experiments/exp2/results/.

Outputs PNG files into experiments/exp2/results/.
"""

from __future__ import annotations

import csv
import sys
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import numpy as np

# ── Load CSVs ─────────────────────────────────────────────────────────────────

RESULTS = Path("experiments/exp2/results")

def load_csv(path: Path) -> list[dict]:
    with open(path) as f:
        return list(csv.DictReader(f))

def latest(prefix: str) -> Path:
    candidates = sorted(RESULTS.glob(f"{prefix}_*.csv"), key=lambda p: p.stat().st_mtime)
    if not candidates:
        raise FileNotFoundError(f"No {prefix}_*.csv found in {RESULTS}")
    return candidates[-1]

if len(sys.argv) >= 3:
    three_node_path = Path(sys.argv[1])
    five_node_path  = Path(sys.argv[2])
else:
    three_node_path = latest("3node")
    five_node_path  = latest("5node")

print(f"3-node data: {three_node_path}")
print(f"5-node data: {five_node_path}")

three_node = load_csv(three_node_path)
five_node  = load_csv(five_node_path)

# 5-node latency validity check: flag contaminated runs (p50 > 60000 ms = 1 min)
_5n_p50 = max((float(r.get("p50_ms", 0)) for r in five_node), default=0)
FIVE_NODE_LATENCY_VALID = _5n_p50 < 60_000
if not FIVE_NODE_LATENCY_VALID:
    print(f"WARNING: 5-node p50={_5n_p50/1000:.0f}s — election storm contamination detected. "
          "5-node latency will be excluded from figures.")

RATES = [100, 500, 1000]

def get(rows: list[dict], rate: int, field: str) -> float:
    suffix = f"_{rate}tpm"
    for r in rows:
        if r["label"].endswith(suffix):
            return float(r[field])
    return 0.0

# ── Style ─────────────────────────────────────────────────────────────────────

BLUE3   = "#2980b9"
GREEN5  = "#27ae60"
ORANGE  = "#e67e22"
RED     = "#e74c3c"
GRAY    = "#95a5a6"
DARK    = "#2c3e50"
LIGHT   = "#ecf0f1"

def style_ax(ax, title, xlabel="", ylabel=""):
    ax.set_facecolor("#f8f9fa")
    ax.grid(axis="y", color="white", linewidth=1.4, zorder=0)
    ax.set_title(title, fontsize=12, fontweight="bold", color=DARK, pad=10)
    if xlabel: ax.set_xlabel(xlabel, fontsize=10, color=DARK)
    if ylabel: ax.set_ylabel(ylabel, fontsize=10, color=DARK)
    ax.tick_params(colors=DARK, labelsize=9)
    for spine in ax.spines.values():
        spine.set_visible(False)


# ── Figure 1: Throughput & Failure Rate ──────────────────────────────────────

def fig_throughput():
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(13, 5))
    fig.patch.set_facecolor("white")
    fig.suptitle("Experiment 2 — Throughput vs. Coordinator Cluster Size",
                 fontsize=14, fontweight="bold", color=DARK, y=1.02)

    x = np.arange(len(RATES))
    w = 0.3

    targets_3 = [get(three_node, r, "actual_rate_tpm") for r in RATES]
    targets_5 = [get(five_node,  r, "actual_rate_tpm") for r in RATES]

    b3 = ax1.bar(x - w/2, targets_3, width=w, color=BLUE3,  zorder=3, label="3-node")
    b5 = ax1.bar(x + w/2, targets_5, width=w, color=GREEN5, zorder=3, label="5-node")

    for i, rate in enumerate(RATES):
        ax1.hlines(rate, i - w, i + w, colors=GRAY, linewidths=1.5,
                   linestyles="--", zorder=4)

    for bars in (b3, b5):
        for bar in bars:
            ax1.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 8,
                     f"{bar.get_height():.0f}", ha="center", va="bottom",
                     fontsize=8, color=DARK)

    ax1.set_xticks(x)
    ax1.set_xticklabels([f"{r} tpm" for r in RATES])
    ax1.legend(fontsize=10)
    style_ax(ax1, "A. Actual Throughput Achieved",
             xlabel="Target submission rate", ylabel="Actual tasks/min")
    ax1.set_ylim(0, 1150)

    sub_3 = [get(three_node, r, "submitted") for r in RATES]
    sub_5 = [get(five_node,  r, "submitted") for r in RATES]
    fail_3 = [get(three_node, r, "failed") for r in RATES]
    fail_5 = [get(five_node,  r, "failed") for r in RATES]

    ax2.bar(x - w/2, sub_3, width=w, color=BLUE3,  zorder=3, label="3-node submitted", alpha=0.85)
    ax2.bar(x + w/2, sub_5, width=w, color=GREEN5, zorder=3, label="5-node submitted", alpha=0.85)

    for i, (f3, f5) in enumerate(zip(fail_3, fail_5)):
        ax2.text(i - w/2, sub_3[i] + 15, f"fail={int(f3)}", ha="center",
                 fontsize=8, color="#27ae60", fontweight="bold")
        ax2.text(i + w/2, sub_5[i] + 15, f"fail={int(f5)}", ha="center",
                 fontsize=8, color="#27ae60", fontweight="bold")

    ax2.set_xticks(x)
    ax2.set_xticklabels([f"{r} tpm" for r in RATES])
    ax2.legend(fontsize=9)
    style_ax(ax2, "B. Tasks Submitted — 0% Failure Rate",
             xlabel="Target submission rate", ylabel="Tasks submitted")
    ax2.set_ylim(0, 2400)

    plt.tight_layout()
    out = RESULTS / "fig1_throughput.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 2: 3-node Per-Rate Assignment Latency ─────────────────────────────

def fig_latency():
    fig, axes = plt.subplots(1, 3, figsize=(14, 5))
    fig.patch.set_facecolor("white")

    note = ("3-node vs 5-node" if FIVE_NODE_LATENCY_VALID
            else "3-node only (5-node excluded — election storm contamination)")
    fig.suptitle(
        f"Experiment 2 — Assignment Latency per Submission Rate\n({note})",
        fontsize=11, fontweight="bold", color=DARK, y=1.04,
    )

    metrics = [("p50_ms", "p50"), ("p95_ms", "p95"), ("p99_ms", "p99")]
    x = np.arange(len(RATES))
    w = 0.3 if FIVE_NODE_LATENCY_VALID else 0.45

    for ax, (field, label) in zip(axes, metrics):
        vals_3 = [get(three_node, r, field) for r in RATES]
        if FIVE_NODE_LATENCY_VALID:
            vals_5 = [get(five_node, r, field) for r in RATES]
            b3 = ax.bar(x - w/2, vals_3, width=w, color=BLUE3,  zorder=3, label="3-node")
            b5 = ax.bar(x + w/2, vals_5, width=w, color=GREEN5, zorder=3, label="5-node")
            for bar in list(b3) + list(b5):
                v = bar.get_height()
                ax.text(bar.get_x() + bar.get_width()/2, v + 5,
                        f"{v:.0f}ms", ha="center", va="bottom", fontsize=8, color=DARK)
            ax.legend(fontsize=9)
            y_max = max(max(vals_3), max(vals_5))
        else:
            bars = ax.bar(x, vals_3, width=w, color=BLUE3, zorder=3)
            for bar, v in zip(bars, vals_3):
                ax.text(bar.get_x() + bar.get_width()/2, v + 5,
                        f"{v:.0f}ms", ha="center", va="bottom", fontsize=9,
                        color=DARK, fontweight="bold")
            y_max = max(vals_3)

        ax.set_xticks(x)
        ax.set_xticklabels([f"{r}\ntpm" for r in RATES], fontsize=9)
        style_ax(ax, f"{label.upper()} Assignment Latency",
                 xlabel="Submission rate", ylabel="Latency (ms)")
        ax.set_ylim(0, max(y_max * 1.4, 50))

    plt.tight_layout()
    out = RESULTS / "fig2_latency_percentiles.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 3: Latency Breakdown (per rate) ────────────────────────────────────

def fig_latency_breakdown():
    fig, ax = plt.subplots(figsize=(10, 5))
    fig.patch.set_facecolor("white")

    pcts   = ["p50", "p95", "p99", "max"]
    fields = ["p50_ms", "p95_ms", "p99_ms", "max_ms"]

    rate_colors = {100: "#2980b9", 500: "#8e44ad", 1000: "#c0392b"}
    x = np.arange(len(pcts))
    w = 0.25

    offsets = {100: -w, 500: 0, 1000: w}
    for rate in RATES:
        vals = [get(three_node, rate, f) / 1000 for f in fields]
        bars = ax.bar(x + offsets[rate], vals, width=w,
                      color=rate_colors[rate], zorder=3,
                      label=f"3-node {rate} tpm", alpha=0.9)
        for bar in bars:
            v = bar.get_height()
            if v > 0:
                ax.text(bar.get_x() + bar.get_width()/2, v + 0.5,
                        f"{v:.0f}s", ha="center", va="bottom",
                        fontsize=8, color=DARK, fontweight="bold")

    ax.annotate("SQS long-poll\nwake-up artifact\n(appears at ≥500 tpm)",
                xy=(1 + offsets[500], 1.0), xytext=(1.9, 30),
                fontsize=8, color=ORANGE,
                arrowprops=dict(arrowstyle="->", color=ORANGE, lw=1.5))

    ax.annotate("Raft election stall\nduring 3→5 node deploy\n(1 outlier task, ~128 s)",
                xy=(3 + offsets[1000], 128), xytext=(2.0, 100),
                fontsize=8, color=RED,
                arrowprops=dict(arrowstyle="->", color=RED, lw=1.5))

    ax.set_xticks(x)
    ax.set_xticklabels(pcts, fontsize=11)
    ax.legend(fontsize=9, loc="upper left")
    style_ax(ax,
             "3-node Assignment Latency by Rate — per-window measurement\n"
             "(p50 = 0 at all rates; p95 jumps from 0→1000 ms at 500 tpm)",
             xlabel="Percentile", ylabel="Latency (s)")
    ax.set_ylim(0, 160)

    plt.tight_layout()
    out = RESULTS / "fig3_latency_breakdown.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 4: Summary Scorecard ───────────────────────────────────────────────

def fig_scorecard():
    fig, ax = plt.subplots(figsize=(12, 5))
    fig.patch.set_facecolor("white")
    ax.set_facecolor("white")
    ax.axis("off")

    columns = ["Metric",
               "3-node\n100 tpm", "3-node\n500 tpm", "3-node\n1000 tpm",
               "5-node\n100 tpm", "5-node\n500 tpm", "5-node\n1000 tpm"]

    rows_data = [
        ["Submitted",       "199",   "998",    "1,996", "199",   "998",    "1,996"],
        ["Failed",          "0",     "0",      "0",     "0",     "0",      "0"    ],
        ["Actual rate tpm", "99.5",  "499",    "998",   "99.5",  "499",    "998"  ],
        ["Failure rate",    "0%",    "0%",     "0%",    "0%",    "0%",     "0%"   ],
        ["p50 latency",     "0 ms",  "0 ms",   "0 ms",  "N/A †", "N/A †",  "N/A †"],
        ["p95 latency",     "0 ms",  "1,000 ms","1,000 ms","N/A †","N/A †","N/A †"],
        ["p99 latency",     "0 ms",  "1,000 ms","1,000 ms","N/A †","N/A †","N/A †"],
        ["max latency",     "0 ms",  "1,000 ms","128,000 ms","N/A †","N/A †","N/A †"],
    ]

    cell_colors = []
    for row in rows_data:
        row_colors = ["#ecf0f1"]
        for val in row[1:]:
            if val in ("0", "0%", "0 ms"):
                row_colors.append("#d5f5e3")
            elif "N/A" in val:
                row_colors.append("#fef9e7")
            else:
                row_colors.append("#fdfefe")
        cell_colors.append(row_colors)

    tbl = ax.table(
        cellText=rows_data,
        colLabels=columns,
        cellLoc="center",
        loc="center",
        cellColours=cell_colors,
    )
    tbl.auto_set_font_size(False)
    tbl.set_fontsize(9)
    tbl.scale(1.0, 1.6)

    for j in range(len(columns)):
        cell = tbl[0, j]
        cell.set_facecolor(DARK)
        cell.set_text_props(color="white", fontweight="bold")

    ax.set_title("Experiment 2 — Full Results Scorecard",
                 fontsize=13, fontweight="bold", color=DARK, pad=20)
    ax.text(0.01, -0.04,
            "† 5-node latency invalid: election storm during the 5-node run prevented Raft commits; "
            "dispatched_at was set ~12 h later when the cluster stabilised for Exp 3.",
            transform=ax.transAxes, fontsize=7.5, color=GRAY, style="italic")

    plt.tight_layout()
    out = RESULTS / "fig4_scorecard.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 5: Rate-dependent latency (3-node) ────────────────────────────────

def fig_cluster_comparison():
    fig, ax = plt.subplots(figsize=(9, 5))
    fig.patch.set_facecolor("white")

    categories = ["p50\n(ms)", "p95\n(ms)", "p99\n(ms)"]
    fields     = ["p50_ms",    "p95_ms",    "p99_ms"]

    if FIVE_NODE_LATENCY_VALID:
        # Side-by-side 3-node vs 5-node per rate (averaged across rates)
        v3 = [np.mean([get(three_node, r, f) for r in RATES]) for f in fields]
        v5 = [np.mean([get(five_node,  r, f) for r in RATES]) for f in fields]
        x = np.arange(len(categories))
        w = 0.3
        b3 = ax.bar(x - w/2, v3, width=w, color=BLUE3,  zorder=3, label="3-node")
        b5 = ax.bar(x + w/2, v5, width=w, color=GREEN5, zorder=3, label="5-node")
        for bar in list(b3) + list(b5):
            ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 10,
                    f"{bar.get_height():.0f}", ha="center", va="bottom",
                    fontsize=10, fontweight="bold", color=DARK)
        for i, (a, b) in enumerate(zip(v3, v5)):
            delta = abs(a - b)
            ax.text(i, max(a, b) + 60, f"Δ={delta:.0f}ms",
                    ha="center", fontsize=9, color=GRAY, style="italic")
        ax.set_xticks(x)
        ax.set_xticklabels(categories, fontsize=11)
        ax.legend(fontsize=11)
        style_ax(ax, "3-node vs 5-node: Latency Comparison (averaged across rates)",
                 ylabel="Latency (ms)")
        ax.set_ylim(0, 1600)
    else:
        # Fall back: show 3-node rate-dependent profile
        rate_colors = {100: "#2980b9", 500: "#8e44ad", 1000: "#c0392b"}
        x = np.arange(len(categories))
        w = 0.25
        offsets = {100: -w, 500: 0, 1000: w}
        for rate in RATES:
            vals = [get(three_node, rate, f) for f in fields]
            bars = ax.bar(x + offsets[rate], vals, width=w,
                          color=rate_colors[rate], zorder=3,
                          label=f"{rate} tpm", alpha=0.9)
            for bar in bars:
                ax.text(bar.get_x() + bar.get_width()/2,
                        bar.get_height() + 10,
                        f"{bar.get_height():.0f}", ha="center", va="bottom",
                        fontsize=9, fontweight="bold", color=DARK)
        ax.set_xticks(x)
        ax.set_xticklabels(categories, fontsize=11)
        ax.legend(fontsize=10, title="Submission rate", title_fontsize=9)
        style_ax(ax,
                 "3-node: Latency by Rate (5-node excluded — election storm contamination)",
                 ylabel="Latency (ms)")
        ax.set_ylim(0, 1400)
        ax.text(0.98, 0.96,
                "p50 = 0 ms at all rates.\np95 jumps 0→1000 ms\nbetween 100 and 500 tpm\n(SQS long-poll batch fill).",
                transform=ax.transAxes, fontsize=8.5, color=DARK,
                verticalalignment="top", horizontalalignment="right",
                bbox=dict(boxstyle="round,pad=0.5", facecolor="#eaf2ff",
                          edgecolor=BLUE3, alpha=0.9))

    plt.tight_layout()
    out = RESULTS / "fig5_cluster_comparison.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    print("Generating Experiment 2 graphs (windowed data)...")
    fig_throughput()
    fig_latency()
    fig_latency_breakdown()
    fig_scorecard()
    fig_cluster_comparison()
    print(f"\nAll figures saved to {RESULTS}")
