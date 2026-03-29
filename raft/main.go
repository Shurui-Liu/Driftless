package main

// main.go — entry point for the Raft coordinator ECS task.
//
// Environment variables (set in your Terraform ECS task definition):
//   NODE_ID      — stable unique name, e.g. "coordinator-a"
//   GRPC_PORT    — port this node listens on, e.g. "50051"
//   PEER_ADDRS   — comma-separated addresses of the OTHER coordinators
//                  e.g. "coordinator-b.raft.local:50051,coordinator-c.raft.local:50051"
//
// ECS Cloud Map populates the DNS names automatically once the services are up.

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/yourorg/raft-coordinator/raft"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	nodeID := mustEnv("NODE_ID")
	grpcPort := envOr("GRPC_PORT", "50051")
	peerAddrs := strings.Split(mustEnv("PEER_ADDRS"), ",")

	// ── Dial peers ───────────────────────────────────────────────────────────
	// Each peer address is "coordinator-X.raft.local:50051".
	// Cloud Map resolves these once all three ECS tasks are healthy.
	peers := make([]raft.Peer, 0, len(peerAddrs))
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
		peers = append(peers, p)
		grpcPeers = append(grpcPeers, p)
		log.Info("dialed peer", "id", peerID, "addr", addr)
	}

	// ── Build and start the Raft node ─────────────────────────────────────────
	node := raft.NewNode(nodeID, peers, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var wg sync.WaitGroup

	// Raft state machine runs in its own goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		node.Run(ctx)
		log.Info("raft node stopped")
	}()

	// gRPC server runs in its own goroutine.
	server := raft.NewGRPCServer(node)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(ctx, ":"+grpcPort); err != nil {
			log.Error("grpc server error", "err", err)
			cancel() // bring down the whole node on server failure
		}
	}()

	log.Info("coordinator started", "id", nodeID, "port", grpcPort, "peers", peerAddrs)
	wg.Wait()

	// ── Clean up peer connections ─────────────────────────────────────────────
	for _, p := range grpcPeers {
		if err := p.Close(); err != nil {
			log.Warn("error closing peer connection", "peer", p.ID(), "err", err)
		}
	}
	log.Info("coordinator shut down cleanly")
}

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

// peerIDFromAddr derives a stable peer ID from its DNS address.
// "coordinator-b.raft.local:50051" → "coordinator-b"
func peerIDFromAddr(addr string, fallbackIdx int) string {
	host := strings.SplitN(addr, ":", 2)[0]
	parts := strings.SplitN(host, ".", 2)
	if parts[0] != "" {
		return parts[0]
	}
	// fallback — should never happen with well-formed Cloud Map DNS
	return "peer-" + string(rune('a'+fallbackIdx))
}
