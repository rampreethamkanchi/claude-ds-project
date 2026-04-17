// tests/chaos_test.go
//
// Chaos test: 20 simulated "clients" each running as a goroutine, all firing
// changesets at a 3-node in-memory cluster concurrently.
//
// After 3 seconds of chaos, we stop submissions, wait for all entries to commit,
// then assert that ALL three nodes have exactly the same head_text and head_rev.
//
// This test is the automated equivalent of "open 20 browser tabs and type randomly".
package tests

import (
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-editor/internal/fsm"
	"distributed-editor/internal/ot"

	"distributed-editor/internal/raft"
)

// SimulatedClient models the behaviour of a real browser client.
// It generates random edits and submits them to the cluster via Raft Apply()
// (bypassing WebSocket to keep the test self-contained).
type SimulatedClient struct {
	id           string
	submissionID atomic.Int64

	// OT client state: document = A · X · Y
	mu       sync.Mutex
	A        ot.Changeset // acknowledged server state
	X        ot.Changeset // submitted, unACKed
	Y        ot.Changeset // local, unsubmitted
	serverRev int
	waitingACK bool
}

func newSimClient(id, initialText string) *SimulatedClient {
	c := &SimulatedClient{
		id: id,
		A:  ot.TextToChangeset(initialText),
	}
	n := len(initialText)
	c.X = ot.Identity(n)
	c.Y = ot.Identity(n)
	c.serverRev = 0
	return c
}

// currentText reconstructs the client's current view: apply A, then X, then Y.
func (c *SimulatedClient) currentText() string {
	text, _ := ot.ApplyChangeset("", c.A)

	xResult, err := ot.ApplyChangeset(text, c.X)
	if err == nil {
		text = xResult
	}
	yResult, err := ot.ApplyChangeset(text, c.Y)
	if err == nil {
		text = yResult
	}
	return text
}

// randomEdit generates a random insert or delete on the client's current document.
func (c *SimulatedClient) randomEdit(rng *rand.Rand) ot.Changeset {
	cur := c.currentText()
	n := len(cur)

	if n == 0 || rng.Float32() < 0.6 {
		// Insert a random letter at a random position.
		pos := rng.Intn(n + 1)
		char := string(rune('a' + rng.Intn(26)))
		return ot.MakeInsert(n, pos, char)
	}
	// Delete one character at a random position.
	pos := rng.Intn(n)
	return ot.MakeDelete(n, pos, 1)
}

// applyLocalEdit folds an edit into Y.
func (c *SimulatedClient) applyLocalEdit(e ot.Changeset) {
	c.mu.Lock()
	defer c.mu.Unlock()
	composed, err := ot.Compose(c.Y, e)
	if err == nil {
		c.Y = composed
	}
}

// TestChaos_Convergence is the main chaos test.
func TestChaos_Convergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_ = logger

	// Build 3-node in-memory cluster.
	cluster := newMemCluster(t, "")
	defer cluster.shutdown()

	leaderIdx := cluster.waitForLeader(t)
	t.Logf("chaos test: initial leader = node %d", leaderIdx)

	// We want the leader reference to always be current.
	getLeader := func() *raft.RaftNode {
		for _, r := range cluster.nodes {
			if r.IsLeader() {
				return r
			}
		}
		return cluster.nodes[leaderIdx]
	}

	// ── Spawn 20 simulated clients ────────────────────────────────────────────
	const numClients = 20
	const duration = 3 * time.Second

	var wg sync.WaitGroup
	var totalOps atomic.Int64

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientIdx int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(clientIdx) * 137))
			clientID := fmt.Sprintf("chaos-client-%03d", clientIdx)

			// Track local submission counter and base_rev.
			var subID int64
			baseRev := 0

			deadline := time.Now().Add(duration)
			for time.Now().Before(deadline) {
				// Pause between 10–100ms (simulates typing speed).
				time.Sleep(time.Duration(10+rng.Intn(90)) * time.Millisecond)

				// Generate a simple random insert.
				// We use the cluster's current head_rev as our base to keep things simple.
				// A real client would track its own A/X/Y state.
				sm := cluster.fsms[0] // read state from node 0
				currentText := sm.HeadText()
				currentRev := sm.HeadRev()
				baseRev = currentRev

				n := len(currentText)
				var cs ot.Changeset
				if n == 0 || rng.Float32() < 0.7 {
					pos := rng.Intn(n + 1)
					ch := string(rune('a' + rng.Intn(26)))
					cs = ot.MakeInsert(n, pos, ch)
				} else {
					pos := rng.Intn(n)
					cs = ot.MakeDelete(n, pos, 1)
				}

				subID++
				entry := fsm.RaftLogEntry{
					ClientID:     clientID,
					SubmissionID: subID,
					BaseRev:      baseRev,
					Changeset:    cs,
				}

				data, err := fsm.MarshalEntry(entry)
				if err != nil {
					continue
				}

				// Find the current leader and apply.
				leader := getLeader()
				if leader == nil {
					continue
				}
				fut := leader.Apply(data, 500*time.Millisecond)
				if fut.Error() == nil {
					totalOps.Add(1)
				}
			}
		}(i)
	}

	// Wait for all goroutines to finish submitting.
	wg.Wait()
	t.Logf("chaos phase done: %d total operations submitted", totalOps.Load())

	// Wait a bit for in-flight entries to commit.
	time.Sleep(500 * time.Millisecond)

	// ── Assert convergence ────────────────────────────────────────────────────
	finalText := cluster.waitForConvergence(t)
	t.Logf("all 3 nodes converged to text of length %d, rev=%d",
		len(finalText), cluster.fsms[0].HeadRev())

	// Verify revision counts match.
	rev0 := cluster.fsms[0].HeadRev()
	rev1 := cluster.fsms[1].HeadRev()
	rev2 := cluster.fsms[2].HeadRev()
	if rev0 != rev1 || rev1 != rev2 {
		t.Errorf("revision mismatch: node0=%d node1=%d node2=%d", rev0, rev1, rev2)
	}
	t.Logf("convergence verified: rev=%d text=%q", rev0, finalText)
}

// TestChaos_FollowEquivalenceMassive runs 1000 rounds of random A+B pairs
// and checks the OT math invariant — not a cluster test, but a stress test
// of the follow() function at scale.
func TestChaos_FollowEquivalenceMassive(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// Random printable ASCII string generator.
	randStr := func(maxLen int) string {
		n := rng.Intn(maxLen + 1)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(32 + rng.Intn(95)) // printable ASCII
		}
		return string(b)
	}

	for round := 0; round < 1000; round++ {
		doc := randStr(20)
		targetA := randStr(20)
		targetB := randStr(20)

		A := ot.DiffToChangeset(doc, targetA)
		B := ot.DiffToChangeset(doc, targetB)

		if err := A.Validate(); err != nil {
			t.Fatalf("round %d: A invalid: %v", round, err)
		}
		if err := B.Validate(); err != nil {
			t.Fatalf("round %d: B invalid: %v", round, err)
		}

		fAB, err := ot.Follow(A, B)
		if err != nil {
			t.Fatalf("round %d: Follow(A,B) failed: %v", round, err)
		}
		fBA, err := ot.Follow(B, A)
		if err != nil {
			t.Fatalf("round %d: Follow(B,A) failed: %v", round, err)
		}

		docA, err := ot.ApplyChangeset(doc, A)
		if err != nil {
			t.Fatalf("round %d: apply A: %v", round, err)
		}
		docB, err := ot.ApplyChangeset(doc, B)
		if err != nil {
			t.Fatalf("round %d: apply B: %v", round, err)
		}

		r1, err := ot.ApplyChangeset(docA, fAB)
		if err != nil {
			t.Fatalf("round %d: apply f(A,B): %v\nfAB=%v", round, err, fAB)
		}
		r2, err := ot.ApplyChangeset(docB, fBA)
		if err != nil {
			t.Fatalf("round %d: apply f(B,A): %v\nfBA=%v", round, err, fBA)
		}

		if r1 != r2 {
			t.Fatalf("round %d: divergence!\ndoc=%q A→%q B→%q\nr1=%q r2=%q",
				round, doc, targetA, targetB, r1, r2)
		}
	}
	t.Logf("1000 rounds of OT math verified successfully")
}
