// Package fsm — types.go
//
// Defines the core data structures shared between the Raft state machine,
// the storage layer, and the server's WebSocket handler.
package fsm

import (
	"distributed-editor/internal/ot"
	"encoding/json"
)

// RaftLogEntry is what gets stored in the Raft log for every client submission.
//
// IMPORTANT: We store the RAW, UN-TRANSFORMED changeset here.
// The OT transformation (follow) happens deterministically inside Apply(),
// ensuring every server node independently arrives at the same result.
type RaftLogEntry struct {
	ClientID     string        `json:"client_id"`     // UUID of the submitting client
	SubmissionID int64         `json:"submission_id"` // monotonically increasing per client (dedup)
	BaseRev      int           `json:"base_rev"`      // server revision the client's doc was based on
	Changeset    ot.Changeset  `json:"changeset"`     // raw changeset relative to BaseRev
}

// RevisionRecord is an entry in the append-only revision log stored by the state machine.
// Each record corresponds to one committed Raft entry, after OT transformation.
type RevisionRecord struct {
	RevNumber int          `json:"rev_number"` // 0-indexed revision number
	Changeset ot.Changeset `json:"changeset"`  // TRANSFORMED changeset (relative to prev revision)
	Source    string       `json:"source"`     // client_id that authored this revision
}

// ApplyResult is the return value from DocumentStateMachine.Apply().
// It carries the transformed changeset (C') needed to broadcast to connected clients.
type ApplyResult struct {
	CPrime      ot.Changeset // the transformed changeset to broadcast
	NewRev      int          // the new head revision after this commit
	ClientID    string       // originating client (so the server knows who to ACK)
	IsDuplicate bool         // true if this was a duplicate submission (not applied to doc)
}

// SnapshotData is serialized to disk by FSM.Snapshot() and read back by FSM.Restore().
// It encodes the complete state machine state so a restarting node can skip log replay.
type SnapshotData struct {
	HeadRev             int                `json:"head_rev"`
	HeadText            string             `json:"head_text"`
	RevisionLog         []RevisionRecord   `json:"revision_log"`
	ClientLastSubmission map[string]int64  `json:"client_last_submission"` // dedup map
}

// MarshalEntry serializes a RaftLogEntry to JSON bytes for the Raft log.
func MarshalEntry(e RaftLogEntry) ([]byte, error) {
	return json.Marshal(e)
}

// UnmarshalEntry deserializes a RaftLogEntry from JSON bytes.
func UnmarshalEntry(data []byte) (RaftLogEntry, error) {
	var e RaftLogEntry
	err := json.Unmarshal(data, &e)
	return e, err
}
