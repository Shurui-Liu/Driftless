package raft

// snapshot_test.go — snapshot round-trip and recovery tests using an
// in-memory Snapshotter (no S3 dependency).

import (
	"context"
	"sync"
	"testing"
	"time"
)

// memSnapshotter is an in-process Snapshotter used by tests.
type memSnapshotter struct {
	mu   sync.Mutex
	snap *RaftSnapshot
}

func (m *memSnapshotter) Save(_ context.Context, s RaftSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy so the caller can mutate Data without affecting stored snapshot.
	cp := s
	cp.Data = append([]byte(nil), s.Data...)
	m.snap = &cp
	return nil
}

func (m *memSnapshotter) Load(_ context.Context) (*RaftSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil {
		return nil, nil
	}
	cp := *m.snap
	cp.Data = append([]byte(nil), m.snap.Data...)
	return &cp, nil
}

// snapshotableSM is a StateMachine that also supports Snapshot/Restore.
type snapshotableSM struct {
	mu      sync.Mutex
	applied []LogEntry
}

func (s *snapshotableSM) Apply(e LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, e)
}

func (s *snapshotableSM) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Serialize just the commands so the restored SM can compare.
	out := make([]byte, 0, len(s.applied))
	for _, e := range s.applied {
		out = append(out, e.Command...)
	}
	return out, nil
}

func (s *snapshotableSM) Restore(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = s.applied[:0]
	// Store a single synthetic entry representing "restored state".
	if len(data) > 0 {
		s.applied = append(s.applied, LogEntry{Command: append([]byte(nil), data...)})
	}
	return nil
}

func (s *snapshotableSM) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.applied)
}

// TestSnapshot_TakeAndCompactLog proposes entries, takes a snapshot, and
// verifies the in-memory log was compacted and lastIncludedIndex advanced.
func TestSnapshot_TakeAndCompactLog(t *testing.T) {
	nodes := buildCluster(3)
	sms := make([]*snapshotableSM, 3)
	for i, n := range nodes {
		sms[i] = &snapshotableSM{}
		n.SetStateMachine(sms[i])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range nodes {
		go n.Run(ctx)
	}

	leader := findLeader(nodes, 2*time.Second)
	if leader == nil {
		t.Fatal("no leader elected")
	}

	const N = 6
	for i := 0; i < N; i++ {
		ch, ok := leader.Propose([]byte{byte('a' + i)})
		if !ok {
			t.Fatalf("propose %d rejected", i)
		}
		select {
		case res := <-ch:
			if !res.Committed {
				t.Fatalf("entry %d not committed", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("entry %d wait timed out", i)
		}
	}

	// Wait for apply to finish on leader.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		leader.mu.Lock()
		done := leader.lastApplied >= N
		leader.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	store := &memSnapshotter{}
	if err := leader.TakeSnapshot(ctx, store); err != nil {
		t.Fatalf("TakeSnapshot failed: %v", err)
	}

	// In-memory log should have been compacted down to just the placeholder.
	leader.mu.Lock()
	firstIdx := leader.firstIndex()
	lastIdx := leader.lastIndex()
	leader.mu.Unlock()
	if firstIdx != N {
		t.Errorf("expected firstIndex=%d after compaction, got %d", N, firstIdx)
	}
	if lastIdx != N {
		t.Errorf("expected lastIndex=%d after compaction, got %d", N, lastIdx)
	}

	// Stored snapshot should match.
	stored, err := store.Load(ctx)
	if err != nil || stored == nil {
		t.Fatalf("load snapshot: err=%v stored=%v", err, stored)
	}
	if stored.LastIncludedIndex != N {
		t.Errorf("snapshot LastIncludedIndex: expected %d, got %d", N, stored.LastIncludedIndex)
	}
	if len(stored.Data) != N {
		t.Errorf("snapshot data: expected %d bytes, got %d", N, len(stored.Data))
	}
}

// TestSnapshot_RestoreResurrectsState verifies a fresh node loaded with a
// stored snapshot comes back with the right log baseline and SM state.
func TestSnapshot_RestoreResurrectsState(t *testing.T) {
	store := &memSnapshotter{}
	ctx := context.Background()

	// Fabricate a snapshot as if a prior run had committed 3 entries.
	if err := store.Save(ctx, RaftSnapshot{
		LastIncludedIndex: 3,
		LastIncludedTerm:  2,
		CurrentTerm:       2,
		Data:              []byte("xyz"),
	}); err != nil {
		t.Fatal(err)
	}

	n := newTestNode("resurrected", nil)
	sm := &snapshotableSM{}
	n.SetStateMachine(sm)

	if err := n.RestoreSnapshot(ctx, store); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	n.mu.Lock()
	if got := n.firstIndex(); got != 3 {
		t.Errorf("firstIndex: expected 3, got %d", got)
	}
	if got := n.lastIndex(); got != 3 {
		t.Errorf("lastIndex: expected 3, got %d", got)
	}
	if n.commitIndex != 3 || n.lastApplied != 3 {
		t.Errorf("commitIndex/lastApplied: expected 3/3, got %d/%d", n.commitIndex, n.lastApplied)
	}
	if n.currentTerm != 2 {
		t.Errorf("currentTerm: expected 2, got %d", n.currentTerm)
	}
	n.mu.Unlock()

	if got := sm.count(); got != 1 {
		t.Errorf("restored SM entries: expected 1, got %d", got)
	}
}

// TestHandleInstallSnapshot_InstallsStateAndAdvancesPointers verifies that a
// follower receiving a leader-shipped snapshot rewrites its log baseline,
// advances commit/apply pointers, and restores SM state in a single step.
func TestHandleInstallSnapshot_InstallsStateAndAdvancesPointers(t *testing.T) {
	n := newTestNode("follower-under-test", nil)
	sm := &snapshotableSM{}
	n.SetStateMachine(sm)

	reply := n.HandleInstallSnapshot(InstallSnapshotArgs{
		Term:              4,
		LeaderID:          "leader-x",
		LastIncludedIndex: 7,
		LastIncludedTerm:  3,
		Data:              []byte("abc"),
	})

	if reply.Term != 4 {
		t.Errorf("reply.Term: expected 4, got %d", reply.Term)
	}
	if !reply.Success {
		t.Errorf("expected Success=true")
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if got := n.firstIndex(); got != 7 {
		t.Errorf("firstIndex: expected 7, got %d", got)
	}
	if got := n.lastIndex(); got != 7 {
		t.Errorf("lastIndex: expected 7, got %d", got)
	}
	if n.commitIndex != 7 || n.lastApplied != 7 {
		t.Errorf("commit/apply: expected 7/7, got %d/%d", n.commitIndex, n.lastApplied)
	}
	if n.currentTerm != 4 {
		t.Errorf("currentTerm: expected 4, got %d", n.currentTerm)
	}
	if n.lastSnapshot == nil || n.lastSnapshot.LastIncludedIndex != 7 {
		t.Errorf("lastSnapshot not cached correctly: %+v", n.lastSnapshot)
	}
	if c := sm.count(); c != 1 {
		t.Errorf("SM Restore not applied: got count %d", c)
	}
}

// TestHandleInstallSnapshot_StaleTermRejected verifies that a snapshot from a
// stale leader is rejected without mutating follower state.
func TestHandleInstallSnapshot_StaleTermRejected(t *testing.T) {
	n := newTestNode("follower", nil)
	n.mu.Lock()
	n.currentTerm = 5
	n.mu.Unlock()

	reply := n.HandleInstallSnapshot(InstallSnapshotArgs{
		Term:              2,
		LastIncludedIndex: 10,
		LastIncludedTerm:  1,
	})

	if reply.Success {
		t.Errorf("expected Success=false for stale snapshot")
	}
	if reply.Term != 5 {
		t.Errorf("reply.Term: expected 5, got %d", reply.Term)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.firstIndex() != 0 {
		t.Errorf("stale install should not have compacted; firstIndex=%d", n.firstIndex())
	}
}

// TestSnapshot_ReplicationContinuesAfterCompaction verifies that after the
// leader compacts its log, new entries still replicate and apply everywhere.
func TestSnapshot_ReplicationContinuesAfterCompaction(t *testing.T) {
	nodes := buildCluster(3)
	sms := make([]*snapshotableSM, 3)
	for i, n := range nodes {
		sms[i] = &snapshotableSM{}
		n.SetStateMachine(sms[i])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range nodes {
		go n.Run(ctx)
	}

	leader := findLeader(nodes, 2*time.Second)
	if leader == nil {
		t.Fatal("no leader")
	}

	// Commit 3 entries, wait for apply on all nodes, then snapshot.
	for i := 0; i < 3; i++ {
		ch, ok := leader.Propose([]byte{byte('a' + i)})
		if !ok {
			t.Fatalf("propose %d rejected", i)
		}
		<-ch
	}
	// Let followers apply before we snapshot on leader. Snapshot compacts only
	// the leader's log; followers still have the full log but that's fine.
	time.Sleep(200 * time.Millisecond)
	if err := leader.TakeSnapshot(ctx, &memSnapshotter{}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Leader's firstIndex should now be 3.
	leader.mu.Lock()
	first := leader.firstIndex()
	leader.mu.Unlock()
	if first != 3 {
		t.Fatalf("expected firstIndex=3 after snapshot, got %d", first)
	}

	// Propose more entries — they must still commit and apply on every node.
	for i := 0; i < 2; i++ {
		ch, ok := leader.Propose([]byte{byte('x' + i)})
		if !ok {
			t.Fatalf("post-snapshot propose %d rejected", i)
		}
		select {
		case res := <-ch:
			if !res.Committed {
				t.Fatalf("post-snapshot entry %d not committed", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("post-snapshot entry %d wait timed out", i)
		}
	}

	// All 3 followers+leader should have seen 5 applies total.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, sm := range sms {
			if sm.count() < 5 {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, sm := range sms {
		if c := sm.count(); c != 5 {
			t.Errorf("node %d: expected 5 applies, got %d", i, c)
		}
	}
}
