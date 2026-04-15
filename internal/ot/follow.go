// Package ot — follow.go
//
// Implements Follow(A, B) → B', also written f(A, B).
//
// Given two changesets A and B that both apply to the SAME original document
// (A.OldLen == B.OldLen), Follow computes B' such that:
//
//     Apply(Apply(doc, A), B') == Apply(Apply(doc, B), A')
//
// In other words, B' is B "rebased" on top of A.
// This is the central operation of the EasySync algorithm.
//
// The algorithm simultaneously scans A and B op-by-op:
//
//   A=Insert,  B=Insert  → tie-breaker: if A.text <= B.text, A goes first → B' Retain(len(A))
//                                        if A.text >  B.text, B goes first → B' Insert(B.text)
//   A=Insert,  B=*       → A inserted new chars; B must skip them → B' Retain(len(A.insert))
//   A=*       , B=Insert → B inserts chars independent of A       → B' Insert(B.text)
//   A=Retain,  B=Retain  → both keep char → B' Retain(n)
//   A=Retain,  B=Delete  → A kept, B removes → B' Delete(n)
//   A=Delete,  B=Retain  → A removed, B wanted to keep → B' does nothing (char is gone)
//   A=Delete,  B=Delete  → both deleted → B' does nothing
package ot

// Follow computes B' = f(A, B).
// B' is the version of B that can be applied AFTER A has already been applied.
//
// Precondition: A.OldLen == B.OldLen
func Follow(A, B Changeset) (Changeset, error) {
	result := Changeset{
		// B' applies to A's result.
		OldLen: A.NewLen,
		// NewLen will be computed as we build ops.
		NewLen: 0,
	}

	// We use the same opSlice peek/consume machinery as in compose.go.
	type opSlice struct {
		ops []Op
		pos int
		off int // byte offset into current Insert's Chars
	}

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

	// addOp appends an op, merging with the previous if same type (canonical form).
	addOp := func(op Op) {
		if len(result.Ops) > 0 {
			last := &result.Ops[len(result.Ops)-1]
			if last.Type == op.Type {
				switch op.Type {
				case OpRetain:
					last.Len += op.Len
					result.NewLen += op.Len
					return
				case OpDelete:
					last.Len += op.Len
					// delete doesn't change NewLen
					return
				case OpInsert:
					last.Chars += op.Chars
					result.NewLen += len(op.Chars)
					return
				}
			}
		}
		result.Ops = append(result.Ops, op)
		switch op.Type {
		case OpRetain:
			result.NewLen += op.Len
		case OpInsert:
			result.NewLen += len(op.Chars)
		}
	}

	aSlice := &opSlice{ops: A.Ops}
	bSlice := &opSlice{ops: B.Ops}

	for {
		aOp, aOk := peek(aSlice)
		bOp, bOk := peek(bSlice)

		if !aOk && !bOk {
			break
		}

		// ── Case: both A and B are inserting at the same position ──
		// Use lexicographic tie-breaker to guarantee commutativity:
		//   f(A,B) and f(B,A) must produce the same merged document.
		// If A.text <= B.text → A's insert goes first in the merged doc.
		//   Therefore B' must Retain over A's inserted chars, then insert B's text.
		// If B.text < A.text → B's insert goes first.
		//   Therefore B' inserts B's text first (so it precedes A's insert).
		if aOk && aOp.Type == OpInsert && bOk && bOp.Type == OpInsert {
			if aOp.Chars <= bOp.Chars {
				// A's text is placed first; B' must skip over it.
				addOp(Op{Type: OpRetain, Len: len(aOp.Chars)})
				consume(aSlice, len(aOp.Chars))
			} else {
				// B's text is placed first; B' inserts it now.
				addOp(Op{Type: OpInsert, Chars: bOp.Chars})
				consume(bSlice, len(bOp.Chars))
			}
			continue
		}

		// ── Case: A inserts, B is not inserting ──
		// A's insert produces new chars that B knows nothing about.
		// B' must Retain over those chars (skip them without touching).
		if aOk && aOp.Type == OpInsert {
			addOp(Op{Type: OpRetain, Len: len(aOp.Chars)})
			consume(aSlice, len(aOp.Chars))
			continue
		}

		// ── Case: B inserts, A is not inserting ──
		// B's intent is to insert text; that intent is preserved unchanged.
		if bOk && bOp.Type == OpInsert {
			addOp(Op{Type: OpInsert, Chars: bOp.Chars})
			consume(bSlice, len(bOp.Chars))
			continue
		}

		// ── Both A and B are now operating on original document characters ──
		// (Both are Retain or Delete, consuming the same source chars.)
		if !aOk || !bOk {
			break
		}

		// Take the minimum overlap.
		n := aOp.Len
		if bOp.Len < n {
			n = bOp.Len
		}

		switch {
		case aOp.Type == OpRetain && bOp.Type == OpRetain:
			// Both keep the chars: B' retains them.
			addOp(Op{Type: OpRetain, Len: n})

		case aOp.Type == OpRetain && bOp.Type == OpDelete:
			// A kept the chars, B wants to delete them: B' deletes.
			addOp(Op{Type: OpDelete, Len: n})

		case aOp.Type == OpDelete && bOp.Type == OpRetain:
			// A already deleted the chars. B wanted to keep them, but they're gone.
			// B' does nothing — the chars no longer exist in A's result.

		case aOp.Type == OpDelete && bOp.Type == OpDelete:
			// Both deleted the same chars. B' does nothing (already gone).
		}

		consume(aSlice, n)
		consume(bSlice, n)
	}

	return result, nil
}
