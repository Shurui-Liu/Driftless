package main

// main.go — entry point for the Raft coordinator ECS task.
//
// Environment variables (set in your Terraform ECS task definition):
//   NODE_ID            — stable unique name, e.g. "coordinator-a"
//   GRPC_PORT          — port this node listens on, e.g. "50051"
//   HTTP_PORT          — state-server HTTP port (default 8080)
//   PEERS_TABLE        — DynamoDB table for peer discovery (production)
//   EXPECTED_PEER_IDS  — comma-separated IDs of the OTHER coordinators this
//                        node should wait for, e.g. "coordinator-b,coordinator-c"
//
// Local / single-node dev fallback: leave PEERS_TABLE unset and pass
//   PEER_ADDRS         — comma-separated host:port peer addresses
//
// On ECS, each coordinator registers its private IP in the peers DynamoDB
// table on startup and waits for the EXPECTED_PEER_IDS to appear before
// dialing. This replaces the previous Cloud Map (raft.local) setup.

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/shurui-liu/driftless/raft"
	peersreg "github.com/shurui-liu/driftless/raft/peers"
)

// resolvedPeer is the address form used to dial a peer, after discovery.
type resolvedPeer struct {
	ID       string
	GRPCAddr string // "10.0.1.42:50051"
	HTTPURL  string // "http://10.0.1.42:8080"
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	nodeID := mustEnv("NODE_ID")
	grpcPort := envOr("GRPC_PORT", "50051")
	httpPort := envOr("HTTP_PORT", "8080") // state observer polls this

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Lazy AWS config loader — shared by peer registry, dispatcher, snapshotter.
	var awsCfg aws.Config
	var awsCfgLoaded bool
	loadAWS := func() aws.Config {
		if awsCfgLoaded {
			return awsCfg
		}
		c, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			log.Error("load AWS config", "err", err)
			os.Exit(1)
		}
		awsCfg = c
		awsCfgLoaded = true
		return awsCfg
	}

	// ── Peer resolution ──────────────────────────────────────────────────────
	// Production: register self in the DynamoDB peers table, wait for
	// expected peers, resolve their IPs.
	// Local: parse PEER_ADDRS directly (no AWS needed).

	var resolved []resolvedPeer
	var peersRegistry *peersreg.Registry
	var peersSelf peersreg.Info

	if table := os.Getenv("PEERS_TABLE"); table != "" {
		reg := peersreg.NewRegistry(dynamodb.NewFromConfig(loadAWS()), table)

		selfIP, err := peersreg.DiscoverSelfIP(ctx)
		if err != nil {
			log.Error("discover self IP", "err", err)
			os.Exit(1)
		}
		self := peersreg.Info{
			NodeID:   nodeID,
			GRPCAddr: selfIP + ":" + grpcPort,
			HTTPAddr: selfIP + ":" + httpPort,
		}
		if err := reg.Register(ctx, self, false); err != nil {
			log.Error("register self in peers table", "err", err)
			os.Exit(1)
		}
		peersRegistry = reg
		peersSelf = self
		log.Info("registered self", "node_id", nodeID, "grpc_addr", self.GRPCAddr)

		expected := splitPeers(os.Getenv("EXPECTED_PEER_IDS"))
		if len(expected) > 0 {
			log.Info("waiting for peers", "expected", expected)
			infos, err := reg.WaitForPeers(ctx, expected, 2*time.Second)
			if err != nil {
				log.Error("wait for peers", "err", err)
				os.Exit(1)
			}
			for _, info := range infos {
				resolved = append(resolved, resolvedPeer{
					ID:       info.NodeID,
					GRPCAddr: info.GRPCAddr,
					HTTPURL:  "http://" + info.HTTPAddr,
				})
			}
		}
	} else {
		// Local/dev fallback: addresses supplied directly.
		for i, addr := range splitPeers(os.Getenv("PEER_ADDRS")) {
			resolved = append(resolved, resolvedPeer{
				ID:       peerIDFromAddr(addr, i),
				GRPCAddr: addr,
				HTTPURL:  peerHTTPURL(addr, httpPort),
			})
		}
	}

	// ── Dial peers ───────────────────────────────────────────────────────────
	peers := make([]raft.Peer, 0, len(resolved))
	grpcPeers := make([]*raft.GRPCPeer, 0, len(resolved))
	for _, rp := range resolved {
		p, err := raft.NewGRPCPeer(rp.ID, rp.GRPCAddr, rp.HTTPURL)
		if err != nil {
			log.Error("failed to dial peer", "id", rp.ID, "addr", rp.GRPCAddr, "err", err)
			os.Exit(1)
		}
		peers = append(peers, p)
		grpcPeers = append(grpcPeers, p)
		log.Info("dialed peer", "id", rp.ID, "addr", rp.GRPCAddr)
	}

	// ── Build the Raft node ───────────────────────────────────────────────────
	node := raft.NewNode(nodeID, peers, log)

	// Peers heartbeat now that node exists; each tick publishes live leadership.
	if peersRegistry != nil {
		peersRegistry.StartHeartbeat(ctx, peersSelf, 10*time.Second, node.IsLeader)
	}

	var wg sync.WaitGroup

	// Worker heartbeat tracker (exposed via /heartbeat and /workers).
	tracker := raft.NewWorkerTracker()

	// ── AWS wiring (dispatcher + snapshotter) ─────────────────────────────────
	// Dispatcher needs SQS + DynamoDB; snapshotter needs S3. Each is enabled
	// independently via env vars so local/unit runs can skip AWS entirely.
	ingestURL := os.Getenv("SQS_INGEST_URL")
	assignURL := os.Getenv("SQS_ASSIGNMENT_URL")
	tasksTable := os.Getenv("DYNAMO_TABLE")
	snapshotBucket := os.Getenv("SNAPSHOT_S3_BUCKET")
	snapshotPrefix := envOr("SNAPSHOT_S3_PREFIX", "raft")

	dispatcherEnabled := ingestURL != "" && assignURL != "" && tasksTable != ""
	snapshotEnabled := snapshotBucket != ""

	var disp *raft.Dispatcher
	if dispatcherEnabled {
		cfg := loadAWS()
		disp = raft.NewDispatcher(
			node,
			sqs.NewFromConfig(cfg),
			dynamodb.NewFromConfig(cfg),
			raft.DispatcherConfig{
				IngestQueueURL:     ingestURL,
				AssignmentQueueURL: assignURL,
				TasksTable:         tasksTable,
			},
			tracker,
			log,
		)
		// Dispatcher is both the ingest consumer AND the replicated state
		// machine — Apply fires on every commit (on every node).
		node.SetStateMachine(disp)
	} else {
		log.Info("dispatcher disabled — set SQS_INGEST_URL, SQS_ASSIGNMENT_URL, DYNAMO_TABLE to enable")
	}

	var snapshotter *raft.S3Snapshotter
	if snapshotEnabled {
		cfg := loadAWS()
		snapshotter = raft.NewS3Snapshotter(s3.NewFromConfig(cfg), snapshotBucket, snapshotPrefix, nodeID)
		// Restore MUST happen before Run starts the apply loop — otherwise
		// applyLoop would replay entries that are already folded into the
		// snapshot and Apply would see them again.
		if err := node.RestoreSnapshot(ctx, snapshotter); err != nil {
			log.Error("restore snapshot", "err", err)
			os.Exit(1)
		}
	} else {
		log.Info("snapshotter disabled — set SNAPSHOT_S3_BUCKET to enable")
	}

	// ── Start goroutines ──────────────────────────────────────────────────────
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

	httpServer := raft.NewHTTPServer(node, tracker)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := httpServer.Serve(ctx, ":"+httpPort); err != nil {
			log.Error("http state server error", "err", err)
		}
	}()

	if disp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			disp.Run(ctx)
			log.Info("dispatcher stopped")
		}()
	}

	if snapshotter != nil {
		threshold, _ := strconv.Atoi(envOr("SNAPSHOT_THRESHOLD", "1000"))
		intervalSec, _ := strconv.Atoi(envOr("SNAPSHOT_INTERVAL_SECONDS", "60"))
		wg.Add(1)
		go func() {
			defer wg.Done()
			node.RunSnapshotLoop(ctx, snapshotter, threshold, time.Duration(intervalSec)*time.Second)
			log.Info("snapshot loop stopped")
		}()
	}

	log.Info("coordinator started", "id", nodeID, "grpc_port", grpcPort, "http_port", httpPort, "peers", len(resolved))
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

func splitPeers(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// peerIDFromAddr derives a stable peer ID from a host:port address.
// Used only by the PEER_ADDRS fallback path.
func peerIDFromAddr(addr string, fallbackIdx int) string {
	host := strings.SplitN(addr, ":", 2)[0]
	parts := strings.SplitN(host, ".", 2)
	if parts[0] != "" {
		return parts[0]
	}
	return "peer-" + string(rune('a'+fallbackIdx))
}

// peerHTTPURL derives a peer's state-server URL from its gRPC address and the
// shared HTTP port. Used only by the PEER_ADDRS fallback path.
func peerHTTPURL(grpcAddr, httpPort string) string {
	host := strings.SplitN(grpcAddr, ":", 2)[0]
	return "http://" + host + ":" + httpPort
}
