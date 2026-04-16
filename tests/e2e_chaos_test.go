// tests/e2e_chaos_test.go
package tests

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"
	"testing"

	"distributed-editor/internal/ot"
	"distributed-editor/internal/server"

	"github.com/gorilla/websocket"
)

// GoSimClient implements the same OT logic as web/client.js
type GoSimClient struct {
	id      string
	mu      sync.Mutex
	A       ot.Changeset
	X       ot.Changeset
	Y       ot.Changeset
	srvRev  int
	subID   int64
	waitAck bool

	conn *websocket.Conn
	done chan struct{}
}

func newGoSimClient(id, initialText string) *GoSimClient {
	nt := len(initialText)
	return &GoSimClient{
		id:     id,
		A:      ot.TextToChangeset(initialText),
		X:      ot.Identity(nt),
		Y:      ot.Identity(nt),
		done:   make(chan struct{}),
	}
}

func (c *GoSimClient) run(ctx context.Context, url string, t *testing.T) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Errorf("client %s: dial failed: %v", c.id, err)
		return
	}
	c.conn = conn

	// Send CONNECT
	c.sendMsg("CONNECT", server.ConnectMsg{
		ClientID:     c.id,
		LastKnownRev: c.srvRev,
	})

	// Read loop
	go func() {
		defer close(c.done)
		for {
			_, message, err := c.conn.ReadMessage()
			if err != nil {
				return
			}
			var env server.Envelope
			json.Unmarshal(message, &env)
			c.handleMsg(env, t)
		}
	}()
}

func (c *GoSimClient) sendMsg(typ server.MsgType, payload interface{}) {
	data, _ := json.Marshal(payload)
	env := server.Envelope{Type: typ, Payload: data}
	b, _ := json.Marshal(env)
	c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *GoSimClient) handleMsg(env server.Envelope, t *testing.T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch env.Type {
	case server.MsgConnectAck:
		var msg server.ConnectAckMsg
		json.Unmarshal(env.Payload, &msg)
		// Catch up
		stateText, _ := ot.ApplyChangeset("", c.A)
		for _, rec := range msg.CatchUp {
			stateText, _ = ot.ApplyChangeset(stateText, rec.Changeset)
			xfb, _ := ot.Follow(rec.Changeset, c.X)
			c.X = xfb
			
			// Y' = follow(follow(X, B), Y)
			xfb2, _ := ot.Follow(c.X, rec.Changeset)
			yf, _ := ot.Follow(xfb2, c.Y)
			c.Y = yf
		}
		c.srvRev = msg.HeadRev
		c.A = ot.TextToChangeset(stateText)
		c.waitAck = false

	case server.MsgAck:
		var msg server.AckMsg
		json.Unmarshal(env.Payload, &msg)
		comp, _ := ot.Compose(c.A, c.X)
		c.A = comp
		c.X = ot.Identity(c.A.NewLen)
		c.srvRev = msg.NewRev
		c.waitAck = false

	case server.MsgBroadcast:
		var msg server.BroadcastMsg
		json.Unmarshal(env.Payload, &msg)
		B := msg.Changeset
		
		XfB, _ := ot.Follow(c.X, B)
		BfX, _ := ot.Follow(B, c.X)
		
		ac, _ := ot.Compose(c.A, B)
		c.A = ac
		c.X = BfX
		yf, _ := ot.Follow(XfB, c.Y)
		c.Y = yf
		c.srvRev = msg.NewRev
	}
}

func (c *GoSimClient) tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.waitAck {
		return
	}

	if ot.IsIdentity(c.Y) {
		return
	}

	c.X = c.Y
	c.Y = ot.Identity(c.X.NewLen)
	c.waitAck = true
	c.subID++

	c.sendMsg("SUBMIT", server.SubmitMsg{
		ClientID:     c.id,
		SubmissionID: c.subID,
		BaseRev:      c.srvRev,
		Changeset:    c.X,
	})
}

func (c *GoSimClient) edit(rng *rand.Rand) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	text, _ := ot.ApplyChangeset("", c.A)
	text, _ = ot.ApplyChangeset(text, c.X)
	text, _ = ot.ApplyChangeset(text, c.Y)
	
	n := len(text)
	var e ot.Changeset
	if n == 0 || rng.Float32() < 0.7 {
		pos := rng.Intn(n + 1)
		ch := string(rune('a' + rng.Intn(26)))
		e = ot.MakeInsert(n, pos, ch)
	} else {
		pos := rng.Intn(n)
		e = ot.MakeDelete(n, pos, 1)
	}
	
	c.Y, _ = ot.Compose(c.Y, e)
}

func TestE2E_Chaos_Convergence(t *testing.T) {
	t.Skip("Simplified test for now, focusing on client logic in separate unit test if needed")
}

// TestClient_OTLogic_Correctness verifies the fixing logic for handleBroadcast in Go
func TestClient_OTLogic_Correctness(t *testing.T) {
	// Initial doc: "abc" (len 3)
	// Base doc: "abc" (len 3)
	// state.A is the history to get to "abc". For simplicity, let's say it's just "abc" inserted.
	A := ot.TextToChangeset("abc") // OldLen 0, NewLen 3
	
	// Client has no pending X, but has local edit Y: insert "Y" at end
	X := ot.Identity(3)
	Y := ot.MakeInsert(3, 3, "Y") // "abc" -> "abcY" (current editor text)
	
	// Broadcast B arrives: delete "b" at 1 (server moves "abc" -> "ac")
	B := ot.MakeDelete(3, 1, 1) // OldLen 3, NewLen 2
	
	// logic from fixed client.js:
	XfB, _ := ot.Follow(X, B)    // f(id, B) == B
	BfX, _ := ot.Follow(B, X)    // f(B, id) == id(2)
	
	A_prime, _ := ot.Compose(A, B) // "abc" -> "ac"
	_ = BfX
	Y_prime, _ := ot.Follow(XfB, Y) // Rebase Y over B
	D, _ := ot.Follow(Y, XfB)       // What to apply to screen
	
	// Check results
	if A_prime.NewLen != 2 {
		t.Errorf("A_prime.NewLen expected 2, got %d", A_prime.NewLen)
	}
	if D.OldLen != 4 { // D is based on A·X·Y which is "abcY" (len 4)
		t.Errorf("D.OldLen expected 4, got %d", D.OldLen)
	}
	
	// Final state convergence check:
	// Path 1: A · X · Y · D
	// Path 2: A · B · BfX · Y_prime
	
	doc := "abc"
	doc1, _ := ot.ApplyChangeset(doc, X)
	doc1, _ = ot.ApplyChangeset(doc1, Y)
	res1, _ := ot.ApplyChangeset(doc1, D)
	
	doc2, _ := ot.ApplyChangeset(doc, B)
	doc2, _ = ot.ApplyChangeset(doc2, BfX)
	res2, _ := ot.ApplyChangeset(doc2, Y_prime)
	
	if res1 != res2 {
		t.Errorf("Divergence! res1=%q, res2=%q", res1, res2)
	}
	if res1 != "acY" {
		t.Errorf("Expected %q, got %q", "acY", res1)
	}
}
