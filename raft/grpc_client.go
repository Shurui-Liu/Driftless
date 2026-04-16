package raft

// grpc_client.go — implements the Peer interface so node.go can call remote coordinators.
//
// One GRPCPeer is created per remote coordinator node. In ECS, the address
// comes from Cloud Map service discovery (e.g. "coordinator-b.raft.local:50051").

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/shurui-liu/driftless/raft/proto/raftpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// rpcTimeout caps how long we wait for a single RPC.
// Must be well under electionTimeoutMin so a slow peer doesn't stall an election.
const rpcTimeout = 80 * time.Millisecond

// GRPCPeer implements the Peer interface. Election RPCs go over gRPC; the
// bulk InstallSnapshot RPC goes over HTTP to the peer's state server, which
// avoids regenerating protobuf stubs and keeps large snapshot bodies off the
// latency-sensitive gRPC path.
type GRPCPeer struct {
	id       string
	addr     string
	httpURL  string // e.g. "http://coordinator-b.raft.local:8080"
	conn     *grpc.ClientConn
	client   pb.RaftCoordinatorClient
	httpDoer *http.Client
}

// NewGRPCPeer dials a remote coordinator and returns a ready-to-use Peer.
// Call Close() when the node shuts down.
//
// grpcAddr: "coordinator-b.raft.local:50051" (ECS Cloud Map DNS name).
// httpURL:  "http://coordinator-b.raft.local:8080" — used for InstallSnapshot.
//           Pass "" to disable snapshot shipping (mostly for tests).
func NewGRPCPeer(id, grpcAddr, httpURL string) (*GRPCPeer, error) {
	addr := grpcAddr
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), // use TLS in prod
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Send a keepalive ping if the connection is idle for 10s.
			// Keeps the ECS NAT table alive between heartbeat bursts.
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return &GRPCPeer{
		id:       id,
		addr:     addr,
		httpURL:  httpURL,
		conn:     conn,
		client:   pb.NewRaftCoordinatorClient(conn),
		httpDoer: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// ID returns the stable peer identifier (used as map key in node.go).
func (p *GRPCPeer) ID() string { return p.id }

// IsReady reports whether the underlying connection is currently usable.
// node.go can skip unreachable peers rather than burning the rpcTimeout.
func (p *GRPCPeer) IsReady() bool {
	s := p.conn.GetState()
	return s == connectivity.Idle || s == connectivity.Ready
}

// Close tears down the connection. Call from main shutdown logic.
func (p *GRPCPeer) Close() error { return p.conn.Close() }

// ── Peer interface ────────────────────────────────────────────────────────────

func (p *GRPCPeer) RequestVote(ctx context.Context, args RequestVoteArgs) (RequestVoteReply, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	resp, err := p.client.RequestVote(ctx, &pb.RequestVoteRequest{
		Term:         int64(args.Term),
		CandidateId:  args.CandidateID,
		LastLogIndex: int64(args.LastLogIndex),
		LastLogTerm:  int64(args.LastLogTerm),
	})
	if err != nil {
		return RequestVoteReply{}, fmt.Errorf("RequestVote → %s: %w", p.addr, err)
	}

	return RequestVoteReply{
		Term:        int(resp.Term),
		VoteGranted: resp.VoteGranted,
	}, nil
}

func (p *GRPCPeer) AppendEntries(ctx context.Context, args AppendEntriesArgs) (AppendEntriesReply, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	pbEntries := make([]*pb.LogEntry, len(args.Entries))
	for i, e := range args.Entries {
		pbEntries[i] = &pb.LogEntry{
			Term:    int64(e.Term),
			Index:   int64(e.Index),
			Command: e.Command,
		}
	}

	resp, err := p.client.AppendEntries(ctx, &pb.AppendEntriesRequest{
		Term:         int64(args.Term),
		LeaderId:     args.LeaderID,
		PrevLogIndex: int64(args.PrevLogIndex),
		PrevLogTerm:  int64(args.PrevLogTerm),
		Entries:      pbEntries,
		LeaderCommit: int64(args.LeaderCommit),
	})
	if err != nil {
		return AppendEntriesReply{}, fmt.Errorf("AppendEntries → %s: %w", p.addr, err)
	}

	return AppendEntriesReply{
		Term:    int(resp.Term),
		Success: resp.Success,
	}, nil
}

// InstallSnapshot ships a RaftSnapshot to a lagging follower over HTTP.
// Uses the peer's state-server endpoint rather than gRPC to avoid regenerating
// protobuf stubs and to keep large bodies off the election-latency path.
func (p *GRPCPeer) InstallSnapshot(ctx context.Context, args InstallSnapshotArgs) (InstallSnapshotReply, error) {
	if p.httpURL == "" {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot → %s: httpURL not configured", p.id)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.httpURL+"/install-snapshot", bytes.NewReader(body))
	if err != nil {
		return InstallSnapshotReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpDoer.Do(req)
	if err != nil {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot → %s: %w", p.addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot → %s: status %d: %s", p.id, resp.StatusCode, string(raw))
	}
	var reply InstallSnapshotReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot decode: %w", err)
	}
	return reply, nil
}
