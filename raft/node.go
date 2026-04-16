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
	// ID returns a stable identifier used as the map key for nextIndex / matchIndex.
	ID() string
	RequestVote(ctx context.Context, args RequestVoteArgs) (RequestVoteReply, error)
	AppendEntries(ctx context.Context, args AppendEntriesArgs) (AppendEntriesReply, error)
	// InstallSnapshot ships a serialized state-machine snapshot to a follower
	// that has fallen behind our log's firstIndex. Over HTTP in production.
	InstallSnapshot(ctx context.Context, args InstallSnapshotArgs) (InstallSnapshotReply, error)
}

// InstallSnapshotArgs is the payload the leader sends to a lagging follower.
// Non-chunked: we assume snapshots fit in a single HTTP body. For much larger
// state, this can evolve into a chunked/streamed variant.
type InstallSnapshotArgs struct {
	Term              int    `json:"term"`                // leader's term
	LeaderID          string `json:"leader_id"`           // for operator logging
	LastIncludedIndex int    `json:"last_included_index"` // snapshot covers log up to here
	LastIncludedTerm  int    `json:"last_included_term"`  // term at LastIncludedIndex
	Data              []byte `json:"data"`                // opaque SM snapshot bytes
}

// InstallSnapshotReply is the follower's response.
type InstallSnapshotReply struct {
	Term    int  `json:"term"`              // follower's term (leader steps down if stale)
	Success bool `json:"success,omitempty"` // true if snapshot was installed
}

// StateMachine is applied on every node once an entry is committed.
// For Driftless, the state machine is the task queue: it materializes
// assign/complete transitions and publishes downstream side effects (only
// on the leader) such as the assignment-queue SQS send.
type StateMachine interface {
	Apply(entry LogEntry)
}

// ProposeResult is delivered exactly once per Propose call.
// Committed=true means the entry was replicated to a majority and applied
// to the state machine. Committed=false means the node stepped down before
// the entry could commit — the caller should retry or drop.
type ProposeResult struct {
	Committed bool
	Index     int
	Term      int
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
	nextIndex    map[string]int           // peer ID → next log index to send
	matchIndex   map[string]int           // peer ID → highest log index known replicated
	replicateSig map[string]chan struct{} // per-peer kick channel (non-blocking send)

	// State machine + commit plumbing
	sm          StateMachine
	applyCh     chan struct{}            // signaled when commitIndex advances
	commitWaits map[int]chan ProposeResult // log index → Propose waiter

	// Most recently produced or received snapshot — the leader serves this
	// via InstallSnapshot to followers that lag below firstIndex. nil if the
	// node has never snapshotted or restored.
	lastSnapshot *RaftSnapshot

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
		applyCh:            make(chan struct{}, 1),
		commitWaits:        make(map[int]chan ProposeResult),
	}
	// Log starts with a dummy entry at index 0 so real entries begin at 1.
	n.logEntries = []LogEntry{{Term: 0, Index: 0}}
	return n
}

// SetStateMachine installs the state machine invoked for every committed entry.
// Safe to call after Run has started — applyLoop reads n.sm under the node mutex.
func (n *Node) SetStateMachine(sm StateMachine) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sm = sm
}

// Run starts the Raft state-machine and election loop. Call this in a goroutine.
func (n *Node) Run(ctx context.Context) {
	// Apply loop drives committed entries into the state machine on every node.
	go n.applyLoop(ctx)

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
	n.mu.Lock()
	term := n.currentTerm
	n.mu.Unlock()
	n.log.Info("became follower", "term", term)
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
	n.mu.Lock()
	term := n.currentTerm
	n.mu.Unlock()
	n.log.Info("became leader", "term", term)

	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Drain any stale step-down signal left over from a prior term.
	select {
	case <-n.stepDownCh:
	default:
	}

	// Spin up one replicator per peer. Each one loops on a heartbeat ticker
	// and a kick channel (triggered by Propose or by a back-off retry).
	var wg sync.WaitGroup
	for _, p := range n.peer {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.replicatePeer(leaderCtx, p)
		}()
	}

	// Block until we step down — either because ctx is cancelled or because
	// a replicator saw a higher term in an AppendEntries reply.
	select {
	case <-ctx.Done():
	case <-n.stepDownCh:
		n.mu.Lock()
		n.state = Follower
		n.mu.Unlock()
	}
	cancel()
	wg.Wait()

	// Clean up any Propose waiters whose entries didn't commit in our term.
	n.abortPendingProposals()
}

// replicatePeer is the per-peer replication loop. It wakes up on a heartbeat
// tick or when Propose kicks its trigger channel, then sends the entries
// starting at nextIndex[peer].
func (n *Node) replicatePeer(ctx context.Context, p Peer) {
	peerID := p.ID()

	n.mu.Lock()
	sig, ok := n.replicateSig[peerID]
	n.mu.Unlock()
	if !ok {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Fire once immediately so followers reset their election timers.
	n.sendAppend(ctx, p)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-sig:
		}
		n.sendAppend(ctx, p)
	}
}

func (n *Node) sendAppend(ctx context.Context, p Peer) {
	peerID := p.ID()

	n.mu.Lock()
	if n.state != Leader {
		n.mu.Unlock()
		return
	}
	term := n.currentTerm
	next := n.nextIndex[peerID]
	// Follower is below our snapshot floor — ship the snapshot instead.
	// Only valid if we actually have one cached; otherwise clamp next forward
	// and hope log is still contiguous (fresh cluster, no snapshots yet).
	if next <= n.firstIndex() && n.lastSnapshot != nil {
		n.mu.Unlock()
		n.sendInstallSnapshot(ctx, p)
		return
	}
	if next <= n.firstIndex() {
		next = n.firstIndex() + 1
		n.nextIndex[peerID] = next
	}
	prevIdx := next - 1
	prevTerm := n.termAt(prevIdx)
	entries := n.entriesFrom(next)
	args := AppendEntriesArgs{
		Term:         term,
		LeaderID:     n.id,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: n.commitIndex,
	}
	n.mu.Unlock()

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
		return
	}

	// Ignore replies from an old term — we may have stepped down and come back.
	if n.state != Leader || n.currentTerm != term {
		return
	}

	if reply.Success {
		newMatch := prevIdx + len(entries)
		if newMatch > n.matchIndex[peerID] {
			n.matchIndex[peerID] = newMatch
		}
		if newMatch+1 > n.nextIndex[peerID] {
			n.nextIndex[peerID] = newMatch + 1
		}
		n.maybeAdvanceCommitLocked()
	} else {
		// Log conflict — back off and retry. Simple one-step decrement; a
		// real impl would use the "conflicting term" hint in the reply.
		if n.nextIndex[peerID] > n.firstIndex()+1 {
			n.nextIndex[peerID]--
		}
		if ch, ok := n.replicateSig[peerID]; ok {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// maybeAdvanceCommitLocked walks from the last log index down looking for the
// largest N with quorum replication AND log[N].Term == currentTerm (Raft's
// safety rule that prevents committing entries from earlier terms by count).
// Caller must hold n.mu.
func (n *Node) maybeAdvanceCommitLocked() {
	lastIdx := n.lastIndex()
	majority := (len(n.peer)+1)/2 + 1

	for N := lastIdx; N > n.commitIndex; N-- {
		if N < n.firstIndex() {
			break
		}
		if n.termAt(N) != n.currentTerm {
			continue
		}
		count := 1 // leader itself
		for _, p := range n.peer {
			if n.matchIndex[p.ID()] >= N {
				count++
			}
		}
		if count >= majority {
			n.commitIndex = N
			select {
			case n.applyCh <- struct{}{}:
			default:
			}
			return
		}
	}
}

// abortPendingProposals fires failure results for any Propose calls whose
// entries never committed in our term. Called after step-down.
func (n *Node) abortPendingProposals() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for idx, ch := range n.commitWaits {
		if idx > n.commitIndex {
			select {
			case ch <- ProposeResult{Committed: false, Index: idx}:
			default:
			}
			close(ch)
			delete(n.commitWaits, idx)
		}
	}
}

// ── RPC Handlers (called by the gRPC server) ─────────────────────────────────

// HandleRequestVote processes an incoming vote request from a Candidate.
func (n *Node) HandleRequestVote(args RequestVoteArgs) RequestVoteReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Rule: if we see a higher term, update ours and reset voted-for.
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

// HandleAppendEntries is the follower side of Raft log replication.
// Rules implemented (per the Raft paper §5.3):
//   1. Reject if leader term is stale.
//   2. Accept → step down from Candidate/Leader if term >= ours.
//   3. Reject if our log doesn't contain PrevLogIndex/PrevLogTerm.
//   4. Truncate conflicting entries and append new ones.
//   5. Advance commitIndex to min(leaderCommit, index of last new entry).
func (n *Node) HandleAppendEntries(args AppendEntriesArgs) AppendEntriesReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply := AppendEntriesReply{Term: n.currentTerm}

	if args.Term < n.currentTerm {
		return reply // rule 1 — stale leader
	}

	// rule 2 — recognize new leader
	if args.Term > n.currentTerm || n.state != Follower {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}
	reply.Term = n.currentTerm

	// Reset election timer — we have a live leader.
	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}

	// rule 3 — log consistency
	// PrevLogIndex below our snapshot floor is already committed; accept blindly.
	if args.PrevLogIndex > n.lastIndex() {
		return reply
	}
	if args.PrevLogIndex >= n.firstIndex() &&
		n.termAt(args.PrevLogIndex) != args.PrevLogTerm {
		return reply
	}

	// rule 4 — append/truncate
	for i, e := range args.Entries {
		idx := args.PrevLogIndex + 1 + i
		if idx <= n.firstIndex() {
			continue // covered by snapshot, skip
		}
		existing, ok := n.entryAt(idx)
		switch {
		case !ok:
			n.logEntries = append(n.logEntries, e)
		case existing.Term != e.Term:
			n.truncateAt(idx)
			n.logEntries = append(n.logEntries, e)
		}
	}

	// rule 5 — advance commitIndex
	if args.LeaderCommit > n.commitIndex {
		lastNew := args.PrevLogIndex + len(args.Entries)
		newCommit := args.LeaderCommit
		if lastNew < newCommit {
			newCommit = lastNew
		}
		if newCommit > n.commitIndex {
			n.commitIndex = newCommit
			select {
			case n.applyCh <- struct{}{}:
			default:
			}
		}
	}

	reply.Success = true
	return reply
}

// ── InstallSnapshot ──────────────────────────────────────────────────────────

// HandleInstallSnapshot is invoked on a follower when the leader ships it a
// snapshot because it's fallen behind firstIndex. After a successful install,
// the follower's log, commit/apply pointers, and SM state all reflect the
// snapshot — subsequent AppendEntries can continue from there.
func (n *Node) HandleInstallSnapshot(args InstallSnapshotArgs) InstallSnapshotReply {
	n.mu.Lock()
	reply := InstallSnapshotReply{Term: n.currentTerm}

	if args.Term < n.currentTerm {
		n.mu.Unlock()
		return reply // stale leader
	}
	if args.Term > n.currentTerm || n.state != Follower {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.state = Follower
	}
	reply.Term = n.currentTerm

	// Reset the election timer — a live leader just contacted us.
	select {
	case n.resetElectionTimer <- struct{}{}:
	default:
	}

	// Ignore a snapshot that's older than what we already have.
	if args.LastIncludedIndex <= n.firstIndex() && n.firstIndex() > 0 {
		reply.Success = true
		n.mu.Unlock()
		return reply
	}

	// Discard the log — every committed entry up to LastIncludedIndex is
	// folded into the snapshot, and anything beyond that must be re-fetched
	// via subsequent AppendEntries (a conflict is fine; new leader rules it).
	n.logEntries = []LogEntry{{Term: args.LastIncludedTerm, Index: args.LastIncludedIndex}}
	n.commitIndex = args.LastIncludedIndex
	n.lastApplied = args.LastIncludedIndex

	sm := n.sm
	// Cache the received snapshot so we can forward it if we become leader.
	snap := RaftSnapshot{
		LastIncludedIndex: args.LastIncludedIndex,
		LastIncludedTerm:  args.LastIncludedTerm,
		CurrentTerm:       n.currentTerm,
		VotedFor:          n.votedFor,
		Data:              append([]byte(nil), args.Data...),
		TakenAt:           time.Now().UTC(),
	}
	n.lastSnapshot = &snap
	n.mu.Unlock()

	// Restore SM outside the lock — it may do I/O.
	if smSnap, ok := sm.(SnapshotableStateMachine); ok && len(args.Data) > 0 {
		if err := smSnap.Restore(args.Data); err != nil {
			n.log.Warn("state machine restore failed", "err", err)
			return reply // Success remains false
		}
	}

	n.log.Info("snapshot installed",
		"last_included_index", args.LastIncludedIndex,
		"last_included_term", args.LastIncludedTerm,
		"from_leader", args.LeaderID,
	)
	reply.Success = true
	return reply
}

// sendInstallSnapshot is the leader's counterpart: ship our cached snapshot
// to a follower whose nextIndex has fallen below our firstIndex. Updates
// nextIndex/matchIndex on success.
func (n *Node) sendInstallSnapshot(ctx context.Context, p Peer) {
	peerID := p.ID()

	n.mu.Lock()
	if n.state != Leader || n.lastSnapshot == nil {
		n.mu.Unlock()
		return
	}
	snap := *n.lastSnapshot // copy
	term := n.currentTerm
	n.mu.Unlock()

	args := InstallSnapshotArgs{
		Term:              term,
		LeaderID:          n.id,
		LastIncludedIndex: snap.LastIncludedIndex,
		LastIncludedTerm:  snap.LastIncludedTerm,
		Data:              snap.Data,
	}
	reply, err := p.InstallSnapshot(ctx, args)
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
	if !reply.Success || n.state != Leader || n.currentTerm != term {
		return
	}
	// Advance peer pointers to just after the snapshot boundary.
	if snap.LastIncludedIndex > n.matchIndex[peerID] {
		n.matchIndex[peerID] = snap.LastIncludedIndex
	}
	if snap.LastIncludedIndex+1 > n.nextIndex[peerID] {
		n.nextIndex[peerID] = snap.LastIncludedIndex + 1
	}
	n.maybeAdvanceCommitLocked()
}

// ── Propose / apply loop ─────────────────────────────────────────────────────

// Propose appends a command to the leader's log and returns a channel that
// will receive exactly one ProposeResult. Returns (nil, false) if this node
// is not the leader — callers should redirect or retry later.
func (n *Node) Propose(cmd []byte) (<-chan ProposeResult, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.state != Leader {
		return nil, false
	}

	lastIdx, _ := n.lastLogIndexAndTerm()
	entry := LogEntry{Term: n.currentTerm, Index: lastIdx + 1, Command: cmd}
	n.logEntries = append(n.logEntries, entry)

	// Leader counts itself in matchIndex for commit advancement.
	n.matchIndex[n.id] = entry.Index

	result := make(chan ProposeResult, 1)
	n.commitWaits[entry.Index] = result

	// Kick every replicator — a fresh entry is waiting.
	for _, ch := range n.replicateSig {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return result, true
}

// applyLoop advances lastApplied toward commitIndex, invoking the state
// machine for each newly committed entry and notifying Propose waiters.
func (n *Node) applyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.applyCh:
		}

		for {
			n.mu.Lock()
			next := n.lastApplied + 1
			if next > n.commitIndex {
				n.mu.Unlock()
				break
			}
			entry, ok := n.entryAt(next)
			if !ok {
				// Could happen briefly if a snapshot advances lastApplied
				// past next — skip to whatever firstIndex now says.
				if next <= n.firstIndex() {
					n.lastApplied = n.firstIndex()
				}
				n.mu.Unlock()
				break
			}
			sm := n.sm
			waiter, hasWaiter := n.commitWaits[next]
			if hasWaiter {
				delete(n.commitWaits, next)
			}
			n.mu.Unlock()

			if sm != nil && entry.Command != nil {
				sm.Apply(entry)
			}

			// Bump lastApplied only after Apply returns so Snapshot never
			// serializes a state machine that's behind lastApplied.
			n.mu.Lock()
			n.lastApplied = next
			n.mu.Unlock()

			if hasWaiter {
				waiter <- ProposeResult{Committed: true, Index: entry.Index, Term: entry.Term}
				close(waiter)
			}
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (n *Node) lastLogIndexAndTerm() (int, int) {
	last := n.logEntries[len(n.logEntries)-1]
	return last.Index, last.Term
}

// ── Log index helpers ─────────────────────────────────────────────────────────
// After a snapshot, logEntries[0] is a placeholder carrying the last-included
// index and term; real entries continue from there. All lookups by global
// index must go through these helpers to survive compaction.

// firstIndex is the lowest log index present in memory (placeholder's index).
// Caller must hold n.mu.
func (n *Node) firstIndex() int { return n.logEntries[0].Index }

// lastIndex is the highest log index present in memory.
// Caller must hold n.mu.
func (n *Node) lastIndex() int { return n.logEntries[len(n.logEntries)-1].Index }

// termAt returns the term of the entry at global index idx, or 0 if idx is
// outside the in-memory log. Caller must hold n.mu.
func (n *Node) termAt(idx int) int {
	first := n.firstIndex()
	if idx < first || idx > n.lastIndex() {
		return 0
	}
	return n.logEntries[idx-first].Term
}

// entryAt returns (entry, true) if idx is in the in-memory log.
// Caller must hold n.mu.
func (n *Node) entryAt(idx int) (LogEntry, bool) {
	first := n.firstIndex()
	if idx < first || idx > n.lastIndex() {
		return LogEntry{}, false
	}
	return n.logEntries[idx-first], true
}

// entriesFrom returns a defensive copy of all entries at global index >= idx.
// If idx falls below firstIndex, only entries strictly above the snapshot
// placeholder are returned (caller should install a snapshot to catch up).
// Caller must hold n.mu.
func (n *Node) entriesFrom(idx int) []LogEntry {
	first := n.firstIndex()
	if idx <= first {
		idx = first + 1
	}
	if idx > n.lastIndex() {
		return nil
	}
	start := idx - first
	out := make([]LogEntry, len(n.logEntries)-start)
	copy(out, n.logEntries[start:])
	return out
}

// truncateAt drops every entry with global index >= idx. Used when a follower
// sees a conflict and must discard suffix. Caller must hold n.mu.
func (n *Node) truncateAt(idx int) {
	first := n.firstIndex()
	if idx <= first {
		return // never drop the placeholder
	}
	if idx > n.lastIndex() {
		return
	}
	n.logEntries = n.logEntries[:idx-first]
}

// compactLogLocked drops all entries up to and including upToIdx, replacing
// logEntries[0] with a placeholder at (upToIdx, upToTerm). Caller must hold n.mu.
func (n *Node) compactLogLocked(upToIdx, upToTerm int) {
	if upToIdx <= n.firstIndex() {
		return // already compacted past this point
	}
	first := n.firstIndex()
	placeholder := LogEntry{Term: upToTerm, Index: upToIdx}
	if upToIdx >= n.lastIndex() {
		n.logEntries = []LogEntry{placeholder}
		return
	}
	tail := n.logEntries[upToIdx-first+1:]
	newLog := make([]LogEntry, 0, len(tail)+1)
	newLog = append(newLog, placeholder)
	newLog = append(newLog, tail...)
	n.logEntries = newLog
}

func (n *Node) initLeaderState() {
	lastIdx, _ := n.lastLogIndexAndTerm()
	n.nextIndex = make(map[string]int, len(n.peer))
	n.matchIndex = make(map[string]int, len(n.peer)+1)
	n.replicateSig = make(map[string]chan struct{}, len(n.peer))
	for _, p := range n.peer {
		id := p.ID()
		n.nextIndex[id] = lastIdx + 1
		n.matchIndex[id] = 0
		n.replicateSig[id] = make(chan struct{}, 1)
	}
	// Leader counts its own log progress for commit calculation.
	n.matchIndex[n.id] = lastIdx
}

func randomElectionTimeout() time.Duration {
	spread := int64(electionTimeoutMax - electionTimeoutMin)
	return electionTimeoutMin + time.Duration(rand.Int63n(spread))
}

// ── State observation ─────────────────────────────────────────────────────────

// NodeState.String returns a human-readable role name for JSON serialization.
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
// Serialized as JSON by the HTTP /state endpoint consumed by the Python State Observer.
type NodeSnapshot struct {
	NodeID      string         `json:"node_id"`
	State       string         `json:"state"`
	CurrentTerm int            `json:"current_term"`
	VotedFor    string         `json:"voted_for"`
	CommitIndex int            `json:"commit_index"`
	LastApplied int            `json:"last_applied"`
	LogLength   int            `json:"log_length"`
	MatchIndex  map[string]int `json:"match_index,omitempty"` // leader only: peer → highest replicated index
	NextIndex   map[string]int `json:"next_index,omitempty"`  // leader only: peer → next index to send
}

// IsLeader reports whether this node currently believes it is the Raft leader.
// Used by the dispatcher to gate task assignment — only the leader may dispatch.
func (n *Node) IsLeader() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state == Leader
}

// CurrentTerm returns the node's current term — exposed for dispatcher logging.
func (n *Node) CurrentTerm() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentTerm
}

// Snapshot returns a consistent copy of the node's observable state.
// Safe to call concurrently; acquires the node mutex internally.
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
