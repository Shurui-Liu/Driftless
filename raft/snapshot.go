package raft

// snapshot.go — Raft log compaction via state-machine snapshots.
//
// A snapshot captures the full SM state as of lastApplied, plus Raft's
// persistent term/vote, so a restarted node can skip straight to the tail of
// the log without replaying every committed entry.
//
// The node takes snapshots periodically when the uncompacted log grows past
// a threshold. On boot, the node loads the most recent snapshot (if any),
// restores the SM, and resumes replication from the snapshot's index.

import (
	"context"
	"errors"
	"time"
)

// RaftSnapshot is what's persisted to storage. Self-contained — everything
// needed to resurrect a node without any other state.
type RaftSnapshot struct {
	// Raft bookkeeping that must survive restart to preserve safety.
	LastIncludedIndex int    `json:"last_included_index"`
	LastIncludedTerm  int    `json:"last_included_term"`
	CurrentTerm       int    `json:"current_term"`
	VotedFor          string `json:"voted_for"`

	// Opaque state-machine bytes — produced by SnapshotableStateMachine.Snapshot.
	Data []byte `json:"data"`

	// Wall-clock timestamp for operator visibility; not used for correctness.
	TakenAt time.Time `json:"taken_at"`
}

// Snapshotter is the storage backend for RaftSnapshots (e.g. S3).
// Load returns (nil, nil) when no snapshot exists — not an error.
type Snapshotter interface {
	Save(ctx context.Context, snap RaftSnapshot) error
	Load(ctx context.Context) (*RaftSnapshot, error)
}

// SnapshotableStateMachine is the capability the state machine must implement
// to participate in snapshotting. Nodes whose SM doesn't implement this
// interface simply skip snapshotting (no crash, just unbounded log growth).
type SnapshotableStateMachine interface {
	StateMachine
	// Snapshot returns a serialized, self-contained view of SM state as of
	// the last Apply call. Must be safe to call concurrently with Apply —
	// the node calls it under its own lock, but SMs with their own state
	// should internally lock too.
	Snapshot() ([]byte, error)
	// Restore replaces SM state with the given bytes. Called during boot
	// before Run starts the apply loop, so no concurrency concerns.
	Restore([]byte) error
}

// ErrNoStateMachine is returned by TakeSnapshot if no SM is set or the SM
// doesn't implement SnapshotableStateMachine.
var ErrNoStateMachine = errors.New("raft: state machine does not support snapshots")

// RestoreSnapshot loads the most recent snapshot from storage and seeds the
// node + state machine. Must be called BEFORE Run. Safe to call with a
// Snapshotter that returns (nil, nil) — that just means a fresh start.
func (n *Node) RestoreSnapshot(ctx context.Context, snap Snapshotter) error {
	s, err := snap.Load(ctx)
	if err != nil {
		return err
	}
	if s == nil {
		return nil // fresh node
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	// Seed Raft state.
	n.logEntries = []LogEntry{{Term: s.LastIncludedTerm, Index: s.LastIncludedIndex}}
	n.commitIndex = s.LastIncludedIndex
	n.lastApplied = s.LastIncludedIndex
	n.currentTerm = s.CurrentTerm
	n.votedFor = s.VotedFor

	// Seed state machine.
	if sm, ok := n.sm.(SnapshotableStateMachine); ok && len(s.Data) > 0 {
		if err := sm.Restore(s.Data); err != nil {
			return err
		}
	}
	// Cache for leader-side InstallSnapshot service.
	snapCopy := *s
	n.lastSnapshot = &snapCopy
	n.log.Info("snapshot restored",
		"last_included_index", s.LastIncludedIndex,
		"last_included_term", s.LastIncludedTerm,
		"current_term", s.CurrentTerm,
	)
	return nil
}

// TakeSnapshot captures SM state up to lastApplied, writes it to storage,
// and compacts the in-memory log. Safe to call concurrently with Raft
// traffic; it only blocks the node mutex briefly to read/write bookkeeping.
func (n *Node) TakeSnapshot(ctx context.Context, store Snapshotter) error {
	n.mu.Lock()
	sm, ok := n.sm.(SnapshotableStateMachine)
	if !ok {
		n.mu.Unlock()
		return ErrNoStateMachine
	}
	// Nothing new to snapshot.
	if n.lastApplied <= n.firstIndex() {
		n.mu.Unlock()
		return nil
	}
	lastIdx := n.lastApplied
	lastTerm := n.termAt(lastIdx)
	currentTerm := n.currentTerm
	votedFor := n.votedFor
	n.mu.Unlock()

	// Serialize outside the main lock — SM has its own locking.
	data, err := sm.Snapshot()
	if err != nil {
		return err
	}

	rs := RaftSnapshot{
		LastIncludedIndex: lastIdx,
		LastIncludedTerm:  lastTerm,
		CurrentTerm:       currentTerm,
		VotedFor:          votedFor,
		Data:              data,
		TakenAt:           time.Now().UTC(),
	}
	if err := store.Save(ctx, rs); err != nil {
		return err
	}

	// Only compact AFTER a successful save — otherwise a crash between
	// compact and save would lose committed state.
	n.mu.Lock()
	n.compactLogLocked(lastIdx, lastTerm)
	// Cache for InstallSnapshot to lagging followers.
	snapCopy := rs
	n.lastSnapshot = &snapCopy
	n.mu.Unlock()

	n.log.Info("snapshot taken", "last_included_index", lastIdx, "bytes", len(data))
	return nil
}

// RunSnapshotLoop periodically takes a snapshot when the in-memory tail has
// grown past threshold entries. Blocks until ctx is cancelled.
func (n *Node) RunSnapshotLoop(
	ctx context.Context,
	store Snapshotter,
	threshold int,
	interval time.Duration,
) {
	if threshold < 1 {
		threshold = 1000
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.mu.Lock()
		uncompacted := n.lastApplied - n.firstIndex()
		n.mu.Unlock()
		if uncompacted < threshold {
			continue
		}
		if err := n.TakeSnapshot(ctx, store); err != nil {
			n.log.Warn("snapshot failed", "err", err)
		}
	}
}
