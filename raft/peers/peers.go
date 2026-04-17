// Package peers replaces AWS Cloud Map service discovery with a DynamoDB
// registry. Each coordinator writes its grpc_addr / http_addr to the table
// on startup and refreshes a heartbeat TTL periodically. Peers and downstream
// services (worker, observer) read from this table to resolve coordinator
// addresses.
package peers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Info is the registration record for a single coordinator node.
type Info struct {
	NodeID   string
	GRPCAddr string // "10.0.1.42:50051"
	HTTPAddr string // "10.0.1.42:8080"
}

// Registry handles read/write access to the peers DynamoDB table.
type Registry struct {
	db    *dynamodb.Client
	table string
	ttl   time.Duration // how long after last heartbeat to consider a row stale
}

func NewRegistry(db *dynamodb.Client, table string) *Registry {
	return &Registry{db: db, table: table, ttl: 30 * time.Second}
}

// Register writes this node's entry. Safe to call repeatedly; it overwrites.
func (r *Registry) Register(ctx context.Context, self Info) error {
	now := time.Now().Unix()
	expires := now + int64(r.ttl.Seconds())
	_, err := r.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(r.table),
		Item: map[string]types.AttributeValue{
			"node_id":      &types.AttributeValueMemberS{Value: self.NodeID},
			"grpc_addr":    &types.AttributeValueMemberS{Value: self.GRPCAddr},
			"http_addr":    &types.AttributeValueMemberS{Value: self.HTTPAddr},
			"heartbeat_ts": &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			"expires_at":   &types.AttributeValueMemberN{Value: strconv.FormatInt(expires, 10)},
		},
	})
	return err
}

// StartHeartbeat refreshes the self record every interval until ctx is done.
func (r *Registry) StartHeartbeat(ctx context.Context, self Info, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.Register(ctx, self); err != nil && !errors.Is(err, context.Canceled) {
					// Best-effort; TTL will expire the row if we stay dead.
					fmt.Fprintf(os.Stderr, "peers: heartbeat failed: %v\n", err)
				}
			}
		}
	}()
}

// Lookup returns a single peer's record, or an error if not found or stale.
func (r *Registry) Lookup(ctx context.Context, nodeID string) (Info, error) {
	out, err := r.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(r.table),
		Key: map[string]types.AttributeValue{
			"node_id": &types.AttributeValueMemberS{Value: nodeID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Info{}, err
	}
	if len(out.Item) == 0 {
		return Info{}, fmt.Errorf("peers: node %q not registered", nodeID)
	}
	info, err := decode(out.Item)
	if err != nil {
		return Info{}, err
	}
	// Treat rows older than 2x TTL as stale — the TTL attribute only expires
	// rows with minutes of lag, so we gate on heartbeat_ts for freshness.
	if err := r.checkFresh(out.Item); err != nil {
		return Info{}, err
	}
	return info, nil
}

// WaitForPeers polls until every expected ID resolves to a fresh record or ctx expires.
func (r *Registry) WaitForPeers(ctx context.Context, expected []string, poll time.Duration) ([]Info, error) {
	found := make(map[string]Info, len(expected))
	for {
		for _, id := range expected {
			if _, ok := found[id]; ok {
				continue
			}
			info, err := r.Lookup(ctx, id)
			if err == nil {
				found[id] = info
			}
		}
		if len(found) == len(expected) {
			out := make([]Info, 0, len(expected))
			for _, id := range expected {
				out = append(out, found[id])
			}
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func decode(item map[string]types.AttributeValue) (Info, error) {
	get := func(k string) (string, error) {
		v, ok := item[k].(*types.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("peers: missing or non-string attribute %q", k)
		}
		return v.Value, nil
	}
	id, err := get("node_id")
	if err != nil {
		return Info{}, err
	}
	g, err := get("grpc_addr")
	if err != nil {
		return Info{}, err
	}
	h, err := get("http_addr")
	if err != nil {
		return Info{}, err
	}
	return Info{NodeID: id, GRPCAddr: g, HTTPAddr: h}, nil
}

func (r *Registry) checkFresh(item map[string]types.AttributeValue) error {
	hv, ok := item["heartbeat_ts"].(*types.AttributeValueMemberN)
	if !ok {
		return fmt.Errorf("peers: missing heartbeat_ts")
	}
	ts, err := strconv.ParseInt(hv.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("peers: bad heartbeat_ts: %w", err)
	}
	age := time.Since(time.Unix(ts, 0))
	if age > 2*r.ttl {
		return fmt.Errorf("peers: stale entry (age %s)", age)
	}
	return nil
}

// DiscoverSelfIP returns this ECS task's private IP via the task metadata
// endpoint, or the first non-loopback interface IP as a fallback for local runs.
func DiscoverSelfIP(ctx context.Context) (string, error) {
	if uri := os.Getenv("ECS_CONTAINER_METADATA_URI_V4"); uri != "" {
		if ip, err := ipFromECSMetadata(ctx, uri+"/task"); err == nil {
			return ip, nil
		}
	}
	return firstNonLoopbackIP()
}

func ipFromECSMetadata(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var meta struct {
		Containers []struct {
			Networks []struct {
				IPv4Addresses []string `json:"IPv4Addresses"`
			} `json:"Networks"`
		} `json:"Containers"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	for _, c := range meta.Containers {
		for _, n := range c.Networks {
			if len(n.IPv4Addresses) > 0 {
				return n.IPv4Addresses[0], nil
			}
		}
	}
	return "", fmt.Errorf("peers: no IPv4 in ECS task metadata")
}

func firstNonLoopbackIP() (string, error) {
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range ifaces {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip != nil && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("peers: no non-loopback IPv4 interface found")
}
