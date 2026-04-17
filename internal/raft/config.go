// Package raft — config.go
//
// Configuration for a Raft node. Provides sensible defaults tuned for
// localhost clusters (low latency). For production across data centers,
// increase the timeout values.
package raft

import (
	"log/slog"
	"time"
)

// Config holds all configuration needed to create a RaftNode.
type Config struct {
	// ServerID is the unique identifier for this server in the cluster.
	// Must match one of the entries in the Peers list.
	ServerID string

	// Peers lists ALL nodes in the cluster, including this server.
	// On first startup, Raft uses this to know who to contact for elections
	// and replication. The server identifies itself by matching ServerID.
	Peers []PeerConfig

	// HeartbeatInterval is how often the leader sends empty AppendEntries
	// (heartbeats) to maintain its authority and prevent elections.
	// Must be significantly shorter than ElectionTimeoutMin.
	HeartbeatInterval time.Duration

	// ElectionTimeoutMin and ElectionTimeoutMax define the range for the
	// randomized election timeout. Each follower picks a random timeout
	// in [Min, Max) — the randomization reduces split-vote probability.
	// (See §5.2 of the Raft paper.)
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration

	// LogStore persists log entries to disk so they survive restarts.
	LogStore LogStore

	// StableStore persists currentTerm and votedFor to disk.
	StableStore StableStore

	// FSM is the user's state machine that receives committed log entries.
	FSM FSM

	// Logger for structured logging. If nil, a default logger is used.
	Logger *slog.Logger
}

// DefaultConfig returns a Config with sensible defaults for a localhost cluster.
// The caller MUST set ServerID, Peers, LogStore, StableStore, and FSM.
func DefaultConfig() *Config {
	return &Config{
		// Heartbeat every 50ms — fast enough for localhost where RTT is <1ms.
		HeartbeatInterval: 50 * time.Millisecond,

		// Election timeout: random in [150ms, 300ms).
		// The paper recommends broadcastTime << electionTimeout << MTBF.
		// For localhost: broadcastTime ≈ 1ms, so 150-300ms is very safe.
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
	}
}

// validate checks that all required fields are set.
func (c *Config) validate() error {
	if c.ServerID == "" {
		return ErrConfigMissingField("ServerID")
	}
	if len(c.Peers) == 0 {
		return ErrConfigMissingField("Peers")
	}
	if c.LogStore == nil {
		return ErrConfigMissingField("LogStore")
	}
	if c.StableStore == nil {
		return ErrConfigMissingField("StableStore")
	}
	if c.FSM == nil {
		return ErrConfigMissingField("FSM")
	}
	if c.HeartbeatInterval <= 0 {
		return ErrConfigMissingField("HeartbeatInterval")
	}
	if c.ElectionTimeoutMin <= 0 || c.ElectionTimeoutMax <= c.ElectionTimeoutMin {
		return ErrConfigMissingField("ElectionTimeout (invalid range)")
	}
	// Verify that this server is in the peer list.
	found := false
	for _, p := range c.Peers {
		if p.ID == c.ServerID {
			found = true
			break
		}
	}
	if !found {
		return ErrConfigMissingField("ServerID not found in Peers list")
	}
	return nil
}

// ErrConfigMissingField is returned when a required config field is missing.
type ErrConfigMissingField string

func (e ErrConfigMissingField) Error() string {
	return "raft: missing or invalid config field: " + string(e)
}
