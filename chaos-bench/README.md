# chaos-bench — Driftless benchmark & chaos client

Drives load against a deployed Driftless environment and injects ECS-level
faults. Writes a JSONL history of every `submit`, `complete`, `timeout`,
and chaos event so later tooling (plots, Porcupine linearizability check)
can operate on a single artifact per run.

## Install

```bash
cd chaos-bench
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
```

Credentials come from the usual boto3 chain (env vars, `~/.aws/credentials`,
etc.). Voclabs sandbox works out of the box.

## Environment

| Variable | Default | Notes |
|---|---|---|
| `AWS_REGION` | `us-east-1` | |
| `AWS_ACCOUNT_ID` | — | required for default bucket/queue names |
| `DRIFTLESS_ENV` | `dev` | `prod` for multi-AZ env when added |
| `COORDINATOR_SERVICES` | `<prefix>-coordinator-a` | comma-separated ECS service names |
| `HISTORY_DIR` | `./histories` | one subdir per run |

All resource names can be overridden individually — see `config.py`.

## Quick runs

```bash
# 100 tasks/min for 60 s
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text) \
python main.py bench --rate 100 --duration 60

# Inject a leader kill mid-run (from another terminal)
python main.py chaos kill_leader --leader-service raft-coordinator-dev-coordinator-a

# Summarize + plot
python main.py plot histories/run-*/history.jsonl
```

## Mapping to proposal experiments

| Experiment | Command recipe |
|---|---|
| **Exp 1 — Leader election** | `python exp1.py run --duration 180 --rate 300 --kill-period 30` then `python exp1.py analyze <path>` and `python exp1.py dups <path>` |
| **Exp 2 — Throughput scaling** | `terraform apply -var coordinator_count=3`, run `bench` at rates 100/500/1000, repeat with `=5` |
| **Exp 3 — Exactly-once** | `bench --rate 500 --duration 1200` (~10k tasks) + periodic `chaos kill_*`; feed `history.jsonl` into Porcupine adapter |
| **Exp 4 — Snapshot recovery** | `python exp4.py --duration 1800 --rate 2000 --target-commit 50000` (snapshots on). For contrast: redeploy with `SNAPSHOT_S3_BUCKET=""` and rerun. |

## Experiment 1 — Leader Election Correctness

`exp1.py run` starts three concurrent threads against the live cluster:

1. **Submitter** — drives `--rate` tasks/min for `--duration` seconds (same code path as `bench`).
2. **Chaos loop** — every `--kill-period` seconds, auto-detects the current leader via the peers DDB table and runs `ecs:StopTask` on it.
3. **Leader tracker** — scans the peers DDB `is_leader` bit every 150 ms and emits a `leader_observed` event each time the holder changes.

Everything lands in one `history.jsonl`. `exp1.py analyze` computes:

- **Election convergence time** — for each `chaos.kill_leader` event, the delta to the first `leader_observed` with a different node_id. Reports p50/p95/mean/max.
- **Lost task count** — `submitted − completed − timed_out`.

`exp1.py dups` (backed by `dup_scan.py`) reads every submitted `task_id` out of the history and scans the tasks DDB for items with `receive_count > 1`. The worker increments this atomically on every assignment receipt, so any `> 1` is a duplicate dispatch (Raft-layer double-commit or SQS redelivery).

## Experiment 4 — Snapshot Recovery

`exp4.py` drives a sustained 2000 tasks/min workload and blocks until the leader's `commit_index` crosses `--target-commit` (default 50000). It reads the leader's commit index out of the coordinator's CloudWatch `raft tick` log lines (emitted every 2 s) — works from outside the VPC.

Once the threshold is hit, the script:

1. Verifies a snapshot exists at `s3://<raft-snapshots-bucket>/raft/<follower_node_id>/snapshot.json` via `HeadObject` and records its size.
2. Runs `ecs:StopTask` on one follower service.
3. Uses `follower_recovery.FollowerRecoveryObserver` to watch the follower's log stream for:
   - `coordinator started` → ECS restart-to-ready
   - `snapshot restored` → recovered via snapshot (absent on full-replay runs)
   - `raft tick` with `commit_index ≥ target − tolerance` → rejoined quorum
4. Writes `exp4_summary.json` with both absolute timestamps and millisecond deltas.

For the full-log-replay contrast run, blank `SNAPSHOT_S3_BUCKET` on the coordinator task definition (redeploy), rerun `exp4.py`, and compare `recovery_timeline.catchup_ms` between runs.

## File layout

```
chaos-bench/
  main.py              CLI entry (bench/chaos/plot)
  config.py            env-var driven config
  submitter.py         S3+DDB+SQS task submission (bypasses private ingest API)
  bench.py             workload orchestrator
  chaos.py             ECS-level fault injection
  leader_detect.py     find current leader via peers DDB
  leader_tracker.py    thread that emits leader_observed events on change
  recorder.py          JSONL history writer
  dup_scan.py          DDB scan for receive_count > 1
  exp1.py              Experiment 1 — leader election correctness
  exp4.py              Experiment 4 — snapshot recovery
  follower_recovery.py CloudWatch-log-driven recovery timeline observer
  plot.py              summary.json + latency/throughput PNGs
  requirements.txt
```

## Notes & gaps

- Leader identity discovery is not yet automatic — pass `--leader-service`
  explicitly. An upcoming addition will poll each coordinator's
  `/state` HTTP endpoint (same path the observer uses) and detect the
  leader before killing.
- Porcupine itself is a Go library; this scaffold only produces the
  history file. A small Go adapter under `raft/cmd/lincheck/` will take
  that file and run the check.
- No Locust yet — the built-in submitter goes directly via boto3, which
  is more reliable than HTTP when the ingest API is behind a private NLB.
