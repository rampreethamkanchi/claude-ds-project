// Package fsm — fsm.go
//
// DocumentStateMachine implements the custom raft.FSM interface.
//
// This is the central component of the system. Every Raft node runs this same
// state machine. When Raft commits a log entry, it calls Apply() on every node.
// The OT transformation (follow) runs inside Apply(), making it deterministic:
// given the same sequence of committed log entries, every server reaches the
// same document state independently.
//
// Key invariants maintained:
//  1. head_text == apply_all(empty, revision_log[*].Changeset)
//  2. All servers with the same committed log produce the same head_text.
//  3. client_last_submission prevents duplicate application of re-submitted entries.
package fsm

import (
	"log/slog"
	"sync"

	"distributed-editor/internal/ot"
)

// DocumentStateMachine is the Raft FSM that manages the collaborative document.
// All exported methods are goroutine-safe (protected by mu).
type DocumentStateMachine struct {
	mu sync.RWMutex

	// headText is the current document text (cached; derivable from revision log).
	headText string

	// headRev is the current revision index (== len(revisionLog)).
	headRev int

	// revisionLog is the append-only, ordered list of accepted revisions.
	// revisionLog[r] is the changeset that created revision r+1 from revision r.
	revisionLog []RevisionRecord

	// clientLastSubmission tracks the highest submission_id we've applied per client.
	// Used to deduplicate re-submitted entries after a leader crash.
	clientLastSubmission map[string]int64

	// logger is a structured logger with server_id bound.
	logger *slog.Logger

	// onCommit is called after every successful Apply() with the result.
	// The server WebSocket handler sets this to broadcast C' to connected clients.
	onCommit func(result ApplyResult)
}

// NewDocumentStateMachine creates a new FSM with the given initial document text.
func NewDocumentStateMachine(initialText string, logger *slog.Logger) *DocumentStateMachine {
	return &DocumentStateMachine{
		headText:             initialText,
		headRev:              0,
		revisionLog:          make([]RevisionRecord, 0),
		clientLastSubmission: make(map[string]int64),
		logger:               logger,
		onCommit:             func(ApplyResult) {}, // no-op default
	}
}

// SetOnCommit registers the callback invoked after each Apply().
// The callback is called from the Raft goroutine — it should not block.
func (fsm *DocumentStateMachine) SetOnCommit(cb func(ApplyResult)) {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	fsm.onCommit = cb
}

// ─────────────────────────────────────────────
// raft.FSM interface — Apply
// ─────────────────────────────────────────────

// Apply is called by Raft on EVERY NODE when a log entry is committed.
// This is the heart of the system: it runs the OT transformation to catch
// the client's raw changeset up from base_rev to head_rev.
//
// It returns an *ApplyResult (or nil if dedup fired), which the Raft leader
// can retrieve via raft.ApplyFuture.Response().
func (fsm *DocumentStateMachine) Apply(data []byte) interface{} {
	// Deserialize the committed log entry.
	entry, err := UnmarshalEntry(data)
	if err != nil {
		fsm.logger.Error("failed to unmarshal log entry", "err", err)
		return nil
	}

	fsm.mu.Lock()
	defer fsm.mu.Unlock()

	// ── Idempotency guard ──────────────────────────────────────────────────────
	// If we've already applied this submission_id from this client, ignore it.
	lastSub, seen := fsm.clientLastSubmission[entry.ClientID]
	if seen && entry.SubmissionID <= lastSub {
		fsm.logger.Info("skipping duplicate entry",
			"client_id", entry.ClientID,
			"submission_id", entry.SubmissionID,
			"last_applied", lastSub,
		)
		// We still return a result so the leader can ACK the client,
		// but we mark it as a duplicate and include the current headRev.
		res := &ApplyResult{
			ClientID:    entry.ClientID,
			NewRev:      fsm.headRev,
			IsDuplicate: true,
		}
		// Notify server handler so it can ACK (but not broadcast).
		go func() {
			fsm.onCommit(*res)
		}()
		return res
	}

	// ── OT Transformation ─────────────────────────────────────────────────────
	C := entry.Changeset

	for r := entry.BaseRev; r < fsm.headRev; r++ {
		historical := fsm.revisionLog[r].Changeset
		C, err = ot.Follow(historical, C)
		if err != nil {
			fsm.logger.Error("OT follow failed",
				"rev", r, "client_id", entry.ClientID, "err", err)
			return nil
		}
	}

	// ── Apply C' to document ──────────────────────────────────────────────────
	newText, err := ot.ApplyChangeset(fsm.headText, C)
	if err != nil {
		fsm.logger.Error("apply_changeset failed",
			"client_id", entry.ClientID,
			"head_rev", fsm.headRev,
			"changeset", C,
			"err", err,
		)
		return nil
	}

	// ── Append to revision log ────────────────────────────────────────────────
	record := RevisionRecord{
		RevNumber: fsm.headRev,
		Changeset: C,
		Source:    entry.ClientID,
	}
	fsm.revisionLog = append(fsm.revisionLog, record)

	// Update state.
	fsm.headText = newText
	fsm.headRev++
	fsm.clientLastSubmission[entry.ClientID] = entry.SubmissionID

	fsm.logger.Info("applied changeset",
		"client_id", entry.ClientID,
		"base_rev", entry.BaseRev,
		"new_rev", fsm.headRev,
		"new_len", len(newText),
	)

	// ── Notify server handler ─────────────────────────────────────────────────
	result := ApplyResult{
		CPrime:   C,
		NewRev:   fsm.headRev,
		ClientID: entry.ClientID,
	}

	go func() {
		fsm.onCommit(result)
	}()

	return &result
}

// ─────────────────────────────────────────────
// Read-only accessors (for the server handler)
// ─────────────────────────────────────────────

// HeadText returns the current document text.
func (fsm *DocumentStateMachine) HeadText() string {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	return fsm.headText
}

// HeadRev returns the current revision number.
func (fsm *DocumentStateMachine) HeadRev() int {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	return fsm.headRev
}

// RevisionsSince returns all revision records from `fromRev` onwards.
func (fsm *DocumentStateMachine) RevisionsSince(fromRev int) []RevisionRecord {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	if fromRev >= len(fsm.revisionLog) {
		return nil
	}
	slice := fsm.revisionLog[fromRev:]
	result := make([]RevisionRecord, len(slice))
	copy(result, slice)
	return result
}
