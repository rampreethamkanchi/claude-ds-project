// cmd/server/main.go
//
// Entry point for a cluster node.
//
// Usage:
//   go run ./cmd/server \
//     -id node-1 \
//     -grpc-addr localhost:12000 \
//     -ws-addr localhost:8080 \
//     -data-dir ./data/node-1 \
//     -peers localhost:12000,localhost:12001,localhost:12002 \
//     -bootstrap
//
// For all nodes in a 3-node cluster, run three instances with different ports.
// Only pass -bootstrap on the FIRST startup; remove it on subsequent restarts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/server"
)

func main() {
	// ── Command-line flags ────────────────────────────────────────────────────
	nodeID    := flag.String("id",        "node-1",             "Unique node ID")
	grpcAddr  := flag.String("grpc-addr", "localhost:12000",    "gRPC (Raft transport) listen address")
	wsAddr    := flag.String("ws-addr",   "localhost:8080",     "WebSocket (client) listen address")
	dataDir   := flag.String("data-dir",  "./data/node-1",      "Directory for BoltDB and snapshots")
	peersFlag := flag.String("peers",     "localhost:12000",    "Comma-separated list of ALL peer gRPC addresses (including self)")
	bootstrap := flag.Bool("bootstrap",   false,                "Bootstrap a new cluster (first startup only)")
	initText  := flag.String("init-text", "",                   "Initial document text (only used with -bootstrap)")

	flag.Parse()

	peers := strings.Split(*peersFlag, ",")
	for i, p := range peers {
		peers[i] = strings.TrimSpace(p)
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
	sm := fsm.NewDocumentStateMachine(*initText, logger)

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

	// ── Build the server Node and HTTP mux ────────────────────────────────────
	// wsLeaderAddr is filled with the WS address of the leader — we derive it
	// from the gRPC address by replacing the port with the WS port offset.
	// For this project we just pass our own WS address; the client re-resolves itself.
	node := server.NewNode(rn, sm, *wsAddr, logger)

	mux := http.NewServeMux()
	// WebSocket endpoint — clients connect here.
	mux.Handle("/ws", node)
	// Simple health check endpoint.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		state := "follower"
		if rn.IsLeader() {
			state = "leader"
		}
		fmt.Fprintf(w, `{"node_id":%q,"raft_state":%q,"head_rev":%d}`,
			*nodeID, state, sm.HeadRev())
	})
	// Serve the web client UI.
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
