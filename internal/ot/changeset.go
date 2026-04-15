// Package ot implements the EasySync Operational Transformation algorithm.
// This is the mathematical heart of the collaborative editor.
//
// A Changeset describes how to transform a document from one state to another.
// It is a list of operations applied sequentially:
//   - Retain(n):  keep n characters from the source document
//   - Insert(s):  insert string s at the current position
//   - Delete(n):  skip (remove) n characters from the source document
//
// Key invariant: sum(Retain.Len + Delete.Len) == changeset.OldLen
// Key invariant: sum(Retain.Len + Insert.Len)  == changeset.NewLen
package ot

import (
	"encoding/json"
	"fmt"
)

// OpType is the type of a single operation in a changeset.
type OpType string

const (
	OpRetain OpType = "retain" // keep characters from source
	OpInsert OpType = "insert" // add new characters
	OpDelete OpType = "delete" // remove characters from source
)

// Op is one operation within a Changeset.
// For Retain and Delete, Len is the number of characters.
// For Insert, Chars holds the text to insert (Len is ignored / derived from len(Chars)).
type Op struct {
	Type  OpType `json:"op"`
	Len   int    `json:"n,omitempty"`    // used by retain and delete
	Chars string `json:"chars,omitempty"` // used by insert
}

// Changeset is the fundamental unit of change in EasySync.
// OldLen is the expected length of the document BEFORE applying this changeset.
// NewLen is the length of the document AFTER applying it.
type Changeset struct {
	OldLen int  `json:"old_len"`
	NewLen int  `json:"new_len"`
	Ops    []Op `json:"ops"`
}

// String returns a human-readable representation of the changeset for logging.
func (c Changeset) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// Identity returns the identity changeset for a document of length n.
// Applying the identity to a document leaves it unchanged.
// Identity = Retain(n) entire document.
func Identity(n int) Changeset {
	if n == 0 {
		return Changeset{OldLen: 0, NewLen: 0, Ops: []Op{}}
	}
	return Changeset{
		OldLen: n,
		NewLen: n,
		Ops:    []Op{{Type: OpRetain, Len: n}},
	}
}

// IsIdentity returns true if the changeset makes no change to the document.
// A changeset is identity if it consists of only a single Retain of the full length,
// or is completely empty (zero-length document).
func IsIdentity(c Changeset) bool {
	if c.OldLen != c.NewLen {
		return false
	}
	// Empty document with no ops is also identity.
	if c.OldLen == 0 && len(c.Ops) == 0 {
		return true
	}
	// Must be a single Retain of the full document length.
	if len(c.Ops) == 1 && c.Ops[0].Type == OpRetain && c.Ops[0].Len == c.OldLen {
		return true
	}
	return false
}

// TextToChangeset converts a text string into a changeset that creates it
// from the empty document. Useful for initialising A on the client.
func TextToChangeset(text string) Changeset {
	n := len(text)
	if n == 0 {
		return Changeset{OldLen: 0, NewLen: 0, Ops: []Op{}}
	}
	return Changeset{
		OldLen: 0,
		NewLen: n,
		Ops:    []Op{{Type: OpInsert, Chars: text}},
	}
}

// MakeInsert creates a changeset that inserts `text` at position `pos`
// in a document of length `oldLen`.
func MakeInsert(oldLen, pos int, text string) Changeset {
	ops := []Op{}
	if pos > 0 {
		ops = append(ops, Op{Type: OpRetain, Len: pos})
	}
	ops = append(ops, Op{Type: OpInsert, Chars: text})
	tail := oldLen - pos
	if tail > 0 {
		ops = append(ops, Op{Type: OpRetain, Len: tail})
	}
	return Changeset{
		OldLen: oldLen,
		NewLen: oldLen + len(text),
		Ops:    ops,
	}
}

// MakeDelete creates a changeset that deletes `count` characters at position `pos`
// in a document of length `oldLen`.
func MakeDelete(oldLen, pos, count int) Changeset {
	ops := []Op{}
	if pos > 0 {
		ops = append(ops, Op{Type: OpRetain, Len: pos})
	}
	ops = append(ops, Op{Type: OpDelete, Len: count})
	tail := oldLen - pos - count
	if tail > 0 {
		ops = append(ops, Op{Type: OpRetain, Len: tail})
	}
	return Changeset{
		OldLen: oldLen,
		NewLen: oldLen - count,
		Ops:    ops,
	}
}

// Validate checks the internal consistency of a changeset.
// Useful for catching bugs during testing.
func (c Changeset) Validate() error {
	sourceConsumed := 0
	newLen := 0
	for _, op := range c.Ops {
		switch op.Type {
		case OpRetain:
			if op.Len <= 0 {
				return fmt.Errorf("retain len must be positive, got %d", op.Len)
			}
			sourceConsumed += op.Len
			newLen += op.Len
		case OpInsert:
			if len(op.Chars) == 0 {
				return fmt.Errorf("insert chars must not be empty")
			}
			newLen += len(op.Chars)
		case OpDelete:
			if op.Len <= 0 {
				return fmt.Errorf("delete len must be positive, got %d", op.Len)
			}
			sourceConsumed += op.Len
		default:
			return fmt.Errorf("unknown op type: %s", op.Type)
		}
	}
	if sourceConsumed != c.OldLen {
		return fmt.Errorf("ops consume %d chars but OldLen=%d", sourceConsumed, c.OldLen)
	}
	if newLen != c.NewLen {
		return fmt.Errorf("ops produce %d chars but NewLen=%d", newLen, c.NewLen)
	}
	return nil
}


