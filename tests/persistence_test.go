// tests/persistence_test.go
//
// Tests for session persistence across refreshes and server-side ACK for duplicates.
package tests

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/ot"


)

// TestPersistence_DuplicateAck verifies that re-submitting an already applied entry
// returns an ApplyResult with IsDuplicate=true so the server can still ACK.
func TestPersistence_DuplicateAck(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sm := fsm.NewDocumentStateMachine("", logger)

	// Create a dummy Raft log with some data.
	entry := fsm.RaftLogEntry{
		ClientID:     "client-1",
		SubmissionID: 5,
		BaseRev:      0,
		Changeset:    ot.MakeInsert(0, 0, "hello"),
	}
	data, _ := fsm.MarshalEntry(entry)

	// First application.
	res1Raw := sm.Apply(data)
	res1 := res1Raw.(*fsm.ApplyResult)
	if res1.IsDuplicate {
		t.Fatal("first apply should not be a duplicate")
	}
	if sm.HeadRev() != 1 {
		t.Errorf("expected rev 1, got %d", sm.HeadRev())
	}

	// Second (duplicate) application.
	res2Raw := sm.Apply(data)
	if res2Raw == nil {
		t.Fatal("duplicate apply returned nil, expected non-nil ApplyResult")
	}
	res2 := res2Raw.(*fsm.ApplyResult)
	if !res2.IsDuplicate {
		t.Error("second apply should have IsDuplicate=true")
	}
	if res2.NewRev != 1 {
		t.Errorf("expected duplicate ACK to report current rev 1, got %d", res2.NewRev)
	}
	if sm.HeadRev() != 1 {
		t.Errorf("duplicate apply should not increment head rev, got %d", sm.HeadRev())
	}
}

// TestPersistence_ClientSequenceOverrun verifies the server's behavior when a client
// reuses an ID but restarts its sequence (simulating a refresh with lost submission state).
func TestPersistence_ClientSequenceOverrun(t *testing.T) {
	cluster := newMemCluster(t, "")
	defer cluster.shutdown()

	leaderIdx := cluster.waitForLeader(t)
	leader := cluster.nodes[leaderIdx]

	// 1. Client A submits ID 1.
	applyEntry(t, leader, fsm.RaftLogEntry{
		ClientID: "client-A", SubmissionID: 1, BaseRev: 0, Changeset: ot.MakeInsert(0, 0, "A"),
	})
	cluster.waitForConvergence(t)

	// 2. Client A submits ID 2.
	applyEntry(t, leader, fsm.RaftLogEntry{
		ClientID: "client-A", SubmissionID: 2, BaseRev: 1, Changeset: ot.MakeInsert(1, 1, "B"),
	})
	cluster.waitForConvergence(t)

	// 3. Client A "refreshes" and erroneously re-submits ID 1 (sequence overrun).
	// We want to ensure this returns an ACK so the client isn't stuck.
	// In the real system, Node.handleSubmit would call Raft.Apply and get the response.
	entry3 := fsm.RaftLogEntry{
		ClientID: "client-A", SubmissionID: 1, BaseRev: 2, Changeset: ot.MakeInsert(2, 2, "X"),
	}
	data3, _ := fsm.MarshalEntry(entry3)
	fut := leader.Apply(data3, 1*time.Second)
	if err := fut.Error(); err != nil {
		t.Fatalf("raft apply failed: %v", err)
	}
	
	resRaw := fut.Response()
	if resRaw == nil {
		t.Fatal("expected non-nil response for duplicate")
	}
	res := resRaw.(*fsm.ApplyResult)
	if !res.IsDuplicate {
		t.Error("expected IsDuplicate=true for sequence overrun")
	}
	if res.NewRev != 2 {
		t.Errorf("expected rev 2, got %d", res.NewRev)
	}

	// Verify document state is still "AB", hasn't changed from the rejected "X".
	text := cluster.waitForConvergence(t)
	if text != "AB" {
		t.Errorf("expected %q, got %q", "AB", text)
	}
}
