package raft

// replication_test.go — end-to-end replication tests wiring real Node
// instances together via in-process peer adapters (no gRPC).

import (
	"context"
	"sync"
	"testing"
	"time"
)

// inprocPeer routes RPCs directly to another Node's handlers.
type inprocPeer struct {
	id     string
	target *Node
}

func (p *inprocPeer) ID() string { return p.id }

func (p *inprocPeer) RequestVote(_ context.Context, args RequestVoteArgs) (RequestVoteReply, error) {
	return p.target.HandleRequestVote(args), nil
}

func (p *inprocPeer) AppendEntries(_ context.Context, args AppendEntriesArgs) (AppendEntriesReply, error) {
	return p.target.HandleAppendEntries(args), nil
}

func (p *inprocPeer) InstallSnapshot(_ context.Context, args InstallSnapshotArgs) (InstallSnapshotReply, error) {
	return p.target.HandleInstallSnapshot(args), nil
}

// recordingSM captures every Apply call.
type recordingSM struct {
	mu      sync.Mutex
	applied []LogEntry
}

func (r *recordingSM) Apply(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, e)
}

func (r *recordingSM) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

// buildCluster wires N in-process nodes pointing at each other.
func buildCluster(n int) []*Node {
	nodes := make([]*Node, n)
	for i := 0; i < n; i++ {
		nodes[i] = newTestNode(peerName(i), nil)
	}
	for i, me := range nodes {
		peers := make([]Peer, 0, n-1)
		for j, other := range nodes {
			if i == j {
				continue
			}
			peers = append(peers, &inprocPeer{id: peerName(j), target: other})
		}
		me.peer = peers
	}
	return nodes
}

func peerName(i int) string { return "node-" + string(rune('a'+i)) }

func findLeader(nodes []*Node, timeout time.Duration) *Node {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.IsLeader() {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// TestReplication_ProposeCommitsAndAppliesAcrossCluster verifies the full
// Propose → replicate → commit → apply pipeline on a 3-node cluster.
func TestReplication_ProposeCommitsAndAppliesAcrossCluster(t *testing.T) {
	nodes := buildCluster(3)
	sms := make([]*recordingSM, 3)
	for i, n := range nodes {
		sms[i] = &recordingSM{}
		n.SetStateMachine(sms[i])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range nodes {
		go n.Run(ctx)
	}

	leader := findLeader(nodes, 2*time.Second)
	if leader == nil {
		t.Fatal("no leader elected within 2s")
	}

	resultCh, ok := leader.Propose([]byte(`{"type":"test","n":1}`))
	if !ok {
		t.Fatal("Propose rejected by leader")
	}

	select {
	case res := <-resultCh:
		if !res.Committed {
			t.Fatalf("entry not committed: %+v", res)
		}
		if res.Index != 1 {
			t.Errorf("expected index 1, got %d", res.Index)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit wait timed out")
	}

	// Wait for Apply to fan out to followers.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allApplied := true
		for _, sm := range sms {
			if sm.count() < 1 {
				allApplied = false
				break
			}
		}
		if allApplied {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for i, sm := range sms {
		if c := sm.count(); c != 1 {
			t.Errorf("node %d: expected 1 apply, got %d", i, c)
		}
	}
}

// TestReplication_MultipleProposalsPreserveOrder checks that a batch of
// proposals all commit and apply in order on every node.
func TestReplication_MultipleProposalsPreserveOrder(t *testing.T) {
	nodes := buildCluster(3)
	sms := make([]*recordingSM, 3)
	for i, n := range nodes {
		sms[i] = &recordingSM{}
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

	const N = 5
	var waits []<-chan ProposeResult
	for i := 0; i < N; i++ {
		ch, ok := leader.Propose([]byte{byte('a' + i)})
		if !ok {
			t.Fatalf("propose %d rejected", i)
		}
		waits = append(waits, ch)
	}
	for i, ch := range waits {
		select {
		case res := <-ch:
			if !res.Committed {
				t.Fatalf("entry %d not committed", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("entry %d wait timed out", i)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, sm := range sms {
			if sm.count() < N {
				done = false
				break
			}
		}
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for idx, sm := range sms {
		sm.mu.Lock()
		if len(sm.applied) != N {
			t.Errorf("node %d: expected %d applies, got %d", idx, N, len(sm.applied))
		}
		for i, e := range sm.applied {
			if len(e.Command) != 1 || e.Command[0] != byte('a'+i) {
				t.Errorf("node %d entry %d: wrong command %v", idx, i, e.Command)
			}
		}
		sm.mu.Unlock()
	}
}
