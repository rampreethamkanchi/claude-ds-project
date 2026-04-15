// tests/cluster_test.go
//
// In-memory 3-node Raft cluster tests.
//
// These tests spin up a full 3-node cluster using hashicorp/raft's InmemTransport
// so no real TCP sockets are needed. Tests run in milliseconds.
//
// Key assertions:
//  1. A changeset submitted to the leader is applied on ALL 3 nodes.
//  2. All nodes agree on head_text and head_rev after concurrent submissions.
//  3. After killing the leader, the cluster elects a new leader and continues.
package tests

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/ot"

	"github.com/hashicorp/raft"
)

// ─────────────────────────────────────────────
// Helper: build an in-memory 3-node cluster
// ─────────────────────────────────────────────

type memCluster struct {
	nodes  []*raft.Raft
	fsms   []*fsm.DocumentStateMachine
	trans  []*raft.InmemTransport
	logger *slog.Logger
}

// newMemCluster creates a 3-node in-memory Raft cluster, all sharing the same
// initial document text.
func newMemCluster(t *testing.T, initialText string) *memCluster {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const n = 3
	cluster := &memCluster{logger: logger}

	addrs := [n]raft.ServerAddress{"node-0", "node-1", "node-2"}
	var rafts [n]*raft.Raft
	var trans [n]*raft.InmemTransport

	// Create transports.
	for i := 0; i < n; i++ {
		_, trans[i] = raft.NewInmemTransport(addrs[i])
		cluster.trans = append(cluster.trans, trans[i])
	}

	// Wire transports together so they can talk to each other.
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				trans[i].Connect(addrs[j], trans[j])
			}
		}
	}

	// Build the initial cluster configuration.
	var servers []raft.Server
	for i := 0; i < n; i++ {
		servers = append(servers, raft.Server{
			Suffrage: raft.Voter,
			ID:       raft.ServerID(addrs[i]),
			Address:  addrs[i],
		})
	}
	configuration := raft.Configuration{Servers: servers}

	// Create each Raft node with an in-memory log/stable store.
	for i := 0; i < n; i++ {
		nodeLogger := logger.With("node", addrs[i])
		sm := fsm.NewDocumentStateMachine(initialText, nodeLogger)
		cluster.fsms = append(cluster.fsms, sm)

		cfg := raft.DefaultConfig()
		cfg.LocalID = raft.ServerID(addrs[i])
		cfg.HeartbeatTimeout = 50 * time.Millisecond
		cfg.ElectionTimeout = 100 * time.Millisecond
		cfg.CommitTimeout = 10 * time.Millisecond
		cfg.LeaderLeaseTimeout = 50 * time.Millisecond

		logStore := raft.NewInmemStore()
		stableStore := raft.NewInmemStore()
		snapStore := raft.NewInmemSnapshotStore()

		r, err := raft.NewRaft(cfg, sm, logStore, stableStore, snapStore, trans[i])
		if err != nil {
			t.Fatalf("node %d: NewRaft: %v", i, err)
		}

		// Bootstrap on node 0 only.
		if i == 0 {
			if fut := r.BootstrapCluster(configuration); fut.Error() != nil {
				t.Fatalf("bootstrap: %v", fut.Error())
			}
		}

		rafts[i] = r
		cluster.nodes = append(cluster.nodes, r)
	}

	return cluster
}

// waitForLeader polls until one node becomes leader or times out.
func (c *memCluster) waitForLeader(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i, r := range c.nodes {
			if r.State() == raft.Leader {
				return i
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no leader elected within 5 seconds")
	return -1
}

// applyEntry proposes a changeset to the given Raft node and waits for commit.
func applyEntry(t *testing.T, r *raft.Raft, entry fsm.RaftLogEntry) {
	t.Helper()
	data, err := fsm.MarshalEntry(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	fut := r.Apply(data, 2*time.Second)
	if err := fut.Error(); err != nil {
		t.Fatalf("raft apply: %v", err)
	}
}

// waitForConvergence polls until all FSMs report the same head_text.
func (c *memCluster) waitForConvergence(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		texts := make([]string, len(c.fsms))
		for i, sm := range c.fsms {
			texts[i] = sm.HeadText()
		}
		allSame := true
		for _, tx := range texts[1:] {
			if tx != texts[0] {
				allSame = false
				break
			}
		}
		if allSame {
			return texts[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	texts := make([]string, len(c.fsms))
	for i, sm := range c.fsms {
		texts[i] = sm.HeadText()
	}
	t.Fatalf("nodes did not converge:\n  node0=%q\n  node1=%q\n  node2=%q",
		texts[0], texts[1], texts[2])
	return ""
}

// shutdown shuts down all nodes.
func (c *memCluster) shutdown() {
	for _, r := range c.nodes {
		r.Shutdown()
	}
}

// ─────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────

// TestCluster_SingleWrite verifies that one write replicates to all 3 nodes.
func TestCluster_SingleWrite(t *testing.T) {
	cluster := newMemCluster(t, "")
	defer cluster.shutdown()

	leaderIdx := cluster.waitForLeader(t)
	leader := cluster.nodes[leaderIdx]

	// Submit: insert "hello" into the empty document.
	entry := fsm.RaftLogEntry{
		ClientID:     "client-A",
		SubmissionID: 1,
		BaseRev:      0,
		Changeset:    ot.MakeInsert(0, 0, "hello"),
	}
	applyEntry(t, leader, entry)

	// All three nodes must converge to "hello".
	text := cluster.waitForConvergence(t)
	if text != "hello" {
		t.Errorf("expected %q, got %q", "hello", text)
	}
	t.Logf("converged to: %q (rev=%d)", text, cluster.fsms[0].HeadRev())
}

// TestCluster_ConcurrentWrites verifies convergence after simultaneous submissions
// from two different clients based on the same base_rev.
func TestCluster_ConcurrentWrites(t *testing.T) {
	cluster := newMemCluster(t, "hello")
	defer cluster.shutdown()

	leaderIdx := cluster.waitForLeader(t)
	leader := cluster.nodes[leaderIdx]

	// Client A: append " world" (base_rev=0, "hello" → "hello world")
	entryA := fsm.RaftLogEntry{
		ClientID:     "client-A",
		SubmissionID: 1,
		BaseRev:      0,
		Changeset:    ot.MakeInsert(5, 5, " world"),
	}
	applyEntry(t, leader, entryA)

	// Client B: delete "hello" and replace with "Hi" — based on rev 0 as well.
	del := ot.MakeDelete(5, 0, 5)
	ins, _ := ot.Compose(del, ot.MakeInsert(0, 0, "Hi"))
	entryB := fsm.RaftLogEntry{
		ClientID:     "client-B",
		SubmissionID: 1,
		BaseRev:      0,
		Changeset:    ins,
	}
	applyEntry(t, leader, entryB)

	// Both must converge to the same text (exact value depends on OT resolution).
	text := cluster.waitForConvergence(t)
	t.Logf("concurrent writes converged to: %q", text)

	// Verify the document is non-empty and consistent.
	for i, sm := range cluster.fsms {
		if sm.HeadText() != text {
			t.Errorf("node %d diverged: got %q, want %q", i, sm.HeadText(), text)
		}
		if sm.HeadRev() != 2 {
			t.Errorf("node %d: expected rev=2, got %d", i, sm.HeadRev())
		}
	}
}

// TestCluster_Deduplication verifies that re-submitting the same entry (same
// client_id + submission_id) only applies it once.
func TestCluster_Deduplication(t *testing.T) {
	cluster := newMemCluster(t, "")
	defer cluster.shutdown()

	leaderIdx := cluster.waitForLeader(t)
	leader := cluster.nodes[leaderIdx]

	entry := fsm.RaftLogEntry{
		ClientID:     "client-A",
		SubmissionID: 42,
		BaseRev:      0,
		Changeset:    ot.MakeInsert(0, 0, "hello"),
	}

	// Apply same entry twice (simulating a reconnect re-submission).
	applyEntry(t, leader, entry)
	applyEntry(t, leader, entry) // second apply — should be dedup'd

	cluster.waitForConvergence(t)

	// The document should contain "hello" exactly once.
	for i, sm := range cluster.fsms {
		if sm.HeadText() != "hello" {
			t.Errorf("node %d: expected %q, got %q", i, "hello", sm.HeadText())
		}
	}
}

// TestCluster_LeaderFailover simulates killing the leader and verifies that
// the cluster elects a new leader and continues accepting writes.
func TestCluster_LeaderFailover(t *testing.T) {
	cluster := newMemCluster(t, "start")
	defer cluster.shutdown()

	// Wait for initial leader.
	leaderIdx := cluster.waitForLeader(t)
	t.Logf("initial leader: node %d", leaderIdx)

	// Apply one entry to initial leader.
	entry1 := fsm.RaftLogEntry{
		ClientID: "client-A", SubmissionID: 1,
		BaseRev: 0, Changeset: ot.MakeInsert(5, 5, " v1"),
	}
	applyEntry(t, cluster.nodes[leaderIdx], entry1)
	cluster.waitForConvergence(t)

	// Kill the leader.
	cluster.nodes[leaderIdx].Shutdown()
	t.Logf("killed leader node %d", leaderIdx)

	// Wait for a new leader to be elected from the remaining 2 nodes.
	var newLeader *raft.Raft
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i, r := range cluster.nodes {
			if i == leaderIdx {
				continue // skip the dead node
			}
			if r.State() == raft.Leader {
				newLeader = r
				t.Logf("new leader elected: node %d", i)
				break
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if newLeader == nil {
		t.Fatal("new leader not elected within 5 seconds")
	}

	// Apply a second entry to the new leader.
	entry2 := fsm.RaftLogEntry{
		ClientID: "client-B", SubmissionID: 1,
		BaseRev: 1, Changeset: ot.MakeInsert(8, 8, " v2"),
	}
	applyEntry(t, newLeader, entry2)

	// The two surviving nodes must converge.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		texts := []string{}
		for i, sm := range cluster.fsms {
			if i == leaderIdx {
				continue
			}
			texts = append(texts, sm.HeadText())
		}
		if len(texts) == 2 && texts[0] == texts[1] {
			t.Logf("surviving nodes converged to: %q", texts[0])
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	texts := []string{}
	for i, sm := range cluster.fsms {
		if i != leaderIdx {
			texts = append(texts, sm.HeadText())
		}
	}
	t.Errorf("surviving nodes did not converge: %v", texts)
}
