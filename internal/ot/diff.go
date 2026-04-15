// Package ot — diff.go
//
// DiffToChangeset computes a Changeset from oldText to newText using
// a simple longest-common-subsequence (LCS) diff.
//
// This is used by the browser client (via a JS port) and in tests.
// It is intentionally simple — the OT math doesn't care HOW the changeset
// was generated, only that it is valid (i.e., OldLen == len(oldText)).
package ot

// DiffToChangeset computes the minimal changeset to transform oldText into newText.
// It uses a simple O(n*m) LCS approach, good enough for documents of typical size.
func DiffToChangeset(oldText, newText string) Changeset {
	old := []rune(oldText)
	nw := []rune(newText)
	n, m := len(old), len(nw)

	// Build LCS table.
	// dp[i][j] = length of LCS of old[:i] and nw[:j]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if old[i-1] == nw[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				if dp[i-1][j] > dp[i][j-1] {
					dp[i][j] = dp[i-1][j]
				} else {
					dp[i][j] = dp[i][j-1]
				}
			}
		}
	}

	// Backtrack the LCS table to produce diff ops.
	// We collect raw segments: ('=', rune), ('+', rune), ('-', rune)
	type seg struct {
		kind rune // '=' retain, '+' insert, '-' delete
		r    rune
	}
	var segs []seg
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && old[i-1] == nw[j-1]:
			segs = append(segs, seg{'=', old[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			segs = append(segs, seg{'+', nw[j-1]})
			j--
		default:
			segs = append(segs, seg{'-', old[i-1]})
			i--
		}
	}

	// Reverse (we built it backwards).
	for l, r := 0, len(segs)-1; l < r; l, r = l+1, r-1 {
		segs[l], segs[r] = segs[r], segs[l]
	}

	// Merge consecutive segments of the same kind into Changeset ops.
	c := Changeset{OldLen: len(oldText), NewLen: len(newText)}
	addOp := func(op Op) {
		if len(c.Ops) > 0 {
			last := &c.Ops[len(c.Ops)-1]
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
		c.Ops = append(c.Ops, op)
	}

	for _, s := range segs {
		switch s.kind {
		case '=':
			// Retain: character count in bytes (UTF-8).
			addOp(Op{Type: OpRetain, Len: len(string(s.r))})
		case '+':
			addOp(Op{Type: OpInsert, Chars: string(s.r)})
		case '-':
			addOp(Op{Type: OpDelete, Len: len(string(s.r))})
		}
	}

	return c
}
