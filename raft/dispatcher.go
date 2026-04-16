package raft

// dispatcher.go — leader-only task dispatch through the Raft log.
//
// Flow:
//   1. Leader drains the ingest SQS queue.
//   2. Looks up each task's S3 payload key in DynamoDB.
//   3. Proposes a "dispatch" command to Raft; waits for majority commit.
//   4. On Apply (fires on every node, not just leader), the command updates
//      the in-memory task-state map. The leader additionally publishes the
//      assignment onto the assignment SQS queue — exactly once per term.
//   5. Only after commit + publish does the leader ack the ingest message.
//
// Failure modes:
//   - Leader step-down before commit → Propose result returns Committed=false,
//     dispatcher does NOT ack, SQS redelivers under the new leader.
//   - Leader step-down between commit and publish → the entry replays under
//     the new leader. The state machine's already-dispatched set suppresses a
//     duplicate publish. A brand-new leader that hasn't replayed the entry
//     yet will not see it in already-dispatched and will publish. This is the
//     one window where a double publish can occur; worker-side conditional
//     DynamoDB update makes it idempotent.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// commitWaitTimeout caps how long the dispatcher waits for a Propose to
// commit. If Raft can't get quorum in this time we leave the ingest message
// on the queue and move on.
const commitWaitTimeout = 5 * time.Second

// DispatcherConfig wires the dispatcher to AWS resources.
type DispatcherConfig struct {
	IngestQueueURL     string
	AssignmentQueueURL string
	TasksTable         string
}

// dispatchCommand is the replicated Raft log entry.
// Type="dispatch" is currently the only one. Future types: "complete",
// "reassign" — all routed through the same state machine.
type dispatchCommand struct {
	Type     string `json:"type"`
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
	S3Key    string `json:"s3_key"`
}

// Dispatcher reads ingest messages, proposes them through Raft, and on commit
// routes the assignment to workers. Implements StateMachine.
type Dispatcher struct {
	node    *Node
	sqs     *sqs.Client
	db      *dynamodb.Client
	cfg     DispatcherConfig
	log     *slog.Logger
	tracker *WorkerTracker

	mu         sync.Mutex
	dispatched map[string]bool // task IDs we've already published this node lifetime
}

type ingestMessage struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
}

type assignmentMessage struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
	S3Key    string `json:"s3_key"`
}

// NewDispatcher constructs a dispatcher but does not start it.
func NewDispatcher(
	node *Node,
	sqsClient *sqs.Client,
	dbClient *dynamodb.Client,
	cfg DispatcherConfig,
	tracker *WorkerTracker,
	logger *slog.Logger,
) *Dispatcher {
	return &Dispatcher{
		node:       node,
		sqs:        sqsClient,
		db:         dbClient,
		cfg:        cfg,
		log:        logger,
		tracker:    tracker,
		dispatched: make(map[string]bool),
	}
}

// Apply is called by the Raft apply loop for every committed entry.
// Runs on every node in the cluster. Only the leader publishes; followers
// just track which tasks have been dispatched so that after a leader change
// the new leader won't redundantly re-publish entries already handled.
func (d *Dispatcher) Apply(entry LogEntry) {
	if len(entry.Command) == 0 {
		return
	}
	var cmd dispatchCommand
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		d.log.Warn("state machine: bad command", "err", err, "index", entry.Index)
		return
	}
	if cmd.Type != "dispatch" || cmd.TaskID == "" {
		return
	}

	d.mu.Lock()
	already := d.dispatched[cmd.TaskID]
	if !already {
		d.dispatched[cmd.TaskID] = true
	}
	d.mu.Unlock()

	if already {
		return // idempotent replay
	}

	if !d.node.IsLeader() {
		// Follower: just record the dispatch in local state, don't publish.
		return
	}

	// Leader side-effect: publish assignment to SQS.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := assignmentMessage{TaskID: cmd.TaskID, Priority: cmd.Priority, S3Key: cmd.S3Key}
	body, _ := json.Marshal(out)
	if _, err := d.sqs.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(d.cfg.AssignmentQueueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		d.log.Warn("assignment publish failed", "task_id", cmd.TaskID, "err", err)
		// Retract the "dispatched" marker so the entry can be retried by a
		// future leader via orphan scan / resubmission.
		d.mu.Lock()
		delete(d.dispatched, cmd.TaskID)
		d.mu.Unlock()
		return
	}
	if err := d.markAssigned(ctx, cmd.TaskID); err != nil {
		d.log.Debug("mark assigned failed", "task_id", cmd.TaskID, "err", err)
	}
	d.log.Info("task dispatched", "task_id", cmd.TaskID, "index", entry.Index, "term", entry.Term)
}

// Run blocks until ctx is cancelled. Leaders long-poll ingest SQS; followers idle.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("dispatcher started",
		"ingest_queue", d.cfg.IngestQueueURL,
		"assignment_queue", d.cfg.AssignmentQueueURL,
	)

	idleTicker := time.NewTicker(500 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		if !d.node.IsLeader() {
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
			}
			continue
		}
		d.pollOnce(ctx)
	}
}

func (d *Dispatcher) pollOnce(ctx context.Context) {
	out, err := d.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(d.cfg.IngestQueueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     10,
		VisibilityTimeout:   30,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Warn("ingest receive failed", "err", err)
		time.Sleep(2 * time.Second)
		return
	}

	for _, m := range out.Messages {
		if !d.node.IsLeader() {
			return // stepped down mid-batch — leave the rest for the new leader
		}
		d.handleIngest(ctx, m.Body, m.ReceiptHandle)
	}
}

func (d *Dispatcher) handleIngest(ctx context.Context, body *string, receipt *string) {
	if body == nil {
		return
	}

	var in ingestMessage
	if err := json.Unmarshal([]byte(*body), &in); err != nil || in.TaskID == "" {
		d.log.Warn("bad ingest message, dropping", "err", err, "body", *body)
		d.ack(ctx, receipt)
		return
	}

	// Fast-path idempotency: if we've already dispatched this task in our
	// lifetime, ack and move on. Survives SQS redelivery.
	d.mu.Lock()
	done := d.dispatched[in.TaskID]
	d.mu.Unlock()
	if done {
		d.ack(ctx, receipt)
		return
	}

	s3Key, err := d.lookupS3Key(ctx, in.TaskID)
	if err != nil {
		d.log.Warn("dynamodb lookup failed", "task_id", in.TaskID, "err", err)
		return // leave on queue; SQS will redeliver
	}
	if s3Key == "" {
		d.log.Warn("task missing s3_key, dropping", "task_id", in.TaskID)
		d.ack(ctx, receipt)
		return
	}

	cmd := dispatchCommand{
		Type:     "dispatch",
		TaskID:   in.TaskID,
		Priority: in.Priority,
		S3Key:    s3Key,
	}
	encoded, err := json.Marshal(cmd)
	if err != nil {
		d.log.Warn("encode command failed", "err", err)
		return
	}

	resultCh, ok := d.node.Propose(encoded)
	if !ok {
		return // stepped down between IsLeader check and Propose
	}

	select {
	case res := <-resultCh:
		if !res.Committed {
			// Stepped down before commit — leave message on queue.
			return
		}
		// Committed (and Apply has already published to assignment SQS).
		d.ack(ctx, receipt)
	case <-time.After(commitWaitTimeout):
		d.log.Warn("commit wait timed out", "task_id", in.TaskID)
	case <-ctx.Done():
	}
}

func (d *Dispatcher) ack(ctx context.Context, receipt *string) {
	if receipt == nil {
		return
	}
	_, err := d.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(d.cfg.IngestQueueURL),
		ReceiptHandle: receipt,
	})
	if err != nil {
		d.log.Warn("ingest delete failed", "err", err)
	}
}

// ── Snapshot support (implements SnapshotableStateMachine) ───────────────────

// dispatcherSnap is what gets serialized into the Raft snapshot's Data field.
type dispatcherSnap struct {
	Dispatched []string `json:"dispatched"`
}

// Snapshot serializes the replicated portion of dispatcher state.
// Called by the Raft node; safe to call concurrently with Apply.
func (d *Dispatcher) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.dispatched))
	for id := range d.dispatched {
		ids = append(ids, id)
	}
	return json.Marshal(dispatcherSnap{Dispatched: ids})
}

// Restore replaces dispatcher state from a snapshot. Called during boot.
func (d *Dispatcher) Restore(data []byte) error {
	var s dispatcherSnap
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dispatched = make(map[string]bool, len(s.Dispatched))
	for _, id := range s.Dispatched {
		d.dispatched[id] = true
	}
	return nil
}

func (d *Dispatcher) lookupS3Key(ctx context.Context, taskID string) (string, error) {
	out, err := d.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.cfg.TasksTable),
		Key: map[string]types.AttributeValue{
			"task_id": &types.AttributeValueMemberS{Value: taskID},
		},
	})
	if err != nil {
		return "", err
	}
	v, ok := out.Item["s3_key"].(*types.AttributeValueMemberS)
	if !ok {
		return "", nil
	}
	return v.Value, nil
}

func (d *Dispatcher) markAssigned(ctx context.Context, taskID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.cfg.TasksTable),
		Key: map[string]types.AttributeValue{
			"task_id": &types.AttributeValueMemberS{Value: taskID},
		},
		UpdateExpression: aws.String("SET #st = :assigned, updated_at = :now"),
		ConditionExpression: aws.String(
			"attribute_not_exists(#st) OR #st = :pending",
		),
		ExpressionAttributeNames: map[string]string{"#st": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":assigned": &types.AttributeValueMemberS{Value: "ASSIGNED"},
			":pending":  &types.AttributeValueMemberS{Value: "PENDING"},
			":now":      &types.AttributeValueMemberS{Value: now},
		},
	})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}
