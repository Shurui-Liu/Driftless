"""main.py — CLI entry.

Subcommands:
  bench   run a workload for a fixed duration at a target rate
  chaos   inject a single fault (kill_leader|kill_follower|kill_worker)
  plot    render summary + plots from a history file
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sys

from bench import RunParams, run_bench
from chaos import ChaosInjector
from config import Config
from leader_detect import find_leader


def cmd_bench(args: argparse.Namespace) -> int:
    cfg = Config.from_env()
    params = RunParams(
        rate_per_min=args.rate,
        duration_s=args.duration,
        payload_bytes=args.payload_bytes,
        priority=args.priority,
        completion_timeout_s=args.timeout,
    )
    run_name = args.run_name or dt.datetime.utcnow().strftime("run-%Y%m%dT%H%M%SZ")
    path = run_bench(cfg, params, run_name)
    print(f"history written to {path}", file=sys.stderr)
    return 0


def cmd_chaos(args: argparse.Namespace) -> int:
    cfg = Config.from_env()
    inj = ChaosInjector(cfg)

    leader_service = args.leader_service
    if args.target in ("kill_leader", "kill_follower") and not leader_service:
        info = find_leader(cfg)
        if not info:
            print("could not detect leader via /state polling", file=sys.stderr)
            return 3
        leader_service = info.ecs_service
        print(
            f"detected leader: node_id={info.node_id} service={info.ecs_service} term={info.term}",
            file=sys.stderr,
        )

    if args.target == "kill_leader":
        r = inj.kill_leader(leader_service)
    elif args.target == "kill_follower":
        followers = [s for s in cfg.coordinator_services if s != leader_service]
        r = inj.kill_follower(followers)
    elif args.target == "kill_worker":
        r = inj.kill_worker()
    else:
        print(f"unknown target: {args.target}", file=sys.stderr)
        return 2
    print(json.dumps(r.__dict__ if r else {}, default=str))
    return 0


def cmd_plot(args: argparse.Namespace) -> int:
    from plot import render

    render(args.path)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(prog="chaos-bench")
    sub = ap.add_subparsers(dest="cmd", required=True)

    bp = sub.add_parser("bench", help="run a timed workload")
    bp.add_argument("--rate", type=float, default=100.0, help="tasks/min")
    bp.add_argument("--duration", type=float, default=60.0, help="seconds")
    bp.add_argument("--payload-bytes", type=int, default=128)
    bp.add_argument("--priority", type=int, default=5)
    bp.add_argument("--timeout", type=float, default=60.0)
    bp.add_argument("--run-name", default=None)
    bp.set_defaults(func=cmd_bench)

    cp = sub.add_parser("chaos", help="inject a fault")
    cp.add_argument("target", choices=["kill_leader", "kill_follower", "kill_worker"])
    cp.add_argument("--leader-service", default=None,
                    help="ECS service of the leader; auto-detected if omitted")
    cp.set_defaults(func=cmd_chaos)

    pp = sub.add_parser("plot", help="render plots from a history file")
    pp.add_argument("path")
    pp.set_defaults(func=cmd_plot)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
