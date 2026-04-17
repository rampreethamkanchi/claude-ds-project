// Package raft — inmem.go
//
// In-memory implementations of LogStore, StableStore, and Transport.
// These are used exclusively in tests to avoid disk I/O and real TCP sockets,
// making tests fast (milliseconds) and deterministic.
//
// The InmemTransport connects nodes directly via Go channels — no serialization,
// no network. It also supports Disconnect/Reconnect for partition simulation.
package raft

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// InmemLogStore
// ─────────────────────────────────────────────────────────────────────────────

// InmemLogStore stores log entries in memory. Not crash-safe — for tests only.
type InmemLogStore struct {
	mu      sync.RWMutex
	entries map[uint64]*LogEntry // index → entry
	lowIdx  uint64              // first index (0 if empty)
	highIdx uint64              // last index (0 if empty)
}

// NewInmemLogStore creates a new in-memory log store.
func NewInmemLogStore() *InmemLogStore {
	return &InmemLogStore{
		entries: make(map[uint64]*LogEntry),
	}
}

func (s *InmemLogStore) FirstIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lowIdx, nil
}

func (s *InmemLogStore) LastIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.highIdx, nil
}

func (s *InmemLogStore) GetLog(index uint64) (*LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[index]
	if !ok {
		return nil, fmt.Errorf("log entry %d not found", index)
	}
	// Return a copy to prevent mutation.
	cp := *entry
	cp.Data = make([]byte, len(entry.Data))
	copy(cp.Data, entry.Data)
	return &cp, nil
}

func (s *InmemLogStore) StoreLog(entry *LogEntry) error {
	return s.StoreLogs([]*LogEntry{entry})
}

func (s *InmemLogStore) StoreLogs(entries []*LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		// Store a copy to prevent external mutation.
		cp := *e
		cp.Data = make([]byte, len(e.Data))
		copy(cp.Data, e.Data)
		s.entries[e.Index] = &cp

		if s.lowIdx == 0 || e.Index < s.lowIdx {
			s.lowIdx = e.Index
		}
		if e.Index > s.highIdx {
			s.highIdx = e.Index
		}
	}
	return nil
}

func (s *InmemLogStore) DeleteRange(min, max uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := min; i <= max; i++ {
		delete(s.entries, i)
	}
	// Recalculate lowIdx and highIdx.
	s.lowIdx = 0
	s.highIdx = 0
	for idx := range s.entries {
		if s.lowIdx == 0 || idx < s.lowIdx {
			s.lowIdx = idx
		}
		if idx > s.highIdx {
			s.highIdx = idx
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// InmemStableStore
// ─────────────────────────────────────────────────────────────────────────────

// InmemStableStore stores key-value pairs in memory. For tests only.
type InmemStableStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewInmemStableStore creates a new in-memory stable store.
func NewInmemStableStore() *InmemStableStore {
	return &InmemStableStore{
		data: make(map[string][]byte),
	}
}

func (s *InmemStableStore) Set(key []byte, val []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	s.data[string(key)] = cp
	return nil
}

func (s *InmemStableStore) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[string(key)]
	if !ok {
		return nil, nil
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (s *InmemStableStore) SetUint64(key []byte, val uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, val)
	return s.Set(key, buf)
}

func (s *InmemStableStore) GetUint64(key []byte) (uint64, error) {
	val, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if len(val) == 0 {
		return 0, nil
	}
	return binary.BigEndian.Uint64(val), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// InmemTransport
// ─────────────────────────────────────────────────────────────────────────────

// InmemTransport connects Raft nodes directly via Go channels.
// It supports simulating network partitions via Disconnect/Reconnect.
type InmemTransport struct {
	localAddr string
	rpcCh     chan *RPC // incoming RPCs from other nodes

	mu       sync.RWMutex
	peers    map[string]*InmemTransport // connected peers
	shutdown bool
}

// NewInmemTransport creates a new in-memory transport with the given address.
func NewInmemTransport(addr string) *InmemTransport {
	return &InmemTransport{
		localAddr: addr,
		rpcCh:     make(chan *RPC, 256),
		peers:     make(map[string]*InmemTransport),
	}
}

// Consumer returns the channel that delivers incoming RPCs to the Raft node.
func (t *InmemTransport) Consumer() <-chan *RPC {
	return t.rpcCh
}

// LocalAddr returns this transport's address.
func (t *InmemTransport) LocalAddr() string {
	return t.localAddr
}

// Connect adds a peer transport so RPCs can be sent to it.
func (t *InmemTransport) Connect(addr string, peer *InmemTransport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peers[addr] = peer
}

// Disconnect removes a peer, simulating a network partition.
func (t *InmemTransport) Disconnect(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peers, addr)
}

// DisconnectAll removes all peers (full isolation).
func (t *InmemTransport) DisconnectAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peers = make(map[string]*InmemTransport)
}

// SendAppendEntries sends an AppendEntries RPC to the target peer.
// It delivers the request directly to the peer's rpcCh channel and waits
// for the response — no serialization, no network.
func (t *InmemTransport) SendAppendEntries(target string, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	t.mu.RLock()
	peer, ok := t.peers[target]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("inmem_transport: peer %q not connected (partition?)", target)
	}

	// Create an RPC and send it to the peer's channel.
	respCh := make(chan RPCResponse, 1)
	rpc := &RPC{Command: req, RespCh: respCh}

	select {
	case peer.rpcCh <- rpc:
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("inmem_transport: timeout sending AppendEntries to %s", target)
	}

	// Wait for the response.
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Response.(*AppendEntriesResponse), nil
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("inmem_transport: timeout waiting for AppendEntries response from %s", target)
	}
}

// SendRequestVote sends a RequestVote RPC to the target peer.
func (t *InmemTransport) SendRequestVote(target string, req *RequestVoteRequest) (*RequestVoteResponse, error) {
	t.mu.RLock()
	peer, ok := t.peers[target]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("inmem_transport: peer %q not connected (partition?)", target)
	}

	respCh := make(chan RPCResponse, 1)
	rpc := &RPC{Command: req, RespCh: respCh}

	select {
	case peer.rpcCh <- rpc:
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("inmem_transport: timeout sending RequestVote to %s", target)
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Response.(*RequestVoteResponse), nil
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("inmem_transport: timeout waiting for RequestVote response from %s", target)
	}
}

// Shutdown closes the transport.
func (t *InmemTransport) Shutdown() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shutdown = true
	t.peers = make(map[string]*InmemTransport)
	return nil
}
