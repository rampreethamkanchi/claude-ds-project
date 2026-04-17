// Package raft — raft.go
//
// The main RaftNode struct and state machine loop.
//
// This file orchestrates the entire Raft protocol by running a dedicated
// goroutine that transitions between three states: Follower, Candidate, Leader.
//
// Architecture:
//   - Main goroutine (run): handles state transitions, incoming RPCs, timers.
//   - Replication goroutines (runReplicator): one per peer, active only when leader.
//   - Apply goroutine (applyLoop): applies committed entries to the FSM.
//   - Transport goroutines: accept TCP connections and deliver RPCs.
//
// Locking strategy:
//   r.mu protects all shared state. It is acquired for reads/writes of state
//   variables and released before I/O operations (RPC sends) to avoid deadlock.
//
// The state machine loop design is faithful to Figure 2 of the Raft paper:
//   "Rules for Servers" — each state has its own specific behavior.
package raft

import (
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"
)

// RaftNode is one Raft server in the cluster.
type RaftNode struct {
	// ── Configuration ─────────────────────────────────────────────────────
	config    *Config
	transport Transport

	// ── Persistent state (on all servers, §5.2 Figure 2) ──────────────────
	// These are saved to StableStore before responding to any RPC.
	currentTerm uint64 // latest term this server has seen
	votedFor    string // candidateId that received our vote in currentTerm ("" if none)
	logStore    LogStore
	stableStore StableStore

	// ── Volatile state (on all servers) ───────────────────────────────────
	state       ServerState // Follower, Candidate, or Leader
	commitIndex uint64      // index of highest log entry known to be committed
	lastApplied uint64      // index of highest log entry applied to FSM

	// ── Volatile state (leaders only, reinitialized after election) ───────
	peers map[string]*peerState // peerID → replication state

	// ── Leader tracking ──────────────────────────────────────────────────
	leaderID   string // current known leader's server ID
	leaderAddr string // current known leader's network address

	// ── FSM ──────────────────────────────────────────────────────────────
	fsm FSM

	// ── Pending futures (leader only) ────────────────────────────────────
	// Maps log index → ApplyFuture for entries proposed by this leader.
	// Resolved when the entry is committed and applied to the FSM.
	pendingFutures map[uint64]*ApplyFuture

	// ── Internal channels ────────────────────────────────────────────────
	applyCh        chan *applyRequest // client Apply() requests
	commitNotifyCh chan struct{}      // signals new committed entries
	shutdownCh     chan struct{}      // closed on shutdown
	shutdownOnce   sync.Once

	// ── Synchronization ──────────────────────────────────────────────────
	mu sync.RWMutex

	// ── Logger ───────────────────────────────────────────────────────────
	logger *slog.Logger
}

// NewRaftNode creates and starts a new Raft node.
//
// The node begins as a Follower and participates in elections. Callers should
// use Apply() to propose commands (only succeeds on the leader) and State()
// to check the current role.
func NewRaftNode(config *Config, transport Transport) (*RaftNode, error) {
	// Validate configuration.
	if err := config.validate(); err != nil {
		return nil, err
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	logger = logger.With("raft_id", config.ServerID)

	r := &RaftNode{
		config:         config,
		transport:      transport,
		logStore:       config.LogStore,
		stableStore:    config.StableStore,
		fsm:            config.FSM,
		state:          Follower,
		peers:          make(map[string]*peerState),
		pendingFutures: make(map[uint64]*ApplyFuture),
		applyCh:        make(chan *applyRequest, 256),
		commitNotifyCh: make(chan struct{}, 1),
		shutdownCh:     make(chan struct{}),
		logger:         logger,
	}

	// Restore persistent state from the StableStore (currentTerm, votedFor).
	if err := r.restoreState(); err != nil {
		return nil, fmt.Errorf("raft: restore state: %w", err)
	}

	// Determine lastApplied from the log (on fresh start, it's 0).
	// On restart, the FSM will be restored from a snapshot or replayed,
	// so lastApplied starts at 0 and the apply loop catches up.
	r.lastApplied = 0
	r.commitIndex = 0

	// Start the main run loop and the apply loop.
	go r.run()
	go r.applyLoop()

	r.logger.Info("raft node started",
		"term", r.currentTerm,
		"state", r.state.String(),
	)

	return r, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

// Apply proposes a command to the replicated log. Only the leader can accept
// Apply requests; if this node is not the leader, Apply returns ErrNotLeader.
//
// The timeout applies to the entire commitment process: if the entry is not
// committed within 'timeout', the future resolves with ErrTimeout.
//
// The returned ApplyFuture can be used to wait for the entry to be committed
// and retrieve the FSM's response.
func (r *RaftNode) Apply(data []byte, timeout time.Duration) *ApplyFuture {
	future := newApplyFuture()

	// Quick check: are we the leader?
	r.mu.RLock()
	if r.state != Leader {
		r.mu.RUnlock()
		future.respond(ErrNotLeader, nil)
		return future
	}
	r.mu.RUnlock()

	req := &applyRequest{data: data, future: future}

	select {
	case r.applyCh <- req:
	case <-time.After(timeout):
		future.respond(ErrTimeout, nil)
		return future
	case <-r.shutdownCh:
		future.respond(ErrShutdown, nil)
		return future
	}

	// Start a timeout goroutine: if the entry is not committed within
	// 'timeout', resolve the future with ErrTimeout. This prevents the
	// caller from blocking forever when a majority is unreachable.
	// If the entry IS committed before the timeout, respond() uses
	// sync.Once to ignore this second call.
	go func() {
		select {
		case <-time.After(timeout):
			future.respond(ErrTimeout, nil)
		case <-r.shutdownCh:
			future.respond(ErrShutdown, nil)
		}
	}()

	return future
}

// State returns the current server state (Follower, Candidate, or Leader).
func (r *RaftNode) State() ServerState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// IsLeader returns true if this node is currently the Raft leader.
func (r *RaftNode) IsLeader() bool {
	return r.State() == Leader
}

// LeaderAddr returns the network address of the current known leader.
// Returns "" if the leader is unknown.
func (r *RaftNode) LeaderAddr() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.leaderAddr
}

// LeaderID returns the server ID of the current known leader.
func (r *RaftNode) LeaderID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.leaderID
}

// CurrentTerm returns the current Raft term.
func (r *RaftNode) CurrentTerm() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentTerm
}

// Shutdown gracefully stops the Raft node.
// After Shutdown, the node reports as Follower (not Leader), so test code
// and client code that checks IsLeader() sees the correct state.
func (r *RaftNode) Shutdown() error {
	r.shutdownOnce.Do(func() {
		r.logger.Info("shutting down raft node")
		close(r.shutdownCh)

		// Set state to Follower so IsLeader() returns false.
		r.mu.Lock()
		r.state = Follower
		// Fail all pending futures.
		for idx, f := range r.pendingFutures {
			f.respond(ErrShutdown, nil)
			delete(r.pendingFutures, idx)
		}
		r.mu.Unlock()
	})
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Main run loop
// ─────────────────────────────────────────────────────────────────────────────

// run is the main goroutine that drives the Raft state machine.
// It loops forever, transitioning between Follower, Candidate, and Leader
// states. Each state has its own sub-loop with different behavior.
func (r *RaftNode) run() {
	for {
		select {
		case <-r.shutdownCh:
			return
		default:
		}

		switch r.State() {
		case Follower:
			r.runFollower()
		case Candidate:
			r.runCandidate()
		case Leader:
			r.runLeader()
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Follower state
// ─────────────────────────────────────────────────────────────────────────────

// runFollower runs the follower loop. The follower is passive: it responds
// to RPCs from the leader and candidates, and starts an election if it
// doesn't hear from a leader before the election timeout.
func (r *RaftNode) runFollower() {
	r.logger.Debug("entering follower state")

	timer := time.NewTimer(r.randomElectionTimeout())
	defer timer.Stop()

	for r.State() == Follower {
		select {
		case rpc := <-r.transport.Consumer():
			resetTimer := r.handleRPC(rpc)
			if resetTimer {
				timer.Reset(r.randomElectionTimeout())
			}

		case <-timer.C:
			// Election timeout expired — no heartbeat from leader.
			// Transition to Candidate and start an election.
			r.logger.Info("election timeout — transitioning to candidate")
			r.mu.Lock()
			r.state = Candidate
			r.mu.Unlock()
			return

		case <-r.shutdownCh:
			return
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Candidate state
// ─────────────────────────────────────────────────────────────────────────────

// runCandidate runs the candidate loop. The candidate:
//  1. Increments its term and votes for itself.
//  2. Sends RequestVote RPCs to all peers in parallel.
//  3. Waits for: (a) winning majority → become Leader,
//     (b) higher term from peer → become Follower,
//     (c) election timeout → start new election.
func (r *RaftNode) runCandidate() {
	r.logger.Debug("entering candidate state")

	// Start the election: increment term, vote for self, send RequestVote RPCs.
	voteCh := r.startElection()

	votes := 1 // we voted for ourselves
	totalNodes := len(r.config.Peers)
	quorum := totalNodes/2 + 1

	// Immediate majority check — critical for single-node clusters.
	// In a 1-node cluster, quorum=1 and votes=1, so we win immediately
	// without waiting for any RPCs.
	if votes >= quorum {
		r.logger.Info("won election immediately (single-node or pre-majority)",
			"term", r.CurrentTerm(),
			"votes", votes,
		)
		r.mu.Lock()
		r.state = Leader
		r.leaderID = r.config.ServerID
		for _, p := range r.config.Peers {
			if p.ID == r.config.ServerID {
				r.leaderAddr = p.Address
				break
			}
		}
		r.mu.Unlock()
		return
	}

	timer := time.NewTimer(r.randomElectionTimeout())
	defer timer.Stop()

	for r.State() == Candidate {
		select {
		case vote, ok := <-voteCh:
			if !ok {
				// All votes received — if we don't have a majority, timeout will fire.
				voteCh = nil // prevent re-reading from closed channel
				continue
			}

			r.mu.Lock()
			// If the voter has a higher term, step down immediately.
			if vote.Term > r.currentTerm {
				r.stepDown(vote.Term)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()

			if vote.VoteGranted {
				votes++
				r.logger.Debug("received vote", "votes", votes, "quorum", quorum)

				if votes >= quorum {
					// We won the election!
					r.logger.Info("won election — becoming leader",
						"term", r.CurrentTerm(),
						"votes", votes,
					)
					r.mu.Lock()
					r.state = Leader
					r.leaderID = r.config.ServerID
					for _, p := range r.config.Peers {
						if p.ID == r.config.ServerID {
							r.leaderAddr = p.Address
							break
						}
					}
					r.mu.Unlock()
					return
				}
			}

		case rpc := <-r.transport.Consumer():
			resetTimer := r.handleRPC(rpc)
			if resetTimer {
				// If we stepped down to follower (due to a valid AppendEntries
				// from a legitimate leader), exit the candidate loop.
				if r.State() != Candidate {
					return
				}
				timer.Reset(r.randomElectionTimeout())
			}

		case <-timer.C:
			// Election timeout — split vote or no response. Start a new election.
			r.logger.Info("election timeout — retrying")
			return // will loop back to runCandidate in the main loop

		case <-r.shutdownCh:
			return
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Leader state
// ─────────────────────────────────────────────────────────────────────────────

// runLeader runs the leader loop. The leader:
//  1. Initializes nextIndex and matchIndex for all peers.
//  2. Appends a no-op entry to commit entries from previous terms (§5.4.2).
//  3. Starts a replication goroutine for each peer.
//  4. Processes client Apply() requests by appending entries to the log.
//  5. Continues until it steps down (higher term seen) or shuts down.
func (r *RaftNode) runLeader() {
	r.logger.Info("entering leader state", "term", r.CurrentTerm())

	// ── Initialize leader volatile state ─────────────────────────────────
	r.mu.Lock()
	lastIdx, _ := r.logStore.LastIndex()

	r.peers = make(map[string]*peerState)
	for _, p := range r.config.Peers {
		if p.ID == r.config.ServerID {
			continue // skip self
		}
		r.peers[p.ID] = &peerState{
			id:         p.ID,
			addr:       p.Address,
			nextIndex:  lastIdx + 1, // optimistically assume follower is up-to-date
			matchIndex: 0,           // pessimistically assume nothing is replicated
			triggerCh:  make(chan struct{}, 1),
		}
	}
	r.mu.Unlock()

	// ── Start per-peer replication goroutines ─────────────────────────────
	stopCh := make(chan struct{})
	defer close(stopCh) // stops all replication goroutines when we leave leader state

	r.mu.RLock()
	for _, peer := range r.peers {
		go r.runReplicator(peer, stopCh)
	}
	r.mu.RUnlock()

	// ── Append a no-op entry for the current term (§5.4.2) ───────────────
	// This ensures that entries from previous terms get committed.
	// Without this, a new leader might never commit older entries because
	// the §5.4.2 rule requires a current-term entry to be committed first.
	r.appendNoOp()

	// ── Leader main loop ─────────────────────────────────────────────────
	for r.State() == Leader {
		select {
		case rpc := <-r.transport.Consumer():
			r.handleRPC(rpc)
			// If we stepped down due to a higher term, exit.
			if r.State() != Leader {
				r.failPendingFutures()
				return
			}

		case req := <-r.applyCh:
			// Client wants to apply a command — append it to our log.
			r.handleApplyRequest(req)

		case <-r.shutdownCh:
			r.failPendingFutures()
			return
		}
	}

	r.failPendingFutures()
}

// handleApplyRequest appends a client command to the leader's log and
// triggers replication to all peers.
func (r *RaftNode) handleApplyRequest(req *applyRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != Leader {
		req.future.respond(ErrNotLeader, nil)
		return
	}

	// Create a new log entry.
	lastIdx, _ := r.logStore.LastIndex()
	entry := &LogEntry{
		Index: lastIdx + 1,
		Term:  r.currentTerm,
		Data:  req.data,
	}

	// Persist the entry.
	if err := r.logStore.StoreLog(entry); err != nil {
		r.logger.Error("failed to store log entry", "err", err)
		req.future.respond(fmt.Errorf("store log: %w", err), nil)
		return
	}

	// Track the future so we can resolve it when the entry is committed.
	r.pendingFutures[entry.Index] = req.future

	r.logger.Debug("appended entry to log",
		"index", entry.Index,
		"term", entry.Term,
		"data_len", len(entry.Data),
	)

	// Wake all replication goroutines to send the new entry.
	r.triggerReplication()
}

// appendNoOp appends a no-op entry with the current term to the leader's log.
// This is necessary to commit entries from previous terms (§5.4.2).
func (r *RaftNode) appendNoOp() {
	r.mu.Lock()

	lastIdx, _ := r.logStore.LastIndex()
	entry := &LogEntry{
		Index: lastIdx + 1,
		Term:  r.currentTerm,
		Data:  nil, // no-op: empty data
	}

	if err := r.logStore.StoreLog(entry); err != nil {
		r.logger.Error("failed to store no-op entry", "err", err)
		r.mu.Unlock()
		return
	}

	r.logger.Debug("appended no-op entry", "index", entry.Index, "term", entry.Term)
	r.triggerReplication()
	r.mu.Unlock()
}

// failPendingFutures fails all outstanding Apply futures (e.g., on leadership loss).
func (r *RaftNode) failPendingFutures() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for idx, f := range r.pendingFutures {
		f.respond(ErrLeaderLost, nil)
		delete(r.pendingFutures, idx)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RPC dispatch
// ─────────────────────────────────────────────────────────────────────────────

// handleRPC dispatches an incoming RPC to the appropriate handler.
// Returns true if the election timer should be reset (valid heartbeat or vote granted).
func (r *RaftNode) handleRPC(rpc *RPC) bool {
	switch cmd := rpc.Command.(type) {
	case *AppendEntriesRequest:
		resp, resetTimer := r.handleAppendEntries(cmd)
		rpc.Respond(resp, nil)
		return resetTimer

	case *RequestVoteRequest:
		resp, resetTimer := r.handleRequestVote(cmd)
		rpc.Respond(resp, nil)
		return resetTimer

	default:
		r.logger.Warn("unknown RPC type", "type", fmt.Sprintf("%T", cmd))
		rpc.Respond(nil, fmt.Errorf("unknown RPC type: %T", cmd))
		return false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Apply loop — applies committed entries to the FSM
// ─────────────────────────────────────────────────────────────────────────────

// applyLoop runs in a dedicated goroutine. It watches for commitIndex advances
// and applies newly committed entries to the FSM. This runs on ALL nodes —
// both leaders and followers apply entries to their local FSM.
//
// On the leader, it also resolves pending ApplyFutures with the FSM's response.
func (r *RaftNode) applyLoop() {
	for {
		select {
		case <-r.commitNotifyCh:
			r.applyCommitted()
		case <-r.shutdownCh:
			return
		}
	}
}

// applyCommitted applies entries from lastApplied+1 to commitIndex to the FSM.
func (r *RaftNode) applyCommitted() {
	r.mu.Lock()
	lastApplied := r.lastApplied
	commitIndex := r.commitIndex
	r.mu.Unlock()

	for idx := lastApplied + 1; idx <= commitIndex; idx++ {
		entry, err := r.logStore.GetLog(idx)
		if err != nil {
			r.logger.Error("failed to get log entry for apply",
				"index", idx, "err", err)
			continue
		}

		// Skip no-op entries (nil or empty data).
		var response interface{}
		if len(entry.Data) > 0 {
			response = r.fsm.Apply(entry.Data)
		}

		r.mu.Lock()
		r.lastApplied = idx

		// If this is the leader and we have a pending future, resolve it.
		if f, ok := r.pendingFutures[idx]; ok {
			f.respond(nil, response)
			delete(r.pendingFutures, idx)
		}
		r.mu.Unlock()
	}
}

// notifyCommit signals the apply goroutine that new entries have been committed.
// Non-blocking: if a notification is already pending, we skip (the apply loop
// will process all available entries when it wakes up).
func (r *RaftNode) notifyCommit() {
	select {
	case r.commitNotifyCh <- struct{}{}:
	default:
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Persistent state management
// ─────────────────────────────────────────────────────────────────────────────

// persistState saves currentTerm and votedFor to the StableStore.
// Must be called (under r.mu lock) before responding to any RPC
// that modifies these fields.
func (r *RaftNode) persistState() {
	if err := r.stableStore.SetUint64(KeyCurrentTerm, r.currentTerm); err != nil {
		r.logger.Error("failed to persist currentTerm", "err", err)
	}
	if err := r.stableStore.Set(KeyVotedFor, []byte(r.votedFor)); err != nil {
		r.logger.Error("failed to persist votedFor", "err", err)
	}
}

// restoreState loads currentTerm and votedFor from the StableStore.
func (r *RaftNode) restoreState() error {
	term, err := r.stableStore.GetUint64(KeyCurrentTerm)
	if err != nil {
		return fmt.Errorf("restore currentTerm: %w", err)
	}
	r.currentTerm = term

	votedFor, err := r.stableStore.Get(KeyVotedFor)
	if err != nil {
		return fmt.Errorf("restore votedFor: %w", err)
	}
	r.votedFor = string(votedFor)

	r.logger.Debug("restored persistent state",
		"current_term", r.currentTerm,
		"voted_for", r.votedFor,
	)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// State transitions
// ─────────────────────────────────────────────────────────────────────────────

// stepDown transitions to Follower state and updates the current term.
// Called whenever we see a higher term from any server (the fundamental
// Raft rule: "if you see a higher term, step down immediately").
//
// Must be called with r.mu held.
func (r *RaftNode) stepDown(newTerm uint64) {
	r.logger.Info("stepping down",
		"old_term", r.currentTerm,
		"new_term", newTerm,
		"old_state", r.state.String(),
	)
	r.currentTerm = newTerm
	r.state = Follower
	r.votedFor = "" // haven't voted in this new term yet
	r.leaderID = ""
	r.leaderAddr = ""
	r.persistState()
}

// lastLogInfo returns the index and term of the last log entry.
// Returns (0, 0) if the log is empty.
// Must be called with r.mu held (at least RLock).
func (r *RaftNode) lastLogInfo() (uint64, uint64) {
	lastIdx, err := r.logStore.LastIndex()
	if err != nil || lastIdx == 0 {
		return 0, 0
	}
	entry, err := r.logStore.GetLog(lastIdx)
	if err != nil {
		return 0, 0
	}
	return entry.Index, entry.Term
}

// randomElectionTimeout returns a random duration in [ElectionTimeoutMin, ElectionTimeoutMax).
// The randomization is critical for avoiding split votes.
func (r *RaftNode) randomElectionTimeout() time.Duration {
	min := r.config.ElectionTimeoutMin
	max := r.config.ElectionTimeoutMax
	delta := max - min
	return min + time.Duration(rand.Int63n(int64(delta)))
}
