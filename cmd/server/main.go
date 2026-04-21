// cmd/server/main.go
//
// Entry point for a cluster node.
//
// Usage:
//   go run ./cmd/server -config config.json
//   or
//   go run ./cmd/server -id node-1 -grpc-addr localhost:12000 ...
//
// For all nodes in a 3-node cluster, run three instances with different ports.
// Only pass -bootstrap on the FIRST startup; remove it on subsequent restarts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/ot"
	"distributed-editor/internal/server"
)

// Config defines the structure for our JSON configuration file.
type Config struct {
	NodeID    string   `json:"node_id"`
	GRPCAddr  string   `json:"grpc_addr"`
	WSAddr    string   `json:"ws_addr"`
	DataDir   string   `json:"data_dir"`
	Peers     []string `json:"peers"`
	Bootstrap bool     `json:"bootstrap"`
}

func main() {
	// ── Command-line flags ────────────────────────────────────────────────────
	configPath := flag.String("config", "", "Path to configuration JSON file")

	nodeID    := flag.String("id",        "node-1",             "Unique node ID")
	grpcAddr  := flag.String("grpc-addr", "localhost:12000",    "gRPC (Raft transport) listen address")
	wsAddr    := flag.String("ws-addr",   "localhost:8080",     "WebSocket (client) listen address")
	dataDir   := flag.String("data-dir",  "./data/node-1",      "Directory for BoltDB and snapshots")
	peersFlag := flag.String("peers",     "localhost:12000",    "Comma-separated list of ALL peer gRPC addresses (including self)")
	bootstrap := flag.Bool("bootstrap",   false,                "Bootstrap a new cluster (first startup only)")
	initText  := flag.String("init-text", "",                   "Initial document text (only used with -bootstrap)")

	flag.Parse()

	var peers []string

	// ── Load from config file if provided ─────────────────────────────────────
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			log.Fatalf("failed to open config file: %v", err)
		}
		defer file.Close()

		var cfg Config
		if err := json.NewDecoder(file).Decode(&cfg); err != nil {
			log.Fatalf("failed to decode config file: %v", err)
		}

		// Config file overrides defaults (if field is not empty)
		if cfg.NodeID != "" {
			*nodeID = cfg.NodeID
		}
		if cfg.GRPCAddr != "" {
			*grpcAddr = cfg.GRPCAddr
		}
		if cfg.WSAddr != "" {
			*wsAddr = cfg.WSAddr
		}
		if cfg.DataDir != "" {
			*dataDir = cfg.DataDir
		}
		if len(cfg.Peers) > 0 {
			peers = cfg.Peers
		}
		if cfg.Bootstrap {
			*bootstrap = true
		}
	}

	// ── Parse peers from flag if not loaded from config ───────────────────────
	if len(peers) == 0 {
		peers = strings.Split(*peersFlag, ",")
		for i, p := range peers {
			peers[i] = strings.TrimSpace(p)
		}
	}

	// ── Structured logger ─────────────────────────────────────────────────────
	// Every log entry includes server_id so multi-server logs are easy to trace.
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(logHandler).With(
		"server_id", *nodeID,
		"grpc_addr", *grpcAddr,
		"ws_addr",   *wsAddr,
	)

	logger.Info("starting node",
		"node_id",   *nodeID,
		"peers",     peers,
		"bootstrap", *bootstrap,
	)

	// ── Build the FSM ─────────────────────────────────────────────────────────
	sm := fsm.NewDocumentStateMachine("", logger)

	// ── Start Raft ────────────────────────────────────────────────────────────
	raftCfg := server.RaftConfig{
		NodeID:    *nodeID,
		BindAddr:  *grpcAddr,
		DataDir:   *dataDir,
		Peers:     peers,
		Bootstrap: *bootstrap,
	}
	rn, err := server.SetupRaft(raftCfg, sm, logger)
	if err != nil {
		logger.Error("failed to start raft", "err", err)
		os.Exit(1)
	}
	defer rn.Shutdown()

	// If we are bootstrapping and have initial text, we must propose it.
	if *bootstrap && *initText != "" {
		go func() {
			logger.Info("waiting to become leader to propose initial text...")
			for !rn.IsLeader() {
				time.Sleep(100 * time.Millisecond)
			}
			entry := fsm.RaftLogEntry{
				ClientID:     "system-bootstrap",
				SubmissionID: 1,
				BaseRev:      0,
				Changeset:    ot.MakeInsert(0, 0, *initText),
			}
			data, err := fsm.MarshalEntry(entry)
			if err == nil {
				rn.Raft.Apply(data, 5*time.Second)
				logger.Info("proposed initial text to cluster")
			}
		}()
	}

	// ── Build the server Node and HTTP mux ────────────────────────────────────
	node := server.NewNode(rn, sm, *wsAddr, logger)

	mux := http.NewServeMux()
	mux.Handle("/ws", node)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		state := "follower"
		if rn.IsLeader() {
			state = "leader"
		}
		fmt.Fprintf(w, `{"node_id":%q,"raft_state":%q,"head_rev":%d}`,
			*nodeID, state, sm.HeadRev())
	})
	mux.Handle("/", http.FileServer(http.Dir("./web")))

	httpSrv := &http.Server{
		Addr:    *wsAddr,
		Handler: mux,
	}

	// ── Start HTTP server ─────────────────────────────────────────────────────
	go func() {
		logger.Info("HTTP/WebSocket server listening", "addr", *wsAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	// ── Wait for OS signal ────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx) //nolint:errcheck
}
