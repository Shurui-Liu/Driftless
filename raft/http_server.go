package raft

// http_server.go — lightweight HTTP server exposing /state and /health.
//
// The Python State Observer polls GET /state (default port 8080) to read
// the node's Raft state (term, role, commit index, replication lag).
// This is intentionally separate from the gRPC server so it never blocks
// consensus traffic and requires zero protobuf tooling from the observer.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HTTPServer serves the /state, /health, /heartbeat, and /workers endpoints.
type HTTPServer struct {
	node    *Node
	workers *WorkerTracker
}

// NewHTTPServer creates a monitoring server backed by the given Raft node.
// If tracker is non-nil, /heartbeat and /workers are exposed.
func NewHTTPServer(node *Node, tracker *WorkerTracker) *HTTPServer {
	return &HTTPServer{node: node, workers: tracker}
}

// Serve starts the HTTP server on addr (e.g. ":8080") and blocks until ctx
// is cancelled or a fatal listen error occurs.
func (s *HTTPServer) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", s.handleState)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/install-snapshot", s.handleInstallSnapshot)
	if s.workers != nil {
		mux.HandleFunc("/heartbeat", s.workers.handleHeartbeat)
		mux.HandleFunc("/workers", s.workers.handleWorkers)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.node.log.Info("http state server listening", "addr", addr)
	if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleState returns the node's current Raft state as JSON.
// Called by the Python State Observer every few seconds.
func (s *HTTPServer) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.node.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		s.node.log.Warn("http /state encode error", "err", err)
	}
}

// handleInstallSnapshot receives a RaftSnapshot from the current leader and
// hands it to the node for installation. Body is JSON-encoded InstallSnapshotArgs.
func (s *HTTPServer) handleInstallSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Cap the body to keep a rogue peer from OOM'ing us. 64 MiB is plenty for
	// our state-machine sizes; bump if you change the SM to something larger.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)

	var args InstallSnapshotArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	reply := s.node.HandleInstallSnapshot(args)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		s.node.log.Warn("install-snapshot encode error", "err", err)
	}
}

// handleHealth returns 200 OK for ECS health checks and load balancer probes.
func (s *HTTPServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
