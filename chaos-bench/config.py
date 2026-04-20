"""config.py — environment-driven configuration for the bench/chaos client.

All AWS resource names default to the Terraform `dev` environment. Override
any value via the matching env var.
"""

from __future__ import annotations

import os
from dataclasses import dataclass


@dataclass
class Config:
    aws_region: str
    account_id: str

    # Resource names (match terraform/environments/dev outputs)
    cluster_name: str          # ECS cluster
    tasks_table: str           # DynamoDB task metadata
    peers_table: str           # DynamoDB peer registry
    payload_bucket: str        # S3 bucket for payloads + results
    snapshot_bucket: str       # S3 bucket for Raft snapshots
    ingest_queue_url: str      # SQS ingest queue

    # ECS service names (used by chaos injector)
    coordinator_services: list[str]  # e.g. ["raft-coordinator-dev-coordinator-a", ...]
    worker_service: str
    ingest_service: str

    # Output
    history_dir: str           # where JSONL histories are written

    @classmethod
    def from_env(cls) -> "Config":
        region = os.environ.get("AWS_REGION", "us-east-1")
        account = os.environ.get("AWS_ACCOUNT_ID", "")
        env = os.environ.get("DRIFTLESS_ENV", "dev")
        project = os.environ.get("DRIFTLESS_PROJECT", "raft-coordinator")
        prefix = f"{project}-{env}"

        coord_ids = os.environ.get(
            "COORDINATOR_SERVICES",
            f"{prefix}-coordinator-a",
        ).split(",")

        return cls(
            aws_region=region,
            account_id=account,
            cluster_name=os.environ.get("CLUSTER_NAME", f"{prefix}-cluster"),
            tasks_table=os.environ.get("TASKS_TABLE", f"{prefix}-tasks"),
            peers_table=os.environ.get("PEERS_TABLE", f"{prefix}-peers"),
            payload_bucket=os.environ.get(
                "PAYLOAD_BUCKET", f"{prefix}-task-data-{account}"
            ),
            snapshot_bucket=os.environ.get(
                "SNAPSHOT_BUCKET", f"{prefix}-raft-snapshots-{account}"
            ),
            ingest_queue_url=os.environ.get(
                "INGEST_QUEUE_URL",
                f"https://sqs.{region}.amazonaws.com/{account}/{prefix}-ingest",
            ),
            coordinator_services=[s.strip() for s in coord_ids if s.strip()],
            worker_service=os.environ.get("WORKER_SERVICE", f"{prefix}-worker"),
            ingest_service=os.environ.get("INGEST_SERVICE", f"{prefix}-ingest"),
            history_dir=os.environ.get("HISTORY_DIR", "./histories"),
        )
