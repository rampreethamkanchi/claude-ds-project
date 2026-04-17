// Package raft — replication.go
//
// Implements log replication from §5.3 of the Raft paper.
//
// Key invariants maintained:
//   - Log Matching Property: if two logs contain an entry with the same
//     index and term, all preceding entries are identical.
//   - Leader Append-Only: a leader never overwrites or deletes its own entries.
//   - §5.4.2: a leader only counts replicas for entries from its OWN term
//     when advancing commitIndex (prevents the subtle Figure 8 scenario).
package raft

import (
	"sort"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Replication goroutine (Leader side — one per peer)
// ─────────────────────────────────────────────────────────────────────────────

// peerState tracks replication state for one peer. Only used while this
// node is the leader. Reinitialized on every election win.
type peerState struct {
	id      string
	addr    string

	// nextIndex is the index of the next log entry to send to this peer.
	// Initialized to leader's lastLogIndex + 1 after election.
	nextIndex uint64

	// matchIndex is the highest log entry known to be replicated on this peer.
	// Initialized to 0 after election.
	matchIndex uint64

	// triggerCh is used to wake the replication goroutine when new entries
	// are appended to the leader's log.
	triggerCh chan struct{}
}

// runReplicator is a long-running goroutine (one per peer) that manages
// log replication to a single follower. It runs while this node is leader.
//
// It sends AppendEntries RPCs in two situations:
//  1. On heartbeat timer tick (even if nothing new to send — maintains authority).
//  2. When triggered by new log entries (via triggerCh).
func (r *RaftNode) runReplicator(peer *peerState, stopCh <-chan struct{}) {
	heartbeat := time.NewTicker(r.config.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		// Attempt to replicate. This handles both heartbeats and new entries.
		r.replicateOnce(peer)

		select {
		case <-peer.triggerCh:
			// New entries to send — loop back and replicate immediately.
		case <-heartbeat.C:
			// Periodic heartbeat — loop back and send (possibly empty) AppendEntries.
		case <-stopCh:
			return // leader stepped down
		case <-r.shutdownCh:
			return // server shutting down
		}
	}
}

// replicateOnce sends one AppendEntries RPC to a peer and processes the response.
// This is the core of log replication.
func (r *RaftNode) replicateOnce(peer *peerState) {
	// ── Build the AppendEntries request ──────────────────────────────────────
	r.mu.RLock()

	// If we're no longer leader, bail out.
	if r.state != Leader {
		r.mu.RUnlock()
		return
	}

	currentTerm := r.currentTerm
	leaderID := r.config.ServerID
	leaderCommit := r.commitIndex

	// PrevLogIndex / PrevLogTerm: the entry just before what we're about to send.
	prevLogIndex := peer.nextIndex - 1
	var prevLogTerm uint64
	if prevLogIndex > 0 {
		entry, err := r.logStore.GetLog(prevLogIndex)
		if err != nil {
			// If we can't find the entry, the follower is too far behind.
			// For our project we just set prevLogTerm=0 and let the consistency
			// check fail, causing nextIndex to decrement.
			prevLogTerm = 0
		} else {
			prevLogTerm = entry.Term
		}
	}

	// Gather entries from nextIndex onwards.
	lastIdx, _ := r.logStore.LastIndex()
	var entries []LogEntry
	if peer.nextIndex <= lastIdx {
		for i := peer.nextIndex; i <= lastIdx; i++ {
			entry, err := r.logStore.GetLog(i)
			if err != nil {
				break
			}
			entries = append(entries, *entry)
		}
	}

	r.mu.RUnlock()

	// ── Send the RPC ─────────────────────────────────────────────────────────
	req := &AppendEntriesRequest{
		Term:         currentTerm,
		LeaderID:     leaderID,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: leaderCommit,
	}

	resp, err := r.transport.SendAppendEntries(peer.addr, req)
	if err != nil {
		r.logger.Debug("AppendEntries RPC failed",
			"peer", peer.id,
			"err", err,
		)
		return // will retry on next heartbeat
	}

	// ── Process the response ─────────────────────────────────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// If response has a higher term, we're stale — step down immediately.
	if resp.Term > r.currentTerm {
		r.logger.Info("stepping down: peer has higher term",
			"peer", peer.id,
			"peer_term", resp.Term,
			"our_term", r.currentTerm,
		)
		r.stepDown(resp.Term)
		return
	}

	if resp.Success {
		// ── Success: the follower accepted all entries ────────────────────
		// Update nextIndex and matchIndex for this peer.
		if len(entries) > 0 {
			peer.matchIndex = entries[len(entries)-1].Index
			peer.nextIndex = peer.matchIndex + 1

			r.logger.Debug("replication success",
				"peer", peer.id,
				"match_index", peer.matchIndex,
			)
		}

		// Check if we can advance commitIndex.
		r.advanceCommitIndex()

	} else {
		// ── Failure: log inconsistency ──────────────────────────────────
		// The follower's log doesn't contain an entry at prevLogIndex with
		// prevLogTerm. We need to find the right nextIndex.
		//
		// Conflict optimization (from the Raft students' guide):
		// Instead of decrementing nextIndex by 1 (which could take O(n) RPCs),
		// use the conflict information to jump back faster.
		if resp.ConflictTerm > 0 {
			// The follower found a conflicting term. Search our log for the
			// last entry of that term.
			conflictIdx := uint64(0)
			lastIdx, _ := r.logStore.LastIndex()
			for i := lastIdx; i >= 1; i-- {
				entry, err := r.logStore.GetLog(i)
				if err != nil {
					break
				}
				if entry.Term == resp.ConflictTerm {
					conflictIdx = i
					break
				}
			}

			if conflictIdx > 0 {
				// We have entries from that term — set nextIndex to the entry
				// after our last entry of that term.
				peer.nextIndex = conflictIdx + 1
			} else {
				// We don't have any entries from that term — use the follower's
				// conflict index (first index of the conflicting term).
				peer.nextIndex = resp.ConflictIndex
			}
		} else {
			// The follower's log is simply too short.
			peer.nextIndex = resp.ConflictIndex
		}

		// Safety: nextIndex must be at least 1.
		if peer.nextIndex < 1 {
			peer.nextIndex = 1
		}

		r.logger.Debug("replication conflict — backing up",
			"peer", peer.id,
			"new_next_index", peer.nextIndex,
			"conflict_term", resp.ConflictTerm,
			"conflict_index", resp.ConflictIndex,
		)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Handling incoming AppendEntries (Follower side)
// ─────────────────────────────────────────────────────────────────────────────

// handleAppendEntries processes an incoming AppendEntries RPC.
//
// Per the Raft paper (§5.3), the receiver:
//  1. Rejects if term < currentTerm.
//  2. Rejects if log doesn't contain an entry at prevLogIndex with prevLogTerm.
//  3. Deletes conflicting entries and appends new ones.
//  4. Updates commitIndex.
//
// Returns (response, true) if this was a valid heartbeat/replication from
// the current leader (so the election timer should be reset).
func (r *RaftNode) handleAppendEntries(req *AppendEntriesRequest) (*AppendEntriesResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := &AppendEntriesResponse{
		Term:    r.currentTerm,
		Success: false,
	}

	// ── Step 1: Reject if the sender's term is stale ─────────────────────
	if req.Term < r.currentTerm {
		return resp, false
	}

	// If the sender's term is ≥ ours, accept it as the leader.
	// This handles: (a) term > ours → step down, (b) term == ours → normal.
	if req.Term > r.currentTerm {
		r.stepDown(req.Term)
	} else if r.state != Follower {
		// We were a candidate (or stale leader), but a legitimate leader exists
		// for this term — convert to follower. stepDown sets state=Follower,
		// clears votedFor, and persists.
		r.stepDown(req.Term)
	}

	// Record the current leader so clients know where to redirect.
	r.state = Follower
	r.leaderID = req.LeaderID
	// Find the leader's address from our peer list.
	for _, p := range r.config.Peers {
		if p.ID == req.LeaderID {
			r.leaderAddr = p.Address
			break
		}
	}

	resp.Term = r.currentTerm

	// ── Step 2: Log consistency check ────────────────────────────────────
	// Check that our log contains an entry at prevLogIndex with prevLogTerm.
	if req.PrevLogIndex > 0 {
		lastIdx, _ := r.logStore.LastIndex()

		if lastIdx < req.PrevLogIndex {
			// Our log is too short — we don't have an entry at prevLogIndex.
			resp.ConflictIndex = lastIdx + 1
			resp.ConflictTerm = 0
			return resp, true // still reset election timer (valid leader)
		}

		prevEntry, err := r.logStore.GetLog(req.PrevLogIndex)
		if err != nil || prevEntry.Term != req.PrevLogTerm {
			// We have an entry at prevLogIndex, but with a different term.
			// This means our logs diverged — report the conflict for fast backup.
			if prevEntry != nil {
				resp.ConflictTerm = prevEntry.Term
				// Find the first index of the conflicting term.
				resp.ConflictIndex = req.PrevLogIndex
				for i := req.PrevLogIndex - 1; i >= 1; i-- {
					e, err := r.logStore.GetLog(i)
					if err != nil || e.Term != resp.ConflictTerm {
						break
					}
					resp.ConflictIndex = i
				}
			} else {
				resp.ConflictIndex = req.PrevLogIndex
			}
			return resp, true
		}
	}

	// ── Step 3: Append new entries (deleting any conflicts) ──────────────
	for i, entry := range req.Entries {
		existing, err := r.logStore.GetLog(entry.Index)
		if err != nil {
			// Entry doesn't exist — append this and all remaining entries.
			newEntries := make([]*LogEntry, 0, len(req.Entries)-i)
			for j := i; j < len(req.Entries); j++ {
				e := req.Entries[j]
				newEntries = append(newEntries, &e)
			}
			if err := r.logStore.StoreLogs(newEntries); err != nil {
				r.logger.Error("failed to store log entries", "err", err)
				return resp, true
			}
			break
		}
		if existing.Term != entry.Term {
			// Conflict: delete this entry and everything after it.
			lastIdx, _ := r.logStore.LastIndex()
			if err := r.logStore.DeleteRange(entry.Index, lastIdx); err != nil {
				r.logger.Error("failed to delete conflicting entries", "err", err)
				return resp, true
			}
			// Now append this and all remaining entries.
			newEntries := make([]*LogEntry, 0, len(req.Entries)-i)
			for j := i; j < len(req.Entries); j++ {
				e := req.Entries[j]
				newEntries = append(newEntries, &e)
			}
			if err := r.logStore.StoreLogs(newEntries); err != nil {
				r.logger.Error("failed to store log entries", "err", err)
				return resp, true
			}
			break
		}
		// Entry matches — continue to next.
	}

	// ── Step 4: Update commitIndex ───────────────────────────────────────
	if req.LeaderCommit > r.commitIndex {
		lastIdx, _ := r.logStore.LastIndex()
		newCommit := req.LeaderCommit
		if lastIdx < newCommit {
			newCommit = lastIdx
		}
		r.commitIndex = newCommit

		// Signal the apply goroutine that new entries are committed.
		r.notifyCommit()
	}

	resp.Success = true
	return resp, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Commit index advancement (Leader only)
// ─────────────────────────────────────────────────────────────────────────────

// advanceCommitIndex checks if any new entries can be committed.
// An entry is committed when it is replicated on a majority of servers.
//
// CRITICAL (§5.4.2): The leader only commits entries from its OWN term.
// Entries from previous terms are committed indirectly when a current-term
// entry at a higher index is committed (this prevents the Figure 8 scenario).
//
// Must be called with r.mu held.
func (r *RaftNode) advanceCommitIndex() {
	// Collect matchIndex from all peers (including ourself).
	matches := make([]uint64, 0, len(r.peers)+1)
	lastIdx, _ := r.logStore.LastIndex()
	matches = append(matches, lastIdx) // leader has all its own entries

	for _, peer := range r.peers {
		matches = append(matches, peer.matchIndex)
	}

	// Sort descending. The value at position (majority - 1) is the highest
	// index replicated on at least 'majority' nodes.
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })

	majority := len(matches)/2 + 1
	newCommit := matches[majority-1]

	if newCommit <= r.commitIndex {
		return // nothing new to commit
	}

	// §5.4.2: Only commit entries from the current term.
	// Check that the entry at newCommit has our current term.
	entry, err := r.logStore.GetLog(newCommit)
	if err != nil {
		return
	}
	if entry.Term != r.currentTerm {
		return // can't commit entries from a previous term by counting
	}

	// Advance commitIndex. All entries up to newCommit are now committed.
	r.commitIndex = newCommit

	r.logger.Debug("advanced commit index",
		"commit_index", r.commitIndex,
		"majority", majority,
		"matches", matches,
	)

	// Wake the apply goroutine.
	r.notifyCommit()
}

// triggerReplication wakes all replication goroutines to send new entries.
// Called after the leader appends new entries to its log.
//
// Must be called with r.mu held (or after unlocking — triggerCh is safe).
func (r *RaftNode) triggerReplication() {
	for _, peer := range r.peers {
		// Non-blocking send — if the goroutine is busy, it'll pick up
		// the new entries on its next iteration.
		select {
		case peer.triggerCh <- struct{}{}:
		default:
		}
	}
}
