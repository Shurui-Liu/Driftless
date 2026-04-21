"""chaos.py — fault injection via the ECS control plane.

Injections implemented:
  • kill_leader   — stop a random running ECS task of the leader service
  • kill_follower — same for any non-leader coordinator
  • kill_worker   — stop a worker task to trigger reassignment

Leader identity is discovered by polling each coordinator's /state HTTP
endpoint (the same endpoint the state observer uses).
"""

from __future__ import annotations

import random
import time
from dataclasses import dataclass

import boto3
from botocore.exceptions import ClientError

from config import Config


@dataclass
class KillResult:
    service: str
    task_arn: str
    killed_ns: int


class ChaosInjector:
    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        session = boto3.session.Session(region_name=cfg.aws_region)
        self.ecs = session.client("ecs")
        self.ddb = session.client("dynamodb")
        self.sqs = session.client("sqs")

    def _running_tasks(self, service: str) -> list[str]:
        resp = self.ecs.list_tasks(
            cluster=self.cfg.cluster_name,
            serviceName=service,
            desiredStatus="RUNNING",
        )
        return resp.get("taskArns", [])

    def stop_random_task(self, service: str, reason: str) -> KillResult | None:
        arns = self._running_tasks(service)
        if not arns:
            return None
        victim = random.choice(arns)
        self.ecs.stop_task(
            cluster=self.cfg.cluster_name,
            task=victim,
            reason=reason,
        )
        return KillResult(service=service, task_arn=victim, killed_ns=time.time_ns())

    def kill_leader(self, leader_service: str) -> KillResult | None:
        return self.stop_random_task(leader_service, "chaos: leader kill")

    def kill_follower(self, non_leader_services: list[str]) -> KillResult | None:
        if not non_leader_services:
            return None
        svc = random.choice(non_leader_services)
        return self.stop_random_task(svc, "chaos: follower kill")

    def kill_worker(self) -> KillResult | None:
        return self.stop_random_task(self.cfg.worker_service, "chaos: worker kill")

    def inject_sqs_redelivery(self, queue_url: str, count: int = 5) -> int:
        """Force up to `count` in-flight ingest messages to redeliver immediately.

        Simulates an SQS visibility timeout expiry: we receive messages (making
        them invisible) then immediately reset visibility to 0, so the ingest
        service sees the same task_id again. The Raft dispatcher's deduplication
        map must prevent a second assignment from being published.
        """
        resp = self.sqs.receive_message(
            QueueUrl=queue_url,
            MaxNumberOfMessages=min(count, 10),
            VisibilityTimeout=10,
        )
        msgs = resp.get("Messages", [])
        redelivered = 0
        for msg in msgs:
            try:
                self.sqs.change_message_visibility(
                    QueueUrl=queue_url,
                    ReceiptHandle=msg["ReceiptHandle"],
                    VisibilityTimeout=0,
                )
                redelivered += 1
            except ClientError as e:
                # Receipt handle already invalid — message was processed and deleted.
                # This is expected when the coordinator is fast; skip silently.
                code = e.response.get("Error", {}).get("Code", "")
                if code not in ("InvalidParameterValue", "ReceiptHandleIsInvalid"):
                    raise
        return redelivered
