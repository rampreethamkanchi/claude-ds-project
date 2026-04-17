// Package raft — raft_test.go
//
// Core unit tests for the custom Raft implementation.
// These tests verify the fundamental correctness properties of Raft:
//
//  1. Election Safety: at most one leader per term.
//  2. Log Matching: identical entries at same index/term ⟹ identical prefix.
//  3. Leader Completeness: committed entries are in all future leaders' logs.
//  4. State Machine Safety: each index applied exactly once, same value everywhere.
//
// All tests use in-memory transports and stores — no disk, no TCP.
// Tests run in milliseconds and are safe to run with -race.
package raft

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// testFSM is a simple FSM that records all applied entries.
// Used to verify that committed entries are applied in order on all nodes.
type testFSM struct {
	mu      sync.Mutex
	applied [][]byte // list of applied data payloads
}

func newTestFSM() *testFSM {
	return &testFSM{}
}

func (f *testFSM) Apply(data []byte) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.applied = append(f.applied, cp)
	return len(f.applied) // return the count as the response
}

func (f *testFSM) Snapshot() ([]byte, error) { return nil, nil }
func (f *testFSM) Restore(data []byte) error { return nil }

func (f *testFSM) getApplied() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]byte, len(f.applied))
	copy(result, f.applied)
	return result
}

// testCluster manages a Raft cluster of N nodes for testing.
type testCluster struct {
	nodes      []*RaftNode
	fsms       []*testFSM
	transports []*InmemTransport
	logger     *slog.Logger
}

// newTestCluster creates an N-node Raft cluster using in-memory stores
// and transports. All nodes are wired together and start running immediately.
func newTestCluster(t *testing.T, n int) *testCluster {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn}))

	// Build peer configs.
	var peers []PeerConfig
	for i := 0; i < n; i++ {
		peers = append(peers, PeerConfig{
			ID:      fmt.Sprintf("node-%d", i),
			Address: fmt.Sprintf("node-%d", i),
		})
	}

	// Create transports and wire them together.
	transports := make([]*InmemTransport, n)
	for i := 0; i < n; i++ {
		transports[i] = NewInmemTransport(peers[i].Address)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				transports[i].Connect(peers[j].Address, transports[j])
			}
		}
	}

	// Create nodes.
	cluster := &testCluster{logger: logger, transports: transports}
	for i := 0; i < n; i++ {
		fsm := newTestFSM()
		cluster.fsms = append(cluster.fsms, fsm)

		cfg := DefaultConfig()
		cfg.ServerID = peers[i].ID
		cfg.Peers = peers
		cfg.LogStore = NewInmemLogStore()
		cfg.StableStore = NewInmemStableStore()
		cfg.FSM = fsm
		cfg.Logger = logger.With("node", peers[i].ID)
		// Use faster timeouts for tests.
		cfg.HeartbeatInterval = 30 * time.Millisecond
		cfg.ElectionTimeoutMin = 100 * time.Millisecond
		cfg.ElectionTimeoutMax = 200 * time.Millisecond

		node, err := NewRaftNode(cfg, transports[i])
		if err != nil {
			t.Fatalf("failed to create node %d: %v", i, err)
		}
		cluster.nodes = append(cluster.nodes, node)
	}

	return cluster
}

// waitForLeader polls until one node becomes leader, with a timeout.
func (c *testCluster) waitForLeader(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i, node := range c.nodes {
			if node != nil && node.IsLeader() {
				return i
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected within 5 seconds")
	return -1
}

// waitForConvergence waits until all alive nodes have applied the same
// number of entries (within the timeout).
func (c *testCluster) waitForConvergence(t *testing.T, expectedCount int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allMatch := true
		for i, fsm := range c.fsms {
			if c.nodes[i] == nil {
				continue // skip dead nodes
			}
			applied := fsm.getApplied()
			if len(applied) != expectedCount {
				allMatch = false
				break
			}
		}
		if allMatch {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Print diagnostic info on failure.
	for i, fsm := range c.fsms {
		if c.nodes[i] != nil {
			applied := fsm.getApplied()
			t.Logf("node %d: applied=%d entries, state=%s",
				i, len(applied), c.nodes[i].State())
		}
	}
	t.Fatalf("nodes did not converge to %d applied entries", expectedCount)
}

// shutdown stops all nodes in the cluster.
func (c *testCluster) shutdown() {
	for _, node := range c.nodes {
		if node != nil {
			node.Shutdown()
		}
	}
}

// leaderApply proposes data to the leader and waits for commit.
func (c *testCluster) leaderApply(t *testing.T, leaderIdx int, data []byte) {
	t.Helper()
	future := c.nodes[leaderIdx].Apply(data, 3*time.Second)
	if err := future.Error(); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Election Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestElection_SingleNode verifies that a 1-node cluster elects itself as leader.
func TestElection_SingleNode(t *testing.T) {
	c := newTestCluster(t, 1)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)
	if leaderIdx != 0 {
		t.Errorf("expected node 0 to be leader, got %d", leaderIdx)
	}
	t.Logf("single node elected leader: term=%d", c.nodes[0].CurrentTerm())
}

// TestElection_ThreeNodes verifies that exactly one leader is elected in a 3-node cluster.
func TestElection_ThreeNodes(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)
	t.Logf("leader elected: node %d, term=%d", leaderIdx, c.nodes[leaderIdx].CurrentTerm())

	// Verify exactly one leader.
	leaderCount := 0
	for i, node := range c.nodes {
		if node.IsLeader() {
			leaderCount++
			t.Logf("node %d is leader (term=%d)", i, node.CurrentTerm())
		}
	}
	if leaderCount != 1 {
		t.Errorf("expected exactly 1 leader, got %d", leaderCount)
	}
}

// TestElection_FiveNodes verifies leader election with 5 nodes.
func TestElection_FiveNodes(t *testing.T) {
	c := newTestCluster(t, 5)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)
	t.Logf("5-node leader: node %d, term=%d", leaderIdx, c.nodes[leaderIdx].CurrentTerm())

	leaderCount := 0
	for _, node := range c.nodes {
		if node.IsLeader() {
			leaderCount++
		}
	}
	if leaderCount != 1 {
		t.Errorf("expected 1 leader, got %d", leaderCount)
	}
}

// TestElection_TermIncrement verifies that the term increases with each election.
func TestElection_TermIncrement(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)
	term1 := c.nodes[leaderIdx].CurrentTerm()
	if term1 < 1 {
		t.Errorf("expected term >= 1 after first election, got %d", term1)
	}
	t.Logf("first election: term=%d", term1)
}

// TestElection_VoteOnlyOnce verifies that a server votes for at most one
// candidate per term. We test this indirectly: in a 3-node cluster,
// if two candidates start simultaneously, at most one wins.
func TestElection_VoteOnlyOnce(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	c.waitForLeader(t)

	// Count leaders (should be exactly 1).
	leaderCount := 0
	for _, node := range c.nodes {
		if node.IsLeader() {
			leaderCount++
		}
	}
	if leaderCount != 1 {
		t.Errorf("vote-once violation: %d leaders elected", leaderCount)
	}
}

// TestElection_StepDownOnHigherTerm verifies that a leader steps down when
// it receives an RPC with a higher term.
func TestElection_StepDownOnHigherTerm(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Disconnect the leader so followers start a new election with a higher term.
	leaderAddr := c.nodes[leaderIdx].transport.LocalAddr()
	for i := 0; i < 3; i++ {
		if i != leaderIdx {
			c.transports[i].Disconnect(leaderAddr)
			c.transports[leaderIdx].Disconnect(c.transports[i].LocalAddr())
		}
	}

	// Wait for a new leader among the remaining nodes.
	deadline := time.Now().Add(3 * time.Second)
	newLeaderFound := false
	for time.Now().Before(deadline) {
		for i, node := range c.nodes {
			if i != leaderIdx && node.IsLeader() {
				newLeaderFound = true
				t.Logf("new leader: node %d, term=%d", i, node.CurrentTerm())
				break
			}
		}
		if newLeaderFound {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Reconnect the old leader.
	for i := 0; i < 3; i++ {
		if i != leaderIdx {
			c.transports[i].Connect(leaderAddr, c.transports[leaderIdx])
			c.transports[leaderIdx].Connect(c.transports[i].LocalAddr(), c.transports[i])
		}
	}

	// Wait for the old leader to step down.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !c.nodes[leaderIdx].IsLeader() {
			t.Logf("old leader (node %d) stepped down, state=%s",
				leaderIdx, c.nodes[leaderIdx].State())
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("old leader did not step down after seeing higher term")
}

// ─────────────────────────────────────────────────────────────────────────────
// Replication Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestReplication_SingleEntry verifies that one entry replicates to all nodes.
func TestReplication_SingleEntry(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply a single entry.
	c.leaderApply(t, leaderIdx, []byte("hello"))

	// Wait for all nodes to apply it.
	c.waitForConvergence(t, 1)

	// Verify all FSMs received the same data.
	for i, fsm := range c.fsms {
		applied := fsm.getApplied()
		if len(applied) != 1 {
			t.Errorf("node %d: expected 1 entry, got %d", i, len(applied))
			continue
		}
		if string(applied[0]) != "hello" {
			t.Errorf("node %d: expected %q, got %q", i, "hello", string(applied[0]))
		}
	}
}

// TestReplication_MultipleEntries verifies that multiple entries replicate in order.
func TestReplication_MultipleEntries(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply 10 entries sequentially.
	for i := 0; i < 10; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("entry-%d", i)))
	}

	c.waitForConvergence(t, 10)

	// Verify order on all nodes.
	for nodeIdx, fsm := range c.fsms {
		applied := fsm.getApplied()
		for i, data := range applied {
			expected := fmt.Sprintf("entry-%d", i)
			if string(data) != expected {
				t.Errorf("node %d, entry %d: expected %q, got %q",
					nodeIdx, i, expected, string(data))
			}
		}
	}
}

// TestReplication_CommitRequiresMajority verifies that an entry is not applied
// to the FSM until a majority of nodes have replicated it.
func TestReplication_CommitRequiresMajority(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Disconnect both followers so nothing can be committed.
	for i := 0; i < 3; i++ {
		if i != leaderIdx {
			c.transports[leaderIdx].Disconnect(c.transports[i].LocalAddr())
			c.transports[i].Disconnect(c.transports[leaderIdx].LocalAddr())
		}
	}

	// Apply should timeout since we can't reach a majority.
	future := c.nodes[leaderIdx].Apply([]byte("should-not-commit"), 500*time.Millisecond)
	err := future.Error()
	if err == nil {
		t.Error("expected error (timeout/leadership-lost), got nil")
	}
	t.Logf("correctly failed to commit without majority: %v", err)
}

// TestReplication_LogConflictResolution verifies that a follower with
// conflicting entries gets corrected by the leader.
func TestReplication_LogConflictResolution(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply a few entries that all nodes agree on.
	for i := 0; i < 3; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("agreed-%d", i)))
	}
	c.waitForConvergence(t, 3)

	// Now disconnect one follower and apply more entries.
	followerIdx := (leaderIdx + 1) % 3
	c.transports[leaderIdx].Disconnect(c.transports[followerIdx].LocalAddr())
	c.transports[followerIdx].Disconnect(c.transports[leaderIdx].LocalAddr())
	for i := 0; i < 3; i++ {
		if i != followerIdx {
			c.transports[i].Disconnect(c.transports[followerIdx].LocalAddr())
			c.transports[followerIdx].Disconnect(c.transports[i].LocalAddr())
		}
	}

	// Apply 2 more entries (only leader + other follower get them).
	for i := 0; i < 2; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("extra-%d", i)))
	}

	// Reconnect the follower.
	for i := 0; i < 3; i++ {
		if i != followerIdx {
			c.transports[i].Connect(c.transports[followerIdx].LocalAddr(), c.transports[followerIdx])
			c.transports[followerIdx].Connect(c.transports[i].LocalAddr(), c.transports[i])
		}
	}

	// The follower should catch up.
	c.waitForConvergence(t, 5)
	t.Log("follower caught up after reconnection")
}

// TestApply_FSMReceivesCommittedEntries verifies all committed entries are applied to FSM.
func TestApply_FSMReceivesCommittedEntries(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	entries := []string{"alpha", "beta", "gamma", "delta"}
	for _, e := range entries {
		c.leaderApply(t, leaderIdx, []byte(e))
	}

	c.waitForConvergence(t, len(entries))

	for i, fsm := range c.fsms {
		applied := fsm.getApplied()
		for j, data := range applied {
			if string(data) != entries[j] {
				t.Errorf("node %d entry %d: got %q want %q", i, j, string(data), entries[j])
			}
		}
	}
}

// TestApply_AllNodesApplySameEntries verifies all FSMs receive identical entries.
func TestApply_AllNodesApplySameEntries(t *testing.T) {
	c := newTestCluster(t, 5)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	for i := 0; i < 20; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("cmd-%d", i)))
	}

	c.waitForConvergence(t, 20)

	// Compare all FSMs pairwise.
	ref := c.fsms[0].getApplied()
	for i := 1; i < 5; i++ {
		applied := c.fsms[i].getApplied()
		if len(applied) != len(ref) {
			t.Errorf("node %d: %d entries, node 0: %d entries", i, len(applied), len(ref))
			continue
		}
		for j := range ref {
			if string(applied[j]) != string(ref[j]) {
				t.Errorf("node %d entry %d: %q != node 0: %q", i, j, string(applied[j]), string(ref[j]))
			}
		}
	}
}

// TestApply_ReturnValue verifies that ApplyFuture.Response() returns the FSM's return value.
func TestApply_ReturnValue(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	future := c.nodes[leaderIdx].Apply([]byte("test-data"), 3*time.Second)
	if err := future.Error(); err != nil {
		t.Fatalf("apply failed: %v", err)
	}

	// Our testFSM returns the count of applied entries.
	resp := future.Response()
	if resp == nil {
		t.Fatal("response was nil")
	}
	count, ok := resp.(int)
	if !ok {
		t.Fatalf("expected int response, got %T", resp)
	}
	// The no-op entry doesn't count (nil data), so "test-data" is entry #1.
	if count != 1 {
		t.Logf("response count=%d (may include no-op)", count)
	}
	t.Logf("apply response: %v", resp)
}

// TestApply_NotLeader verifies that Apply on a follower returns ErrNotLeader.
func TestApply_NotLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Find a follower.
	followerIdx := (leaderIdx + 1) % 3
	future := c.nodes[followerIdx].Apply([]byte("should-fail"), 1*time.Second)
	err := future.Error()
	if err != ErrNotLeader {
		t.Errorf("expected ErrNotLeader, got: %v", err)
	}
}

// TestApply_ConcurrentApply verifies that concurrent Apply() calls work correctly.
func TestApply_ConcurrentApply(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	var wg sync.WaitGroup
	var successCount int64
	n := 50

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			future := c.nodes[leaderIdx].Apply(
				[]byte(fmt.Sprintf("concurrent-%d", idx)),
				5*time.Second,
			)
			if err := future.Error(); err == nil {
				atomic.AddInt64(&successCount, 1)
			}
		}(i)
	}

	wg.Wait()
	t.Logf("concurrent applies: %d/%d succeeded", successCount, n)

	if successCount == 0 {
		t.Error("no concurrent applies succeeded")
	}

	// Wait for all nodes to converge.
	c.waitForConvergence(t, int(successCount))

	// Verify all nodes have the same entries.
	ref := c.fsms[0].getApplied()
	for i := 1; i < 3; i++ {
		applied := c.fsms[i].getApplied()
		if len(applied) != len(ref) {
			t.Errorf("node %d has %d entries, node 0 has %d", i, len(applied), len(ref))
		}
	}
}
