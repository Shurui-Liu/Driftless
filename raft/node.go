package raft

import (
	"context"
	"errors"
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
const (
	electionTimeoutMin = 150 * time.Millisecond
	electionTimeoutMax = 300 * time.Millisecond
	heartbeatInterval  = 50 * time.Millisecond
)

// ErrNotLeader is returned by AppendCommand when this node is not the leader.
var ErrNotLeader = errors.New("not leader")

// ApplyFunc is called for each log entry once committed by a quorum.
// Runs in its own goroutine; must be safe to call concurrently.
type ApplyFunc func(LogEntry)

// LogEntry is one entry in the replicated Raft log.
type LogEntry struct {
	Term    int
	Index   int
	Command []byte // encoded task assignment / state change
}

// RequestVoteArgs is the RPC payload sent by a Candidate.
type RequestVoteArgs struct {
	Term         int
	CandidateID  string
	LastLogIndex int
	LastLogTerm  int
}

// RequestVoteReply is the response from a peer.
type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

// AppendEntriesArgs is used for both heartbeats (Entries=nil) and log replication.
type AppendEntriesArgs struct {
	Term         int
	LeaderID     string
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

// AppendEntriesReply tells the leader whether the follower accepted.
type AppendEntriesReply struct {
	Term    int
	Success bool
}

// Peer is a remote Raft node the coordinator talks to via gRPC.
type Peer interface {
	ID() string
	RequestVote(ctx context.Context, args RequestVoteArgs) (RequestVoteReply, error)
	AppendEntries(ctx context.Context, args AppendEntriesArgs) (AppendEntriesReply, error)
}

// Node is the core Raft state machine. All fields after mu are protected by mu.
type Node struct {
	mu   sync.Mutex
	id   string
	log  *slog.Logger
	peer []Peer

	// Persistent state (must survive crashes)
	currentTerm int
	votedFor    string
	logEntries  []LogEntry

	// Volatile state
	state       NodeState
	commitIndex int
	lastApplied int

	// Leader-only volatile state (reinitialized after election)
	nextIndex  map[string]int
	matchIndex map[string]int

	// Channels / timers driving the election loop
	resetElectionTimer chan struct{}
	stepDownCh         chan struct{}

	// Apply callback — invoked in a goroutine for each newly committed entry.
	applyFn ApplyFunc
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

// SetApplyFunc registers the callback invoked for each committed log entry.
// Must be called before Run().
func (n *Node) SetApplyFunc(fn ApplyFunc) {
	n.applyFn = fn
}

// IsLeader returns true if this node is currently the Raft leader.
func (n *Node) IsLeader() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state == Leader
}

// AppendCommand appends a new command to the leader's log and returns its index.
// Returns ErrNotLeader if this node is not the current leader.
func (n *Node) AppendCommand(cmd []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != Leader {
		return 0, ErrNotLeader
	}
	idx := len(n.logEntries)
	n.logEntries = append(n.logEntries, LogEntry{
		Term:    n.currentTerm,
		Index:   idx,
		Command: cmd,
	})
	return idx, nil
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
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(randomElectionTimeout())

		case <-timer.C:
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
	n.votedFor = n.id
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
	case <-timer.C:
		n.log.Info("election timed out, retrying", "term", term)
	}

	wg.Wait()
}

// ── Leader ────────────────────────────────────────────────────────────────────

func (n *Node) runLeader(ctx context.Context) {
	n.log.Info("became leader", "term", n.currentTerm)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Immediate first replication to assert leadership.
	n.replicateLog(ctx)

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
			n.replicateLog(ctx)
		}
	}
}

// replicateLog sends AppendEntries to all peers, carrying any un-replicated log entries.
func (n *Node) replicateLog(ctx context.Context) {
	n.mu.Lock()
	if n.state != Leader {
		n.mu.Unlock()
		return
	}
	term := n.currentTerm
	leaderID := n.id
	commitIdx := n.commitIndex
	n.mu.Unlock()

	for _, p := range n.peer {
		p := p
		go func() {
			n.mu.Lock()
			if n.state != Leader || n.currentTerm != term {
				n.mu.Unlock()
				return
			}
			peerID := p.ID()
			nextIdx := n.nextIndex[peerID]
			prevLogIdx := nextIdx - 1
			prevLogTerm := 0
			if prevLogIdx > 0 && prevLogIdx < len(n.logEntries) {
				prevLogTerm = n.logEntries[prevLogIdx].Term
			}
			var entries []LogEntry
			if nextIdx < len(n.logEntries) {
				entries = make([]LogEntry, len(n.logEntries)-nextIdx)
				copy(entries, n.logEntries[nextIdx:])
			}
			n.mu.Unlock()

			reply, err := p.AppendEntries(ctx, AppendEntriesArgs{
				Term:         term,
				LeaderID:     leaderID,
				PrevLogIndex: prevLogIdx,
				PrevLogTerm:  prevLogTerm,
				Entries:      entries,
				LeaderCommit: commitIdx,
			})
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
				return
			}

			if n.state != Leader || n.currentTerm != term {
				return
			}

			if reply.Success {
				newMatch := prevLogIdx + len(entries)
				if newMatch > n.matchIndex[peerID] {
					n.matchIndex[peerID] = newMatch
				}
				n.nextIndex[peerID] = newMatch + 1
				n.tryAdvanceCommitIndex()
			} else if n.nextIndex[peerID] > 1 {
				// Consistency failure: back off and retry on next heartbeat.
				n.nextIndex[peerID]--
			}
		}()
	}
}

// tryAdvanceCommitIndex advances commitIndex to the highest index replicated by a quorum.
// Must be called with n.mu held.
func (n *Node) tryAdvanceCommitIndex() {
	for idx := len(n.logEntries) - 1; idx > n.commitIndex; idx-- {
		// Only commit entries from the current term (Raft §5.4.2).
		if n.logEntries[idx].Term != n.currentTerm {
			continue
		}
		count := 1 // leader self
		for _, p := range n.peer {
			if n.matchIndex[p.ID()] >= idx {
				count++
			}
		}
		majority := (len(n.peer)+1)/2 + 1
		if count >= majority {
			n.commitIndex = idx
			n.maybeApply()
			break
		}
	}
}

// maybeApply fires applyFn for each entry between lastApplied and commitIndex.
// Must be called with n.mu held.
func (n *Node) maybeApply() {
	fn := n.applyFn
	for n.lastApplied < n.commitIndex {
		n.lastApplied++
		entry := n.logEntries[n.lastApplied]
		if fn != nil {
			go fn(entry)
		}
	}
}

// ── RPC Handlers (called by the gRPC server) ─────────────────────────────────

// HandleRequestVote processes an incoming vote request from a Candidate.
func (n *Node) HandleRequestVote(args RequestVoteArgs) RequestVoteReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply := RequestVoteReply{Term: n.currentTerm}

	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}

	if args.Term < n.currentTerm {
		return reply
	}

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

	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}
	return reply
}

// HandleAppendEntries processes heartbeats and log replication from a leader.
func (n *Node) HandleAppendEntries(args AppendEntriesArgs) AppendEntriesReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply := AppendEntriesReply{Term: n.currentTerm}

	if args.Term < n.currentTerm {
		return reply
	}

	if args.Term > n.currentTerm || n.state == Candidate {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}

	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}

	// Log consistency check: prevLogIndex must exist with a matching term.
	if args.PrevLogIndex > 0 {
		if args.PrevLogIndex >= len(n.logEntries) {
			return reply
		}
		if n.logEntries[args.PrevLogIndex].Term != args.PrevLogTerm {
			return reply
		}
	}

	// Append new entries, truncating any conflicting suffix.
	for i, entry := range args.Entries {
		idx := args.PrevLogIndex + 1 + i
		if idx < len(n.logEntries) {
			if n.logEntries[idx].Term != entry.Term {
				n.logEntries = append(n.logEntries[:idx], args.Entries[i:]...)
				break
			}
			// Entry already present with matching term — skip.
		} else {
			n.logEntries = append(n.logEntries, args.Entries[i:]...)
			break
		}
	}

	// Advance commitIndex per leader's leaderCommit.
	if args.LeaderCommit > n.commitIndex {
		lastNew := args.PrevLogIndex + len(args.Entries)
		if args.LeaderCommit < lastNew {
			n.commitIndex = args.LeaderCommit
		} else {
			n.commitIndex = lastNew
		}
		n.maybeApply()
	}

	reply.Success = true
	return reply
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
	for _, p := range n.peer {
		n.nextIndex[p.ID()] = lastIdx + 1
		n.matchIndex[p.ID()] = 0
	}
}

func randomElectionTimeout() time.Duration {
	spread := int64(electionTimeoutMax - electionTimeoutMin)
	return electionTimeoutMin + time.Duration(rand.Int63n(spread))
}

// ── State observation ─────────────────────────────────────────────────────────

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	default:
		return "unknown"
	}
}

// NodeSnapshot is a point-in-time view of a node's Raft state.
type NodeSnapshot struct {
	NodeID      string         `json:"node_id"`
	State       string         `json:"state"`
	CurrentTerm int            `json:"current_term"`
	VotedFor    string         `json:"voted_for"`
	CommitIndex int            `json:"commit_index"`
	LastApplied int            `json:"last_applied"`
	LogLength   int            `json:"log_length"`
	MatchIndex  map[string]int `json:"match_index,omitempty"` // leader only
	NextIndex   map[string]int `json:"next_index,omitempty"`  // leader only
}

// Snapshot returns a consistent copy of the node's observable state.
func (n *Node) Snapshot() NodeSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()

	snap := NodeSnapshot{
		NodeID:      n.id,
		State:       n.state.String(),
		CurrentTerm: n.currentTerm,
		VotedFor:    n.votedFor,
		CommitIndex: n.commitIndex,
		LastApplied: n.lastApplied,
		LogLength:   len(n.logEntries),
	}

	if n.state == Leader {
		snap.MatchIndex = make(map[string]int, len(n.matchIndex))
		for k, v := range n.matchIndex {
			snap.MatchIndex[k] = v
		}
		snap.NextIndex = make(map[string]int, len(n.nextIndex))
		for k, v := range n.nextIndex {
			snap.NextIndex[k] = v
		}
	}

	return snap
}
