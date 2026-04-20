# Experiment 2 — 3-node coordinator cluster
# Usage: terraform apply -var-file=3node.tfvars \
#   -var="account_id=$ACCOUNT_ID" \
#   -var="coordinator_image=$ECR/raft-coordinator:latest" \
#   -var="worker_image=$ECR/raft-worker:latest" \
#   -var="ingest_image=$ECR/raft-ingest:latest" \
#   -var="observer_image=$ECR/raft-observer:latest"

coordinator_count = 3
account_id        = "471112634889"
coordinator_image = "471112634889.dkr.ecr.us-east-1.amazonaws.com/raft-coordinator:latest"
worker_image      = "471112634889.dkr.ecr.us-east-1.amazonaws.com/raft-worker:latest"
ingest_image      = "471112634889.dkr.ecr.us-east-1.amazonaws.com/raft-ingest:latest"
observer_image    = "471112634889.dkr.ecr.us-east-1.amazonaws.com/raft-observer:latest"
