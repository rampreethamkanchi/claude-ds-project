// Package ot — apply.go
// Implements ApplyChangeset: given a text string and a Changeset, produce the new text.
package ot

import (
	"fmt"
	"strings"
)

// ApplyChangeset applies changeset C to text and returns the resulting string.
//
// The algorithm scans through the ops list:
//   - Retain(n): copy the next n chars from the source text to the result.
//   - Insert(s): append s to the result (no source chars consumed).
//   - Delete(n): advance the source position by n (skip those chars).
//
// Panics (returns error) if the changeset's OldLen doesn't match len(text).
func ApplyChangeset(text string, c Changeset) (string, error) {
	// Sanity check: the changeset must be designed for this document length.
	if c.OldLen != len(text) {
		return "", fmt.Errorf(
			"ApplyChangeset: changeset OldLen=%d does not match text length=%d",
			c.OldLen, len(text),
		)
	}

	var result strings.Builder
	result.Grow(c.NewLen)

	srcPos := 0 // current read position in source text

	for _, op := range c.Ops {
		switch op.Type {
		case OpRetain:
			// Copy op.Len characters verbatim from source into result.
			if srcPos+op.Len > len(text) {
				return "", fmt.Errorf(
					"ApplyChangeset: retain %d at pos %d exceeds text length %d",
					op.Len, srcPos, len(text),
				)
			}
			result.WriteString(text[srcPos : srcPos+op.Len])
			srcPos += op.Len

		case OpInsert:
			// Write the inserted characters — source position does NOT advance.
			result.WriteString(op.Chars)

		case OpDelete:
			// Skip op.Len characters in source — they are removed.
			if srcPos+op.Len > len(text) {
				return "", fmt.Errorf(
					"ApplyChangeset: delete %d at pos %d exceeds text length %d",
					op.Len, srcPos, len(text),
				)
			}
			srcPos += op.Len

		default:
			return "", fmt.Errorf("ApplyChangeset: unknown op type: %s", op.Type)
		}
	}

	// After processing all ops, the entire source must have been consumed.
	if srcPos != len(text) {
		return "", fmt.Errorf(
			"ApplyChangeset: only consumed %d of %d source chars",
			srcPos, len(text),
		)
	}

	return result.String(), nil
}
