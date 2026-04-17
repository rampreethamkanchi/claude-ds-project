// Package raft — future.go
//
// ApplyFuture is the async result mechanism for the Apply() call.
// When a client calls RaftNode.Apply(data, timeout), it receives an
// ApplyFuture. The client blocks on future.Error() until the entry
// is either committed (returns nil) or fails (returns an error).
//
// Thread safety:
//   - respond() may be called from multiple goroutines (commit path, timeout,
//     shutdown, leadership loss). sync.Once guarantees only the first call wins.
//   - Error() may be called from any goroutine. sync.Once guarantees the
//     channel is read exactly once; subsequent calls return the cached result.
package raft

import (
	"errors"
	"sync"
)

// Common errors returned by ApplyFuture.Error().
var (
	ErrNotLeader  = errors.New("raft: not the leader")
	ErrShutdown   = errors.New("raft: server is shutting down")
	ErrTimeout    = errors.New("raft: apply timeout")
	ErrLeaderLost = errors.New("raft: leadership lost before commit")
)

// ApplyFuture represents the pending result of an Apply() call.
// It is safe to call Error() and Response() from any goroutine.
type ApplyFuture struct {
	// errCh receives the result once the entry is committed or fails.
	// Buffered with capacity 1 so respond() never blocks.
	errCh chan error

	// response holds the return value from FSM.Apply() after commit.
	response interface{}

	// mu protects response from concurrent reads/writes.
	mu sync.Mutex

	// respondOnce ensures only the FIRST call to respond() takes effect.
	// This is critical because respond() can be called from:
	//   1. The apply loop (entry committed)
	//   2. The timeout goroutine (deadline exceeded)
	//   3. failPendingFutures (leadership lost)
	//   4. Shutdown
	respondOnce sync.Once

	// readOnce ensures Error() reads from errCh exactly once.
	readOnce sync.Once
	err      error
}

// newApplyFuture creates a new pending future.
func newApplyFuture() *ApplyFuture {
	return &ApplyFuture{
		errCh: make(chan error, 1),
	}
}

// Error blocks until the entry is committed or an error occurs.
// It is safe to call Error() multiple times — subsequent calls return
// the cached result immediately.
func (f *ApplyFuture) Error() error {
	f.readOnce.Do(func() {
		f.err = <-f.errCh
	})
	return f.err
}

// Response returns the value that FSM.Apply() returned for this entry.
// Must be called AFTER Error() returns nil — otherwise the response
// is undefined.
func (f *ApplyFuture) Response() interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.response
}

// respond resolves the future with a result. Only the first call takes effect;
// subsequent calls are silently ignored (via sync.Once).
//
// This is called from:
//   - The apply loop after FSM.Apply() returns (success path)
//   - The timeout goroutine (timeout path)
//   - failPendingFutures when the leader steps down
//   - Shutdown when the server is stopping
func (f *ApplyFuture) respond(err error, response interface{}) {
	f.respondOnce.Do(func() {
		f.mu.Lock()
		f.response = response
		f.mu.Unlock()
		// Non-blocking send — channel has capacity 1.
		f.errCh <- err
	})
}

// applyRequest is an internal message sent from Apply() to the leader's
// run loop, requesting that a new entry be appended to the log.
type applyRequest struct {
	data   []byte       // the command data
	future *ApplyFuture // the future to resolve when committed
}
