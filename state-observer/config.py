"""config.py — Configuration loaded from environment variables.

Required:
  COORDINATOR_ADDRS   Comma-separated host:port for each coordinator's HTTP state
                      endpoint.  e.g. "coordinator-a:8080,coordinator-b:8080,coordinator-c:8080"

Optional:
  POLL_INTERVAL_S     How often to query coordinators (default: 2.0)
  CW_NAMESPACE        CloudWatch custom metric namespace (default: Driftless/Raft)
  CW_PUBLISH_INTERVAL_S  Seconds between CloudWatch PutMetricData calls (default: 60)
  PROMETHEUS_PORT     Port for the Prometheus /metrics endpoint (default: 9090)
  AWS_REGION          AWS region for CloudWatch + SNS (default: us-east-1)
  SNS_TOPIC_ARN       SNS topic ARN for split-brain alerts (optional)
  DISABLE_CLOUDWATCH  Set to "1" to skip CloudWatch publishing (useful locally)
  DISABLE_SNS         Set to "1" to skip SNS alerts (useful locally)
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import List


@dataclass
class Config:
    coordinator_addrs: List[str]
    peers_table: str = ""
    coordinator_node_ids: List[str] = field(default_factory=list)
    poll_interval_s: float = 2.0
    cw_namespace: str = "Driftless/Raft"
    cw_publish_interval_s: float = 60.0
    prometheus_port: int = 9090
    aws_region: str = "us-east-1"
    sns_topic_arn: str = ""
    disable_cloudwatch: bool = False
    disable_sns: bool = False

    @classmethod
    def from_env(cls) -> "Config":
        raw = os.environ.get("COORDINATOR_ADDRS", "localhost:8080")
        addrs = [a.strip() for a in raw.split(",") if a.strip()]
        ids_raw = os.environ.get("COORDINATOR_NODE_IDS", "")
        node_ids = [i.strip() for i in ids_raw.split(",") if i.strip()]
        return cls(
            coordinator_addrs=addrs,
            peers_table=os.environ.get("PEERS_TABLE", ""),
            coordinator_node_ids=node_ids,
            poll_interval_s=float(os.environ.get("POLL_INTERVAL_S", "2")),
            cw_namespace=os.environ.get("CW_NAMESPACE", "Driftless/Raft"),
            cw_publish_interval_s=float(os.environ.get("CW_PUBLISH_INTERVAL_S", "60")),
            prometheus_port=int(os.environ.get("PROMETHEUS_PORT", "9090")),
            aws_region=os.environ.get("AWS_REGION", "us-east-1"),
            sns_topic_arn=os.environ.get("SNS_TOPIC_ARN", ""),
            disable_cloudwatch=os.environ.get("DISABLE_CLOUDWATCH", "0") == "1",
            disable_sns=os.environ.get("DISABLE_SNS", "0") == "1",
        )

    def __str__(self) -> str:
        return (
            f"coordinators={self.coordinator_addrs} "
            f"poll={self.poll_interval_s}s "
            f"prometheus=:{self.prometheus_port} "
            f"cw_namespace={self.cw_namespace} "
            f"sns={'disabled' if self.disable_sns else self.sns_topic_arn or 'not set'}"
        )
