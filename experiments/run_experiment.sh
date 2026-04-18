#!/usr/bin/env bash
# run_experiment.sh — Experiment 2: Throughput & Latency vs. Coordinator Cluster Size
#
# Runs load at 100 / 500 / 1000 tasks/min against a deployed coordinator cluster
# and collects p50/p95/p99 assignment latency + Raft replication lag per follower.
#
# Prerequisites:
#   1. Build the load generator:
#        cd experiments/load-generator && go build -o ../../bin/loadgen . && cd -
#
#   2. Install metrics-collector deps:
#        pip install -r experiments/metrics-collector/requirements.txt
#
#   3. Build & push the coordinator image:
#        cd raft
#        go mod tidy            # resolves new AWS SDK deps + updates go.sum
#        docker build -t raft-coordinator .
#        docker tag raft-coordinator $ECR_URI
#        docker push $ECR_URI
#        cd -
#
#   4. Deploy infrastructure (choose cluster size via tfvars):
#        cd terraform/environments/experiment
#        terraform init
#        terraform apply -var-file=3node.tfvars   # or 5node.tfvars
#        cd -
#
#   5. Deploy task-ingest-api and worker-node separately and set INGEST_URL.
#
# Environment variables (required):
#   CLUSTER_SIZE      — 3 or 5
#   INGEST_URL        — http://<task-ingest-api>/tasks
#   TASKS_TABLE       — DynamoDB table name (from terraform output tasks_table_name)
#   COORDINATOR_ADDRS — comma-separated coordinator HTTP URLs for replication lag
#                       e.g. "http://coordinator-a:8080,http://coordinator-b:8080"
#
# Example:
#   CLUSTER_SIZE=3 \
#   INGEST_URL=http://10.2.1.50:8080/tasks \
#   TASKS_TABLE=raft-coordinator-experiment-tasks \
#   COORDINATOR_ADDRS=http://10.2.11.10:8080,http://10.2.11.11:8080,http://10.2.11.12:8080 \
#   bash experiments/run_experiment.sh

set -euo pipefail

CLUSTER_SIZE="${CLUSTER_SIZE:?Set CLUSTER_SIZE=3 or 5}"
INGEST_URL="${INGEST_URL:?Set INGEST_URL to the task ingest API endpoint}"
TASKS_TABLE="${TASKS_TABLE:?Set TASKS_TABLE to the DynamoDB tasks table name}"
COORDINATOR_ADDRS="${COORDINATOR_ADDRS:-}"

RATES=(100 500 1000)
WARMUP_S=30       # warm-up window (not measured)
MEASURE_S=120     # measurement window per rate
COOLDOWN_S=90     # drain time before collecting metrics

RESULTS_DIR="experiments/results"
mkdir -p "$RESULTS_DIR"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_FILE="$RESULTS_DIR/exp2_${CLUSTER_SIZE}node_${TIMESTAMP}.csv"

LOADGEN="./bin/loadgen"
COLLECTOR="python3 experiments/metrics-collector/collect.py"

if [[ ! -x "$LOADGEN" ]]; then
    echo "ERROR: load generator not found at $LOADGEN"
    echo "Run: cd experiments/load-generator && go build -o ../../bin/loadgen . && cd -"
    exit 1
fi

echo "================================================================"
echo " Experiment 2 — ${CLUSTER_SIZE}-node coordinator cluster"
echo " Rates: ${RATES[*]} tasks/min"
echo " Results → $RESULTS_FILE"
echo "================================================================"

for RATE in "${RATES[@]}"; do
    LABEL="${CLUSTER_SIZE}node_${RATE}tpm"
    echo ""
    echo "--- Rate: ${RATE} tasks/min (label: ${LABEL}) ---"

    # Warm-up: run load but don't count these tasks in the results.
    # They allow the Raft cluster to stabilize and the queues to reach steady state.
    echo "  [1/3] warm-up (${WARMUP_S}s, not measured)…"
    "$LOADGEN" -rate="$RATE" -duration="${WARMUP_S}s" -url="$INGEST_URL" || true

    # Measurement window.
    echo "  [2/3] measuring (${MEASURE_S}s)…"
    "$LOADGEN" -rate="$RATE" -duration="${MEASURE_S}s" -url="$INGEST_URL"

    # Let the queues drain so all submitted tasks are assigned before we sample.
    echo "  [3/3] draining queues (${COOLDOWN_S}s)…"
    sleep "$COOLDOWN_S"

    # Collect metrics from DynamoDB + coordinator /state.
    echo "  collecting metrics…"
    $COLLECTOR \
        --table "$TASKS_TABLE" \
        --label "$LABEL" \
        --coordinator "$COORDINATOR_ADDRS" \
        --output "$RESULTS_FILE"
done

echo ""
echo "================================================================"
echo " Experiment ${CLUSTER_SIZE}-node complete."
echo " Results: $RESULTS_FILE"
echo ""
echo " Re-deploy with the other cluster size and run again:"
echo "   cd terraform/environments/experiment"
if [[ "$CLUSTER_SIZE" == "3" ]]; then
    echo "   terraform apply -var-file=5node.tfvars"
    echo "   CLUSTER_SIZE=5 bash experiments/run_experiment.sh"
else
    echo "   terraform apply -var-file=3node.tfvars"
    echo "   CLUSTER_SIZE=3 bash experiments/run_experiment.sh"
fi
echo "================================================================"
