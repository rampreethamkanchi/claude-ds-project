// Package raft — transport.go
//
// TCP+JSON transport for Raft RPCs. This is the production transport layer.
//
// Protocol format (each message):
//   [1 byte: message type] [4 bytes: payload length (big-endian)] [N bytes: JSON payload]
//
// Message types:
//   0 = AppendEntriesRequest
//   1 = AppendEntriesResponse
//   2 = RequestVoteRequest
//   3 = RequestVoteResponse
//
// Each outgoing RPC opens a new TCP connection (or reuses a pooled one),
// sends the request, reads the response, and returns. The listener goroutine
// accepts incoming connections and dispatches them to handler goroutines.
package raft

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Message type constants for the wire protocol.
const (
	msgAppendEntriesReq  byte = 0
	msgAppendEntriesResp byte = 1
	msgRequestVoteReq    byte = 2
	msgRequestVoteResp   byte = 3
)

// TCPTransport implements the Transport interface using TCP+JSON.
type TCPTransport struct {
	localAddr string       // the address we listen on
	listener  net.Listener // TCP listener for incoming connections
	rpcCh     chan *RPC     // incoming RPCs are placed here for the Raft node

	// Connection pool: reuse outgoing connections to peers.
	connPool map[string]net.Conn
	connMu   sync.Mutex

	shutdownCh chan struct{}
	shutdownMu sync.Mutex
	shutdown   bool

	logger *slog.Logger
}

// NewTCPTransport creates and starts a TCP transport listening on the given address.
// It immediately begins accepting incoming connections in a background goroutine.
func NewTCPTransport(bindAddr string, logger *slog.Logger) (*TCPTransport, error) {
	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("tcp_transport: listen on %s: %w", bindAddr, err)
	}

	t := &TCPTransport{
		localAddr:  bindAddr,
		listener:   listener,
		rpcCh:      make(chan *RPC, 256),
		connPool:   make(map[string]net.Conn),
		shutdownCh: make(chan struct{}),
		logger:     logger,
	}

	// Start accepting incoming connections.
	go t.acceptLoop()

	return t, nil
}

// Consumer returns the channel of incoming RPCs.
func (t *TCPTransport) Consumer() <-chan *RPC {
	return t.rpcCh
}

// LocalAddr returns the TCP address this transport is listening on.
func (t *TCPTransport) LocalAddr() string {
	return t.localAddr
}

// ─────────────────────────────────────────────────────────────────────────────
// Outgoing RPCs
// ─────────────────────────────────────────────────────────────────────────────

// SendAppendEntries sends an AppendEntries RPC to the target server.
func (t *TCPTransport) SendAppendEntries(target string, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}

	// Encode and send the request.
	if err := t.sendMessage(conn, msgAppendEntriesReq, req); err != nil {
		t.removeConn(target)
		conn.Close()
		return nil, fmt.Errorf("send AppendEntries to %s: %w", target, err)
	}

	// Read the response.
	var resp AppendEntriesResponse
	if err := t.readMessage(conn, msgAppendEntriesResp, &resp); err != nil {
		t.removeConn(target)
		conn.Close()
		return nil, fmt.Errorf("read AppendEntries response from %s: %w", target, err)
	}

	return &resp, nil
}

// SendRequestVote sends a RequestVote RPC to the target server.
func (t *TCPTransport) SendRequestVote(target string, req *RequestVoteRequest) (*RequestVoteResponse, error) {
	conn, err := t.getConn(target)
	if err != nil {
		return nil, err
	}

	if err := t.sendMessage(conn, msgRequestVoteReq, req); err != nil {
		t.removeConn(target)
		conn.Close()
		return nil, fmt.Errorf("send RequestVote to %s: %w", target, err)
	}

	var resp RequestVoteResponse
	if err := t.readMessage(conn, msgRequestVoteResp, &resp); err != nil {
		t.removeConn(target)
		conn.Close()
		return nil, fmt.Errorf("read RequestVote response from %s: %w", target, err)
	}

	return &resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Connection pool
// ─────────────────────────────────────────────────────────────────────────────

// getConn returns a cached connection to the target, or creates a new one.
func (t *TCPTransport) getConn(target string) (net.Conn, error) {
	t.connMu.Lock()
	defer t.connMu.Unlock()

	if conn, ok := t.connPool[target]; ok {
		return conn, nil
	}

	// Dial with a timeout to avoid blocking forever.
	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tcp_transport: dial %s: %w", target, err)
	}

	t.connPool[target] = conn
	return conn, nil
}

// removeConn removes a connection from the pool (e.g., after an error).
func (t *TCPTransport) removeConn(target string) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	delete(t.connPool, target)
}

// ─────────────────────────────────────────────────────────────────────────────
// Incoming connections
// ─────────────────────────────────────────────────────────────────────────────

// acceptLoop runs in a background goroutine, accepting new TCP connections
// and spawning a handler goroutine for each.
func (t *TCPTransport) acceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.shutdownCh:
				return // clean shutdown
			default:
				t.logger.Warn("tcp_transport: accept error", "err", err)
				continue
			}
		}
		go t.handleConnection(conn)
	}
}

// handleConnection reads RPCs from an incoming connection, dispatches them
// to the Raft node, and sends responses back.
func (t *TCPTransport) handleConnection(conn net.Conn) {
	defer conn.Close()

	for {
		// Read the message header: [1 byte type][4 bytes length]
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return // connection closed or error
		}

		msgType := header[0]
		payloadLen := binary.BigEndian.Uint32(header[1:5])
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}

		switch msgType {
		case msgAppendEntriesReq:
			var req AppendEntriesRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				t.logger.Warn("tcp_transport: unmarshal AppendEntries", "err", err)
				return
			}
			// Send the RPC to the Raft node and wait for the response.
			respCh := make(chan RPCResponse, 1)
			t.rpcCh <- &RPC{Command: &req, RespCh: respCh}
			rpcResp := <-respCh

			// Send the response back on the same connection.
			if err := t.sendMessage(conn, msgAppendEntriesResp, rpcResp.Response); err != nil {
				return
			}

		case msgRequestVoteReq:
			var req RequestVoteRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				t.logger.Warn("tcp_transport: unmarshal RequestVote", "err", err)
				return
			}
			respCh := make(chan RPCResponse, 1)
			t.rpcCh <- &RPC{Command: &req, RespCh: respCh}
			rpcResp := <-respCh

			if err := t.sendMessage(conn, msgRequestVoteResp, rpcResp.Response); err != nil {
				return
			}

		default:
			t.logger.Warn("tcp_transport: unknown message type", "type", msgType)
			return
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Wire format helpers
// ─────────────────────────────────────────────────────────────────────────────

// sendMessage encodes and sends a framed JSON message.
func (t *TCPTransport) sendMessage(conn net.Conn, msgType byte, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Set a write deadline to avoid blocking forever.
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	// Write header: [1 byte type][4 bytes big-endian length]
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(data)))
	if _, err := conn.Write(header); err != nil {
		return err
	}

	// Write payload.
	if _, err := conn.Write(data); err != nil {
		return err
	}

	return nil
}

// readMessage reads a framed JSON message, verifying the expected type.
func (t *TCPTransport) readMessage(conn net.Conn, expectedType byte, out interface{}) error {
	// Set a read deadline.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read header.
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	msgType := header[0]
	if msgType != expectedType {
		return fmt.Errorf("expected message type %d, got %d", expectedType, msgType)
	}

	payloadLen := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}

	return json.Unmarshal(payload, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// Shutdown
// ─────────────────────────────────────────────────────────────────────────────

// Shutdown stops the transport, closing the listener and all pooled connections.
func (t *TCPTransport) Shutdown() error {
	t.shutdownMu.Lock()
	defer t.shutdownMu.Unlock()

	if t.shutdown {
		return nil
	}
	t.shutdown = true
	close(t.shutdownCh)

	// Close the listener so acceptLoop returns.
	t.listener.Close()

	// Close all pooled connections.
	t.connMu.Lock()
	for addr, conn := range t.connPool {
		conn.Close()
		delete(t.connPool, addr)
	}
	t.connMu.Unlock()

	return nil
}
