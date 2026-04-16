package raft

// snapshot_s3.go — S3 implementation of Snapshotter.
//
// Each node writes snapshots to a deterministic key:
//   s3://{bucket}/{prefix}/{node_id}/snapshot.json
// We overwrite in place on Save; older snapshots aren't retained (S3
// versioning on the bucket, if enabled, gives rollback history for free).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Snapshotter persists RaftSnapshots to an S3 bucket.
type S3Snapshotter struct {
	client *s3.Client
	bucket string
	key    string
}

// NewS3Snapshotter returns a snapshotter rooted at
// s3://{bucket}/{prefix}/{nodeID}/snapshot.json. Prefix may be empty.
func NewS3Snapshotter(client *s3.Client, bucket, prefix, nodeID string) *S3Snapshotter {
	key := nodeID + "/snapshot.json"
	if prefix != "" {
		key = prefix + "/" + key
	}
	return &S3Snapshotter{client: client, bucket: bucket, key: key}
}

// Save writes the snapshot to S3, overwriting any previous object at the key.
func (s *S3Snapshotter) Save(ctx context.Context, snap RaftSnapshot) error {
	body, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s/%s: %w", s.bucket, s.key, err)
	}
	return nil
}

// Load fetches the snapshot from S3. Returns (nil, nil) if no snapshot exists.
func (s *S3Snapshotter) Load(ctx context.Context) (*RaftSnapshot, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, nil
		}
		return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, s.key, err)
	}
	defer out.Body.Close()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read s3 body: %w", err)
	}
	var snap RaftSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return &snap, nil
}
