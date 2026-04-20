# Running Exp 1 and Exp 4

This folder holds the canonical runs and analyses for the two Driftless chaos experiments. Both are driven from `chaos-bench/`.

## Prerequisites (once per lab session)

1. **AWS credentials active** — Voclabs sessions expire; renew if `aws sts get-caller-identity` fails.
2. **Infrastructure deployed** — Terraform must have applied the `dev` environment with `coordinator_count = 3`:
   ```
   cd terraform/environments/dev
   terraform apply -var-file=terraform.tfvars
   ```
   Verify: three ECS services `raft-coordinator-dev-coordinator-{a,b,c}` are all running with a single leader row in the `raft-coordinator-dev-peers` DynamoDB table.
3. **Python environment**:
   ```
   cd chaos-bench
   source .venv/bin/activate
   ```
4. **Exported env vars** (both experiments need these):
   ```
   export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
   export COORDINATOR_SERVICES=raft-coordinator-dev-coordinator-a,raft-coordinator-dev-coordinator-b,raft-coordinator-dev-coordinator-c
   ```

## Exp 1 — Leader election correctness & convergence

**What it does.** 5 iterations. Each iteration: wipe S3 `raft/` snapshots → stop all three coordinators → poll peers table until a single leader with fresh heartbeats emerges → warm up 120 s under load → kill the current leader once → observe convergence for 30 s. Writes a `repeat_summary.json` with convergence p50/p95 and per-iteration lifecycle counts. Duplicate-dispatch scan runs separately against each iteration's history.

**Command** (≈ 20–25 min wall):
```
cd chaos-bench
python exp1.py repeat --iterations 5 --warmup 120 --rate 300 \
  --group-name exp1-repeat-$(date -u +%Y%m%dT%H%M%SZ)
```

**Duplicate scan** after the repeat completes (one invocation per iteration):
```
for h in histories/<group>/iter-*/history.jsonl; do
  python exp1.py dups "$h" > "$(dirname "$h")/exp1_dups.json"
done
```

**What you should see.** Each iteration prints `conv_ms=<n> completed=<n> lost=<n>`. The aggregate JSON at the end reports `convergence_p50_ms`, `convergence_p95_ms`, and `convergence_ms_samples`. Healthy numbers for this setup: p50 ≈ 2.4 s, p95 ≈ 4 s, 0 duplicates across all iterations. See `exp1/analysis.md` for the reference run.

**When to re-run.** Re-run after any change to `raft/node.go` election code, `PeerResolver` in `raft/grpc_client.go`, or the DynamoDB peers-table schema/TTL.

## Exp 4 — Snapshot recovery & S3 log persistence

**What it does.** Drives load until the leader's `commit_index` crosses a threshold → checks S3 for a snapshot of a chosen follower → kills that follower cold via `ecs:StopTask` → observes the restart + snapshot-restore + catch-up timeline → writes `exp4_summary.json` with `restart_ms`, `snapshot_restored_ms`, `catchup_ms`, and snapshot artifact size.

**Command** (≈ 15–20 min wall):
```
cd chaos-bench
python exp4.py --duration 1200 --rate 2000 --target-commit 35000
```

**Contrast run** (full-log replay, no snapshot — only meaningful once the recovery-path bugs below are fixed):
```
SNAPSHOT_S3_BUCKET="" python exp4.py --duration 1200 --rate 2000 --target-commit 35000
```

**What you should see.**
- `restart_ms` ≈ 30–35 s — Fargate scheduling; stable across runs.
- `snapshot_size_bytes` non-zero once compaction has triggered for the target follower (≈ at or above 35 k entries in practice).
- `catchup_ms` — *currently always `null`* on this codebase. See §4 of `exp4/analysis.md` for why (PreVote + InstallSnapshot gaps in `raft/node.go`). Treat any non-null value here as signal that one of those gaps has been closed.

**Reset between runs.** Exp 4 does not auto-reset the cluster. If a run leaves nodes in an election storm, run the reset helper used by Exp 1:
```
python -c "from exp1 import _reset_cluster; from config import Config; _reset_cluster(Config.from_env())"
```

## Known blockers (both experiments)

- **Voclabs credential expiry** mid-run surfaces as `AccessDenied` on DynamoDB `Scan` / `BatchGetItem`. Renew the lab session and re-run — both scripts are safe to restart.
- **`EXPECTED_PEER_IDS` regression.** If any coordinator task definition is re-registered without this env var (e.g. a Terraform apply with `coordinator_count = 1`), a restarted follower will come up with `peers=0` and self-elect, splitting the cluster. The `terraform/environments/dev/terraform.tfvars` file pins `coordinator_count = 3` to prevent this.

## Layout

```
experiments/
  README.md                 this file
  exp1/
    analysis.md             thesis-grade writeup
    repeat_summary.json     aggregate + per-iteration convergence
    iter-0{1..5}/           raw history + per-iteration dup scan
  exp4/
    analysis.md             thesis-grade writeup
    exp4_summary.json       recovery timeline
    history.jsonl           raw submit/kill/threshold/recovery events
    corroboration/          rescued summary from an earlier run (see analysis §3.3)
```
