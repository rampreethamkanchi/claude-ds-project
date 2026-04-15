// Package ot — compose.go
//
// Implements Compose(A, B) → C, where A and B are consecutive changesets such
// that A.NewLen == B.OldLen.
//
// The result C has:
//   C.OldLen == A.OldLen
//   C.NewLen == B.NewLen
//
// Semantics: Apply(Apply(doc, A), B) == Apply(doc, C)
//
// The algorithm uses two parallel cursors — one over A's ops, one over B's ops.
// A's ops describe A's output (what B sees as its source).
// B's ops describe what to do with A's output.
//
// Combination rules (A-op, B-op → result op):
//
//   B=Insert,  *       → result Insert(B)  [B inserts, doesn't consume A output]
//   A=Delete,  *       → result Delete(A)  [A deleted from source; B never saw it]
//   A=Retain,  B=Retain → result Retain
//   A=Retain,  B=Delete → result Delete
//   A=Insert,  B=Retain → result Insert(A) [A's insert passes through B unchanged]
//   A=Insert,  B=Delete → nothing          [B deletes A's inserted text]
package ot

// Compose computes the composition of A followed by B.
// Precondition: A.NewLen == B.OldLen
func Compose(A, B Changeset) (Changeset, error) {
	result := Changeset{OldLen: A.OldLen, NewLen: B.NewLen}

	// addOp appends an op, merging with the previous one if same type (canonical form).
	addOp := func(op Op) {
		if len(result.Ops) > 0 {
			last := &result.Ops[len(result.Ops)-1]
			if last.Type == op.Type {
				switch op.Type {
				case OpRetain:
					last.Len += op.Len
					return
				case OpDelete:
					last.Len += op.Len
					return
				case OpInsert:
					last.Chars += op.Chars
					return
				}
			}
		}
		result.Ops = append(result.Ops, op)
	}

	// opSlice is a cursor over an ops slice, supporting partial consumption.
	type opSlice struct {
		ops []Op
		pos int // index of current op
		off int // bytes already consumed from current op (chars for Insert, len for Retain/Delete)
	}

	// peek returns the current op with remaining length/chars (does NOT advance).
	peek := func(s *opSlice) (Op, bool) {
		if s.pos >= len(s.ops) {
			return Op{}, false
		}
		op := s.ops[s.pos]
		if op.Type == OpInsert {
			op.Chars = op.Chars[s.off:]
		} else {
			op.Len -= s.off
		}
		return op, true
	}

	// consume advances cursor by n units (bytes for Insert, characters for Retain/Delete).
	consume := func(s *opSlice, n int) {
		for n > 0 && s.pos < len(s.ops) {
			op := s.ops[s.pos]
			var rem int
			if op.Type == OpInsert {
				rem = len(op.Chars) - s.off
			} else {
				rem = op.Len - s.off
			}
			if n >= rem {
				n -= rem
				s.pos++
				s.off = 0
			} else {
				s.off += n
				n = 0
			}
		}
	}

	aSlice := &opSlice{ops: A.Ops}
	bSlice := &opSlice{ops: B.Ops}

	for {
		bOp, bOk := peek(bSlice)
		aOp, aOk := peek(aSlice)

		if !bOk && !aOk {
			break
		}

		// ── Rule 1: B inserts — emit Insert, consume nothing from A ──────────
		if bOk && bOp.Type == OpInsert {
			addOp(Op{Type: OpInsert, Chars: bOp.Chars})
			consume(bSlice, len(bOp.Chars))
			continue
		}

		// ── Rule 2: A deletes — emit Delete, consume nothing from B ──────────
		// A's delete removes source characters before B even sees them.
		if aOk && aOp.Type == OpDelete {
			addOp(Op{Type: OpDelete, Len: aOp.Len})
			consume(aSlice, aOp.Len)
			continue
		}

		// ── Both A and B are now operating on A's output characters ──────────
		// A is Retain or Insert; B is Retain or Delete.
		if !aOk || !bOk {
			break
		}

		// Determine the number of A-output characters this step covers.
		var aLen int
		if aOp.Type == OpInsert {
			aLen = len(aOp.Chars)
		} else { // OpRetain
			aLen = aOp.Len
		}
		bLen := bOp.Len // B is always Retain or Delete here
		n := aLen
		if bLen < n {
			n = bLen
		}

		switch {
		case aOp.Type == OpRetain && bOp.Type == OpRetain:
			// A kept the source char; B also keeps it → result retains it.
			addOp(Op{Type: OpRetain, Len: n})

		case aOp.Type == OpRetain && bOp.Type == OpDelete:
			// A kept the source char; B removes it → result deletes it.
			addOp(Op{Type: OpDelete, Len: n})

		case aOp.Type == OpInsert && bOp.Type == OpRetain:
			// A inserts new text; B retains it → result inserts it.
			addOp(Op{Type: OpInsert, Chars: aOp.Chars[:n]})

		case aOp.Type == OpInsert && bOp.Type == OpDelete:
			// A inserts then B immediately deletes it → net effect: nothing.
		}

		consume(aSlice, n)
		consume(bSlice, n)
	}

	return result, nil
}
