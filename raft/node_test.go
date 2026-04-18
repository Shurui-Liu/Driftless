package raft

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ── Mock Peer ─────────────────────────────────────────────────────────────────
// mockPeer lets each test control exactly what a remote node replies.
// Set voteGranted/voteTerm before the election; the fields are safe
// to read/write from multiple goroutines via the embedded mutex.

type mockPeer struct {
	mu sync.Mutex

	id string // stable peer identifier (defaults to a pointer-derived string)

	// RequestVote behaviour
	voteGranted bool
	voteTerm    int           // term returned in the reply (set > candidate term to trigger step-down)
	voteDelay   time.Duration // simulate a slow peer

	// AppendEntries behaviour
	appendSuccess bool
	appendTerm    int

	// Introspection — count how many times each RPC was called
	voteCallCount   int
	appendCallCount int
}

func (m *mockPeer) ID() string {
	if m.id != "" {
		return m.id
	}
	return fmt.Sprintf("mock-%p", m)
}

func (m *mockPeer) RequestVote(_ context.Context, args RequestVoteArgs) (RequestVoteReply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.voteCallCount++
	if m.voteDelay > 0 {
		time.Sleep(m.voteDelay)
	}
	return RequestVoteReply{
		Term:        m.voteTerm,
		VoteGranted: m.voteGranted,
	}, nil
}

func (m *mockPeer) AppendEntries(_ context.Context, args AppendEntriesArgs) (AppendEntriesReply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.appendCallCount++
	return AppendEntriesReply{
		Term:    m.appendTerm,
		Success: m.appendSuccess,
	}, nil
}

func (m *mockPeer) InstallSnapshot(_ context.Context, _ InstallSnapshotArgs) (InstallSnapshotReply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return InstallSnapshotReply{Term: m.appendTerm}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// newTestNode creates a Node with a no-op logger suitable for tests.
func newTestNode(id string, peers []Peer) *Node {
	return NewNode(id, peers, newTestLogger())
}

// waitForState polls until the node reaches the target state or the deadline passes.
// Returns true if the state was reached in time.
func waitForState(n *Node, target NodeState, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		s := n.state
		n.mu.Unlock()
		if s == target {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func stateName(s NodeState) string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestInitialStateIsFollower verifies every new node starts as a Follower.
func TestInitialStateIsFollower(t *testing.T) {
	n := newTestNode("node-a", nil)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != Follower {
		t.Errorf("expected initial state Follower, got %s", stateName(n.state))
	}
}

// TestFollowerBecomesCandiateOnTimeout verifies that a Follower with no
// peers (no heartbeats arriving) eventually times out and becomes a Candidate.
func TestFollowerBecomesCandiateOnTimeout(t *testing.T) {
	n := newTestNode("node-a", nil) // no peers → no heartbeats
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Election timeout is 150–300ms, give it 600ms to be safe.
	if !waitForState(n, Candidate, 600*time.Millisecond) {
		n.mu.Lock()
		s := n.state
		n.mu.Unlock()
		t.Errorf("expected Follower → Candidate after timeout, still in %s", stateName(s))
	}
}

// TestCandidateWinsElectionWithMajority verifies that a Candidate that
// receives votes from a majority of peers becomes Leader.
func TestCandidateWinsElectionWithMajority(t *testing.T) {
	// 3-node cluster: node-a + 2 peers that always grant their votes.
	peers := []Peer{
		&mockPeer{voteGranted: true, voteTerm: 1, appendSuccess: true},
		&mockPeer{voteGranted: true, voteTerm: 1, appendSuccess: true},
	}

	n := newTestNode("node-a", peers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Should elect a leader well within two election timeouts.
	if !waitForState(n, Leader, 800*time.Millisecond) {
		n.mu.Lock()
		s := n.state
		n.mu.Unlock()
		t.Errorf("expected Candidate → Leader after winning election, still in %s", stateName(s))
	}
}

// TestCandidateStepsDownOnHigherTerm verifies that a Candidate immediately
// reverts to Follower when a peer replies with a higher term.
func TestCandidateStepsDownOnHigherTerm(t *testing.T) {
	const highTerm = 9999

	peers := []Peer{
		&mockPeer{voteGranted: false, voteTerm: highTerm},
		&mockPeer{voteGranted: false, voteTerm: highTerm},
	}

	n := newTestNode("node-a", peers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Poll until the node is Follower AND has adopted the high term.
	// Simple waitForState(Follower) races because the node briefly touches
	// Follower between election retries before seeing the high-term reply.
	deadline := time.Now().Add(800 * time.Millisecond)
	steppedDown := false
	var finalTerm int
	for time.Now().Before(deadline) {
		n.mu.Lock()
		s := n.state
		term := n.currentTerm
		n.mu.Unlock()

		if s == Follower && term >= highTerm {
			steppedDown = true
			finalTerm = term
			cancel() // freeze the node immediately
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !steppedDown {
		n.mu.Lock()
		s := n.state
		term := n.currentTerm
		n.mu.Unlock()
		t.Errorf("expected Follower with term>=%d, got %s term=%d",
			highTerm, stateName(s), term)
		return
	}
	if finalTerm < highTerm {
		t.Errorf("expected currentTerm>=%d, got %d", highTerm, finalTerm)
	}
}

// TestLeaderStepsDownOnHigherTermHeartbeatReply verifies that a Leader
// reverts to Follower when a peer's AppendEntries reply carries a higher term.
func TestLeaderStepsDownOnHigherTermHeartbeatReply(t *testing.T) {
	p1 := &mockPeer{voteGranted: true, voteTerm: 1, appendSuccess: true}
	p2 := &mockPeer{voteGranted: true, voteTerm: 1, appendSuccess: true}
	peers := []Peer{p1, p2}

	n := newTestNode("node-a", peers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Wait for node to become Leader.
	if !waitForState(n, Leader, 800*time.Millisecond) {
		t.Fatal("node never became Leader")
	}

	// Now make peers reply with a higher term on the next heartbeat.
	p1.mu.Lock()
	p1.appendTerm = 99
	p1.appendSuccess = false
	p1.mu.Unlock()

	p2.mu.Lock()
	p2.appendTerm = 99
	p2.appendSuccess = false
	p2.mu.Unlock()

	// Leader should step down within one heartbeat interval + buffer.
	if !waitForState(n, Follower, 300*time.Millisecond) {
		n.mu.Lock()
		s := n.state
		n.mu.Unlock()
		t.Errorf("expected Leader → Follower after higher term reply, still in %s", stateName(s))
	}
}

// TestSplitVoteRetries verifies that when no majority is achieved (split vote),
// the node increments its term and starts a new election.
func TestSplitVoteRetries(t *testing.T) {
	// Both peers deny the vote — simulates a split vote situation.
	peers := []Peer{
		&mockPeer{voteGranted: false, voteTerm: 1},
		&mockPeer{voteGranted: false, voteTerm: 1},
	}

	n := newTestNode("node-a", peers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Wait long enough for at least two election attempts.
	time.Sleep(800 * time.Millisecond)
	cancel()

	n.mu.Lock()
	term := n.currentTerm
	n.mu.Unlock()

	// Term should have incremented at least twice (one per failed election).
	if term < 2 {
		t.Errorf("expected term >= 2 after split vote retries, got %d", term)
	}
}

// TestTermIncrementOnElection verifies that the term is incremented each
// time a node starts a new election.
func TestTermIncrementOnElection(t *testing.T) {
	n := newTestNode("node-a", nil)

	n.mu.Lock()
	if n.currentTerm != 0 {
		t.Errorf("expected initial term 0, got %d", n.currentTerm)
	}
	n.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Let one election fire.
	if !waitForState(n, Candidate, 600*time.Millisecond) {
		t.Fatal("node never became Candidate")
	}

	n.mu.Lock()
	term := n.currentTerm
	n.mu.Unlock()

	if term < 1 {
		t.Errorf("expected term >= 1 after first election, got %d", term)
	}
}

// TestVotedForIsSetOnElection verifies a node votes for itself when it
// becomes a Candidate.
func TestVotedForIsSetOnElection(t *testing.T) {
	n := newTestNode("node-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	if !waitForState(n, Candidate, 600*time.Millisecond) {
		t.Fatal("node never became Candidate")
	}

	n.mu.Lock()
	votedFor := n.votedFor
	n.mu.Unlock()

	if votedFor != "node-a" {
		t.Errorf("expected votedFor='node-a', got '%s'", votedFor)
	}
}

// TestHandleRequestVote_GrantsVoteToUpToDateCandidate verifies the RPC handler
// grants a vote when the candidate's term and log are up-to-date.
func TestHandleRequestVote_GrantsVoteToUpToDateCandidate(t *testing.T) {
	n := newTestNode("node-a", nil)

	reply := n.HandleRequestVote(RequestVoteArgs{
		Term:         1,
		CandidateID:  "node-b",
		LastLogIndex: 0,
		LastLogTerm:  0,
	})

	if !reply.VoteGranted {
		t.Error("expected vote to be granted to up-to-date candidate")
	}
	if reply.Term != 1 {
		t.Errorf("expected reply term=1, got %d", reply.Term)
	}
}

// TestHandleRequestVote_DeniesVoteForStaleTerm verifies the handler denies
// a vote request from a candidate with a lower term.
func TestHandleRequestVote_DeniesVoteForStaleTerm(t *testing.T) {
	n := newTestNode("node-a", nil)
	n.mu.Lock()
	n.currentTerm = 5
	n.mu.Unlock()

	reply := n.HandleRequestVote(RequestVoteArgs{
		Term:         3, // stale
		CandidateID:  "node-b",
		LastLogIndex: 0,
		LastLogTerm:  0,
	})

	if reply.VoteGranted {
		t.Error("expected vote to be denied for stale term")
	}
}

// TestHandleRequestVote_DeniesDoubleVoteInSameTerm verifies a node won't
// vote for two different candidates in the same term.
func TestHandleRequestVote_DeniesDoubleVoteInSameTerm(t *testing.T) {
	n := newTestNode("node-a", nil)

	// First vote — should be granted.
	r1 := n.HandleRequestVote(RequestVoteArgs{
		Term: 1, CandidateID: "node-b",
		LastLogIndex: 0, LastLogTerm: 0,
	})
	if !r1.VoteGranted {
		t.Fatal("first vote should be granted")
	}

	// Second vote for a different candidate in the same term — must be denied.
	r2 := n.HandleRequestVote(RequestVoteArgs{
		Term: 1, CandidateID: "node-c",
		LastLogIndex: 0, LastLogTerm: 0,
	})
	if r2.VoteGranted {
		t.Error("second vote in same term should be denied (already voted for node-b)")
	}
}

// TestHandleRequestVote_AllowsRevoteForSameCandidate verifies idempotency —
// voting for the same candidate twice in the same term is allowed.
func TestHandleRequestVote_AllowsRevoteForSameCandidate(t *testing.T) {
	n := newTestNode("node-a", nil)

	args := RequestVoteArgs{
		Term: 1, CandidateID: "node-b",
		LastLogIndex: 0, LastLogTerm: 0,
	}

	r1 := n.HandleRequestVote(args)
	r2 := n.HandleRequestVote(args) // same candidate, same term

	if !r1.VoteGranted || !r2.VoteGranted {
		t.Error("expected both votes to be granted for same candidate in same term")
	}
}

// TestHandleAppendEntries_AcceptsHeartbeatFromCurrentLeader verifies that
// a valid heartbeat resets the election timer and returns success.
func TestHandleAppendEntries_AcceptsHeartbeatFromCurrentLeader(t *testing.T) {
	n := newTestNode("node-a", nil)
	n.mu.Lock()
	n.currentTerm = 2
	n.mu.Unlock()

	reply := n.HandleAppendEntries(AppendEntriesArgs{
		Term:     2,
		LeaderID: "node-b",
		Entries:  nil, // heartbeat
	})

	if !reply.Success {
		t.Error("expected heartbeat to be accepted")
	}
}

// TestHandleAppendEntries_RejectsStaleLeader verifies that a heartbeat from
// a leader with a lower term is rejected.
func TestHandleAppendEntries_RejectsStaleLeader(t *testing.T) {
	n := newTestNode("node-a", nil)
	n.mu.Lock()
	n.currentTerm = 5
	n.mu.Unlock()

	reply := n.HandleAppendEntries(AppendEntriesArgs{
		Term:     3, // stale
		LeaderID: "node-b",
	})

	if reply.Success {
		t.Error("expected stale leader heartbeat to be rejected")
	}
	if reply.Term != 5 {
		t.Errorf("expected reply term=5, got %d", reply.Term)
	}
}

// TestHandleAppendEntries_CandidateStepsDownOnValidLeader verifies that a
// Candidate reverts to Follower when it receives a valid AppendEntries.
func TestHandleAppendEntries_CandidateStepsDownOnValidLeader(t *testing.T) {
	n := newTestNode("node-a", nil)
	n.mu.Lock()
	n.state = Candidate
	n.currentTerm = 2
	n.mu.Unlock()

	reply := n.HandleAppendEntries(AppendEntriesArgs{
		Term:     2,
		LeaderID: "node-b",
	})

	if !reply.Success {
		t.Error("expected AppendEntries to succeed")
	}

	n.mu.Lock()
	s := n.state
	n.mu.Unlock()

	if s != Follower {
		t.Errorf("expected Candidate → Follower on valid AppendEntries, got %s", stateName(s))
	}
}

// TestElectionTimerResetOnHeartbeat verifies that a heartbeat prevents
// the follower from triggering a new election by resetting its timer.
func TestElectionTimerResetOnHeartbeat(t *testing.T) {
	n := newTestNode("node-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go n.Run(ctx)

	// Repeatedly send heartbeats every 50ms for 400ms total.
	// The node should stay Follower the whole time.
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 8; i++ {
			<-ticker.C
			n.HandleAppendEntries(AppendEntriesArgs{
				Term:     1,
				LeaderID: "fake-leader",
			})
		}
	}()

	// After 300ms of heartbeats the node should still be a Follower.
	time.Sleep(300 * time.Millisecond)

	n.mu.Lock()
	s := n.state
	n.mu.Unlock()

	if s != Follower {
		t.Errorf("expected node to remain Follower while receiving heartbeats, got %s", stateName(s))
	}
}
