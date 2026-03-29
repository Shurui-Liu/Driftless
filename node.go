package raft

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// NodeState represents the current role of a Raft node.
type NodeState int

const (
	Follower  NodeState = iota
	Candidate NodeState = iota
	Leader    NodeState = iota
)

// ElectionTimeout is randomized per node to avoid split votes.
// Raft paper recommends 150–300ms; tune based on your network RTT.
const (
	electionTimeoutMin = 150 * time.Millisecond
	electionTimeoutMax = 300 * time.Millisecond
	heartbeatInterval  = 50 * time.Millisecond
)

// LogEntry is one entry in the replicated Raft log.
// For your task queue, the Command will encode task state transitions.
type LogEntry struct {
	Term    int
	Index   int
	Command []byte // encoded task assignment / state change
}

// RequestVoteArgs is the RPC payload sent by a Candidate.
type RequestVoteArgs struct {
	Term         int    // candidate's current term
	CandidateID  string // so peers can record who they voted for
	LastLogIndex int    // index of candidate's last log entry
	LastLogTerm  int    // term of candidate's last log entry
}

// RequestVoteReply is the response from a peer.
type RequestVoteReply struct {
	Term        int  // peer's current term (so candidate can update if stale)
	VoteGranted bool
}

// AppendEntriesArgs is used for both heartbeats (Entries=nil) and log replication.
type AppendEntriesArgs struct {
	Term         int        // leader's term
	LeaderID     string     // so followers can redirect clients
	PrevLogIndex int        // index of log entry immediately before new ones
	PrevLogTerm  int        // term of PrevLogIndex entry
	Entries      []LogEntry // empty for heartbeat
	LeaderCommit int        // leader's commitIndex
}

// AppendEntriesReply tells the leader whether the follower accepted.
type AppendEntriesReply struct {
	Term    int  // follower's term (so leader can step down if stale)
	Success bool
}

// Peer is a remote Raft node the coordinator talks to via gRPC.
type Peer interface {
	RequestVote(ctx context.Context, args RequestVoteArgs) (RequestVoteReply, error)
	AppendEntries(ctx context.Context, args AppendEntriesArgs) (AppendEntriesReply, error)
}

// Node is the core Raft state machine. All fields after mu are protected by mu.
type Node struct {
	mu   sync.Mutex
	id   string
	log  *slog.Logger
	peer []Peer

	// Persistent state (must survive crashes — you'll persist these to S3)
	currentTerm int
	votedFor    string // "" means no vote cast this term
	logEntries  []LogEntry

	// Volatile state
	state       NodeState
	commitIndex int
	lastApplied int

	// Leader-only volatile state (reinitialized after election)
	nextIndex  map[string]int // peer ID → next log index to send
	matchIndex map[string]int // peer ID → highest log index known replicated

	// Channels / timers driving the election loop
	resetElectionTimer chan struct{}
	stepDownCh         chan struct{} // signals Leader → Follower transition
}

// NewNode creates a Raft node. Call Run() in a goroutine to start it.
func NewNode(id string, peers []Peer, logger *slog.Logger) *Node {
	n := &Node{
		id:                 id,
		log:                logger,
		peer:               peers,
		state:              Follower,
		resetElectionTimer: make(chan struct{}, 1),
		stepDownCh:         make(chan struct{}, 1),
	}
	// Log starts with a dummy entry at index 0 so real entries begin at 1.
	n.logEntries = []LogEntry{{Term: 0, Index: 0}}
	return n
}

// Run starts the Raft election loop. Call this in a goroutine.
func (n *Node) Run(ctx context.Context) {
	for {
		n.mu.Lock()
		state := n.state
		n.mu.Unlock()

		switch state {
		case Follower:
			n.runFollower(ctx)
		case Candidate:
			n.runCandidate(ctx)
		case Leader:
			n.runLeader(ctx)
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// ── Follower ──────────────────────────────────────────────────────────────────

func (n *Node) runFollower(ctx context.Context) {
	n.log.Info("became follower", "term", n.currentTerm)
	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-n.resetElectionTimer:
			// Received a valid heartbeat or granted a vote — reset the clock.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(randomElectionTimeout())

		case <-timer.C:
			// No heartbeat received in time — trigger an election.
			n.mu.Lock()
			n.state = Candidate
			n.mu.Unlock()
			return
		}
	}
}

// ── Candidate ─────────────────────────────────────────────────────────────────

func (n *Node) runCandidate(ctx context.Context) {
	n.mu.Lock()
	n.currentTerm++
	n.votedFor = n.id // vote for self
	term := n.currentTerm
	lastIdx, lastTerm := n.lastLogIndexAndTerm()
	n.mu.Unlock()

	n.log.Info("starting election", "term", term)

	votes := 1 // self-vote
	majority := (len(n.peer)+1)/2 + 1

	args := RequestVoteArgs{
		Term:         term,
		CandidateID:  n.id,
		LastLogIndex: lastIdx,
		LastLogTerm:  lastTerm,
	}

	// Send RequestVote RPCs in parallel.
	var voteMu sync.Mutex
	var wg sync.WaitGroup
	wonElection := make(chan struct{}, 1)
	steppedDown := make(chan struct{}, 1)

	for _, p := range n.peer {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := p.RequestVote(ctx, args)
			if err != nil {
				return
			}

			n.mu.Lock()
			defer n.mu.Unlock()

			// If we see a higher term, immediately revert to Follower.
			if reply.Term > n.currentTerm {
				n.currentTerm = reply.Term
				n.votedFor = ""
				n.state = Follower
				select {
				case steppedDown <- struct{}{}:
				default:
				}
				return
			}

			if reply.VoteGranted {
				voteMu.Lock()
				votes++
				v := votes
				voteMu.Unlock()
				if v >= majority {
					select {
					case wonElection <- struct{}{}:
					default:
					}
				}
			}
		}()
	}

	// Wait for election outcome or timeout.
	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-wonElection:
		n.mu.Lock()
		n.state = Leader
		n.initLeaderState()
		n.mu.Unlock()
	case <-steppedDown:
		// Already set to Follower inside the goroutine above.
	case <-timer.C:
		// Split vote — stay Candidate and loop back to start a new election.
		n.log.Info("election timed out, retrying", "term", term)
	}

	wg.Wait()
}

// ── Leader ────────────────────────────────────────────────────────────────────

func (n *Node) runLeader(ctx context.Context) {
	n.log.Info("became leader", "term", n.currentTerm)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Send an immediate heartbeat so followers reset their timers.
	n.broadcastHeartbeat(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stepDownCh:
			n.mu.Lock()
			n.state = Follower
			n.mu.Unlock()
			return
		case <-ticker.C:
			n.broadcastHeartbeat(ctx)
		}
	}
}

func (n *Node) broadcastHeartbeat(ctx context.Context) {
	n.mu.Lock()
	term := n.currentTerm
	leaderID := n.id
	prevIdx, prevTerm := n.lastLogIndexAndTerm()
	commit := n.commitIndex
	n.mu.Unlock()

	args := AppendEntriesArgs{
		Term:         term,
		LeaderID:     leaderID,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Entries:      nil, // heartbeat — no entries
		LeaderCommit: commit,
	}

	for _, p := range n.peer {
		p := p
		go func() {
			reply, err := p.AppendEntries(ctx, args)
			if err != nil {
				return
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if reply.Term > n.currentTerm {
				n.currentTerm = reply.Term
				n.votedFor = ""
				n.state = Follower
				select {
				case n.stepDownCh <- struct{}{}:
				default:
				}
			}
		}()
	}
}

// ── RPC Handlers (called by the gRPC server) ─────────────────────────────────

// HandleRequestVote processes an incoming vote request from a Candidate.
func (n *Node) HandleRequestVote(args RequestVoteArgs) RequestVoteReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Rule: if we see a higher term, update ours and reset voted-for FIRST,
	// then build the reply so it reflects the updated term.
	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}

	reply := RequestVoteReply{Term: n.currentTerm}

	// Deny if request is from a stale term.
	if args.Term < n.currentTerm {
		return reply
	}

	// Grant vote if we haven't voted yet (or already voted for this candidate)
	// AND the candidate's log is at least as up-to-date as ours.
	alreadyVoted := n.votedFor != "" && n.votedFor != args.CandidateID
	if alreadyVoted {
		return reply
	}

	lastIdx, lastTerm := n.lastLogIndexAndTerm()
	logOK := args.LastLogTerm > lastTerm ||
		(args.LastLogTerm == lastTerm && args.LastLogIndex >= lastIdx)
	if !logOK {
		return reply
	}

	n.votedFor = args.CandidateID
	reply.VoteGranted = true

	// Reset our election timer — we just heard from a live candidate.
	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}
	return reply
}

// HandleAppendEntries processes heartbeats (and later, log replication).
func (n *Node) HandleAppendEntries(args AppendEntriesArgs) AppendEntriesReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply := AppendEntriesReply{Term: n.currentTerm}

	if args.Term < n.currentTerm {
		return reply // stale leader
	}

	// Valid leader heartbeat — step down if we're a Candidate or stale Leader.
	if args.Term > n.currentTerm || n.state == Candidate {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}

	// Reset election timer — we have a live leader.
	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}

	reply.Success = true
	return reply
	// NOTE: Full log consistency check + entry appending goes here in Week 2.
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (n *Node) lastLogIndexAndTerm() (int, int) {
	last := n.logEntries[len(n.logEntries)-1]
	return last.Index, last.Term
}

func (n *Node) initLeaderState() {
	lastIdx, _ := n.lastLogIndexAndTerm()
	n.nextIndex = make(map[string]int)
	n.matchIndex = make(map[string]int)
	// TODO: replace with real peer IDs from peer list
	for i := range n.peer {
		id := peerID(i)
		n.nextIndex[id] = lastIdx + 1
		n.matchIndex[id] = 0
	}
}

func randomElectionTimeout() time.Duration {
	spread := int64(electionTimeoutMax - electionTimeoutMin)
	return electionTimeoutMin + time.Duration(rand.Int63n(spread))
}

// peerID is a placeholder — replace with real peer IDs from your ECS service discovery.
func peerID(i int) string {
	return "peer-" + string(rune('A'+i))
}
