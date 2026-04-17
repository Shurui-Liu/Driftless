# Driftless — Task Lifecycle & Verification Guide

End-to-end trace of a task through the Driftless distributed task queue,
plus the AWS CLI commands you can use to verify each stage.

## 1. Architecture

```
                        ┌────────────────────────── AWS us-east-1 ──────────────────────────┐
                        │                                                                   │
client ──POST /tasks──▶ │  task-ingest-api                                                   │
                        │      ├──▶ S3: tasks/{id}/payload                                   │
                        │      ├──▶ DDB raft-coordinator-dev-tasks (status=PENDING)          │
                        │      └──▶ SQS raft-coordinator-dev-ingest  {task_id, priority:int} │
                        │                      │                                             │
                        │                      ▼                                             │
                        │  coordinator-a  (Raft leader — single-node cluster)                │
                        │      • reads SQS-ingest                                            │
                        │      • Propose(dispatchCommand) → Raft log                         │
                        │      • applyLoop → state machine                                   │
                        │      • SM publishes to SQS-assignment                              │
                        │      • acks ingest message                                         │
                        │                      │                                             │
                        │                      ▼                                             │
                        │  worker  (Fargate, N replicas)                                     │
                        │      • reads SQS-assignment                                        │
                        │      • downloads S3 payload                                        │
                        │      • processes                                                   │
                        │      • uploads S3 result                                           │
                        │      • DDB status=COMPLETED                                        │
                        │      • heartbeats → coordinator-a:8080/heartbeat                   │
                        │                                                                   │
                        │  state-observer (Fargate)                                         │
                        │      polls coord:8080/state every 2s                              │
                        │      → Prometheus /metrics :9090                                   │
                        │      → CloudWatch namespace Driftless/Raft                        │
                        │      → SNS alerts on split-brain / quorum loss                    │
                        └───────────────────────────────────────────────────────────────────┘
```

Peer discovery uses DynamoDB table `raft-coordinator-dev-peers`
(replaces AWS Cloud Map, which the voclabs sandbox blocks).
Each coordinator registers `{node_id, grpc_addr, http_addr, heartbeat_ts}`
on startup and refreshes every 10 seconds; observers and workers read this
table to resolve coordinator addresses.

## 2. Lifecycle phases

| Phase | Actor | Side effects | DDB status |
|---|---|---|---|
| Submit | ingest-api | S3 payload, DDB PutItem, SQS ingest msg | PENDING |
| Dispatch | coordinator | Raft log append, Raft commit, SQS assignment msg | PENDING |
| Assign | worker | SQS receive | ASSIGNED |
| Execute | worker | downloads payload, runs handler | ASSIGNED |
| Complete | worker | S3 result, DDB UpdateItem | COMPLETED |

Failure modes set `status=FAILED` and record `error` in the DDB item.

## 3. Fresh task injection (bypassing the ingest API)

Useful when the ingest API is behind a private NLB and unreachable from
your laptop. Mimics what the API does:

```bash
TASK_ID=$(uuidgen | tr A-Z a-z)
NOW=$(date -u +%FT%TZ)
BUCKET=raft-coordinator-dev-task-data-<ACCOUNT_ID>
QUEUE=https://sqs.us-east-1.amazonaws.com/<ACCOUNT_ID>/raft-coordinator-dev-ingest

aws s3 cp - "s3://$BUCKET/tasks/$TASK_ID/payload" <<< "hello-driftless"

aws dynamodb put-item --table-name raft-coordinator-dev-tasks \
  --item "{\"task_id\":{\"S\":\"$TASK_ID\"},\
          \"status\":{\"S\":\"PENDING\"},\
          \"priority\":{\"N\":\"5\"},\
          \"s3_key\":{\"S\":\"tasks/$TASK_ID/payload\"},\
          \"created_at\":{\"S\":\"$NOW\"},\
          \"updated_at\":{\"S\":\"$NOW\"}}"

aws sqs send-message --queue-url $QUEUE \
  --message-body "{\"task_id\":\"$TASK_ID\",\"priority\":5}"

echo "task_id=$TASK_ID"
```

**Critical:** `priority` in the SQS body must be a JSON number, not a
string. The coordinator's `ingestMessage` struct unmarshals into `int`;
`"priority":"5"` is rejected with `bad ingest message, dropping`.

## 4. Per-stage verification

### 4.1 Coordinator is leader

```bash
# Latest log stream (ECS rotates per task)
aws logs describe-log-streams \
  --log-group-name /ecs/raft-coordinator-dev/coordinator \
  --order-by LastEventTime --descending --max-items 1 \
  --query 'logStreams[0].logStreamName' --output text

aws logs tail /ecs/raft-coordinator-dev/coordinator --since 5m --format short \
  | grep -E "became leader|became follower|election"
```

Expected first event after container start:
```
"msg":"starting election","term":1
"msg":"became leader","term":1
```

### 4.2 Task was dispatched

```bash
aws logs tail /ecs/raft-coordinator-dev/coordinator --since 2m --format short \
  | grep "$TASK_ID"
```

Absence of `commit wait timed out` = Raft log committed successfully.

### 4.3 Worker executed it

```bash
aws logs tail /ecs/raft-coordinator-dev/worker --since 2m --format short \
  | grep "$TASK_ID"
```

### 4.4 Terminal state

```bash
aws dynamodb get-item --table-name raft-coordinator-dev-tasks \
  --key "{\"task_id\":{\"S\":\"$TASK_ID\"}}" \
  --query 'Item.status.S' --output text
# → COMPLETED

aws s3 cp "s3://$BUCKET/tasks/$TASK_ID/result" -
# → {"task_id":"...","processed_bytes":N,"worker_id":"ip-..."}
```

### 4.5 Poll until terminal

```bash
while :; do
  S=$(aws dynamodb get-item --table-name raft-coordinator-dev-tasks \
        --key "{\"task_id\":{\"S\":\"$TASK_ID\"}}" \
        --query 'Item.status.S' --output text)
  echo "$(date +%T) status=$S"
  [ "$S" = COMPLETED ] || [ "$S" = FAILED ] && break
  sleep 2
done
```

### 4.6 Peer registry & observer

```bash
# Coordinators currently known to the cluster
aws dynamodb scan --table-name raft-coordinator-dev-peers --output json

# Observer polling logs
aws logs tail /ecs/raft-coordinator-dev/observer --since 2m --format short
```

Heartbeat entries have a TTL (`expires_at`); observer drops any row
whose `heartbeat_ts` is older than `2 × TTL`.

## 5. Troubleshooting matrix

| Symptom | Likely cause | Fix |
|---|---|---|
| `election timed out, retrying` loops forever | Single-node cluster built without the self-win short-circuit | Rebuild `raft/node.go` — `wonElection <- struct{}{}` when `votes >= majority` |
| `commit wait timed out` on well-formed msgs | Single-node: `maybeAdvanceCommitLocked` never called without peer replies | Call it from `Propose` after appending the entry |
| `bad ingest message, dropping … priority of type int` | Orphan-recovery path in ingest API serialized `priority` as string | `strconv.Atoi` + `map[string]any` in `task-ingest-api/main.go` |
| Task stuck `PENDING` | Coordinator not leader, or ingest msg not in SQS | Check `became leader` + `aws sqs get-queue-attributes` for queue depth |
| Task stuck `ASSIGNED` | Worker crashed mid-execution | `aws logs tail /ecs/raft-coordinator-dev/worker` and look for panic around that `task_id` |
| Coordinator restarts repeatedly with `registered self` each time | ECS task keeps crashing; peer row looks fresh but binary is dying | Check exit code and stderr in the log stream |
| Observer reports all `unreachable` | Peers table empty or stale | `dynamodb scan peers` — if empty, coordinator registration is failing (check IAM) |

## 6. Key source files

| File | Responsibility |
|---|---|
| `raft/node.go` | Raft state machine (election, log, commit, apply) |
| `raft/dispatcher.go` | SQS ingest loop + `Propose` caller + assignment publisher |
| `raft/peers/peers.go` | DynamoDB peer registry, self-IP discovery |
| `raft/cmd/coordinator/main.go` | Process entry: load AWS, register, start gRPC + HTTP + dispatcher |
| `task-ingest-api/main.go` | HTTP ingest endpoint + orphan recovery loop |
| `worker-node/main.go` | Assignment consumer, coordinator resolution via peers table, heartbeat loop |
| `state-observer/main.py` | Async poll loop, Prometheus + CloudWatch publishers, SNS alerter |

## 7. Force redeploy after a code change

```bash
# Build & push (Apple Silicon → linux/amd64)
cd raft && docker buildx build --platform linux/amd64 \
  -t <ACCOUNT_ID>.dkr.ecr.us-east-1.amazonaws.com/raft-coordinator-dev-coordinator:latest --push .

# Roll the service
aws ecs update-service \
  --cluster raft-coordinator-dev-cluster \
  --service raft-coordinator-dev-coordinator-a \
  --force-new-deployment

# Wait for the old deployment to drain
until [ $(aws ecs describe-services \
  --cluster raft-coordinator-dev-cluster \
  --services raft-coordinator-dev-coordinator-a \
  --query 'length(services[0].deployments)' --output text) -eq 1 ]; do
    sleep 5
done
```

Same pattern for `raft-coordinator-dev-ingest` and `raft-coordinator-dev-worker`.
