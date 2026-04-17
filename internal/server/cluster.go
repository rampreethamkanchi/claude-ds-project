// Package server — cluster.go
//
// Sets up and bootstraps our custom Raft node.
// Each server process calls SetupRaft() once at startup with its configuration.
//
// We use our custom TCPTransport layer instead of gRPC.
package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/raft"
	"distributed-editor/internal/store"
)

// RaftConfig holds all configuration needed to start a Raft node.
type RaftConfig struct {
	NodeID      string   // unique identifier for this node, e.g. "node-1"
	BindAddr    string   // TCP listen address, e.g. "localhost:12000"
	DataDir     string   // directory for BoltDB files
	Peers       []string // network addresses of ALL cluster nodes
	Bootstrap   bool     // not strictly needed for our custom raft, but kept for compatibility
	InitialText string   // initial document text (only used if bootstrapping)
}

// RaftNode bundles the live Raft instance with its transport and stores.
type RaftNode struct {
	Raft      *raft.RaftNode
	Transport *raft.TCPTransport
	LogStore  *store.BoltStore
	FSM       *fsm.DocumentStateMachine
}

// SetupRaft creates and starts a custom Raft node using the provided configuration.
func SetupRaft(cfg RaftConfig, stateMachine *fsm.DocumentStateMachine, logger *slog.Logger) (*RaftNode, error) {
	// Ensure data directory exists.
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("setup_raft: mkdir %q: %w", cfg.DataDir, err)
	}

	// ── BoltDB: Raft log + stable store ──────────────────────────────────────
	boltPath := filepath.Join(cfg.DataDir, "raft.db")
	boltStore, err := store.NewBoltStore(boltPath)
	if err != nil {
		return nil, fmt.Errorf("setup_raft: bolt store: %w", err)
	}

	// ── TCPTransport ────────────────────────────────────────────────────────
	transport, err := raft.NewTCPTransport(cfg.BindAddr, logger)
	if err != nil {
		boltStore.Close()
		return nil, fmt.Errorf("setup_raft: transport: %w", err)
	}

	// Build peer config
	var peers []raft.PeerConfig
	for _, p := range cfg.Peers {
		// Use network address as ID for simplicity
		peers = append(peers, raft.PeerConfig{
			ID:      p,
			Address: p,
		})
	}

	// ── Custom Raft Configuration ──────────────────────────────────────────
	raftCfg := &raft.Config{
		ServerID:           cfg.BindAddr,
		Peers:              peers,
		LogStore:           boltStore,
		StableStore:        boltStore,
		FSM:                stateMachine,
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Logger:             logger,
	}

	// ── Create Raft instance ────────────────────────────────────────────────
	r, err := raft.NewRaftNode(raftCfg, transport)
	if err != nil {
		boltStore.Close()
		transport.Shutdown()
		return nil, fmt.Errorf("setup_raft: new raft: %w", err)
	}

	return &RaftNode{
		Raft:      r,
		Transport: transport,
		LogStore:  boltStore,
		FSM:       stateMachine,
	}, nil
}

// Shutdown gracefully shuts down the Raft node and its transport.
func (n *RaftNode) Shutdown() {
	n.Raft.Shutdown()
	n.Transport.Shutdown()
	n.LogStore.Close()
}

// IsLeader returns true if this node is the current Raft leader.
func (n *RaftNode) IsLeader() bool {
	return n.Raft.IsLeader()
}

// LeaderAddr returns the address of the current Raft leader, or "" if unknown.
func (n *RaftNode) LeaderAddr() string {
	return n.Raft.LeaderAddr()
}
