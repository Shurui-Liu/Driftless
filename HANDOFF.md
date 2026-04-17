# Driftless Handoff — Experiments Split

This document is for **Teammate B** joining the project to run experiments.
It explains what's already deployed, what you own, and what you still need to
build.

## Current state (as of 2026-04-16)

A working Driftless deployment exists in **Yuming's** voclabs sandbox account
(`us-east-1`). **You do not need to use it** — in fact, voclabs sandboxes are
single-tenant, so you should deploy your own.

What works end-to-end today:

- Terraform-managed VPC, ECS cluster, DynamoDB (tasks, peers, raft-state), S3
  (task-data, raft-snapshots), SQS (ingest, assignment, results), ECR repos
- 3-node Raft coordinator cluster with leader election + log replication
- Worker ECS service that pulls from assignment queue and writes results
- State observer that polls coordinators and publishes to CloudWatch
- Ingest API (private subnet) with orphan-recovery from tasks DDB
- `chaos-bench/` Python client that submits load and injects faults from a
  laptop (no VPC access required — uses boto3 directly)

A known quirk in Yuming's cluster: one coordinator (`coordinator-a`) is still
running a pre-`is_leader` image. Not blocking. A fresh deploy in your account
will not have this.

## Deployment path for your account

Follow `DEPLOY.md` top-to-bottom. Summary:

1. `terraform apply` (creates infra, `coordinator_count=1`)
2. `docker buildx build --push` for all four images
3. `terraform apply -var='coordinator_count=3'` (scale up + pull new images)
4. `cd chaos-bench && pip install -r requirements.txt` to get the client

**If you're not using voclabs**, also copy `terraform/environments/dev/terraform.tfvars.example` → `terraform.tfvars` and set `lab_role_arn` to a role in your account with ECS task execution + DDB + S3 + SQS permissions. See the *Non-sandbox accounts* section in DEPLOY.md.

## Experiment ownership

From `Proposal.pdf`:

| Experiment | Owner | What it measures |
|---|---|---|
| 1. Leader Election Correctness | **A (Yuming)** | Time to re-elect after `kill_leader`, no split-brain |
| 2. Throughput vs Cluster Size | **B (you)** | Throughput + latency at 3 and 5 nodes × 100/500/1000 tasks/min |
| 3. Exactly-Once via Porcupine | **B (you)** | 10k tasks + fault injection, linearizability check |
| 4. Snapshot Recovery | **A (Yuming)** | Cold restart from S3 snapshot with 50k+ log entries |

The 4-experiment split matches the proposal and keeps each person owning one
"infrastructure-heavy" experiment (1, 3 — both need fault injection and
careful timing) and one "load-heavy" experiment (2, 4 — both need sustained
workload generation).

## What you can build on

Already in `chaos-bench/`:

- `bench.py` — timed workload runner (ThreadPool submit + poll, JSONL history)
- `chaos.py` — `kill_leader`, `kill_follower`, `kill_worker` via `ecs:StopTask`
- `leader_detect.py` — reads `is_leader` bit from peers DDB table (works from
  outside the VPC)
- `plot.py` — summary.json + latency CDF + throughput bar chart
- `main.py` — `bench`, `chaos`, `plot` subcommands

## What you still need to build (Experiment 2 + 3)

**For Experiment 2 — Throughput vs Cluster Size:**

- Orchestrator script that sweeps `(cluster_size × rate)` combinations and
  saves a separate history file for each run. Reuses `bench.run_bench()`.
- Plot: throughput (submitted vs completed) + p50/p95/p99 latency bars.
- Add a 5-node variant: `coordinator_count = 5` in tfvars, plus update
  `EXPECTED_PEER_IDS` — see `terraform/modules/ecs_cluster/main.tf`.

**For Experiment 3 — Exactly-Once via Porcupine:**

- A Go binary (`raft/cmd/lincheck/`) that (a) submits 10k tasks, (b) records
  history in Porcupine's `porcupine.Operation` format, (c) runs a fault
  mid-run, (d) verifies linearizability via `porcupine.CheckOperations`.
  Porcupine lib: `github.com/anishathalye/porcupine`.
- The state model: a map from `task_id` → `status ∈ {PENDING, IN_PROGRESS, COMPLETED}`
  with valid transitions. Apply functions on each entry.

## Shared-sandbox rule

**Do not `terraform apply` against the other person's account.** voclabs
sandboxes revoke credentials when a session restarts, and simultaneous applies
from two machines will corrupt state. Each person owns their own sandbox.

When comparing results, exchange the JSONL history files (`chaos-bench/histories/<run-name>/history.jsonl`) — those are self-contained and
portable.

## Where things live

- `LIFECYCLE.md` — architecture + per-stage verification + troubleshooting
- `DEPLOY.md` — step-by-step deployment
- `chaos-bench/README.md` — client usage + how subcommands map to experiments
- `Proposal.pdf` — original 4-experiment design
- `raft/` — coordinator + Raft library (Go)
- `worker-node/` — worker (Go)
- `task-ingest-api/` — ingest HTTP API (Go)
- `state-observer/` — CloudWatch metrics publisher (Python)
- `terraform/` — all infrastructure (modules/ + environments/dev/)
