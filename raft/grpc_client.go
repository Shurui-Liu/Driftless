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
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/shurui-liu/driftless/raft/proto/raftpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// PeerResolver looks up a peer's current grpc/http address by node ID.
// When an RPC fails, the client uses this to detect peer IP changes (e.g.
// after an ECS task restart assigns a new ENI) and rebuild the connection.
// nil is allowed — peers dialed at startup without a resolver are static.
type PeerResolver func(ctx context.Context, nodeID string) (grpcAddr, httpAddr string, err error)

// reresolveCooldown rate-limits DDB lookups after RPC failures so an
// ongoing election storm doesn't hammer the peers table.
const reresolveCooldown = 500 * time.Millisecond

// rpcTimeout caps how long we wait for a single RPC.
// Must be well under electionTimeoutMin so a slow peer doesn't stall an election.
const rpcTimeout = 80 * time.Millisecond

// GRPCPeer implements the Peer interface. Election RPCs go over gRPC; the
// bulk InstallSnapshot RPC goes over HTTP to the peer's state server, which
// avoids regenerating protobuf stubs and keeps large snapshot bodies off the
// latency-sensitive gRPC path.
//
// The ClientConn is rebuilt when the peer's address changes in the registry
// (e.g. Fargate rescheduled the task onto a new ENI). Swaps happen under
// mu; RPCs snapshot the active client under lock before sending, so an
// in-flight call can complete against the old conn even while a swap is
// underway.
type GRPCPeer struct {
	id       string
	httpDoer *http.Client
	resolver PeerResolver

	mu      sync.Mutex
	addr    string
	httpURL string
	conn    *grpc.ClientConn
	client  pb.RaftCoordinatorClient

	lastResolveNs atomic.Int64 // unix-nanos of last re-resolve attempt
}

// NewGRPCPeer dials a remote coordinator and returns a ready-to-use Peer.
// Call Close() when the node shuts down.
//
// grpcAddr: "10.0.1.42:50051" (typically an ECS task IP, since we no longer
//           use Cloud Map DNS).
// httpURL:  "http://10.0.1.42:8080" — used for InstallSnapshot. Pass "" to
//           disable snapshot shipping (mostly for tests).
// resolver: optional. When set, an RPC failure triggers an async re-lookup
//           via this resolver and a conn rebuild if the peer's address
//           changed (ECS restart → new ENI IP). Pass nil for static dials.
func NewGRPCPeer(id, grpcAddr, httpURL string, resolver PeerResolver) (*GRPCPeer, error) {
	conn, err := dialPeer(grpcAddr)
	if err != nil {
		return nil, err
	}
	return &GRPCPeer{
		id:       id,
		addr:     grpcAddr,
		httpURL:  httpURL,
		conn:     conn,
		client:   pb.NewRaftCoordinatorClient(conn),
		httpDoer: &http.Client{Timeout: 30 * time.Second},
		resolver: resolver,
	}, nil
}

func dialPeer(addr string) (*grpc.ClientConn, error) {
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
	return conn, nil
}

// snapshot returns the currently-active client and the address it points at,
// under the swap lock. The caller uses the returned pair for a single RPC;
// a concurrent tryReresolve may close the underlying conn after this returns,
// in which case the in-flight RPC will surface a connection error and be
// retried at the next heartbeat.
func (p *GRPCPeer) snapshot() (pb.RaftCoordinatorClient, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client, p.addr
}

// tryReresolve hits the peer registry for this peer's current address and
// swaps the conn if it changed. Best-effort, rate-limited. Safe to call
// from any goroutine; kicked off async by failed RPC paths.
func (p *GRPCPeer) tryReresolve() {
	if p.resolver == nil {
		return
	}
	now := time.Now().UnixNano()
	last := p.lastResolveNs.Load()
	if now-last < reresolveCooldown.Nanoseconds() {
		return
	}
	if !p.lastResolveNs.CompareAndSwap(last, now) {
		return // another goroutine beat us to it
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	newGRPC, newHTTP, err := p.resolver(ctx, p.id)
	if err != nil || newGRPC == "" {
		return
	}

	p.mu.Lock()
	if newGRPC == p.addr {
		p.mu.Unlock()
		return
	}
	oldConn := p.conn
	oldAddr := p.addr
	newConn, err := dialPeer(newGRPC)
	if err != nil {
		p.mu.Unlock()
		return
	}
	p.conn = newConn
	p.client = pb.NewRaftCoordinatorClient(newConn)
	p.addr = newGRPC
	if newHTTP != "" {
		p.httpURL = "http://" + newHTTP
	}
	p.mu.Unlock()

	// Close outside the lock — Close() can block briefly tearing down streams.
	go func(c *grpc.ClientConn, from string) {
		_ = c.Close()
		_ = from // swallowed; diagnostic only
	}(oldConn, oldAddr)
}

// ID returns the stable peer identifier (used as map key in node.go).
func (p *GRPCPeer) ID() string { return p.id }

// IsReady reports whether the underlying connection is currently usable.
// node.go can skip unreachable peers rather than burning the rpcTimeout.
// A non-ready conn also kicks off an async re-resolve — the peer may have
// been rescheduled to a new IP since last boot.
func (p *GRPCPeer) IsReady() bool {
	p.mu.Lock()
	s := p.conn.GetState()
	p.mu.Unlock()
	ready := s == connectivity.Idle || s == connectivity.Ready
	if !ready {
		go p.tryReresolve()
	}
	return ready
}

// Close tears down the connection. Call from main shutdown logic.
func (p *GRPCPeer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.Close()
}

// ── Peer interface ────────────────────────────────────────────────────────────

func (p *GRPCPeer) RequestVote(ctx context.Context, args RequestVoteArgs) (RequestVoteReply, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	client, addr := p.snapshot()
	resp, err := client.RequestVote(ctx, &pb.RequestVoteRequest{
		Term:         int64(args.Term),
		CandidateId:  args.CandidateID,
		LastLogIndex: int64(args.LastLogIndex),
		LastLogTerm:  int64(args.LastLogTerm),
	})
	if err != nil {
		go p.tryReresolve()
		return RequestVoteReply{}, fmt.Errorf("RequestVote → %s: %w", addr, err)
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

	client, addr := p.snapshot()
	resp, err := client.AppendEntries(ctx, &pb.AppendEntriesRequest{
		Term:         int64(args.Term),
		LeaderId:     args.LeaderID,
		PrevLogIndex: int64(args.PrevLogIndex),
		PrevLogTerm:  int64(args.PrevLogTerm),
		Entries:      pbEntries,
		LeaderCommit: int64(args.LeaderCommit),
	})
	if err != nil {
		go p.tryReresolve()
		return AppendEntriesReply{}, fmt.Errorf("AppendEntries → %s: %w", addr, err)
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
	p.mu.Lock()
	httpURL := p.httpURL
	addr := p.addr
	p.mu.Unlock()
	if httpURL == "" {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot → %s: httpURL not configured", p.id)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL+"/install-snapshot", bytes.NewReader(body))
	if err != nil {
		return InstallSnapshotReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpDoer.Do(req)
	if err != nil {
		go p.tryReresolve()
		return InstallSnapshotReply{}, fmt.Errorf("InstallSnapshot → %s: %w", addr, err)
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
