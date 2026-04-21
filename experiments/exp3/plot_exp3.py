"""plot_exp3.py — Generate presentation graphs for Experiment 3.

Usage (from repo root):
    python experiments/exp3/plot_exp3.py \
        experiments/exp3/exp3-20260421T001513

Outputs four PNG files in the run directory.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import numpy as np

# ── Load data ─────────────────────────────────────────────────────────────────

def load(run_dir: Path):
    events = []
    with open(run_dir / "history.jsonl") as f:
        for line in f:
            line = line.strip()
            if line:
                events.append(json.loads(line))
    summary   = json.loads((run_dir / "exp3_summary.json").read_text())
    dups      = json.loads((run_dir / "exp3_dups.json").read_text())
    divergence= json.loads((run_dir / "exp3_divergence.json").read_text())
    lincheck  = json.loads((run_dir / "exp3_lincheck.json").read_text())
    return events, summary, dups, divergence, lincheck


# ── Style ─────────────────────────────────────────────────────────────────────

PASS_GREEN  = "#2ecc71"
FAIL_RED    = "#e74c3c"
BLUE        = "#3498db"
ORANGE      = "#e67e22"
PURPLE      = "#9b59b6"
GRAY        = "#95a5a6"
DARK        = "#2c3e50"

def style_ax(ax, title, xlabel="", ylabel=""):
    ax.set_facecolor("#f8f9fa")
    ax.grid(axis="y", color="white", linewidth=1.2, zorder=0)
    ax.set_title(title, fontsize=13, fontweight="bold", color=DARK, pad=10)
    if xlabel: ax.set_xlabel(xlabel, fontsize=10, color=DARK)
    if ylabel: ax.set_ylabel(ylabel, fontsize=10, color=DARK)
    ax.tick_params(colors=DARK)
    for spine in ax.spines.values():
        spine.set_visible(False)


# ── Figure 1: Safety & Liveness Summary Dashboard ────────────────────────────

def fig_summary_dashboard(summary, run_dir):
    fig, axes = plt.subplots(1, 3, figsize=(14, 5))
    fig.patch.set_facecolor("white")
    fig.suptitle("Experiment 3 — Exactly-Once Under Faults: Result Summary",
                 fontsize=15, fontweight="bold", color=DARK, y=1.02)

    # ── Panel A: Task outcomes ────────────────────────────────────────────────
    ax = axes[0]
    labels   = ["Submitted", "Completed", "Timed Out", "Lost"]
    values   = [summary["submitted"], summary["completed"],
                summary["timed_out"], summary["lost"]]
    colors   = [BLUE, PASS_GREEN,
                FAIL_RED if summary["timed_out"] > 0 else PASS_GREEN,
                FAIL_RED if summary["lost"] > 0 else PASS_GREEN]
    bars = ax.bar(labels, values, color=colors, width=0.5, zorder=3)
    for bar, val in zip(bars, values):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 2,
                str(val), ha="center", va="bottom", fontsize=11,
                fontweight="bold", color=DARK)
    style_ax(ax, "A. Task Outcomes", ylabel="Tasks")
    ax.set_ylim(0, summary["submitted"] * 1.15)

    # ── Panel B: Safety properties ────────────────────────────────────────────
    ax = axes[1]
    safety_labels = ["Duplicate\nDispatches", "Linearizability\nViolations",
                     "Stuck\nPENDING", "Stuck\nASSIGNED"]
    safety_vals   = [summary["duplicate_dispatches"],
                     summary["linearizability_violations"],
                     summary["stuck_pending"],
                     summary["stuck_assigned"]]
    s_colors = [PASS_GREEN if v == 0 else FAIL_RED for v in safety_vals]
    bars = ax.bar(safety_labels, safety_vals, color=s_colors, width=0.5, zorder=3)
    for bar, val in zip(bars, safety_vals):
        label = str(val) + (" ✓" if val == 0 else " ✗")
        ax.text(bar.get_x() + bar.get_width()/2,
                max(bar.get_height(), 0) + 0.05,
                label, ha="center", va="bottom", fontsize=12,
                fontweight="bold",
                color=PASS_GREEN if val == 0 else FAIL_RED)
    style_ax(ax, "B. Safety Properties", ylabel="Count (target: 0)")
    ax.set_ylim(0, max(max(safety_vals) * 1.4, 1))

    # ── Panel C: Fault injection ──────────────────────────────────────────────
    ax = axes[2]
    fault_labels = ["Leader\nKills", "Follower\nKills", "SQS\nRedeliveries"]
    fault_vals   = [summary["leader_kills"], summary["follower_kills"],
                    summary["sqs_redeliveries_injected"]]
    bars = ax.bar(fault_labels, fault_vals,
                  color=[ORANGE, PURPLE, BLUE], width=0.5, zorder=3)
    for bar, val in zip(bars, fault_vals):
        ax.text(bar.get_x() + bar.get_width()/2, bar.get_height() + 0.1,
                str(val), ha="center", va="bottom", fontsize=12,
                fontweight="bold", color=DARK)
    style_ax(ax, "C. Fault Injections", ylabel="Events")
    ax.set_ylim(0, max(fault_vals) * 1.4 + 1)

    plt.tight_layout()
    out = run_dir / "fig1_summary_dashboard.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 2: Task State Timeline ─────────────────────────────────────────────

def fig_task_state_timeline(events, run_dir):
    run_start_ns = next(e["wall_ns"] for e in events if e["op"] == "run_start")

    # Build per-task first-seen status over time
    task_status: dict[str, str] = {}
    timeline: list[tuple[float, int, int, int]] = []  # (t_s, pending, assigned, completed)

    # Collect all status-change observations sorted by wall time
    obs: list[tuple[int, str, str]] = []
    for e in events:
        if e["op"] == "submit":
            obs.append((e["wall_ns"], e["task_id"], "PENDING"))
        elif e["op"] == "poll_observe":
            obs.append((e["return_ns"], e["task_id"], e["status"]))
        elif e["op"] == "complete":
            obs.append((e["completed_ns"], e["task_id"], e["status"]))
    obs.sort()

    pending = assigned = completed = 0
    points: list[tuple[float, int, int, int]] = []
    for wall_ns, tid, status in obs:
        old = task_status.get(tid)
        if old == status:
            continue
        if old == "PENDING":     pending -= 1
        elif old == "ASSIGNED":  assigned -= 1
        elif old in ("COMPLETED","FAILED"): completed -= 1
        if status == "PENDING":   pending += 1
        elif status == "ASSIGNED": assigned += 1
        elif status in ("COMPLETED","FAILED"): completed += 1
        task_status[tid] = status
        t_s = (wall_ns - run_start_ns) / 1e9
        points.append((t_s, pending, assigned, completed))

    if not points:
        print("No timeline points, skipping fig2")
        return

    ts   = [p[0] for p in points]
    pend = [p[1] for p in points]
    asgn = [p[2] for p in points]
    comp = [p[3] for p in points]

    # Chaos event times
    chaos_times = {
        "leader_kill":    [(e["wall_ns"] - run_start_ns)/1e9 for e in events if e["op"] == "chaos.kill_leader"],
        "follower_kill":  [(e["wall_ns"] - run_start_ns)/1e9 for e in events if e["op"] == "chaos.kill_follower"],
        "sqs_redeliver":  [(e["wall_ns"] - run_start_ns)/1e9 for e in events if e["op"] == "chaos.sqs_redeliver"],
    }

    fig, ax = plt.subplots(figsize=(13, 5))
    fig.patch.set_facecolor("white")

    ax.fill_between(ts, pend, alpha=0.25, color=ORANGE, step="post")
    ax.fill_between(ts, asgn, alpha=0.25, color=BLUE,   step="post")
    ax.fill_between(ts, comp, alpha=0.25, color=PASS_GREEN, step="post")

    ax.step(ts, pend, color=ORANGE,     linewidth=1.8, label="PENDING",   where="post")
    ax.step(ts, asgn, color=BLUE,       linewidth=1.8, label="ASSIGNED",  where="post")
    ax.step(ts, comp, color=PASS_GREEN, linewidth=1.8, label="COMPLETED", where="post")

    y_max = max(max(pend, default=0), max(asgn, default=0), max(comp, default=0))

    for t in chaos_times["leader_kill"]:
        ax.axvline(t, color=FAIL_RED,  linewidth=2, linestyle="--", zorder=5)
        ax.text(t + 0.5, y_max * 0.92, "Leader\nKill", color=FAIL_RED,
                fontsize=8, fontweight="bold", ha="left")

    for t in chaos_times["follower_kill"]:
        ax.axvline(t, color=PURPLE, linewidth=2, linestyle="--", zorder=5)
        ax.text(t + 0.5, y_max * 0.75, "Follower\nKill", color=PURPLE,
                fontsize=8, fontweight="bold", ha="left")

    for i, t in enumerate(chaos_times["sqs_redeliver"]):
        ax.axvline(t, color=GRAY, linewidth=1, linestyle=":", alpha=0.7)
        if i == 0:
            ax.text(t, y_max * 0.55, "SQS\nRedeliver", color=GRAY,
                    fontsize=7, ha="left")

    style_ax(ax, "Figure 2 — Task State Progression Over Time",
             xlabel="Time since run start (s)", ylabel="Task count")
    ax.legend(loc="center right", fontsize=10, framealpha=0.9)
    ax.set_xlim(left=0)
    ax.set_ylim(bottom=0)

    plt.tight_layout()
    out = run_dir / "fig2_state_timeline.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 3: End-to-End Latency CDF ─────────────────────────────────────────

def fig_latency_cdf(events, run_dir):
    submits = {e["task_id"]: e["submitted_ns"] for e in events if e["op"] == "submit"}
    latencies_ms = []
    for e in events:
        if e["op"] == "complete" and e["task_id"] in submits:
            ms = (e["completed_ns"] - submits[e["task_id"]]) / 1e6
            latencies_ms.append(ms)
    if not latencies_ms:
        print("No completions for latency CDF, skipping fig3")
        return

    latencies_ms.sort()
    n = len(latencies_ms)
    cdf = np.arange(1, n + 1) / n

    p50  = latencies_ms[int(n * 0.50)]
    p90  = latencies_ms[int(n * 0.90)]
    p99  = latencies_ms[int(n * 0.99)]
    pmax = latencies_ms[-1]

    fig, ax = plt.subplots(figsize=(9, 5))
    fig.patch.set_facecolor("white")

    ax.plot(latencies_ms, cdf * 100, color=BLUE, linewidth=2.5)
    ax.fill_betweenx(cdf * 100, latencies_ms, alpha=0.12, color=BLUE)

    for pct, val, label in [(50, p50, "p50"), (90, p90, "p90"), (99, p99, "p99")]:
        ax.axvline(val, color=GRAY, linewidth=1, linestyle="--")
        ax.axhline(pct, color=GRAY, linewidth=1, linestyle="--")
        ax.scatter([val], [pct], color=DARK, zorder=5, s=40)
        ax.text(val + (pmax * 0.01), pct + 1.5,
                f"{label}: {val/1000:.1f}s", fontsize=9, color=DARK)

    style_ax(ax, "Figure 3 — End-to-End Task Completion Latency CDF",
             xlabel="Latency: submit → COMPLETED (ms)", ylabel="Percentile (%)")
    ax.set_ylim(0, 102)
    ax.set_xlim(left=0)

    # Annotation box
    stats_text = (f"n = {n}\n"
                  f"p50  = {p50/1000:.2f} s\n"
                  f"p90  = {p90/1000:.2f} s\n"
                  f"p99  = {p99/1000:.2f} s\n"
                  f"max = {pmax/1000:.2f} s")
    ax.text(0.97, 0.05, stats_text, transform=ax.transAxes,
            fontsize=9, verticalalignment="bottom", horizontalalignment="right",
            bbox=dict(boxstyle="round,pad=0.4", facecolor="white",
                      edgecolor=GRAY, alpha=0.9))

    plt.tight_layout()
    out = run_dir / "fig3_latency_cdf.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Figure 4: Chaos Event Impact & Exactly-Once Proof ────────────────────────

def fig_chaos_proof(events, summary, run_dir):
    run_start_ns = next(e["wall_ns"] for e in events if e["op"] == "run_start")

    # Submission rate over time (tasks submitted per 10s bucket)
    submit_times = [(e["submitted_ns"] - run_start_ns) / 1e9
                    for e in events if e["op"] == "submit"]
    completion_times = [(e["completed_ns"] - run_start_ns) / 1e9
                        for e in events if e["op"] == "complete"]

    chaos_events = [(e["op"], (e["wall_ns"] - run_start_ns) / 1e9)
                    for e in events
                    if e["op"] in ("chaos.kill_leader","chaos.kill_follower","chaos.sqs_redeliver")]

    max_t = max(max(submit_times, default=0), max(completion_times, default=0)) + 5
    bucket = 10
    bins = np.arange(0, max_t + bucket, bucket)

    sub_counts, _  = np.histogram(submit_times, bins=bins)
    comp_counts, _ = np.histogram(completion_times, bins=bins)
    bin_centers = (bins[:-1] + bins[1:]) / 2

    fig, (ax_top, ax_bot) = plt.subplots(2, 1, figsize=(13, 8),
                                          gridspec_kw={"height_ratios": [2, 1]})
    fig.patch.set_facecolor("white")

    # Top: throughput over time
    ax_top.bar(bin_centers, sub_counts,  width=bucket*0.45, align="center",
               color=BLUE,       alpha=0.8, label="Submitted",  zorder=3)
    ax_top.bar(bin_centers + bucket*0.45, comp_counts, width=bucket*0.45, align="center",
               color=PASS_GREEN, alpha=0.8, label="Completed",  zorder=3)

    y_max = max(sub_counts.max(), comp_counts.max()) if len(sub_counts) > 0 else 10

    for op, t in chaos_events:
        if op == "chaos.kill_leader":
            ax_top.axvline(t, color=FAIL_RED,  linewidth=2.5, linestyle="--", zorder=5)
            ax_top.text(t + 0.5, y_max * 0.88, "⚡ Leader\nKill", color=FAIL_RED,
                        fontsize=8, fontweight="bold")
        elif op == "chaos.kill_follower":
            ax_top.axvline(t, color=PURPLE, linewidth=2.5, linestyle="--", zorder=5)
            ax_top.text(t + 0.5, y_max * 0.68, "⚡ Follower\nKill", color=PURPLE,
                        fontsize=8, fontweight="bold")
        elif op == "chaos.sqs_redeliver":
            ax_top.axvline(t, color=GRAY, linewidth=1, linestyle=":", alpha=0.6)

    style_ax(ax_top, "Figure 4 — Throughput Under Fault Injection",
             ylabel="Tasks per 10s window")
    ax_top.legend(loc="upper right", fontsize=10)
    ax_top.set_xlim(0, max_t)

    # Add SQS redeliver label once
    sqs_times = [t for op, t in chaos_events if op == "chaos.sqs_redeliver"]
    if sqs_times:
        ax_top.text(sqs_times[0], y_max * 0.05, "SQS redeliveries\n(dotted lines)",
                    color=GRAY, fontsize=7)

    # Bottom: exactly-once proof bars
    ax_bot.set_facecolor("#f8f9fa")
    proof_items = [
        ("Duplicate\nDispatches",       summary["duplicate_dispatches"],       500),
        ("Linearizability\nViolations", summary["linearizability_violations"],  9584),
        ("Stuck PENDING\n(post-run)",   summary["stuck_pending"],               500),
        ("Stuck ASSIGNED\n(post-run)",  summary["stuck_assigned"],              500),
    ]
    labels  = [p[0] for p in proof_items]
    actuals = [p[1] for p in proof_items]
    totals  = [p[2] for p in proof_items]
    x = np.arange(len(labels))
    w = 0.35

    bg_bars = ax_bot.bar(x, totals,  width=w*2, color="#ecf0f1", zorder=2,
                         label="Total operations")
    fg_bars = ax_bot.bar(x, actuals, width=w*2,
                         color=[PASS_GREEN if v == 0 else FAIL_RED for v in actuals],
                         zorder=3, label="Violations (0 = PASS)")

    for bar, val in zip(fg_bars, actuals):
        label_str = "0 ✓" if val == 0 else f"{val} ✗"
        ax_bot.text(bar.get_x() + bar.get_width()/2,
                    max(bar.get_height(), totals[fg_bars.index(bar)] * 0.05) + totals[fg_bars.index(bar)] * 0.02,
                    label_str, ha="center", va="bottom", fontsize=11,
                    fontweight="bold",
                    color=PASS_GREEN if val == 0 else FAIL_RED)

    ax_bot.set_xticks(x)
    ax_bot.set_xticklabels(labels, fontsize=9)
    style_ax(ax_bot, "Exactly-Once Safety Checks (all must be 0)",
             ylabel="Count")
    for spine in ax_bot.spines.values():
        spine.set_visible(False)
    ax_bot.grid(axis="y", color="white", linewidth=1.2)
    ax_bot.set_ylim(0, max(totals) * 1.15)

    plt.tight_layout(h_pad=2)
    out = run_dir / "fig4_chaos_proof.png"
    plt.savefig(out, dpi=150, bbox_inches="tight")
    plt.close()
    print(f"Saved {out}")


# ── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: python plot_exp3.py <run_dir>")
        sys.exit(1)

    run_dir = Path(sys.argv[1])
    if not run_dir.exists():
        print(f"Run dir not found: {run_dir}")
        sys.exit(1)

    events, summary, dups, divergence, lincheck = load(run_dir)

    print(f"Loaded {len(events)} events from {run_dir.name}")
    print(f"  Submitted: {summary['submitted']}  Completed: {summary['completed']}")
    print(f"  Violations: {summary['linearizability_violations']}  Duplicates: {summary['duplicate_dispatches']}")
    print()

    fig_summary_dashboard(summary, run_dir)
    fig_task_state_timeline(events, run_dir)
    fig_latency_cdf(events, run_dir)
    fig_chaos_proof(events, summary, run_dir)

    print("\nAll figures saved to", run_dir)
