package raft

// grpc_server.go — receives incoming Raft RPCs and delegates to node.go.
//
// This file is intentionally thin. All Raft logic lives in node.go;
// the server only translates between protobuf wire types and Go structs.
//
// To generate the pb stubs, run from the repo root:
//   protoc --go_out=. --go-grpc_out=. proto/raft.proto

import (
	"context"
	"fmt"
	"net"

	pb "github.com/shurui-liu/driftless/raft/proto/raftpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCServer wraps a Raft Node and implements the generated RaftCoordinatorServer interface.
type GRPCServer struct {
	pb.UnimplementedRaftCoordinatorServer
	node *Node
}

// NewGRPCServer creates the server. Pass it the Node from node.go.
func NewGRPCServer(node *Node) *GRPCServer {
	return &GRPCServer{node: node}
}

// Serve starts listening on addr (e.g. ":50051") and blocks until ctx is cancelled.
// Wire this up in main.go alongside node.Run().
func (s *GRPCServer) Serve(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc listen %s: %w", addr, err)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			loggingInterceptor(s.node.log),
		),
	)
	pb.RegisterRaftCoordinatorServer(srv, s)

	// Shut down cleanly when the context is cancelled (e.g. SIGTERM in ECS).
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	s.node.log.Info("grpc server listening", "addr", addr)
	return srv.Serve(lis)
}

// ── RPC implementations ───────────────────────────────────────────────────────

func (s *GRPCServer) RequestVote(
	_ context.Context, req *pb.RequestVoteRequest,
) (*pb.RequestVoteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	// Translate protobuf → internal Go types.
	args := RequestVoteArgs{
		Term:         int(req.Term),
		CandidateID:  req.CandidateId,
		LastLogIndex: int(req.LastLogIndex),
		LastLogTerm:  int(req.LastLogTerm),
	}

	reply := s.node.HandleRequestVote(args)

	return &pb.RequestVoteResponse{
		Term:        int64(reply.Term),
		VoteGranted: reply.VoteGranted,
	}, nil
}

func (s *GRPCServer) AppendEntries(
	_ context.Context, req *pb.AppendEntriesRequest,
) (*pb.AppendEntriesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	entries := make([]LogEntry, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = LogEntry{
			Term:    int(e.Term),
			Index:   int(e.Index),
			Command: e.Command,
		}
	}

	args := AppendEntriesArgs{
		Term:         int(req.Term),
		LeaderID:     req.LeaderId,
		PrevLogIndex: int(req.PrevLogIndex),
		PrevLogTerm:  int(req.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: int(req.LeaderCommit),
	}

	reply := s.node.HandleAppendEntries(args)

	return &pb.AppendEntriesResponse{
		Term:    int64(reply.Term),
		Success: reply.Success,
	}, nil
}

// ── Interceptors ──────────────────────────────────────────────────────────────

// loggingInterceptor logs every incoming RPC with its result code.
// Keeps the server code clean while giving you full observability.
func loggingInterceptor(log interface{ Info(string, ...any) }) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		resp, err := handler(ctx, req)
		code := codes.OK
		if err != nil {
			code = status.Code(err)
		}
		log.Info("grpc rpc", "method", info.FullMethod, "code", code)
		return resp, err
	}
}
