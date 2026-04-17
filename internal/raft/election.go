// Package raft — election.go
//
// Implements the leader election protocol from §5.2 and §5.4.1 of the Raft paper.
//
// Key invariants maintained:
//   - Election Safety: at most one leader per term.
//   - Election Restriction (§5.4.1): a candidate's log must be at least as
//     up-to-date as any voter's log for the vote to be granted.
//   - Randomized timeouts reduce the probability of split votes.
package raft

import (
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// Starting an election (Candidate side)
// ─────────────────────────────────────────────────────────────────────────────

// startElection increments the term, votes for self, and sends RequestVote
// RPCs to all other peers in parallel. Returns a channel that delivers
// vote responses as they arrive.
//
// Called when:
//   - A follower's election timer fires (no heartbeat received).
//   - A candidate's election timer fires (split vote, retry).
func (r *RaftNode) startElection() <-chan RequestVoteResponse {
	r.mu.Lock()

	// Step 1: Increment currentTerm.
	r.currentTerm++

	// Step 2: Vote for ourselves.
	r.votedFor = r.config.ServerID

	// Step 3: Persist the new term and vote before sending any RPCs.
	// This ensures that if we crash and restart, we don't accidentally
	// vote for two different candidates in the same term.
	r.persistState()

	// Capture current state for the RPC (read under lock, send without lock).
	term := r.currentTerm
	candidateID := r.config.ServerID
	lastLogIndex, lastLogTerm := r.lastLogInfo()

	r.logger.Info("starting election",
		"term", term,
		"last_log_index", lastLogIndex,
		"last_log_term", lastLogTerm,
	)

	r.mu.Unlock()

	// Step 4: Send RequestVote RPCs to all peers in parallel.
	req := &RequestVoteRequest{
		Term:         term,
		CandidateID:  candidateID,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	// Buffered channel large enough for all peer responses.
	voteCh := make(chan RequestVoteResponse, len(r.config.Peers))

	var wg sync.WaitGroup
	for _, peer := range r.config.Peers {
		if peer.ID == r.config.ServerID {
			continue // don't send to ourselves
		}
		wg.Add(1)
		go func(peerAddr string) {
			defer wg.Done()
			resp, err := r.transport.SendRequestVote(peerAddr, req)
			if err != nil {
				r.logger.Debug("RequestVote RPC failed",
					"peer", peerAddr,
					"err", err,
				)
				return // network error — this vote is lost
			}
			voteCh <- *resp
		}(peer.Address)
	}

	// Close the channel when all RPCs complete (so the caller's range loop ends).
	go func() {
		wg.Wait()
		close(voteCh)
	}()

	return voteCh
}

// ─────────────────────────────────────────────────────────────────────────────
// Handling incoming RequestVote (Voter side)
// ─────────────────────────────────────────────────────────────────────────────

// handleRequestVote processes an incoming RequestVote RPC.
//
// Per the Raft paper (§5.2 and §5.4.1), a server grants its vote if ALL of:
//  1. The candidate's term ≥ our currentTerm.
//  2. We haven't already voted for a different candidate in this term.
//  3. The candidate's log is at least as up-to-date as ours (Election Restriction).
//
// Returns true if we should reset the election timer (i.e., we granted a vote).
func (r *RaftNode) handleRequestVote(req *RequestVoteRequest) (*RequestVoteResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := &RequestVoteResponse{
		Term:        r.currentTerm,
		VoteGranted: false,
	}

	// Rule 1: If the candidate's term is less than ours, reject immediately.
	// A stale candidate has no authority.
	if req.Term < r.currentTerm {
		r.logger.Debug("rejecting RequestVote: stale term",
			"candidate", req.CandidateID,
			"candidate_term", req.Term,
			"our_term", r.currentTerm,
		)
		return resp, false
	}

	// If the candidate's term is greater than ours, we must step down to
	// follower and update our term. This is the "higher term → step down"
	// rule that applies to ALL RPCs.
	if req.Term > r.currentTerm {
		r.stepDown(req.Term)
	}

	resp.Term = r.currentTerm

	// Rule 2: Check if we've already voted for someone else in this term.
	// Each server votes for at most one candidate per term (first-come-first-served).
	if r.votedFor != "" && r.votedFor != req.CandidateID {
		r.logger.Debug("rejecting RequestVote: already voted",
			"candidate", req.CandidateID,
			"voted_for", r.votedFor,
		)
		return resp, false
	}

	// Rule 3: Election Restriction (§5.4.1).
	// The candidate's log must be at least as up-to-date as ours.
	// "At least as up-to-date" means:
	//   - The candidate's last log term is greater than ours, OR
	//   - The last log terms are equal AND the candidate's log is at least as long.
	//
	// This ensures the Leader Completeness Property: any committed entry
	// will be present in the log of every future leader.
	myLastIndex, myLastTerm := r.lastLogInfo()
	if !isLogUpToDate(req.LastLogTerm, req.LastLogIndex, myLastTerm, myLastIndex) {
		r.logger.Debug("rejecting RequestVote: candidate log not up-to-date",
			"candidate", req.CandidateID,
			"candidate_last_term", req.LastLogTerm,
			"candidate_last_index", req.LastLogIndex,
			"our_last_term", myLastTerm,
			"our_last_index", myLastIndex,
		)
		return resp, false
	}

	// All checks passed — grant the vote.
	r.votedFor = req.CandidateID
	r.persistState()

	resp.VoteGranted = true
	r.logger.Info("granted vote",
		"candidate", req.CandidateID,
		"term", r.currentTerm,
	)

	// Return true to signal the caller to reset the election timer.
	// (Granting a vote is one of the events that resets the timer.)
	return resp, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Log up-to-date comparison (§5.4.1)
// ─────────────────────────────────────────────────────────────────────────────

// isLogUpToDate returns true if the candidate's log is at least as up-to-date
// as the voter's log. This is the Election Restriction from §5.4.1.
//
// The comparison is:
//  1. If the candidate's last log term is higher → up-to-date.
//  2. If terms are equal, the longer log wins.
func isLogUpToDate(candidateLastTerm, candidateLastIndex, voterLastTerm, voterLastIndex uint64) bool {
	if candidateLastTerm > voterLastTerm {
		return true
	}
	if candidateLastTerm == voterLastTerm && candidateLastIndex >= voterLastIndex {
		return true
	}
	return false
}
