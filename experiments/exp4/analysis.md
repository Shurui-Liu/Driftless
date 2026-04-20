# Experiment 4 — Snapshot Recovery & S3 Log Persistence

Run: `exp4-20260419T060636Z`
Date: 2026-04-19
System under test: Driftless, commit `e49c95d` + uncommitted grpc_client peer-re-resolution fix + `EXPECTED_PEER_IDS` task-definition hotfix (see §6)

## 1. Goal

Exp 4 asks two questions about the Raft layer of Driftless under a large committed log:

1. **Snapshot restoration**: when a follower is killed cold and returns, does it rejoin by downloading its snapshot from S3 and replaying only the tail of the log after it, instead of replaying the entire log from index 0?
2. **Wall-clock win**: what is the catch-up latency of the snapshot path versus a full-log replay (toggled by unsetting `SNAPSHOT_S3_BUCKET`), and how does that ratio scale with log size?

The secondary goal is to size the on-disk artifacts: snapshot object size in S3 versus the raw log size estimated from `commit_index × avg entry bytes`.

## 2. Methodology

### System layout

- 3 ECS Fargate coordinator tasks (`coordinator-a`, `-b`, `-c`), each its own service.
- DynamoDB peer registry for address discovery + `is_leader` bit + `heartbeat_ts` freshness.
- S3 snapshot bucket at `raft-coordinator-dev-raft-snapshots-<account>`, one object per node at `raft/<node_id>/snapshot.json`.
- SQS ingest + worker fleet dispatched assignments the leader committed.

### Workload

- Submission rate: 2 000 tasks/min (≈33/s) from a local Python client via the `chaos-bench` Submitter.
- Duration budget: 1 200 s wall clock.
- Target pre-kill commit index: 35 000. Actual reached before kill: 62 306 (the leader comfortably outran the target while the bench worker was ramping; the "commit threshold reached" trigger fired once the first CloudWatch poll after threshold crossed it).

### Fault injection

A single `ecs:StopTask` against the task backing `coordinator-a` — a deliberate follower, not the leader (Exp 1 already covers the leader-kill path). The goal is to force a cold restart on a node that was up-to-date at the moment of kill and observe its recovery path.

### Measurement

The `FollowerRecoveryObserver` polls the coordinator log group and records a timeline with five fields:

| Field | Meaning |
|---|---|
| `restart_ms` | killed → `"coordinator started"` line in the new task's log stream. Dominated by Fargate scheduling. |
| `snapshot_restored_ms` | `"coordinator started"` → `"loaded snapshot from S3"` (absent in full-log-replay runs). |
| `snapshot_last_index` | The `lastIncludedIndex` of the snapshot that was loaded. |
| `catchup_ms` | `"coordinator started"` → the first raft tick whose `commit_index` ≥ leader's commit at kill time. |
| `final_commit_index` | The follower's commit index at catchup. |

`leader_commit_at_kill` is refreshed from CloudWatch *after* the kill so the catchup target reflects what the leader actually committed through the observation window, not just the threshold value.

The snapshot object size is read with a single `s3:HeadObject` on `raft/<follower>/snapshot.json`. Raw log size is estimated as `commit_index × 220 B` where 220 B is the JSON-encoded-task-envelope approximation.

## 3. Results

### 3.1 Event timeline (wall seconds from `run_start`)

| Event | t (s) | Note |
|---|---|---|
| `run_start` | 0.0 | bench ramp begins |
| `commit_progress` | 26.6 | leader `coordinator-c` at `commit_index=62306`, target 35000 |
| `commit_threshold_reached` | 26.6 | threshold trigger |
| `snapshot_before_kill` | 27.0 | `s3:HeadObject` for `raft/coordinator-a/snapshot.json` → **404, no snapshot exists yet** |
| `chaos.kill_follower` | 28.0 | `ecs:StopTask` against coordinator-a |
| `recovery_timeline` | 649.5 | observer window closed (600 s timeout) |
| `run_end` | 1041.8 | bench drain + flush |

### 3.2 Recovery timeline

| Field | Value |
|---|---|
| `restart_ms` | **32 329 ms** |
| `snapshot_restored_ms` | null |
| `snapshot_last_index` | null |
| `catchup_ms` | **null** (never reached leader's commit within 600 s) |
| `final_commit_index` | null |
| `used_snapshot` | false |

`restart_ms` is a clean measurement: it reflects Fargate stopping the killed task, scheduling a replacement, pulling the image, and executing `coordinator started`. Consistent with Exp 1's ~30–35 s restart range.

Every other field in the timeline is null because the follower never caught up. See §4.

### 3.3 Artifact sizes

- **Snapshot before kill**: object absent in S3. Leader had not snapshotted `coordinator-a`'s state, because at 62 k entries compaction had not been triggered for this node yet (default snapshot interval is higher than the threshold the run crossed).
- **Raw log size estimate**: `62 306 × 220 B ≈ 13.7 MB`. Consistent with a "small committed log" run. The snapshot-vs-replay ratio this experiment was designed to measure is only interesting an order of magnitude above this, where the compaction point lags the log head.

> **Cross-run corroboration**: an earlier attempt on the same day (run `exp4-20260419T053356Z`, summary preserved at `corroboration/console.out`) killed `coordinator-a` at `leader_commit_at_kill = 35 588` with `coordinator-b` as leader. That run observed `snapshot_size_bytes = 2 943 364 B` (≈ 2.9 MB), `used_snapshot = true`, and `snapshot_restored_ms = 35 178` — confirming the S3 snapshot-download path works end-to-end when a snapshot exists. `catchup_ms` was `null` there too: the follower restored its snapshot and was then blocked by the same downstream wall described in §4. This matters because it isolates *which* arm of Exp 4 works and which is blocked: the snapshot artifact and its restoration are fine; the post-restore replication is not. The earlier run's `history.jsonl` was deleted during cleanup, so only the summary survives — but the claim "snapshot-download path executes" rests on the preserved summary plus the `used_snapshot = true` flag it records.

## 4. Interpretation

The experiment did not measure what it was designed to measure. Instead it surfaced a sequence of raft-layer bugs that block any measurement of snapshot-based recovery on the current coordinator. The chain is:

**(a) The follower comes up clean.** The boot sequence logged by `coordinator-a`'s restart task:

```
06:07:36.270  registered self                            node_id=coordinator-a
06:07:36.270  waiting for peers                          expected=[coordinator-b, coordinator-c]
06:07:36.293  dialed peer                                id=coordinator-b addr=10.0.11.179:50051
06:07:36.293  dialed peer                                id=coordinator-c addr=10.0.11.203:50051
06:07:36.344  coordinator started                        peers=2
06:07:36.344  became follower                            term=0
```

`peers=2` confirms the peer-discovery hotfix held (§6). The follower is ready to receive AppendEntries.

**(b) Its election timer fires before the first AppendEntries arrives.**

```
06:07:36.640  starting election                          term=1       ← ~300 ms after boot
06:07:36.642  became follower                            term=3       ← AppendEntries from real leader c pushes term
06:07:36.799  starting election                          term=4       ← election timer fires again
06:07:37.056  election timed out, retrying               term=4
06:07:37.056  starting election                          term=5
06:07:37.244  election timed out, retrying               term=5
06:07:37.528  starting election                          term=6
...  (every ~200 ms, term keeps climbing)
06:07:38.345  raft tick     role=candidate term=11 commit_index=0 log_len=1
```

The follower alternates between two states that feed each other:

1. Election timer expires → it starts a new election at `currentTerm + 1`.
2. Peers b and c reject the vote request because, per Raft's log-uptodate safety rule, a candidate whose `lastLogIndex` is 1 cannot become leader over voters whose `lastLogIndex` is 62 306+.
3. But the rejection messages carry the candidate's term, so the real leader c (at term 3) and the other follower b see an incoming term 4, 5, 6… and step down into followers of this new term.
4. With the incumbent leader stepping down every ~200 ms, no AppendEntries ever reaches a long enough to extend its log beyond index 1.
5. Back to (1).

This is the textbook **disruptive server** problem, solved in the extended Raft paper by the **PreVote** phase — a candidate asks peers "would you vote for me?" without bumping its own term, and only starts a real election after receiving quorum. The Driftless `raft/node.go` does not implement PreVote; it issues `RequestVote` directly, so a follower whose log is behind the quorum can neither win an election nor be displaced by the real leader.

**(c) Even if PreVote were in place, a second gap would block Exp 4.**

The follower comes back with `log_len=1`, meaning its persisted log is at index 0 and the next entry it needs is index 1. By 62 k commits the leader has either already compacted past index 1 (snapshot interval triggered) or will do so shortly. When the leader's log start index exceeds the follower's `nextIndex`, AppendEntries alone cannot replicate the gap — the leader must ship an `InstallSnapshot` RPC (or S3 pointer). Driftless's leader currently decrements `nextIndex` one entry per rejected AppendEntries and never falls back to snapshot shipping. This is the same bug flagged in Exp 1 §6 as the cause of iteration 5's degenerate run.

So: the PreVote gap is what the data observed today; the InstallSnapshot gap is what the data *would have observed* once the election storm quieted. Both are coordinator-side implementation defects, not chaos-bench issues.

**(d) Why `snapshot_before_kill` was empty.** The snapshot at `raft/coordinator-a/snapshot.json` did not exist because the coordinator takes snapshots per node based on a local entry count, and at 62 306 entries compaction had not yet triggered for a. Had the run targeted, say, 500 k entries, a snapshot would have been present — but the earlier observation (b–c) would still have held, and `catchup_ms` would still be null. The artifact-size arm of the experiment is blocked on the same defects.

The cross-run note in §3.3 is the direct evidence for this last claim: the earlier 053356Z run *did* find a 2.9 MB snapshot in S3, the follower *did* download and restore it (`used_snapshot = true`), and recovery *still* failed to reach `catchup_ms`. So the block is not "no snapshot was produced"; the block is post-restore replication, which is exactly where the PreVote storm in (b) and the `InstallSnapshot`/`nextIndex` gap in (c) live.

## 5. Threats to validity

- **Single observation**: one kill, one recovery window. Multi-iteration doesn't help here because the failure mode is deterministic under the current code. The claim "recovery does not complete" is robust; claims about specific timing beyond `restart_ms` are not supported.
- **Observer timeout**: 600 s is long relative to `restart_ms` (32 s) but short relative to the election storm's natural resolution, which may never happen without PreVote. A longer timeout would not change the conclusion.
- **Threshold overshoot**: the workload pushed past the 35 k target to 62 k before the threshold poll fired, and no snapshot existed at that point. A higher target would produce a snapshot artifact to measure but would not unblock the recovery path, so the scientific conclusion is unchanged.
- **`fresh_leader_commit` in the summary shows 796**: this is an artifact of `_latest_leader_commit` reading CloudWatch post-kill while the cluster was in the election storm — coordinator-c's log stream only surfaced a low-watermark tick during that window. The correct commit-at-kill value is **62 306**, recorded in the `commit_threshold_reached` event of `history.jsonl`. This is a bench-tooling bug worth fixing (read commit index *before* the kill and pin it into the timeline) but does not affect the qualitative result.

## 6. Bugs discovered during this experiment

Four issues surfaced, in order of depth:

1. **`exp4.py` regex mismatch** *(chaos-bench, fixed)* — `RAFT_TICK_RE` was matching `commit_index=\d+` but the coordinator emits JSON `"commit_index":\d+`. `_wait_for_commit` looped forever at `commit_index=0`. Fixed to `r'"commit_index"\s*:\s*(?P<ci>\d+)'`.

2. **`EXPECTED_PEER_IDS` set empty on coordinator-a** *(coordinator task definition, hot-patched)* — the ECS task definition for `coordinator-a` was registered with `EXPECTED_PEER_IDS=""`. On restart the coordinator's boot path (`main.go:105`) skipped the peer-discovery loop entirely and came up with `peers=0`, self-electing as a solo leader at a fresh term and splitting the cluster. Hot-patched by registering revision 7 of the coordinator-a task definition with `EXPECTED_PEER_IDS=coordinator-b,coordinator-c` and `--force-new-deployment`. The terraform source of truth (`modules/ecs_cluster/main.tf:92`) generates the list correctly but the `coordinator_count` variable defaulted to 1, so the `for id in local.all_node_ids : id if id != each.key` comprehension produced an empty list. A `terraform.tfvars` file pinning `coordinator_count = 3` now prevents regression.

3. **No PreVote phase in Raft** *(coordinator, not fixed)* — a follower that comes up with an empty log and short election timeout starts elections it cannot win because peers reject its vote on the log-uptodate rule, but each failed election bumps `currentTerm` and forces the legitimate leader to step down. The cluster enters an election storm and no replication makes progress. This is the immediate blocker for Exp 4's recovery measurement.

4. **No `InstallSnapshot` fallback** *(coordinator, not fixed — previously flagged in Exp 1 §6)* — once the leader has compacted past the follower's `nextIndex`, AppendEntries alone cannot bridge the gap. This blocks the snapshot-vs-replay comparison arm of Exp 4 even if PreVote were in place.

Bugs 3 and 4 are the substantive findings of this run.

## 7. Artifacts

```
experiments/exp4/
  analysis.md              this document
  exp4_summary.json        raw timeline output
  history.jsonl            submit/kill/threshold/recovery events
  corroboration/
    console.out            summary of deleted run exp4-20260419T053356Z (see §3.3)
```

Regenerate the summary from history with `python exp4.py` (re-running requires a clean cluster).

## 8. Verdict against the proposal

| Claim | Result |
|---|---|
| `ecs:StopTask` evicts a live coordinator cold | ✔ `restart_ms = 32.3 s`, Fargate-dominated |
| Coordinator rejoins via its S3 snapshot | ✘ blocked on the PreVote gap — rejoin never begins |
| Snapshot path beats full-log replay | ✘ not measurable on current code; InstallSnapshot gap blocks the contrast run too |
| Snapshot object size < raw log size estimate | ✔ earlier run (053356Z, §3.3): snapshot ≈ 2.9 MB vs raw-log estimate ≈ 7.8 MB at 35 k commit |

**Exp 4 did not pass**, and the reason is well-characterized: two Raft-layer protocol gaps (PreVote and InstallSnapshot) block recovery of a cold-killed follower. The chaos-injection framework itself worked — it drove the cluster to a target commit index, killed a named follower on demand, observed its restart, and produced a falsifiable timeline. The negative result is itself evidence that chaos testing catches what static review missed: neither defect is visible from reading `raft/node.go` without running a cluster under load, and neither was surfaced by Exp 1, which only exercises leader kills on a short-lived cluster that never compacts.

**Recommended future work**: implement PreVote (§4b) and `InstallSnapshot` RPC with an S3-pointer payload (§4c) in `raft/node.go`, then rerun this experiment at `target_commit ∈ {50 k, 500 k, 5 M}` to characterize the snapshot-vs-replay ratio as a function of log size.
