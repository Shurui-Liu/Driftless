package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// ---- Config ----------------------------------------------------------------

var (
	tableName       = os.Getenv("DYNAMO_TABLE")       // e.g. "tasks"
	bucketName      = os.Getenv("S3_BUCKET")          // e.g. "driftless-payloads"
	assignmentQueue = os.Getenv("SQS_ASSIGNMENT_URL") // assignment queue URL
	peersTable      = os.Getenv("PEERS_TABLE")        // coordinator peer registry (DDB)
	coordinatorIDs  = splitCSV(os.Getenv("COORDINATOR_NODE_IDS"))
	legacyCoordURL  = os.Getenv("COORDINATOR_URL") // static override for local dev
	workerID        = getWorkerID()
)

// ---- Server struct ---------------------------------------------------------

type Worker struct {
	db  *dynamodb.Client
	s3  *s3.Client
	sqs *sqs.Client

	coordMu  sync.RWMutex
	coordURL string // current coordinator HTTP URL, refreshed on heartbeat failure
}

// getCoordinator returns the currently-cached coordinator URL.
func (w *Worker) getCoordinator() string {
	w.coordMu.RLock()
	defer w.coordMu.RUnlock()
	return w.coordURL
}

func (w *Worker) setCoordinator(url string) {
	w.coordMu.Lock()
	w.coordURL = url
	w.coordMu.Unlock()
}

// refreshCoordinator scans the peers table for any live coordinator and
// caches its HTTP URL. Returns error if none of COORDINATOR_NODE_IDS resolve.
// No-op when running with a static COORDINATOR_URL override.
func (w *Worker) refreshCoordinator(ctx context.Context) error {
	if peersTable == "" {
		return nil
	}
	for _, id := range coordinatorIDs {
		out, err := w.db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(peersTable),
			Key: map[string]types.AttributeValue{
				"node_id": &types.AttributeValueMemberS{Value: id},
			},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil || len(out.Item) == 0 {
			continue
		}
		v, ok := out.Item["http_addr"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		w.setCoordinator("http://" + v.Value)
		return nil
	}
	return fmt.Errorf("no coordinator found in peers table %q", peersTable)
}

// ---- Message types ---------------------------------------------------------

// AssignmentMessage is what the Raft leader puts on the assignment queue.
type AssignmentMessage struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
	S3Key    string `json:"s3_key"`
}

// HeartbeatRequest is sent to the coordinator to signal the worker is alive.
type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
	TaskID   string `json:"task_id,omitempty"` // empty if worker is idle
}

// ---- Main polling loop -----------------------------------------------------

func (w *Worker) run(ctx context.Context) {
	log.Printf("worker %s started, polling %s", workerID, assignmentQueue)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			w.poll(ctx)
		}
	}
}

func (w *Worker) poll(ctx context.Context) {
	// Long polling — wait up to 20s for a message before returning empty
	out, err := w.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(assignmentQueue),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     20, // long polling
		VisibilityTimeout:   60, // hide message from other workers while processing
	})
	if err != nil {
		log.Printf("sqs receive error: %v", err)
		time.Sleep(2 * time.Second) // back off on error
		return
	}
	if len(out.Messages) == 0 {
		return // nothing to do, loop again
	}

	msg := out.Messages[0]
	var assignment AssignmentMessage
	if err := json.Unmarshal([]byte(*msg.Body), &assignment); err != nil {
		log.Printf("bad message format, discarding: %v", err)
		w.deleteMessage(ctx, msg.ReceiptHandle)
		return
	}

	// Process the task — delete SQS message only on success
	if err := w.processTask(ctx, assignment); err != nil {
		log.Printf("task %s failed: %v", assignment.TaskID, err)
		// Do NOT delete — let visibility timeout expire so it can be redelivered
		// Coordinator will handle reassignment via missed heartbeat
		return
	}

	// Task completed — remove from queue
	w.deleteMessage(ctx, msg.ReceiptHandle)
}

// ---- Task processing -------------------------------------------------------

func (w *Worker) processTask(ctx context.Context, a AssignmentMessage) error {
	log.Printf("processing task %s", a.TaskID)

	// Step 1 — Update status to ASSIGNED in DynamoDB
	if err := w.updateStatus(ctx, a.TaskID, "ASSIGNED", ""); err != nil {
		return fmt.Errorf("mark assigned: %w", err)
	}

	// Step 2 — Download payload from S3
	payload, err := w.downloadPayload(ctx, a.S3Key)
	if err != nil {
		_ = w.updateStatus(ctx, a.TaskID, "FAILED", err.Error())
		return fmt.Errorf("download payload: %w", err)
	}

	// Step 3 — Execute the task (simulated CPU/IO workload)
	result, err := w.execute(ctx, a.TaskID, payload)
	if err != nil {
		_ = w.updateStatus(ctx, a.TaskID, "FAILED", err.Error())
		return fmt.Errorf("execute: %w", err)
	}

	// Step 4 — Upload result to S3
	resultKey := fmt.Sprintf("tasks/%s/result", a.TaskID)
	if err := w.uploadResult(ctx, resultKey, result); err != nil {
		_ = w.updateStatus(ctx, a.TaskID, "FAILED", err.Error())
		return fmt.Errorf("upload result: %w", err)
	}

	// Step 5 — Mark COMPLETED in DynamoDB (with result s3 key)
	if err := w.updateStatus(ctx, a.TaskID, "COMPLETED", resultKey); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}

	log.Printf("task %s completed", a.TaskID)
	return nil
}

// ---- Simulated workload ----------------------------------------------------

// execute simulates a CPU/IO workload for benchmarking experiments.
// In a real system, this would run the actual task logic.
func (w *Worker) execute(ctx context.Context, taskID string, payload []byte) ([]byte, error) {
	log.Printf("executing task %s (%d bytes)", taskID, len(payload))

	// Simulate CPU work — busy loop for 100-500ms
	cpuDuration := time.Duration(100+rand.Intn(400)) * time.Millisecond
	deadline := time.Now().Add(cpuDuration)
	for time.Now().Before(deadline) {
		// spin
	}

	// Simulate IO work — sleep for 50-200ms
	ioDuration := time.Duration(50+rand.Intn(150)) * time.Millisecond
	select {
	case <-time.After(ioDuration):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	result := fmt.Sprintf(`{"task_id":"%s","processed_bytes":%d,"worker_id":"%s"}`,
		taskID, len(payload), workerID)
	return []byte(result), nil
}

// ---- AWS helpers -----------------------------------------------------------

func (w *Worker) downloadPayload(ctx context.Context, s3Key string) ([]byte, error) {
	out, err := w.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (w *Worker) uploadResult(ctx context.Context, key string, data []byte) error {
	_, err := w.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	return err
}

// updateStatus updates task status in DynamoDB.
// Uses a ConditionExpression to prevent overwriting a COMPLETED task
// (guards against a race where two workers both think they own the task).
func (w *Worker) updateStatus(ctx context.Context, taskID, status, resultKey string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	item := map[string]types.AttributeValue{
		":status":     &types.AttributeValueMemberS{Value: status},
		":updated_at": &types.AttributeValueMemberS{Value: now},
		":completed":  &types.AttributeValueMemberS{Value: "COMPLETED"},
	}

	expr := "SET #st = :status, updated_at = :updated_at"
	if resultKey != "" {
		expr += ", result_key = :result_key"
		item[":result_key"] = &types.AttributeValueMemberS{Value: resultKey}
	}

	_, err := w.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"task_id": &types.AttributeValueMemberS{Value: taskID},
		},
		UpdateExpression:    aws.String(expr),
		ConditionExpression: aws.String("#st <> :completed"), // never overwrite COMPLETED
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: item,
	})
	return err
}

func (w *Worker) deleteMessage(ctx context.Context, handle *string) {
	_, err := w.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(assignmentQueue),
		ReceiptHandle: handle,
	})
	if err != nil {
		log.Printf("failed to delete SQS message: %v", err)
	}
}

// ---- Heartbeat -------------------------------------------------------------

// sendHeartbeats pings the coordinator every 5s so it knows this worker is alive.
// If the coordinator misses 3 heartbeats it will reassign the task via a new Raft log entry.
func (w *Worker) sendHeartbeats(ctx context.Context, currentTaskID *string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			hb := HeartbeatRequest{WorkerID: workerID}
			if currentTaskID != nil {
				hb.TaskID = *currentTaskID
			}
			body, _ := json.Marshal(hb)
			url := w.getCoordinator()
			if url == "" {
				continue
			}
			resp, err := http.Post(
				url+"/heartbeat",
				"application/json",
				bytes.NewReader(body),
			)
			if err != nil {
				log.Printf("heartbeat failed: %v", err)
				// Coordinator may have failed over — re-resolve from peers table.
				if rerr := w.refreshCoordinator(ctx); rerr != nil {
					log.Printf("refresh coordinator: %v", rerr)
				}
				continue
			}
			resp.Body.Close()
		case <-ctx.Done():
			return
		}
	}
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func getWorkerID() string {
	if id := os.Getenv("WORKER_ID"); id != "" {
		return id
	}
	h, _ := os.Hostname()
	if h != "" {
		return h
	}
	return fmt.Sprintf("worker-%d", rand.Intn(10000))
}

// ---- Main ------------------------------------------------------------------

func main() {
	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	w := &Worker{
		db:  dynamodb.NewFromConfig(cfg),
		s3:  s3.NewFromConfig(cfg),
		sqs: sqs.NewFromConfig(cfg),
	}

	// Resolve coordinator URL. Prefer static override (local dev), else
	// block until a coordinator appears in the peers table.
	if legacyCoordURL != "" {
		w.setCoordinator(legacyCoordURL)
	} else if peersTable != "" && len(coordinatorIDs) > 0 {
		bootCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		for {
			if err := w.refreshCoordinator(bootCtx); err == nil && w.getCoordinator() != "" {
				break
			}
			select {
			case <-bootCtx.Done():
				cancel()
				log.Fatalf("no coordinator found within timeout; set COORDINATOR_URL for local dev")
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
		log.Printf("resolved coordinator: %s", w.getCoordinator())
	} else {
		log.Printf("warning: no COORDINATOR_URL and no PEERS_TABLE; heartbeats disabled")
	}

	// Start heartbeat sender in background (idle worker, no current task)
	go w.sendHeartbeats(ctx, nil)

	// Start health endpoint for ECS
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		log.Fatal(http.ListenAndServe(":8081", nil))
	}()

	// Start main polling loop
	w.run(ctx)
}
