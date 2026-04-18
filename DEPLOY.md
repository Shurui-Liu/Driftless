# Driftless Deployment Guide

Deploys the full Driftless system to AWS. Defaults target an AWS Academy
(voclabs) Learner Lab account, but any account with an ECS task role works —
see *Non-sandbox accounts* below.

## Prerequisites

- AWS CLI v2 configured with credentials for the target account
- Terraform >= 1.6
- Docker (with `buildx` for cross-arch builds from Apple Silicon)
- Go 1.25+ (only needed to build binaries outside Docker)
- Python 3.10+ (only for `chaos-bench/` experiment client)

Verify:

```bash
aws sts get-caller-identity
terraform -version
docker info
```

## Step 1: Set shell variables

```bash
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=us-east-1
echo "Account: $ACCOUNT_ID  Region: $AWS_REGION"
```

## Step 2: First Terraform apply (infra only, no images yet)

Terraform discovers the account ID automatically via `aws_caller_identity`, so
no hand-editing is required for voclabs. This first apply creates the VPC,
subnets, SQS queues, DynamoDB tables, S3 buckets, ECR repositories, and ECS
cluster. ECS services will fail to start because no images exist yet — that's
fine. `coordinator_count` defaults to `1` to avoid 3× restart churn during this
bootstrap phase.

```bash
cd terraform/environments/dev
terraform init
terraform apply        # review and approve
```

Capture outputs:

```bash
terraform output
```

## Step 3: Build and push Docker images

```bash
aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin \
  $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com
```

All four images build from a subdirectory (`raft/`, `worker-node/`, etc.), not
from the repo root — each has its own `go.mod`. On Apple Silicon use
`docker buildx build --platform linux/amd64 --push` in a single step:

```bash
REG=$ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com

# Coordinator
( cd raft && docker buildx build --platform linux/amd64 --push \
    -t $REG/raft-coordinator-dev-coordinator:latest . )

# Worker
( cd worker-node && docker buildx build --platform linux/amd64 --push \
    -t $REG/raft-coordinator-dev-worker:latest . )

# Ingest API
( cd task-ingest-api && docker buildx build --platform linux/amd64 --push \
    -t $REG/raft-coordinator-dev-ingest:latest . )

# State Observer
( cd state-observer && docker buildx build --platform linux/amd64 --push \
    -t $REG/raft-coordinator-dev-observer:latest . )
```

On x86_64 hosts you can use the usual `docker build` + `docker push` split.

## Step 4: Second Terraform apply (ECS picks up the images)

```bash
cd terraform/environments/dev
terraform apply
```

If tasks are still running the old/missing image, force a fresh deployment:

```bash
CLUSTER=$(terraform output -raw ecs_cluster_name)

aws ecs update-service --cluster $CLUSTER \
  --service raft-coordinator-dev-coordinator-a --force-new-deployment
aws ecs update-service --cluster $CLUSTER \
  --service raft-coordinator-dev-worker --force-new-deployment
aws ecs update-service --cluster $CLUSTER \
  --service raft-coordinator-dev-ingest --force-new-deployment
aws ecs update-service --cluster $CLUSTER \
  --service raft-coordinator-dev-observer --force-new-deployment
```

## Step 5: Verify the system is running

```bash
aws ecs list-services --cluster $CLUSTER
aws ecs describe-services --cluster $CLUSTER --services \
  raft-coordinator-dev-coordinator-a \
  raft-coordinator-dev-worker \
  raft-coordinator-dev-ingest \
  raft-coordinator-dev-observer \
  --query 'services[].{name:serviceName,running:runningCount,desired:desiredCount,status:status}'

aws logs tail /ecs/raft-coordinator-dev/coordinator --follow
aws logs tail /ecs/raft-coordinator-dev/worker --follow
```

Confirm peer registration (the DynamoDB-based replacement for Cloud Map):

```bash
aws dynamodb scan --table-name raft-coordinator-dev-peers \
  --query 'Items[].{id:node_id.S,grpc:grpc_addr.S,leader:is_leader.BOOL}'
```

Each coordinator writes its own row on startup and heartbeats every 10s with
the current `is_leader` bit. Downstream services read this table to discover
the leader.

## Step 6: Submit a test task

The ingest API is on a private subnet. See `LIFECYCLE.md` for the manual
DDB + S3 + SQS recipe that bypasses it entirely, or use one of:

**Option A — ECS Exec into a container:**

```bash
TASK_ARN=$(aws ecs list-tasks --cluster $CLUSTER \
  --service-name raft-coordinator-dev-ingest --query 'taskArns[0]' --output text)
aws ecs execute-command --cluster $CLUSTER --task $TASK_ARN \
  --container ingest --interactive --command "/bin/sh"
# inside the container:
wget -qO- --post-data='{"payload":"aGVsbG8=","priority":3}' \
  --header='Content-Type: application/json' http://localhost:8080/tasks
```

**Option B — chaos-bench client (recommended for experiments):**

The `chaos-bench/` tool submits directly to DynamoDB + S3 + SQS, which works
from any machine with AWS credentials — no VPC access needed.

```bash
cd chaos-bench
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
python main.py bench --rate 60 --duration 30
```

See `chaos-bench/README.md` for the full command set and how each subcommand
maps to the four proposal experiments.

## Scaling to 3 coordinators (for experiments)

After images are pushed:

```bash
cd terraform/environments/dev
terraform apply -var='coordinator_count=3'
```

Or persist it by copying `terraform.tfvars.example` to `terraform.tfvars` and
setting `coordinator_count = 3`. Raft elects a leader automatically once all
three registrations appear in the peers table.

## Non-sandbox accounts

For any account that is **not** an AWS Academy Learner Lab, the `LabRole`
does not exist. Override the task role:

```bash
# copy the example
cp terraform.tfvars.example terraform.tfvars
```

Edit `terraform.tfvars`:

```hcl
lab_role_arn = "arn:aws:iam::123456789012:role/ecsTaskExecutionRole"
```

The role must have:
- `AmazonECSTaskExecutionRolePolicy` (pull from ECR, write to CloudWatch Logs)
- Read/write on the tasks/peers/raft-state DynamoDB tables
- Read/write on the task-data + raft-snapshots S3 buckets
- Send/Receive on all three SQS queues

## Monitoring

- **CloudWatch Logs**: One group per service under `/ecs/raft-coordinator-dev/`
- **State Observer**: Publishes to CloudWatch namespace `Driftless/Raft`,
  reads coordinator addresses from the peers DynamoDB table every tick
- **Coordinator `/state`**: JSON with term, role, commit index, log length —
  polled by the observer; also callable directly from inside the VPC

## Teardown

```bash
cd terraform/environments/dev
terraform destroy
```

ECR repositories have `force_delete = true`, so pushed images are removed.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| ECS task keeps restarting | Image missing, or health check failing | `aws logs tail /ecs/raft-coordinator-dev/coordinator --since 5m` |
| Tasks stuck in PENDING | Coordinator not polling SQS | Check `SQS_INGEST_URL` in coordinator logs; confirm leader |
| Worker not processing | Assignment queue empty | Look for `proposed` + `committed` entries in leader's log |
| Coordinator waits forever for peers | Peer DDB row missing or stale | Scan `raft-coordinator-dev-peers`; a fresh `heartbeat_ts` within 30s means healthy |
| `AccessDeniedException` | `lab_role_arn` wrong or role missing perms | See *Non-sandbox accounts* above |
| Image pull fails | ECR login expired | Re-run `aws ecr get-login-password` (tokens last 12h) |
| `voc-cancel-cred` explicit deny | voclabs session expired | Restart the Learner Lab and re-export creds |

## AWS Academy notes

- The Learner Lab session expires after ~4 hours. Resources are stopped but
  not deleted; restart the lab and re-export creds.
- You cannot create IAM roles. Every service uses the pre-existing `LabRole`,
  which is what the Terraform default resolves to.
- One NAT gateway (~$0.045/hr) — dev uses a single AZ to minimize cost.
