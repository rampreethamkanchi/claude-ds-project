// Package server — cluster.go
//
// Sets up and bootstraps a hashicorp/raft node.
// Each server process calls SetupRaft() once at startup with its configuration.
//
// We use the Jille/raft-grpc-transport library for gRPC-based Raft RPC transport
// between cluster nodes (AppendEntries, RequestVote).
package server

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/store"

	transport "github.com/Jille/raft-grpc-transport"
	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RaftConfig holds all configuration needed to start a Raft node.
type RaftConfig struct {
	NodeID      string   // unique identifier for this node, e.g. "node-1"
	BindAddr    string   // gRPC listen address, e.g. "localhost:12000"
	DataDir     string   // directory for BoltDB files and Raft snapshots
	Peers       []string // gRPC addresses of ALL cluster nodes (including self)
	Bootstrap   bool     // true only on the very first cluster startup
	InitialText string   // initial document text (only used if bootstrapping)
}

// RaftNode bundles the live Raft instance with its transport and stores.
type RaftNode struct {
	Raft      *raft.Raft
	Transport *transport.Manager
	LogStore  *store.BoltStore
	FSM       *fsm.DocumentStateMachine
	grpcSrv   *grpc.Server
}

// SetupRaft creates and starts a Raft node using the provided configuration.
//
// Steps performed:
//  1. Open BoltDB-backed LogStore + StableStore.
//  2. Create a file-based snapshot store.
//  3. Open the gRPC listener and create the grpc-transport Manager.
//  4. Create the Raft instance with our DocumentStateMachine as the FSM.
//  5. If Bootstrap==true, bootstrap a new single-server cluster.
//  6. Start the gRPC server (non-blocking).
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

	// ── File-based snapshot store ─────────────────────────────────────────────
	snapshotDir := filepath.Join(cfg.DataDir, "snapshots")
	snapStore, err := raft.NewFileSnapshotStore(snapshotDir, 2, os.Stderr)
	if err != nil {
		boltStore.Close()
		return nil, fmt.Errorf("setup_raft: snapshot store: %w", err)
	}

	// ── gRPC transport ────────────────────────────────────────────────────────
	// The Jille/raft-grpc-transport library wraps Raft's NetworkTransport interface
	// over gRPC bidirectional streaming — fulfilling the gRPC transport requirement.
	grpcListener, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		boltStore.Close()
		return nil, fmt.Errorf("setup_raft: listen %q: %w", cfg.BindAddr, err)
	}

	grpcSrv := grpc.NewServer()
	mgr := transport.New(
		raft.ServerAddress(cfg.BindAddr),
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	)
	mgr.Register(grpcSrv)

	// ── Raft configuration ────────────────────────────────────────────────────
	raftCfg := raft.DefaultConfig()
	// Internally, use the bind address as the Raft server ID so it aligns with peer lists.
	raftCfg.LocalID = raft.ServerID(cfg.BindAddr)
	// Tune timeouts for localhost clusters where latency is ~0ms.
	raftCfg.HeartbeatTimeout = 50 * time.Millisecond
	raftCfg.ElectionTimeout = 100 * time.Millisecond
	raftCfg.LeaderLeaseTimeout = 50 * time.Millisecond
	raftCfg.CommitTimeout = 10 * time.Millisecond

	// raftCfg.HeartbeatTimeout = 500 * time.Millisecond
	// raftCfg.ElectionTimeout = 1000 * time.Millisecond
	// raftCfg.CommitTimeout = 100 * time.Millisecond

	// ── Create Raft instance ──────────────────────────────────────────────────
	r, err := raft.NewRaft(raftCfg, stateMachine, boltStore, boltStore, snapStore, mgr.Transport())
	if err != nil {
		boltStore.Close()
		grpcListener.Close()
		return nil, fmt.Errorf("setup_raft: new raft: %w", err)
	}

	// ── Bootstrap (first startup only) ───────────────────────────────────────
	// Bootstrap converts a lone node into a single-member cluster so it can elect
	// itself as leader. On subsequent restarts, Bootstrap must NOT be called
	// (it would fail because existing state is present).
	if cfg.Bootstrap {
		// Build the initial cluster configuration from the peer list.
		servers := make([]raft.Server, 0, len(cfg.Peers))
		for _, peer := range cfg.Peers {
			// Use gRPC address as both the server ID and address on first bootstrap.
			// In production you'd assign stable IDs separately; for this project
			// the gRPC address is stable enough.
			servers = append(servers, raft.Server{
				Suffrage: raft.Voter,
				ID:       raft.ServerID(peer),
				Address:  raft.ServerAddress(peer),
			})
		}
		// Override LocalID to match peer format if it matches an address.
		// If NodeID was set differently, this still works; just the bootstrap
		// servers must enumerate all members by ADDRESS.
		configuration := raft.Configuration{Servers: servers}
		if fut := r.BootstrapCluster(configuration); fut.Error() != nil {
			// ErrCantBootstrap means the cluster already exists — that's fine.
			if fut.Error() != raft.ErrCantBootstrap {
				boltStore.Close()
				return nil, fmt.Errorf("setup_raft: bootstrap: %w", fut.Error())
			}
		}
		logger.Info("bootstrapped raft cluster", "node_id", cfg.NodeID, "peers", cfg.Peers)
	}

	// Start the gRPC server in a background goroutine.
	go func() {
		logger.Info("gRPC transport listening", "addr", cfg.BindAddr)
		if err := grpcSrv.Serve(grpcListener); err != nil {
			logger.Error("gRPC server stopped", "err", err)
		}
	}()

	return &RaftNode{
		Raft:      r,
		Transport: mgr,
		LogStore:  boltStore,
		FSM:       stateMachine,
		grpcSrv:   grpcSrv,
	}, nil
}

// Shutdown gracefully shuts down the Raft node and its gRPC server.
func (n *RaftNode) Shutdown() {
	n.grpcSrv.Stop()
	n.Raft.Shutdown().Error() //nolint:errcheck
	n.LogStore.Close()        //nolint:errcheck
}

// IsLeader returns true if this node is the current Raft leader.
func (n *RaftNode) IsLeader() bool {
	return n.Raft.State() == raft.Leader
}

// LeaderAddr returns the gRPC address of the current Raft leader, or "" if unknown.
func (n *RaftNode) LeaderAddr() string {
	addr, _ := n.Raft.LeaderWithID()
	return string(addr)
}
