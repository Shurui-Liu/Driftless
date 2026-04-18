package raft

// heartbeat.go — in-memory worker liveness tracking.
//
// Workers POST {worker_id, task_id} to /heartbeat every few seconds. The
// coordinator records last-seen timestamps so future reassignment logic can
// detect stalled workers. For now this is a pure observability aid — missed
// heartbeats are surfaced via /workers but do not yet trigger Raft log
// entries to revoke an assignment.

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// HeartbeatRequest matches the JSON sent by worker-node.
type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
	TaskID   string `json:"task_id,omitempty"`
}

// WorkerInfo is the observable snapshot of a worker's last heartbeat.
type WorkerInfo struct {
	WorkerID string    `json:"worker_id"`
	TaskID   string    `json:"task_id,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// WorkerTracker stores the most recent heartbeat per worker.
// Safe for concurrent access.
type WorkerTracker struct {
	mu      sync.RWMutex
	workers map[string]WorkerInfo
}

// NewWorkerTracker creates an empty tracker.
func NewWorkerTracker() *WorkerTracker {
	return &WorkerTracker{workers: make(map[string]WorkerInfo)}
}

// Record updates the last-seen time for a worker.
func (t *WorkerTracker) Record(workerID, taskID string) {
	if workerID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.workers[workerID] = WorkerInfo{
		WorkerID: workerID,
		TaskID:   taskID,
		LastSeen: time.Now().UTC(),
	}
}

// List returns a stable snapshot of all tracked workers, newest first.
func (t *WorkerTracker) List() []WorkerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(t.workers))
	for _, w := range t.workers {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Stale returns workers whose last heartbeat is older than threshold.
func (t *WorkerTracker) Stale(threshold time.Duration) []WorkerInfo {
	cutoff := time.Now().UTC().Add(-threshold)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []WorkerInfo
	for _, w := range t.workers {
		if w.LastSeen.Before(cutoff) {
			out = append(out, w)
		}
	}
	return out
}

// handleHeartbeat is wired onto the HTTP server's mux.
func (t *WorkerTracker) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var hb HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if hb.WorkerID == "" {
		http.Error(w, "worker_id required", http.StatusBadRequest)
		return
	}
	t.Record(hb.WorkerID, hb.TaskID)
	w.WriteHeader(http.StatusNoContent)
}

// handleWorkers returns all tracked workers as JSON — useful for debugging
// and for the State Observer.
func (t *WorkerTracker) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(t.List())
}
