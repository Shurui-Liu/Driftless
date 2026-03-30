# Driftless

A distributed task queue and coordination system built on the Raft consensus algorithm. Driftless demonstrates a production-grade approach to distributed task scheduling and execution, deployed on AWS ECS Fargate.

## Architecture Overview

```
Client → Task Ingest API → S3 (payload) → DynamoDB (metadata) → SQS (ingest queue)
                                                                        ↓
                                     Raft Leader reads ingest queue → SQS (assignment queue)
                                                                        ↓
                                                           Worker nodes execute → S3 (results)
```

### Services

| Service | Language | Purpose |
|---------|----------|---------|
| `task-ingest-api` | Go | HTTP API for submitting tasks |
| `raft` | Go | 3-node Raft consensus cluster for task coordination |
| `worker-node` | Go | Distributed workers that execute assigned tasks |
| `state-observer` | Python | Real-time monitoring, metrics, and alerting |

### AWS Infrastructure

- **Compute**: ECS Fargate (coordinator cluster + workers)
- **Storage**: DynamoDB (task metadata + Raft state), S3 (payloads + snapshots)
- **Messaging**: SQS queues (ingest, assignment, results) with dead letter queues
- **Discovery**: AWS Cloud Map (DNS-based peer discovery)
- **Observability**: CloudWatch Logs, CloudWatch Metrics, SNS alerts
- **IaC**: Terraform (modules: networking, storage, messaging, ecs_cluster)

## Getting Started

### Prerequisites

- Go 1.21+
- Python 3.9+
- AWS account with credentials configured (`~/.aws/credentials` or environment variables)
- Terraform 1.0+ (for cloud deployment)
- `protoc` with Go plugins (for regenerating gRPC stubs)

### Build

```bash
# Task Ingest API
cd task-ingest-api && go build -o task-ingest-api main.go

# Raft Coordinator (regenerate proto stubs if needed)
cd raft
protoc --go_out=. --go-grpc_out=. proto/raft.proto
go build -o raft-coordinator main.go

# Worker Node
cd worker-node && go build -o worker-node main.go

# State Observer
cd state-observer && pip install -r requirements.txt
```

### Run Locally

Start a 3-node Raft cluster:

```bash
# Node A
NODE_ID=coordinator-a GRPC_PORT=50051 HTTP_PORT=8080 \
PEER_ADDRS=localhost:50052,localhost:50053 \
./raft/raft-coordinator

# Node B
NODE_ID=coordinator-b GRPC_PORT=50052 HTTP_PORT=8081 \
PEER_ADDRS=localhost:50051,localhost:50053 \
./raft/raft-coordinator

# Node C
NODE_ID=coordinator-c GRPC_PORT=50053 HTTP_PORT=8082 \
PEER_ADDRS=localhost:50051,localhost:50052 \
./raft/raft-coordinator
```

Start the monitoring dashboard (no AWS required locally):

```bash
COORDINATOR_ADDRS=localhost:8080,localhost:8081,localhost:8082 \
DISABLE_CLOUDWATCH=1 DISABLE_SNS=1 \
python state-observer/main.py
```

Start the Task Ingest API and Worker (requires AWS credentials):

```bash
# Task Ingest API
DYNAMO_TABLE=tasks S3_BUCKET=driftless-payloads \
SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/<account>/driftless-ingest \
./task-ingest-api/task-ingest-api

# Worker Node
WORKER_ID=worker-1 DYNAMO_TABLE=tasks S3_BUCKET=driftless-payloads \
SQS_ASSIGNMENT_URL=https://sqs.us-east-1.amazonaws.com/<account>/driftless-assignment \
COORDINATOR_URL=http://localhost:8080 \
./worker-node/worker-node
```

### Deploy to AWS

```bash
cd terraform/environments/dev
terraform init
terraform plan -var="your_account_id=<your-account-id>"
terraform apply
```

Terraform provisions the full stack: VPC, ECS cluster, DynamoDB tables, S3 buckets, SQS queues, and Cloud Map service discovery.

## Configuration

### Task Ingest API

| Variable | Required | Description |
|----------|----------|-------------|
| `DYNAMO_TABLE` | Yes | DynamoDB table for task metadata |
| `S3_BUCKET` | Yes | S3 bucket for task payloads |
| `SQS_QUEUE_URL` | Yes | SQS ingest queue URL |

### Raft Coordinator

| Variable | Default | Description |
|----------|---------|-------------|
| `NODE_ID` | Required | Unique node name (e.g., `coordinator-a`) |
| `GRPC_PORT` | `50051` | Peer-to-peer gRPC port |
| `HTTP_PORT` | `8080` | HTTP state observation port |
| `PEER_ADDRS` | Required | Comma-separated peer gRPC addresses (excluding self) |

Raft timing constants (in `raft/node.go`):
- Election timeout: 150–300ms (randomized)
- Heartbeat interval: 50ms
- RPC timeout: 80ms

### Worker Node

| Variable | Required | Description |
|----------|----------|-------------|
| `WORKER_ID` | Yes | Unique worker identifier |
| `DYNAMO_TABLE` | Yes | DynamoDB tasks table |
| `S3_BUCKET` | Yes | S3 bucket for payloads and results |
| `SQS_ASSIGNMENT_URL` | Yes | SQS assignment queue URL |
| `COORDINATOR_URL` | Yes | HTTP URL of Raft coordinator (for heartbeats) |

### State Observer

| Variable | Default | Description |
|----------|---------|-------------|
| `COORDINATOR_ADDRS` | `localhost:8080` | Comma-separated coordinator HTTP addresses |
| `POLL_INTERVAL_S` | `2.0` | Cluster state poll interval (seconds) |
| `PROMETHEUS_PORT` | `9090` | Prometheus scrape port |
| `CW_NAMESPACE` | `Driftless/Raft` | CloudWatch metric namespace |
| `CW_PUBLISH_INTERVAL_S` | `60.0` | CloudWatch batch publish interval |
| `AWS_REGION` | `us-east-1` | AWS region |
| `SNS_TOPIC_ARN` | `` | SNS topic for critical alerts (optional) |
| `DISABLE_CLOUDWATCH` | `0` | Set to `1` to disable CloudWatch publishing |
| `DISABLE_SNS` | `0` | Set to `1` to disable SNS alerts |

## API Reference

### Task Ingest API

**Submit a task**
```
POST /tasks
Content-Type: application/json

{
  "payload": "<base64-encoded bytes>",
  "priority": 5
}
```
Response `202 Accepted`:
```json
{ "task_id": "<uuid>", "status": "PENDING" }
```

**Health check**
```
GET /health  →  200 OK
```

### Raft Coordinator

**HTTP state endpoint** (used by State Observer and health checks)
```
GET /state  →  200 OK
{
  "node_id": "coordinator-a",
  "state": "leader" | "follower" | "candidate",
  "current_term": 42,
  "commit_index": 17,
  "log_length": 18,
  "match_index": { "coordinator-b": 16 },
  "next_index":  { "coordinator-b": 17 }
}

GET /health  →  200 OK
```

**gRPC service** (peer-to-peer, port 50051)
```protobuf
service RaftCoordinator {
  rpc RequestVote   (RequestVoteRequest)    returns (RequestVoteResponse);
  rpc AppendEntries (AppendEntriesRequest)  returns (AppendEntriesResponse);
}
```

### Prometheus Metrics (State Observer, port 9090)

| Metric | Labels | Description |
|--------|--------|-------------|
| `raft_current_term` | `node_id` | Current Raft term |
| `raft_commit_index` | `node_id` | Committed log index |
| `raft_is_leader` | `node_id` | 1 if this node is leader |
| `raft_node_up` | `node_id` | 1 if node is reachable |
| `raft_replication_lag` | `leader_id`, `peer_id` | Log entries behind leader |
| `raft_has_leader` | — | 1 if cluster has a leader |
| `raft_split_brain` | — | 1 if split-brain detected |
| `raft_nodes_up` | — | Number of reachable nodes |

## Monitoring & Alerts

The State Observer emits alerts via SNS for three conditions:

- **Split-brain**: Two or more leaders detected in the same term
- **No leader**: Cluster leaderless for 10+ seconds
- **Quorum loss**: Fewer than 50% of nodes reachable

Alerts are deduplicated with a 120-second cooldown. The terminal dashboard (`rich`) updates every 2 seconds showing cluster summary, per-node status, and replication lag.

## Task Lifecycle

```
PENDING → ASSIGNED → COMPLETED
```

1. Client POSTs task; API writes payload to S3 and metadata to DynamoDB (`PENDING`), publishes to SQS ingest queue.
2. Raft leader dequeues from ingest queue, publishes to assignment queue.
3. Worker dequeues, updates DynamoDB to `ASSIGNED`, downloads payload from S3, executes workload, uploads result to S3, marks `COMPLETED`.

A background goroutine in the Task Ingest API scans DynamoDB every 30 seconds for stuck `PENDING` tasks and re-publishes them to SQS, recovering tasks lost between a DynamoDB write and an SQS publish.

## Project Structure

```
Driftless/
├── task-ingest-api/       # HTTP task submission service
│   └── main.go
├── raft/                  # Raft consensus coordinator
│   ├── main.go
│   ├── node.go            # Core Raft state machine
│   ├── grpc_server.go     # gRPC peer endpoint
│   ├── grpc_client.go     # gRPC peer client
│   ├── http_server.go     # HTTP state/health endpoint
│   └── proto/raft.proto   # Protocol Buffer definitions
├── worker-node/           # Task execution worker
│   └── main.go
├── state-observer/        # Monitoring, metrics, and alerting
│   ├── main.py
│   ├── raft_client.py
│   ├── metrics.py
│   ├── alerts.py
│   ├── dashboard.py
│   └── config.py
└── terraform/             # Infrastructure as Code
    └── environments/dev/
```
