"""submitter.py — task submission + completion polling.

Bypasses the ingest API (private NLB) and drives S3 + DDB + SQS directly,
matching what the ingest API does internally. Lets a bench run operate
from a laptop against any Terraform-provisioned environment.
"""

from __future__ import annotations

import datetime as dt
import uuid
from dataclasses import dataclass

import boto3

from config import Config


@dataclass
class SubmitResult:
    task_id: str
    submitted_ns: int


class Submitter:
    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        session = boto3.session.Session(region_name=cfg.aws_region)
        self.s3 = session.client("s3")
        self.ddb = session.client("dynamodb")
        self.sqs = session.client("sqs")

    def submit(self, payload: bytes, priority: int = 5) -> SubmitResult:
        import time

        task_id = str(uuid.uuid4())
        key = f"tasks/{task_id}/payload"
        now = dt.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ")

        self.s3.put_object(Bucket=self.cfg.payload_bucket, Key=key, Body=payload)
        self.ddb.put_item(
            TableName=self.cfg.tasks_table,
            Item={
                "task_id": {"S": task_id},
                "status": {"S": "PENDING"},
                "priority": {"N": str(priority)},
                "s3_key": {"S": key},
                "created_at": {"S": now},
                "updated_at": {"S": now},
            },
        )
        self.sqs.send_message(
            QueueUrl=self.cfg.ingest_queue_url,
            MessageBody=f'{{"task_id":"{task_id}","priority":{priority}}}',
        )
        return SubmitResult(task_id=task_id, submitted_ns=time.time_ns())

    def poll_status(self, task_id: str) -> str | None:
        resp = self.ddb.get_item(
            TableName=self.cfg.tasks_table,
            Key={"task_id": {"S": task_id}},
            ConsistentRead=True,
        )
        item = resp.get("Item")
        if not item:
            return None
        return item.get("status", {}).get("S")
