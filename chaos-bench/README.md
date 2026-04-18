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
| **Exp 1 — Leader election** | `bench --rate 300 --duration 120` in background; loop `chaos kill_leader` every 20 s; compute post-fault latency spike from plot |
| **Exp 2 — Throughput scaling** | `terraform apply -var coordinator_count=3`, run `bench` at rates 100/500/1000, repeat with `=5` |
| **Exp 3 — Exactly-once** | `bench --rate 500 --duration 1200` (~10k tasks) + periodic `chaos kill_*`; feed `history.jsonl` into Porcupine adapter |
| **Exp 4 — Snapshot recovery** | `bench --rate 2000 --duration 1500` to fill log, force snapshot, `ecs stop-task` one coordinator cold, measure re-ready time from coordinator log stream |

## File layout

```
chaos-bench/
  main.py        CLI entry
  config.py      env-var driven config
  submitter.py   S3+DDB+SQS task submission (bypasses private ingest API)
  bench.py       workload orchestrator
  chaos.py       ECS-level fault injection
  recorder.py    JSONL history writer
  plot.py        summary.json + latency/throughput PNGs
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
