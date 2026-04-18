# Experiment 2 — 5-node coordinator cluster
# Usage: terraform apply -var-file=5node.tfvars

coordinator_count = 5
account_id        = "YOUR_ACCOUNT_ID"
coordinator_image = "YOUR_ACCOUNT_ID.dkr.ecr.us-east-1.amazonaws.com/raft-coordinator:latest"
