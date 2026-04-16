package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"bytes"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
)

// ---- Config ----------------------------------------------------------------

var (
	tableName  = os.Getenv("DYNAMO_TABLE")  // e.g. "tasks"
	bucketName = os.Getenv("S3_BUCKET")     // e.g. "driftless-payloads"
	queueURL   = os.Getenv("SQS_QUEUE_URL") // ingest queue URL
)

// ---- Server struct ---------------------------------------------------------

// Server bundles all AWS clients so handlers can access them as methods.
type Server struct {
	db  *dynamodb.Client
	s3  *s3.Client
	sqs *sqs.Client
}

// ---- Request / Response types ----------------------------------------------

type SubmitTaskRequest struct {
	Payload  []byte `json:"payload"`  // raw task data (can be large)
	Priority int    `json:"priority"` // 1 (low) to 10 (high)
}

type SubmitTaskResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// SQSMessage is the lightweight signal published to SQS.
// Workers never need the full payload from SQS — they fetch it from S3.
type SQSMessage struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
}

// ---- Handler ---------------------------------------------------------------

func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Validate method
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Decode + validate request body
	var req SubmitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.Payload) == 0 {
		http.Error(w, "payload must not be empty", http.StatusBadRequest)
		return
	}
	if req.Priority < 1 || req.Priority > 10 {
		http.Error(w, "priority must be between 1 and 10", http.StatusBadRequest)
		return
	}

	// 3. Generate task ID
	taskID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	s3Key := fmt.Sprintf("tasks/%s/payload", taskID)

	// 4. Upload payload to S3 FIRST
	//    (DynamoDB will store the s3Key reference — must exist before we write metadata)
	_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(s3Key),
		Body:   bytes.NewReader(req.Payload),
	})
	if err != nil {
		log.Printf("s3 upload failed: %v", err)
		http.Error(w, "failed to store payload", http.StatusInternalServerError)
		return
	}

	// 5. Write task metadata to DynamoDB
	//    Status starts as PENDING — orphan scanner uses this to detect lost tasks
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"task_id":    &types.AttributeValueMemberS{Value: taskID},
			"status":     &types.AttributeValueMemberS{Value: "PENDING"},
			"priority":   &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", req.Priority)},
			"s3_key":     &types.AttributeValueMemberS{Value: s3Key},
			"created_at": &types.AttributeValueMemberS{Value: now},
			"updated_at": &types.AttributeValueMemberS{Value: now},
		},
	})
	if err != nil {
		log.Printf("dynamodb write failed: %v", err)
		http.Error(w, "failed to store task metadata", http.StatusInternalServerError)
		return
	}

	// 6. Publish lightweight message to SQS LAST
	//    Only after DynamoDB is written — coordinator must be able to find the task
	msg, _ := json.Marshal(SQSMessage{TaskID: taskID, Priority: req.Priority})
	_, err = s.sqs.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(string(msg)),
		// MessageGroupId required for FIFO queues — omit for standard queues
	})
	if err != nil {
		// Task is safely in DynamoDB — orphan scanner will re-publish this later
		log.Printf("sqs publish failed for task %s (will be recovered): %v", taskID, err)
	}

	// 7. Return 202 Accepted — task is queued, not yet completed
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(SubmitTaskResponse{
		TaskID: taskID,
		Status: "PENDING",
	})
}

// ---- Orphan scanner --------------------------------------------------------

// runOrphanScanner periodically finds tasks stuck in PENDING that never made
// it to SQS (e.g. API crashed after DynamoDB write but before SQS publish).
func (s *Server) runOrphanScanner(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.recoverOrphans(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) recoverOrphans(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)

	// Scan for tasks that are still PENDING and haven't been updated recently
	out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(tableName),
		FilterExpression: aws.String("#st = :pending AND updated_at < :cutoff"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status", // "status" is a reserved word in DynamoDB
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pending": &types.AttributeValueMemberS{Value: "PENDING"},
			":cutoff":  &types.AttributeValueMemberS{Value: cutoff},
		},
	})
	if err != nil {
		log.Printf("orphan scan failed: %v", err)
		return
	}

	for _, item := range out.Items {
		taskID := item["task_id"].(*types.AttributeValueMemberS).Value
		priority := item["priority"].(*types.AttributeValueMemberN).Value

		msg, _ := json.Marshal(map[string]string{
			"task_id":  taskID,
			"priority": priority,
		})
		_, err := s.sqs.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(queueURL),
			MessageBody: aws.String(string(msg)),
		})
		if err != nil {
			log.Printf("orphan re-publish failed for %s: %v", taskID, err)
		} else {
			log.Printf("orphan recovered: %s", taskID)
		}
	}
}

// ---- Main ------------------------------------------------------------------

func main() {
	ctx := context.Background()

	// Load AWS config (uses env vars / IAM role on ECS automatically)
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	// Initialize server with all 3 AWS clients
	srv := &Server{
		db:  dynamodb.NewFromConfig(cfg),
		s3:  s3.NewFromConfig(cfg),
		sqs: sqs.NewFromConfig(cfg),
	}

	// Start orphan scanner in background
	go srv.runOrphanScanner(ctx)

	// Register routes
	http.HandleFunc("/tasks", srv.handleSubmitTask)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Println("task ingest API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
