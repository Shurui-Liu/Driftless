# Experiment 3 — Exactly-Once Semantics Under Fault Injection

**System:** Driftless — Raft-coordinated distributed task queue on AWS ECS Fargate  
**Run ID:** exp3-20260421T001513  
**Date:** April 21, 2026

---

## Purpose and Tradeoff

### What This Experiment Tests

Driftless makes a strong claim: every submitted task is dispatched to a worker **exactly once**, even when coordinators crash, network links drop, or the message queue re-delivers messages. Experiment 3 tests whether that claim holds under active fault injection — simulated crashes and forced message redeliveries — rather than in a quiescent cluster.

The correctness property under test is **linearizability**: every external observation of task state (PENDING → ASSIGNED → COMPLETED) must be explainable as if the operations occurred in some legal sequential order on an idealized register. No task should ever appear to complete twice, and no completed task should ever regress to an earlier state.

### The Tradeoff Being Explored

Exactly-once dispatch requires layered coordination that introduces latency in exchange for safety:

| Layer | Mechanism | Cost |
|-------|-----------|------|
| Message deduplication | SQS `MessageDeduplicationId` per task UUID | Prevents double-enqueue at publish time |
| Dispatch gate | DynamoDB conditional write (`status = PENDING`) | Serializes competing dispatchers; any duplicate dispatch attempt fails atomically |
| State replication | Raft log replication across 5 coordinator nodes | Every state transition committed to quorum before acknowledged |
| In-memory dedup map | Raft leader tracks dispatched IDs | Prevents re-dispatch of re-delivered SQS messages |

The **safety–liveness tension** is central: Raft's quorum requirement means the cluster refuses to commit state transitions during a leader election (approximately 150–300 ms). If faults arrive faster than the cluster can recover, tasks stall — not incorrectly processed, but delayed. The experiment probes whether the recovery window is sufficient to keep the liveness guarantee (eventual completion of all tasks) while the safety guarantee (no duplicates) holds unconditionally.

### Limitations of the Approach

1. **Scale:** 500 tasks at 250 tasks/min is a fraction of the 10,000-task production target. Duplicate detection at scale could surface hash collisions or DynamoDB hot-partition effects not visible here.
2. **Fault intensity:** One leader kill and one follower kill were injected during a ~156-second window. Production conditions may involve cascading failures, split-brain scenarios from network partitions, or concurrent node losses.
3. **Cold-start constraint:** ECS Fargate containers take ~50 seconds to restart. Kill periods were set to 90 s (leader) and 120 s (follower) to ensure a replacement node is online before the next kill. This prevents testing back-to-back leader kills, which would require quorum with only 3 of 5 nodes.
4. **Fault isolation:** The experiment cannot cleanly attribute latency spikes to a specific fault type (leader kill vs. follower kill vs. SQS redeliver) because all three faults are injected concurrently.
5. **No network partition:** ECS `StopTask` kills the entire container process. True network partitions (where a node appears alive to some peers but not others) are not tested and represent a harder adversarial condition for Raft.

---

## Experimental Setup

**Cluster:** 5-node Raft coordinator cluster on AWS ECS Fargate (us-east-1). Raft election timeout 150–300 ms, heartbeat 50 ms. Snapshots persisted to S3.

**Workload:** 500 tasks submitted at 250 tasks/min via the chaos-bench load generator.

**Fault schedule:**

| Fault | Period | Count Injected |
|-------|--------|----------------|
| SQS message re-delivery (visibility reset to 0) | Every 20 s | 6 events |
| Follower kill (ECS StopTask on non-leader) | 120 s period | 1 kill |
| Leader kill (ECS StopTask on leader) | 90 s period | 1 kill |

**Correctness checker:** Porcupine linearizability checker (Go, `anishathalye/porcupine`) consumed all 9,584 poll observations from `history.jsonl` and verified the full execution history against a sequential task-state model.

---

## Results

### Outcome Summary

| Metric | Value |
|--------|-------|
| Tasks submitted | 500 |
| Tasks completed | **500 (100%)** |
| Tasks timed out | 0 |
| Tasks lost | 0 |
| Duplicate dispatches | **0** |
| Linearizability violations | **0** |
| Porcupine observations checked | 9,584 |
| Leader kills injected | 1 |
| Follower kills injected | 1 |
| SQS redeliveries injected | 6 |

**Figure 1 — Summary Dashboard** (`fig1_summary_dashboard.png`): Three-panel overview showing task outcome breakdown (100% COMPLETED), safety scorecard (0 violations across all three safety invariants), and fault event counts.

![Summary Dashboard](fig1_summary_dashboard.png)

---

### Chaos Event Timeline

The run lasted **155.6 seconds**. Key events:

| Time (s) | Event |
|----------|-------|
| 0 | Run start; coordinator-d elected leader |
| 10–93 | SQS redeliveries every ~20 s (5 events) |
| 89.1 | **Follower kill:** coordinator-a stopped |
| 97.9 | **Leader kill:** coordinator-d stopped |
| 100.4 | New leader **coordinator-b** elected (2.5 s after kill) |
| 114.3 | SQS redeliver event (1 msg, post-failover) |
| 155.6 | Run end — all 500 tasks complete |

The leader failover took **2.5 seconds** from kill to new leader advertisement. During this window the cluster rejected commit requests (quorum unavailable), causing tasks in-flight to briefly stall but not to fail or duplicate.

**Figure 2 — Task State Timeline** (`fig2_state_timeline.png`): Stacked area chart showing cumulative PENDING, ASSIGNED, and COMPLETED counts over wall time. Vertical markers indicate follower kill (orange, t=89 s) and leader kill (red, t=98 s). The COMPLETED curve rises monotonically through both fault events with no visible stall plateau, indicating the failover completed before any tasks had time to time out.

![State Timeline](fig2_state_timeline.png)

---

### End-to-End Latency

Latency is measured from task submission to the first poll observation that returns `COMPLETED`.

| Percentile | Latency |
|-----------|---------|
| p50 | 10.2 s |
| p75 | 18.6 s |
| p90 | 22.9 s |
| p95 | 25.6 s |
| p99 | 31.6 s |
| max | 38.2 s |
| min | 0.7 s |

**Figure 3 — Latency CDF** (`fig3_latency_cdf.png`): Empirical CDF of end-to-end completion latency for all 500 tasks. The distribution has a long right tail extending to 38.2 s driven by tasks that were either (a) submitted just before a fault event and had to wait through re-election, or (b) affected by SQS visibility timeouts (up to 30 s) during redelivery injection.

![Latency CDF](fig3_latency_cdf.png)

The minimum latency (0.7 s) reflects tasks processed immediately by the standing leader from SQS with no queuing delay. The p99 (31.6 s) approximately matches the SQS default visibility timeout (30 s), suggesting the tail is dominated by the re-delivery mechanics rather than Raft election latency, which was only ~2.5 s.

---

### Safety and Liveness Under Faults

**Figure 4 — Chaos Proof** (`fig4_chaos_proof.png`): Two panels. Left: throughput (tasks completed per 10-second bin) over time, with fault events marked — throughput dips to near zero briefly at t≈98 s (leader kill) and recovers within one 10-second bin. Right: safety metric bars showing 0 duplicate dispatches, 0 stuck tasks, and 0 linearizability violations.

![Chaos Proof](fig4_chaos_proof.png)

The throughput dip at the leader kill event is the only observable liveness impact. The cluster drained its in-flight tasks and resumed normal throughput within approximately 10 seconds of the kill, consistent with the 2.5-second election time plus the time to re-dispatch any tasks whose SQS messages became re-visible.

---

## Analysis

### Safety Guarantee: Unconditional

The 0 duplicate dispatch result — confirmed both by the `duplicate_dispatches` counter in the summary and by 9,584 Porcupine linearizability checks — demonstrates that the three-layer deduplication mechanism is effective under the tested fault conditions.

The critical case is an SQS message that becomes re-visible after the coordinator that originally dispatched it is killed. This would naturally cause a second dispatch attempt. The experiment injected 6 such redeliveries explicitly, and the leader kill created additional implicit redeliveries for any in-flight messages at time-of-kill. In all cases, the DynamoDB conditional write (`attribute_not_exists(status) OR status = PENDING`) caught the duplicate attempt: the task's status was already `ASSIGNED`, so the second write failed atomically. The Raft dedup map provided a second backstop for messages that the same leader might re-receive before DynamoDB reflected the committed state.

### Liveness Guarantee: Conditional on Fault Spacing

Liveness held under the tested conditions (one kill every 90+ seconds, 5-node cluster). The mathematical guarantee is straightforward: with 5 nodes, Raft can tolerate 2 simultaneous failures and still elect a leader with a 3-node quorum. The experiment never had more than 2 nodes simultaneously killed (one follower + one leader at the overlap, briefly). This is the designed fault tolerance boundary.

The 2.5-second re-election time is well within the 60-second task timeout, so no task was at risk of timing out due solely to election latency. The latency tail (p99=31.6 s) is attributable to SQS mechanics rather than Raft, and remains within the 60-second timeout budget.

### Latency Decomposition

The latency distribution reflects three overlapping components:

1. **Baseline processing time:** ~0.7–5 s for tasks processed immediately (queue not saturated, no faults). Captures SQS polling interval and DynamoDB write latency.
2. **SQS visibility timeout:** Tasks whose messages were re-delivered by the chaos injector had their visibility clock reset, adding up to 30 s to dispatch time. This explains the p90–p99 range (22–31 s).
3. **Leader failover:** The 2.5-second election gap would affect only tasks whose dispatch was interrupted at exactly the kill moment. Given the 250 tasks/min submission rate, roughly 10 tasks were in-flight during the 2.5-second window, contributing a small cluster of points at the right tail.

### What the Evidence Shows

The experiment provides direct evidence that:

1. **Safety is separable from availability.** During the 2.5-second leader election period, the system refused to dispatch (liveness degraded) but did not dispatch incorrectly (safety maintained). This is the core Raft promise: correctness is never violated; only availability is temporarily reduced.

2. **Exactly-once dispatch survives the interaction of two independent fault sources.** SQS at-least-once delivery and ECS node crashes both independently threaten duplicate dispatch; together they could compound. The layered deduplication handled both simultaneously.

3. **The tail latency budget is adequate.** With a 60-second timeout and p99=31.6 s, there is a 28-second margin before any task would time out. This margin shrinks under sustained or concurrent faults, but for the tested scenario it is comfortable.

---

## Conclusions

Experiment 3 confirms that Driftless provides **exactly-once dispatch semantics under the tested fault conditions**: 500/500 tasks completed with zero duplicates and zero linearizability violations, across one leader kill, one follower kill, and six SQS re-delivery injections.

The central tradeoff — safety at the cost of temporary availability — is validated empirically: the cluster paused for 2.5 seconds after the leader kill (liveness dip) but never produced an incorrect result (safety preserved). The 9,584-observation Porcupine check provides a model-level guarantee, not just an application-level counter.

**Limitations of this analysis:** The test is bounded by scale (500 tasks, not 10,000), fault intensity (one of each kill type, not cascading failures), and the ECS cold-start constraint (90 s between kills). A more adversarial test would inject back-to-back leader kills, network partitions, or clock skew to probe the boundaries of Raft's guarantees. The current result provides strong evidence of correctness within the tested envelope, but should not be extrapolated to conditions outside it without further experimentation.
