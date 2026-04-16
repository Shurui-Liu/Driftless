# Driftless Deployment Guide

Deploys the full Driftless system to AWS using a student (AWS Academy) account.

## Prerequisites

- AWS CLI v2 configured with your Learner Lab credentials
- Terraform >= 1.6
- Docker
- Go 1.25+ (only needed if building locally without Docker)

Verify your setup:

```bash
aws sts get-caller-identity
terraform -version
docker info
```

## Step 1: Get your AWS account ID

```bash
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=us-east-1
echo "Account: $ACCOUNT_ID"
```

## Step 2: Configure Terraform

Edit `terraform/environments/dev/main.tf` line 44 — replace `YOUR_ACCOUNT_ID` with your actual account ID:

```hcl
lab_role_arn = "arn:aws:iam::${ACCOUNT_ID}:role/LabRole"
```

The double colon is correct (IAM ARNs have no region field). Note that the role name on AWS Academy is `LabRole` (capital L and R).

## Step 3: Create infrastructure (first apply)

This creates the VPC, subnets, SQS queues, DynamoDB tables, S3 buckets, ECR repositories, and ECS cluster. ECS services will fail to start because no images are pushed yet — that's expected.

```bash
cd terraform/environments/dev
terraform init
terraform apply
```

Save the outputs — you'll need the ECR URLs:

```bash
terraform output
```

## Step 4: Build and push Docker images

Log in to ECR:

```bash
aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com
```

Build and push all four images from the project root:

```bash
cd /path/to/Driftless

# Coordinator
docker build -t $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-coordinator:latest ./raft
docker push $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-coordinator:latest

# Worker
docker build -t $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-worker:latest ./worker-node
docker push $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-worker:latest

# Ingest API
docker build -t $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-ingest:latest ./task-ingest-api
docker push $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-ingest:latest

# State Observer
docker build -t $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-observer:latest ./state-observer
docker push $ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/raft-coordinator-dev-observer:latest
```

> If building on Apple Silicon (M1/M2/M3), add `--platform linux/amd64` to each `docker build` command since ECS Fargate runs x86_64.

## Step 5: Deploy ECS services

Re-apply Terraform so ECS pulls the newly pushed images:

```bash
cd terraform/environments/dev
terraform apply
```

Force a new deployment if tasks are stuck on the old (missing) image:

```bash
CLUSTER=$(terraform output -raw ecs_cluster_name)

aws ecs update-service --cluster $CLUSTER --service raft-coordinator-dev-coordinator-a --force-new-deployment
aws ecs update-service --cluster $CLUSTER --service raft-coordinator-dev-worker --force-new-deployment
aws ecs update-service --cluster $CLUSTER --service raft-coordinator-dev-ingest --force-new-deployment
aws ecs update-service --cluster $CLUSTER --service raft-coordinator-dev-observer --force-new-deployment
```

## Step 6: Verify the system is running

Check ECS service status:

```bash
aws ecs list-services --cluster $CLUSTER
aws ecs describe-services --cluster $CLUSTER --services \
  raft-coordinator-dev-coordinator-a \
  raft-coordinator-dev-worker \
  raft-coordinator-dev-ingest \
  raft-coordinator-dev-observer \
  --query 'services[].{name:serviceName,running:runningCount,desired:desiredCount,status:status}'
```

Check coordinator logs:

```bash
aws logs tail /ecs/raft-coordinator-dev/coordinator --follow
```

Check worker logs:

```bash
aws logs tail /ecs/raft-coordinator-dev/worker --follow
```

## Step 7: Submit a test task

The ingest API runs on a private subnet (no public IP). To test, either:

**Option A: From a bastion / Cloud9 in the same VPC:**

```bash
curl -X POST http://<ingest-task-private-ip>:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"payload":"aGVsbG8gd29ybGQ=","priority":5}'
```

**Option B: Add an ALB (recommended for demos):**

Add a public ALB in front of the ingest service in Terraform (uses the public subnets already provisioned). This is not included by default to avoid extra cost.

**Option C: Use ECS Exec to run curl inside a container:**

```bash
TASK_ARN=$(aws ecs list-tasks --cluster $CLUSTER --service-name raft-coordinator-dev-ingest --query 'taskArns[0]' --output text)
aws ecs execute-command --cluster $CLUSTER --task $TASK_ARN --container ingest --interactive --command "/bin/sh"
# Inside the container:
# wget -qO- --post-data='{"payload":"aGVsbG8=","priority":3}' --header='Content-Type: application/json' http://localhost:8080/tasks
```

After submitting, watch the task flow through the system:

```bash
# Check DynamoDB for the task
aws dynamodb scan --table-name raft-coordinator-dev-tasks --max-items 5

# Check SQS for pending messages
aws sqs get-queue-attributes \
  --queue-url $(terraform output -raw ingest_queue_url) \
  --attribute-names ApproximateNumberOfMessages
```

## Scaling to 3 coordinators (production-like)

In `terraform/environments/dev/main.tf`, change:

```hcl
coordinator_count = 3
```

Then `terraform apply`. This creates three coordinator task definitions (coordinator-a, -b, -c), each aware of the other two via `PEER_ADDRS`. Raft will elect a leader automatically.

## Monitoring

- **CloudWatch Logs**: Each service has its own log group under `/ecs/raft-coordinator-dev/`
- **State Observer**: Publishes metrics to CloudWatch namespace `Driftless/Raft` and exposes Prometheus metrics on port 9090
- **Coordinator /state endpoint**: Returns JSON with term, role, commit index — polled by the observer

## Teardown

```bash
cd terraform/environments/dev
terraform destroy
```

This removes all AWS resources. ECR repos have `force_delete = true` so images are cleaned up automatically.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| ECS task keeps restarting | Health check failing | Check logs: `aws logs tail /ecs/raft-coordinator-dev/coordinator --since 5m` |
| Tasks stuck in PENDING | Coordinator not polling SQS | Verify `SQS_INGEST_URL` env var in coordinator logs |
| Worker not processing | Assignment queue empty | Check coordinator is leader and dispatching: look for "proposed" in logs |
| Coordinator can't reach peers | Cloud Map DNS not ready | Wait 30-60s after deploy; check `nslookup coordinator-a.raft.local` from inside VPC |
| `AccessDeniedException` | LabRole missing permissions | Verify `lab_role_arn` is set to `arn:aws:iam::<account>:role/LabRole` |
| Image pull fails | ECR login expired | Re-run `aws ecr get-login-password` (tokens expire after 12h) |

## AWS Academy notes

- Your Learner Lab session expires after ~4 hours. Running resources are stopped but not deleted — re-start the lab and resources resume.
- You cannot create IAM roles. All services use the pre-existing `LabRole`.
- NAT gateway costs ~$0.045/hr. The dev config uses one NAT gateway to minimize cost.
