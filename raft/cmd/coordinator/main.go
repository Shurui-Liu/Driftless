package main

// main.go — entry point for the Raft coordinator ECS task.
//
// Environment variables (set in your Terraform ECS task definition):
//   NODE_ID              — stable unique name, e.g. "coordinator-a"
//   GRPC_PORT            — port this node listens on (default "50051")
//   HTTP_PORT            — HTTP state/health port (default "8080")
//   PEER_ADDRS           — comma-separated addresses of the OTHER coordinators
//                          e.g. "coordinator-b.raft.local:50051,coordinator-c.raft.local:50051"
//
// Task dispatcher (optional — only needed when running with SQS/DynamoDB):
//   INGEST_QUEUE_URL     — SQS ingest queue URL
//   ASSIGNMENT_QUEUE_URL — SQS assignment queue URL
//   TASKS_TABLE          — DynamoDB tasks table name

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/shurui-liu/driftless/raft"
)

// assignmentCmd is the Raft log payload for a task assignment decision.
type assignmentCmd struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
	S3Key    string `json:"s3_key"`
}

// ── Task Dispatcher ───────────────────────────────────────────────────────────

// taskDispatcher polls the SQS ingest queue when this node is leader
// and appends task assignment commands to the Raft log.
type taskDispatcher struct {
	node       *raft.Node
	db         *dynamodb.Client
	sqsCli     *sqs.Client
	ingestURL  string
	assignURL  string
	tasksTable string
	log        *slog.Logger
}

func (d *taskDispatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !d.node.IsLeader() {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		d.poll(ctx)
	}
}

func (d *taskDispatcher) poll(ctx context.Context) {
	out, err := d.sqsCli.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(d.ingestURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     1, // short poll so we stay responsive to leadership changes
		VisibilityTimeout:   30,
	})
	if err != nil {
		d.log.Error("receive from ingest queue", "err", err)
		time.Sleep(time.Second)
		return
	}

	for _, msg := range out.Messages {
		var m struct {
			TaskID   string `json:"task_id"`
			Priority int    `json:"priority"`
		}
		if err := json.Unmarshal([]byte(*msg.Body), &m); err != nil {
			d.log.Warn("bad ingest message", "err", err)
			continue
		}

		// Fetch s3_key from DynamoDB (ingest API stored it there).
		item, err := d.db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(d.tasksTable),
			Key: map[string]dbtypes.AttributeValue{
				"task_id": &dbtypes.AttributeValueMemberS{Value: m.TaskID},
			},
			ProjectionExpression: aws.String("task_id, s3_key"),
		})
		if err != nil || item.Item == nil {
			d.log.Warn("task not found in DynamoDB", "task_id", m.TaskID)
			continue
		}
		s3Attr, ok := item.Item["s3_key"].(*dbtypes.AttributeValueMemberS)
		if !ok {
			d.log.Warn("s3_key missing for task", "task_id", m.TaskID)
			continue
		}

		cmd, _ := json.Marshal(assignmentCmd{
			TaskID:   m.TaskID,
			Priority: m.Priority,
			S3Key:    s3Attr.Value,
		})

		if _, err := d.node.AppendCommand(cmd); err != nil {
			// Lost leadership between the IsLeader check and AppendCommand.
			// The message remains visible in SQS and will be picked up by the new leader.
			d.log.Info("lost leadership during log append", "task_id", m.TaskID)
			return
		}

		// Remove from ingest queue only after the entry is in the Raft log.
		d.sqsCli.DeleteMessage(ctx, &sqs.DeleteMessageInput{ //nolint:errcheck
			QueueUrl:      aws.String(d.ingestURL),
			ReceiptHandle: msg.ReceiptHandle,
		})
		d.log.Info("task appended to raft log", "task_id", m.TaskID)
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	nodeID    := mustEnv("NODE_ID")
	grpcPort  := envOr("GRPC_PORT", "50051")
	httpPort  := envOr("HTTP_PORT", "8080")
	peerAddrs := strings.Split(mustEnv("PEER_ADDRS"), ",")

	// Optional — task dispatcher is only active when these are set.
	ingestURL  := os.Getenv("INGEST_QUEUE_URL")
	assignURL  := os.Getenv("ASSIGNMENT_QUEUE_URL")
	tasksTable := os.Getenv("TASKS_TABLE")

	// ── Dial peers ───────────────────────────────────────────────────────────
	peers     := make([]raft.Peer, 0, len(peerAddrs))
	grpcPeers := make([]*raft.GRPCPeer, 0, len(peerAddrs))

	for i, addr := range peerAddrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		peerID := peerIDFromAddr(addr, i)
		p, err := raft.NewGRPCPeer(peerID, addr)
		if err != nil {
			log.Error("failed to dial peer", "addr", addr, "err", err)
			os.Exit(1)
		}
		peers     = append(peers, p)
		grpcPeers = append(grpcPeers, p)
		log.Info("dialed peer", "id", peerID, "addr", addr)
	}

	// ── Build Raft node ───────────────────────────────────────────────────────
	node := raft.NewNode(nodeID, peers, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Task dispatcher (only when SQS/DynamoDB env vars are present) ─────────
	if ingestURL != "" && assignURL != "" && tasksTable != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			log.Error("failed to load AWS config", "err", err)
			os.Exit(1)
		}
		sqsCli := sqs.NewFromConfig(awsCfg)
		dbCli  := dynamodb.NewFromConfig(awsCfg)

		dispatcher := &taskDispatcher{
			node:       node,
			db:         dbCli,
			sqsCli:     sqsCli,
			ingestURL:  ingestURL,
			assignURL:  assignURL,
			tasksTable: tasksTable,
			log:        log,
		}

		// Apply callback: on commit, the leader dispatches the task to SQS
		// and records dispatched_at in DynamoDB for latency tracking.
		node.SetApplyFunc(func(entry raft.LogEntry) {
			if len(entry.Command) == 0 {
				return // dummy index-0 entry
			}
			var cmd assignmentCmd
			if err := json.Unmarshal(entry.Command, &cmd); err != nil {
				return
			}

			// Only the current leader dispatches to SQS to avoid duplicate assignments
			// when the same committed entry is applied on multiple nodes.
			if !node.IsLeader() {
				return
			}

			now := time.Now().UTC().Format(time.RFC3339Nano)

			msg, _ := json.Marshal(map[string]interface{}{
				"task_id":  cmd.TaskID,
				"priority": cmd.Priority,
				"s3_key":   cmd.S3Key,
			})
			if _, err := sqsCli.SendMessage(ctx, &sqs.SendMessageInput{
				QueueUrl:    aws.String(assignURL),
				MessageBody: aws.String(string(msg)),
			}); err != nil {
				log.Error("push to assignment queue failed", "task_id", cmd.TaskID, "err", err)
				return
			}

			// Stamp dispatched_at — this is the latency anchor for the experiment.
			if _, err := dbCli.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName: aws.String(tasksTable),
				Key: map[string]dbtypes.AttributeValue{
					"task_id": &dbtypes.AttributeValueMemberS{Value: cmd.TaskID},
				},
				UpdateExpression: aws.String("SET dispatched_at = :ts, updated_at = :ts"),
				ExpressionAttributeValues: map[string]dbtypes.AttributeValue{
					":ts": &dbtypes.AttributeValueMemberS{Value: now},
				},
			}); err != nil {
				log.Warn("write dispatched_at failed", "task_id", cmd.TaskID, "err", err)
			}

			log.Info("task dispatched", "task_id", cmd.TaskID, "dispatched_at", now)
		})

		go dispatcher.run(ctx)
		log.Info("task dispatcher started", "ingest_queue", ingestURL, "assign_queue", assignURL)
	} else {
		log.Info("task dispatcher disabled (INGEST_QUEUE_URL / ASSIGNMENT_QUEUE_URL / TASKS_TABLE not set)")
	}

	// ── Start goroutines ─────────────────────────────────────────────────────
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		node.Run(ctx)
		log.Info("raft node stopped")
	}()

	server := raft.NewGRPCServer(node)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(ctx, ":"+grpcPort); err != nil {
			log.Error("grpc server error", "err", err)
			cancel()
		}
	}()

	httpServer := raft.NewHTTPServer(node)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := httpServer.Serve(ctx, ":"+httpPort); err != nil {
			log.Error("http state server error", "err", err)
		}
	}()

	log.Info("coordinator started",
		"id", nodeID, "grpc_port", grpcPort, "http_port", httpPort, "peers", peerAddrs)
	wg.Wait()

	for _, p := range grpcPeers {
		if err := p.Close(); err != nil {
			log.Warn("error closing peer connection", "peer", p.ID(), "err", err)
		}
	}
	log.Info("coordinator shut down cleanly")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required env var missing", "key", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// peerIDFromAddr derives a stable peer ID from the Cloud Map DNS address.
// "coordinator-b.raft.local:50051" → "coordinator-b"
func peerIDFromAddr(addr string, fallbackIdx int) string {
	host := strings.SplitN(addr, ":", 2)[0]
	parts := strings.SplitN(host, ".", 2)
	if parts[0] != "" {
		return parts[0]
	}
	return "peer-" + string(rune('a'+fallbackIdx))
}
