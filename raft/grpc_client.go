package raft

// grpc_client.go — implements the Peer interface so node.go can call remote coordinators.
//
// One GRPCPeer is created per remote coordinator node. In ECS, the address
// comes from Cloud Map service discovery (e.g. "coordinator-b.raft.local:50051").

import (
	"context"
	"fmt"
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

// GRPCPeer implements the Peer interface using a gRPC connection to one remote node.
type GRPCPeer struct {
	id     string
	addr   string
	conn   *grpc.ClientConn
	client pb.RaftCoordinatorClient
}

// NewGRPCPeer dials a remote coordinator and returns a ready-to-use Peer.
// Call Close() when the node shuts down.
//
// addr format: "coordinator-b.raft.local:50051"  (ECS Cloud Map DNS name)
func NewGRPCPeer(id, addr string) (*GRPCPeer, error) {
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
		id:     id,
		addr:   addr,
		conn:   conn,
		client: pb.NewRaftCoordinatorClient(conn),
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
