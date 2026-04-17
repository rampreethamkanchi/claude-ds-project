// Package raft — types.go
//
// Core data structures for the Raft consensus protocol.
// All types here map directly to concepts from the Raft paper
// ("In Search of an Understandable Consensus Algorithm" — Ongaro & Ousterhout).
//
// References to specific sections of the paper are included as comments.
package raft

import (
	"errors"
	"fmt"
)

var ErrLogNotFound = errors.New("raft: log entry not found")

// ─────────────────────────────────────────────────────────────────────────────
// Server State (§5.1)
// ─────────────────────────────────────────────────────────────────────────────

// ServerState represents the current role of a Raft server.
// A server is always in exactly one of three states.
type ServerState int

const (
	// Follower is the default state. Followers are passive — they issue no RPCs
	// on their own, and simply respond to RPCs from leaders and candidates.
	Follower ServerState = iota

	// Candidate is the state a server enters when it starts an election.
	// It votes for itself and sends RequestVote RPCs to all other servers.
	Candidate

	// Leader handles all client requests. It replicates log entries to followers.
	// There is at most one leader per term (Election Safety property).
	Leader
)

// String returns a human-readable name for the server state.
func (s ServerState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return fmt.Sprintf("Unknown(%d)", int(s))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Log Entry (§5.3)
// ─────────────────────────────────────────────────────────────────────────────

// LogEntry is a single entry in the replicated log.
// Each entry contains a command for the state machine and the term when the
// entry was received by the leader. Log entries are 1-indexed.
type LogEntry struct {
	Index uint64 // position in the log (1-indexed, monotonically increasing)
	Term  uint64 // the term when this entry was created by the leader
	Data  []byte // command for the state machine (opaque to Raft)
}

// ─────────────────────────────────────────────────────────────────────────────
// AppendEntries RPC (§5.3)
// ─────────────────────────────────────────────────────────────────────────────

// AppendEntriesRequest is sent by the leader to replicate log entries
// and also serves as a heartbeat (when Entries is empty).
//
// Fields map directly to Figure 2 of the Raft paper.
type AppendEntriesRequest struct {
	// Term is the leader's current term.
	Term uint64 `json:"term"`

	// LeaderID is the ID of the leader, so followers can redirect clients.
	LeaderID string `json:"leader_id"`

	// PrevLogIndex is the index of the log entry immediately preceding
	// the new entries. Used for the consistency check.
	PrevLogIndex uint64 `json:"prev_log_index"`

	// PrevLogTerm is the term of the entry at PrevLogIndex.
	// Together with PrevLogIndex, this enforces the Log Matching Property.
	PrevLogTerm uint64 `json:"prev_log_term"`

	// Entries are the log entries to append (empty for heartbeat).
	Entries []LogEntry `json:"entries"`

	// LeaderCommit is the leader's commitIndex, so followers can advance
	// their own commitIndex.
	LeaderCommit uint64 `json:"leader_commit"`
}

// AppendEntriesResponse is the follower's reply to an AppendEntries RPC.
type AppendEntriesResponse struct {
	// Term is the responder's currentTerm, for the leader to update itself
	// and step down if its term is stale.
	Term uint64 `json:"term"`

	// Success is true if the follower contained an entry matching
	// PrevLogIndex and PrevLogTerm — meaning the entries were accepted.
	Success bool `json:"success"`

	// ── Conflict optimization (not in the basic paper, but standard) ──
	// When Success is false due to a log conflict, these fields help the
	// leader find the correct nextIndex faster (instead of decrementing by 1).

	// ConflictTerm is the term of the conflicting entry (0 if log is too short).
	ConflictTerm uint64 `json:"conflict_term"`

	// ConflictIndex is the first index of ConflictTerm in the follower's log,
	// or the follower's log length + 1 if the log is too short.
	ConflictIndex uint64 `json:"conflict_index"`
}

// ─────────────────────────────────────────────────────────────────────────────
// RequestVote RPC (§5.2, §5.4.1)
// ─────────────────────────────────────────────────────────────────────────────

// RequestVoteRequest is sent by candidates to gather votes during an election.
type RequestVoteRequest struct {
	// Term is the candidate's term.
	Term uint64 `json:"term"`

	// CandidateID is the ID of the candidate requesting the vote.
	CandidateID string `json:"candidate_id"`

	// LastLogIndex is the index of the candidate's last log entry.
	// Used for the election restriction (§5.4.1).
	LastLogIndex uint64 `json:"last_log_index"`

	// LastLogTerm is the term of the candidate's last log entry.
	// A voter only grants its vote if the candidate's log is at least
	// as up-to-date as the voter's own log.
	LastLogTerm uint64 `json:"last_log_term"`
}

// RequestVoteResponse is a server's reply to a RequestVote RPC.
type RequestVoteResponse struct {
	// Term is the responder's currentTerm, for the candidate to update itself.
	Term uint64 `json:"term"`

	// VoteGranted is true if the candidate received the vote.
	VoteGranted bool `json:"vote_granted"`
}

// ─────────────────────────────────────────────────────────────────────────────
// RPC routing (internal)
// ─────────────────────────────────────────────────────────────────────────────

// RPC represents an incoming RPC that needs to be processed by the Raft node.
// The transport layer puts RPCs on a channel; the main run loop reads from it,
// processes the request, and sends the response back via RespCh.
type RPC struct {
	// Command is the RPC request payload.
	// It is either *AppendEntriesRequest or *RequestVoteRequest.
	Command interface{}

	// RespCh is used to send the response back to the transport layer.
	// The transport is blocking on this channel waiting for our reply.
	RespCh chan RPCResponse
}

// RPCResponse wraps the response to an RPC along with any transport error.
type RPCResponse struct {
	Response interface{} // *AppendEntriesResponse or *RequestVoteResponse
	Error    error
}

// Respond is a helper to send a response on the RPC's response channel.
func (r *RPC) Respond(resp interface{}, err error) {
	r.RespCh <- RPCResponse{Response: resp, Error: err}
}

// ─────────────────────────────────────────────────────────────────────────────
// FSM Interface
// ─────────────────────────────────────────────────────────────────────────────

// FSM (Finite State Machine) is the interface that the user's application
// state machine must implement. Raft replicates log entries and applies them
// to the FSM on every node, guaranteeing that all nodes apply the same entries
// in the same order — producing identical state.
type FSM interface {
	// Apply is called once a log entry has been committed (replicated to a
	// majority). The data is the raw bytes from the LogEntry.Data field.
	// The return value is stored in the ApplyFuture so the caller on the
	// leader can retrieve it.
	Apply(data []byte) interface{}
}

// ─────────────────────────────────────────────────────────────────────────────
// LogStore Interface
// ─────────────────────────────────────────────────────────────────────────────

// LogStore is the interface for persistent storage of Raft log entries.
// Implementations must be crash-safe: a stored entry must survive process restarts.
type LogStore interface {
	// FirstIndex returns the index of the first log entry (0 if empty).
	FirstIndex() (uint64, error)

	// LastIndex returns the index of the last log entry (0 if empty).
	LastIndex() (uint64, error)

	// GetLog retrieves a single log entry by index. Returns nil if not found.
	GetLog(index uint64) (*LogEntry, error)

	// StoreLog stores a single log entry.
	StoreLog(entry *LogEntry) error

	// StoreLogs stores multiple log entries atomically.
	StoreLogs(entries []*LogEntry) error

	// DeleteRange deletes all log entries in the range [min, max] inclusive.
	// Used during log compaction and conflict resolution.
	DeleteRange(min, max uint64) error
}

// ─────────────────────────────────────────────────────────────────────────────
// StableStore Interface
// ─────────────────────────────────────────────────────────────────────────────

// StableStore is the interface for persistent storage of small key-value pairs.
// Raft uses this to persist currentTerm and votedFor so they survive restarts.
type StableStore interface {
	Set(key []byte, val []byte) error
	Get(key []byte) ([]byte, error)
	SetUint64(key []byte, val uint64) error
	GetUint64(key []byte) (uint64, error)
}

// Well-known keys used in the StableStore.
var (
	KeyCurrentTerm = []byte("CurrentTerm")
	KeyVotedFor    = []byte("VotedFor")
)

// ─────────────────────────────────────────────────────────────────────────────
// Transport Interface
// ─────────────────────────────────────────────────────────────────────────────

// Transport is the interface for sending and receiving Raft RPCs between nodes.
// Two implementations exist:
//   - TCPTransport: real TCP+JSON for production
//   - InmemTransport: in-memory for tests (no network, instant delivery)
type Transport interface {
	// Consumer returns a channel that the Raft node reads for incoming RPCs.
	// The transport puts received RPCs on this channel.
	Consumer() <-chan *RPC

	// LocalAddr returns the network address of this transport.
	LocalAddr() string

	// SendAppendEntries sends an AppendEntries RPC to the target server.
	// This is a synchronous call that blocks until a response is received or timeout.
	SendAppendEntries(target string, req *AppendEntriesRequest) (*AppendEntriesResponse, error)

	// SendRequestVote sends a RequestVote RPC to the target server.
	SendRequestVote(target string, req *RequestVoteRequest) (*RequestVoteResponse, error)

	// Shutdown closes the transport and releases resources.
	Shutdown() error
}

// ─────────────────────────────────────────────────────────────────────────────
// Peer Configuration
// ─────────────────────────────────────────────────────────────────────────────

// PeerConfig describes a single node in the Raft cluster.
type PeerConfig struct {
	ID      string // unique identifier for the node
	Address string // network address for the transport layer
}
