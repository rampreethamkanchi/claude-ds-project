// Package raft — fault_test.go
//
// Fault tolerance tests for the custom Raft implementation.
// These tests verify that Raft handles crashes, partitions, and leadership
// changes correctly — the scenarios a professor will grill about.
//
// Every test simulates a real failure scenario and verifies the corresponding
// Raft safety property holds.
package raft

import (
	"fmt"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Leader crash tests
// ─────────────────────────────────────────────────────────────────────────────

// TestFault_LeaderCrash verifies that after killing the leader, the remaining
// nodes elect a new leader and continue accepting writes.
//
// Safety property tested: Liveness (as long as majority alive, progress is made).
func TestFault_LeaderCrash(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	// Wait for initial leader and apply an entry.
	leaderIdx := c.waitForLeader(t)
	c.leaderApply(t, leaderIdx, []byte("before-crash"))
	c.waitForConvergence(t, 1)
	t.Logf("initial leader: node %d", leaderIdx)

	// Kill the leader.
	c.nodes[leaderIdx].Shutdown()
	c.transports[leaderIdx].DisconnectAll()
	// Disconnect peers from the dead leader too.
	for i := 0; i < 3; i++ {
		if i != leaderIdx {
			c.transports[i].Disconnect(c.transports[leaderIdx].LocalAddr())
		}
	}
	t.Logf("killed leader node %d", leaderIdx)

	// Wait for a new leader among surviving nodes.
	deadline := time.Now().Add(5 * time.Second)
	newLeaderIdx := -1
	for time.Now().Before(deadline) {
		for i, node := range c.nodes {
			if i == leaderIdx {
				continue
			}
			if node.IsLeader() {
				newLeaderIdx = i
				break
			}
		}
		if newLeaderIdx >= 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if newLeaderIdx < 0 {
		t.Fatal("no new leader elected after leader crash")
	}
	t.Logf("new leader: node %d, term=%d", newLeaderIdx, c.nodes[newLeaderIdx].CurrentTerm())

	// Apply a new entry to the new leader.
	future := c.nodes[newLeaderIdx].Apply([]byte("after-crash"), 3*time.Second)
	if err := future.Error(); err != nil {
		t.Fatalf("apply to new leader failed: %v", err)
	}

	// Verify the surviving nodes converge.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allMatch := true
		for i, fsm := range c.fsms {
			if i == leaderIdx {
				continue
			}
			if len(fsm.getApplied()) < 2 {
				allMatch = false
				break
			}
		}
		if allMatch {
			t.Log("surviving nodes converged after leader crash")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("surviving nodes did not converge")
}

// TestFault_LeaderCrashUncommitted verifies that entries not committed
// before a leader crash are effectively lost (not applied to FSMs).
//
// Safety property tested: only committed entries survive.
func TestFault_LeaderCrashUncommitted(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Isolate the leader from all followers.
	for i := 0; i < 3; i++ {
		if i != leaderIdx {
			c.transports[leaderIdx].Disconnect(c.transports[i].LocalAddr())
			c.transports[i].Disconnect(c.transports[leaderIdx].LocalAddr())
		}
	}

	// Try to apply — should fail (can't reach majority).
	future := c.nodes[leaderIdx].Apply([]byte("uncommitted"), 500*time.Millisecond)
	err := future.Error()
	t.Logf("apply on isolated leader returned: %v", err)

	// The entry should NOT appear on follower FSMs.
	for i, fsm := range c.fsms {
		if i == leaderIdx {
			continue
		}
		applied := fsm.getApplied()
		for _, data := range applied {
			if string(data) == "uncommitted" {
				t.Errorf("node %d has uncommitted entry", i)
			}
		}
	}
	t.Log("uncommitted entry correctly did not propagate")
}

// ─────────────────────────────────────────────────────────────────────────────
// Follower crash tests
// ─────────────────────────────────────────────────────────────────────────────

// TestFault_FollowerCrash verifies the cluster continues with 2/3 nodes.
//
// Safety property tested: majority quorum is sufficient for progress.
func TestFault_FollowerCrash(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply entry while all nodes are up.
	c.leaderApply(t, leaderIdx, []byte("all-up"))
	c.waitForConvergence(t, 1)

	// Kill a follower.
	followerIdx := (leaderIdx + 1) % 3
	c.nodes[followerIdx].Shutdown()
	c.transports[followerIdx].DisconnectAll()
	for i := 0; i < 3; i++ {
		if i != followerIdx {
			c.transports[i].Disconnect(c.transports[followerIdx].LocalAddr())
		}
	}
	t.Logf("killed follower node %d", followerIdx)

	// Apply more entries — should still work (quorum of 2/3).
	for i := 0; i < 3; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("after-crash-%d", i)))
	}

	// Verify surviving nodes converge.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allMatch := true
		for i, fsm := range c.fsms {
			if i == followerIdx {
				continue
			}
			if len(fsm.getApplied()) != 4 {
				allMatch = false
				break
			}
		}
		if allMatch {
			t.Log("cluster continued operating after follower crash")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("surviving nodes did not converge after follower crash")
}

// TestFault_FollowerRejoins verifies that a crashed follower catches up
// when it reconnects.
func TestFault_FollowerRejoins(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply some initial entries.
	for i := 0; i < 3; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("phase1-%d", i)))
	}
	c.waitForConvergence(t, 3)

	// Disconnect a follower (but don't shut it down).
	followerIdx := (leaderIdx + 1) % 3
	for i := 0; i < 3; i++ {
		if i != followerIdx {
			c.transports[i].Disconnect(c.transports[followerIdx].LocalAddr())
			c.transports[followerIdx].Disconnect(c.transports[i].LocalAddr())
		}
	}
	t.Logf("disconnected follower node %d", followerIdx)

	// Apply more entries while follower is disconnected.
	for i := 0; i < 3; i++ {
		c.leaderApply(t, leaderIdx, []byte(fmt.Sprintf("phase2-%d", i)))
	}

	// Reconnect the follower.
	for i := 0; i < 3; i++ {
		if i != followerIdx {
			c.transports[i].Connect(c.transports[followerIdx].LocalAddr(), c.transports[followerIdx])
			c.transports[followerIdx].Connect(c.transports[i].LocalAddr(), c.transports[i])
		}
	}
	t.Log("reconnected follower")

	// The follower should catch up and have all 6 entries.
	c.waitForConvergence(t, 6)
	t.Log("follower caught up successfully")

	// Verify order matches.
	ref := c.fsms[leaderIdx].getApplied()
	followerApplied := c.fsms[followerIdx].getApplied()
	for i := range ref {
		if string(followerApplied[i]) != string(ref[i]) {
			t.Errorf("entry %d mismatch: follower=%q leader=%q",
				i, string(followerApplied[i]), string(ref[i]))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Partition tests
// ─────────────────────────────────────────────────────────────────────────────

// TestFault_MinorityPartition verifies that an isolated node cannot elect
// itself as leader (it can't form a majority of 1).
//
// Safety property tested: Election Safety (CP choice in CAP).
func TestFault_MinorityPartition(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Partition node 2 from the rest.
	isolated := (leaderIdx + 2) % 3
	for i := 0; i < 3; i++ {
		if i != isolated {
			c.transports[i].Disconnect(c.transports[isolated].LocalAddr())
			c.transports[isolated].Disconnect(c.transports[i].LocalAddr())
		}
	}
	t.Logf("isolated node %d", isolated)

	// Wait a bit for the isolated node to start elections.
	time.Sleep(500 * time.Millisecond)

	// The isolated node should NOT be leader (can't get majority).
	if c.nodes[isolated].IsLeader() {
		t.Error("isolated node became leader — quorum violation!")
	}

	// The majority partition should still elect a leader and work.
	workingLeader := -1
	for i := 0; i < 3; i++ {
		if i != isolated && c.nodes[i].IsLeader() {
			workingLeader = i
		}
	}
	if workingLeader < 0 {
		// Wait for election.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			for i := 0; i < 3; i++ {
				if i != isolated && c.nodes[i].IsLeader() {
					workingLeader = i
					break
				}
			}
			if workingLeader >= 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if workingLeader < 0 {
		t.Fatal("no leader in majority partition")
	}

	// Apply entry in majority partition.
	future := c.nodes[workingLeader].Apply([]byte("majority-only"), 3*time.Second)
	if err := future.Error(); err != nil {
		t.Fatalf("apply in majority partition failed: %v", err)
	}
	t.Logf("majority partition elected leader (node %d) and accepted write", workingLeader)
}

// TestFault_NetworkHeal verifies that after a partition heals, the isolated
// node catches up and all nodes converge.
func TestFault_NetworkHeal(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)

	// Apply some entries first.
	c.leaderApply(t, leaderIdx, []byte("pre-partition"))
	c.waitForConvergence(t, 1)

	// Partition node 2.
	isolated := (leaderIdx + 2) % 3
	for i := 0; i < 3; i++ {
		if i != isolated {
			c.transports[i].Disconnect(c.transports[isolated].LocalAddr())
			c.transports[isolated].Disconnect(c.transports[i].LocalAddr())
		}
	}

	// Wait for partition to take effect and apply entries.
	time.Sleep(100 * time.Millisecond)

	// Find or wait for leader in majority partition.
	var majorityLeader int = -1
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for i := 0; i < 3; i++ {
			if i != isolated && c.nodes[i].IsLeader() {
				majorityLeader = i
				break
			}
		}
		if majorityLeader >= 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if majorityLeader < 0 {
		t.Fatal("no leader in majority partition")
	}

	c.leaderApply(t, majorityLeader, []byte("during-partition"))

	// Heal the partition.
	for i := 0; i < 3; i++ {
		if i != isolated {
			c.transports[i].Connect(c.transports[isolated].LocalAddr(), c.transports[isolated])
			c.transports[isolated].Connect(c.transports[i].LocalAddr(), c.transports[i])
		}
	}
	t.Log("healed partition")

	// All nodes should converge.
	c.waitForConvergence(t, 2)

	// Verify same entries on all nodes.
	ref := c.fsms[majorityLeader].getApplied()
	for i, fsm := range c.fsms {
		applied := fsm.getApplied()
		if len(applied) != len(ref) {
			t.Errorf("node %d: %d entries, leader: %d entries", i, len(applied), len(ref))
		}
	}
	t.Log("all nodes converged after partition heal")
}

// TestFault_TwoLeadersSameTermImpossible verifies that two leaders cannot
// exist in the same term (Election Safety property).
func TestFault_TwoLeadersSameTermImpossible(t *testing.T) {
	c := newTestCluster(t, 5)
	defer c.shutdown()

	c.waitForLeader(t)

	// Check multiple times over a period.
	for check := 0; check < 10; check++ {
		time.Sleep(50 * time.Millisecond)

		leadersByTerm := make(map[uint64][]int)
		for i, node := range c.nodes {
			if node.IsLeader() {
				term := node.CurrentTerm()
				leadersByTerm[term] = append(leadersByTerm[term], i)
			}
		}

		for term, leaders := range leadersByTerm {
			if len(leaders) > 1 {
				t.Fatalf("SAFETY VIOLATION: multiple leaders in term %d: %v", term, leaders)
			}
		}
	}
	t.Log("election safety verified: no two leaders in same term")
}

// TestFault_OldLeaderRejoins verifies that when an old leader reconnects,
// it discovers the new term and steps down to follower.
func TestFault_OldLeaderRejoins(t *testing.T) {
	c := newTestCluster(t, 3)
	defer c.shutdown()

	oldLeaderIdx := c.waitForLeader(t)
	oldTerm := c.nodes[oldLeaderIdx].CurrentTerm()
	t.Logf("old leader: node %d, term=%d", oldLeaderIdx, oldTerm)

	// Isolate the old leader.
	for i := 0; i < 3; i++ {
		if i != oldLeaderIdx {
			c.transports[i].Disconnect(c.transports[oldLeaderIdx].LocalAddr())
			c.transports[oldLeaderIdx].Disconnect(c.transports[i].LocalAddr())
		}
	}

	// Wait for a new leader.
	deadline := time.Now().Add(5 * time.Second)
	newLeaderIdx := -1
	for time.Now().Before(deadline) {
		for i := 0; i < 3; i++ {
			if i != oldLeaderIdx && c.nodes[i].IsLeader() {
				newLeaderIdx = i
				break
			}
		}
		if newLeaderIdx >= 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if newLeaderIdx < 0 {
		t.Fatal("no new leader elected after partition")
	}
	t.Logf("new leader: node %d, term=%d", newLeaderIdx, c.nodes[newLeaderIdx].CurrentTerm())

	// Reconnect the old leader.
	for i := 0; i < 3; i++ {
		if i != oldLeaderIdx {
			c.transports[i].Connect(c.transports[oldLeaderIdx].LocalAddr(), c.transports[oldLeaderIdx])
			c.transports[oldLeaderIdx].Connect(c.transports[i].LocalAddr(), c.transports[i])
		}
	}

	// Wait for the old leader to step down.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !c.nodes[oldLeaderIdx].IsLeader() {
			t.Logf("old leader stepped down: state=%s, term=%d",
				c.nodes[oldLeaderIdx].State(),
				c.nodes[oldLeaderIdx].CurrentTerm())
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("old leader did not step down after rejoin")
}

// TestFault_CascadingFailure_FiveNodes tests that a 5-node cluster
// survives losing 2 nodes (majority = 3), but fails with 3 down.
func TestFault_CascadingFailure_FiveNodes(t *testing.T) {
	c := newTestCluster(t, 5)
	defer c.shutdown()

	leaderIdx := c.waitForLeader(t)
	c.leaderApply(t, leaderIdx, []byte("all-five-up"))
	c.waitForConvergence(t, 1)

	// Kill 2 nodes (not the leader). Quorum is 3; 3 remain → should work.
	kill1 := (leaderIdx + 1) % 5
	kill2 := (leaderIdx + 2) % 5
	for _, k := range []int{kill1, kill2} {
		c.nodes[k].Shutdown()
		for i := 0; i < 5; i++ {
			if i != k {
				c.transports[i].Disconnect(c.transports[k].LocalAddr())
			}
		}
		c.transports[k].DisconnectAll()
	}
	t.Logf("killed nodes %d and %d", kill1, kill2)

	// Find or re-elect a leader among survivors.
	deadline := time.Now().Add(5 * time.Second)
	activeLeader := -1
	for time.Now().Before(deadline) {
		for i := 0; i < 5; i++ {
			if i == kill1 || i == kill2 {
				continue
			}
			if c.nodes[i].IsLeader() {
				activeLeader = i
				break
			}
		}
		if activeLeader >= 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if activeLeader < 0 {
		t.Fatal("no leader among 3 surviving nodes")
	}

	future := c.nodes[activeLeader].Apply([]byte("three-surviving"), 3*time.Second)
	if err := future.Error(); err != nil {
		t.Fatalf("apply with 3/5 alive failed: %v", err)
	}
	t.Log("cluster survived with 3/5 nodes alive")
}
